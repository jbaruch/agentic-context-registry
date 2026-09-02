package dependency

import (
	"context"
	"fmt"
	"reflect"
	"sort"
)

// Service owns project declaration and lockfile operations.
type Service struct {
	resolver *Resolver
	holds    HoldPolicy
}

// NewService constructs dependency project operations that honor the rollback
// holds declared in agents.yaml.
func NewService(resolver *Resolver) *Service {
	return &Service{resolver: resolver, holds: NewHoldPolicy(resolver)}
}

// NewServiceWithHoldPolicy constructs dependency operations with a substituted
// rollback-hold provider.
func NewServiceWithHoldPolicy(resolver *Resolver, holds HoldPolicy) *Service {
	if holds == nil {
		holds = noHolds{}
	}
	return &Service{resolver: resolver, holds: holds}
}

// ChangeResult describes a mutating dependency operation.
type ChangeResult struct {
	Changed      bool               `json:"changed"`
	Dependencies []LockedDependency `json:"dependencies"`
	Held         []string           `json:"held,omitempty"`
	Resumed      []string           `json:"resumed,omitempty"`
	Notices      []string           `json:"notices,omitempty"`
}

// DependencyStatus pairs requested policy with its optional immutable lock.
type DependencyStatus struct {
	Declaration Declaration       `json:"declaration"`
	Locked      *LockedDependency `json:"locked,omitempty"`
}

// OutdatedStatus classifies one reported latest declaration.
type OutdatedStatus string

const (
	// OutdatedUpdate is an ordinary newer stable release.
	OutdatedUpdate OutdatedStatus = "update"
	// OutdatedHeld is a rollback hold whose barrier still stands.
	OutdatedHeld OutdatedStatus = "held"
	// OutdatedBeyondBarrier is a release published past a rollback barrier,
	// which only acr resume may adopt.
	OutdatedBeyondBarrier OutdatedStatus = "beyond-barrier"
)

// OutdatedDependency reports one latest declaration that has advanced.
type OutdatedDependency struct {
	Source        string         `json:"source"`
	Status        OutdatedStatus `json:"status"`
	CurrentTag    string         `json:"currentTag,omitempty"`
	CurrentCommit string         `json:"currentCommit,omitempty"`
	LatestTag     string         `json:"latestTag"`
	LatestCommit  string         `json:"latestCommit"`
	Hold          *Hold          `json:"hold,omitempty"`
	ResumeCommand string         `json:"resumeCommand,omitempty"`
	Notice        string         `json:"notice,omitempty"`
}

// Actionable reports whether an ordinary reconcile would act on this row. A
// held steady state is reported when the operator asks, and stays silent at
// session start.
func (outdated OutdatedDependency) Actionable() bool {
	return outdated.Status != OutdatedHeld
}

// Install adds or changes one declaration and resolves it. A choice is
// required, and only accepted, when the requested reference rolls a latest
// declaration backwards.
func (service *Service) Install(ctx context.Context, root, source, requested string, choice DowngradeChoice, dryRun bool) (ChangeResult, error) {
	if _, err := ParseSource(source); err != nil {
		return ChangeResult{}, err
	}
	if err := validateRequested(requested); err != nil {
		return ChangeResult{}, fmt.Errorf("invalid requested policy %q for %s: %w", requested, source, err)
	}
	state, err := LoadState(root)
	if err != nil {
		return ChangeResult{}, err
	}
	before := cloneState(state)
	declaration := Declaration{Source: source, Requested: requested}
	refresh := requested == "latest"
	if index, exists := findDeclaration(state.Project.Dependencies, source); exists {
		previous := state.Project.Dependencies[index]
		declaration, refresh, err = applyDowngradeChoice(previous, lockFor(state, source), declaration, choice)
		if err != nil {
			return ChangeResult{}, err
		}
		refresh = refresh || requested == "latest" || previous.Requested != declaration.Requested
		declaration.Extra = previous.Extra
		state.Project.Dependencies[index] = declaration
	} else {
		if choice != DowngradeUnset {
			return ChangeResult{}, fmt.Errorf("%s is not declared, so --%s has nothing to roll back; run 'acr install %s' first", source, choice, source)
		}
		refresh = true
		state.Project.Dependencies = append(state.Project.Dependencies, declaration)
	}
	state, outcome, err := service.resolveState(ctx, state, map[string]bool{source: refresh})
	if err != nil {
		return ChangeResult{}, err
	}
	changed := !reflect.DeepEqual(before, state)
	if changed && !dryRun {
		if err := WriteState(root, state); err != nil {
			return ChangeResult{}, err
		}
	}
	return ChangeResult{Changed: changed, Dependencies: state.Lock.Dependencies, Held: outcome.held, Notices: outcome.notices}, nil
}

// Reconcile refreshes every latest declaration and preserves immutable pins.
func (service *Service) Reconcile(ctx context.Context, root string, dryRun bool) (ChangeResult, error) {
	state, err := LoadState(root)
	if err != nil {
		return ChangeResult{}, err
	}
	before := cloneState(state)
	refresh := make(map[string]bool)
	for _, declaration := range state.Project.Dependencies {
		if declaration.Requested == "latest" {
			refresh[declaration.Source] = true
		}
	}
	state, outcome, err := service.resolveState(ctx, state, refresh)
	if err != nil {
		return ChangeResult{}, err
	}
	changed := !reflect.DeepEqual(before, state)
	if changed && !dryRun {
		if err := WriteState(root, state); err != nil {
			return ChangeResult{}, err
		}
	}
	return ChangeResult{Changed: changed, Dependencies: state.Lock.Dependencies, Held: outcome.held, Notices: outcome.notices}, nil
}

// Update refreshes one or all latest declarations without changing pins.
func (service *Service) Update(ctx context.Context, root, source string, dryRun bool) (ChangeResult, error) {
	state, err := LoadState(root)
	if err != nil {
		return ChangeResult{}, err
	}
	refresh := make(map[string]bool)
	if source != "" {
		if _, err := ParseSource(source); err != nil {
			return ChangeResult{}, err
		}
		index, exists := findDeclaration(state.Project.Dependencies, source)
		if !exists {
			return ChangeResult{}, fmt.Errorf("dependency %s is not declared; run 'acr install %s' first", source, source)
		}
		if state.Project.Dependencies[index].Requested == "latest" {
			refresh[source] = true
		}
	} else {
		for _, declaration := range state.Project.Dependencies {
			if declaration.Requested == "latest" {
				refresh[declaration.Source] = true
			}
		}
	}
	before := cloneState(state)
	state, outcome, err := service.resolveState(ctx, state, refresh)
	if err != nil {
		return ChangeResult{}, err
	}
	changed := !reflect.DeepEqual(before, state)
	if changed && !dryRun {
		if err := WriteState(root, state); err != nil {
			return ChangeResult{}, err
		}
	}
	return ChangeResult{Changed: changed, Dependencies: state.Lock.Dependencies, Held: outcome.held, Notices: outcome.notices}, nil
}

// Resume clears one rollback hold and resolves latest again. It is the only
// path back to latest, and it writes through the same two-file transaction as
// install.
func (service *Service) Resume(ctx context.Context, root, source string, dryRun bool) (ChangeResult, error) {
	if _, err := ParseSource(source); err != nil {
		return ChangeResult{}, err
	}
	state, err := LoadState(root)
	if err != nil {
		return ChangeResult{}, err
	}
	index, exists := findDeclaration(state.Project.Dependencies, source)
	if !exists {
		return ChangeResult{}, fmt.Errorf("dependency %s is not declared; run 'acr install %s' first", source, source)
	}
	if state.Project.Dependencies[index].Hold == nil {
		return ChangeResult{}, fmt.Errorf("%s has no rollback hold; nothing to resume", source)
	}
	before := cloneState(state)
	state.Project.Dependencies[index].Hold = nil
	if lockIndex, locked := findLock(state.Lock.Dependencies, source); locked {
		state.Lock.Dependencies[lockIndex].Hold = nil
	}
	state, outcome, err := service.resolveState(ctx, state, map[string]bool{source: true})
	if err != nil {
		return ChangeResult{}, err
	}
	changed := !reflect.DeepEqual(before, state)
	if changed && !dryRun {
		if err := WriteState(root, state); err != nil {
			return ChangeResult{}, err
		}
	}
	return ChangeResult{Changed: changed, Dependencies: state.Lock.Dependencies, Held: outcome.held, Resumed: []string{source}, Notices: outcome.notices}, nil
}

// List returns stable declaration and lock status without network access.
func (service *Service) List(root string) ([]DependencyStatus, error) {
	state, err := LoadState(root)
	if err != nil {
		return nil, err
	}
	statuses := make([]DependencyStatus, 0, len(state.Project.Dependencies))
	for _, declaration := range state.Project.Dependencies {
		status := DependencyStatus{Declaration: declaration}
		if index, exists := findLock(state.Lock.Dependencies, declaration.Source); exists {
			locked := state.Lock.Dependencies[index]
			status.Locked = &locked
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// Outdated resolves latest identities without downloading archives or writing.
func (service *Service) Outdated(ctx context.Context, root string) ([]OutdatedDependency, error) {
	state, err := LoadState(root)
	if err != nil {
		return nil, err
	}
	var result []OutdatedDependency
	for _, declaration := range state.Project.Dependencies {
		if declaration.Requested != "latest" {
			continue
		}
		release, commit, err := service.resolver.LatestCommit(ctx, declaration.Source)
		if err != nil {
			return nil, err
		}
		outdated := OutdatedDependency{Source: declaration.Source, Status: OutdatedUpdate, LatestTag: release.Tag, LatestCommit: commit}
		var existing *LockedDependency
		current := false
		if index, exists := findLock(state.Lock.Dependencies, declaration.Source); exists {
			locked := state.Lock.Dependencies[index]
			existing = &locked
			outdated.CurrentTag = locked.Tag
			outdated.CurrentCommit = locked.Commit
			current = outdated.CurrentCommit == outdated.LatestCommit && outdated.CurrentTag == outdated.LatestTag && locked.ReleaseID == release.ID
		}
		if current && declaration.Hold == nil {
			continue
		}
		decision, err := service.holds.Resolve(ctx, declaration, existing, release)
		if err != nil {
			return nil, fmt.Errorf("resolve hold for %s: %w", declaration.Source, err)
		}
		if decision.Skip && decision.Pin != nil {
			return nil, fmt.Errorf("resolve hold for %s: decision cannot both skip and pin", declaration.Source)
		}
		if decision.Skip || decision.Pin != nil {
			outdated.Status = OutdatedHeld
			outdated.Hold = cloneHold(declaration.Hold)
			if decision.Notice != "" {
				outdated.Status = OutdatedBeyondBarrier
				outdated.ResumeCommand = "acr resume " + declaration.Source
				outdated.Notice = decision.Notice
			}
			result = append(result, outdated)
			continue
		}
		if current {
			continue
		}
		result = append(result, outdated)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Source < result[right].Source })
	return result, nil
}

func lockFor(state State, source string) *LockedDependency {
	index, exists := findLock(state.Lock.Dependencies, source)
	if !exists {
		return nil
	}
	locked := state.Lock.Dependencies[index]
	return &locked
}

// resolveOutcome reports what one resolution pass did beyond the state itself.
type resolveOutcome struct {
	notices []string
	held    []string
}

func (service *Service) resolveState(ctx context.Context, state State, refresh map[string]bool) (State, resolveOutcome, error) {
	locks := make([]LockedDependency, 0, len(state.Project.Dependencies))
	var outcome resolveOutcome
	for _, declaration := range state.Project.Dependencies {
		var existing *LockedDependency
		if index, exists := findLock(state.Lock.Dependencies, declaration.Source); exists {
			locked := state.Lock.Dependencies[index]
			existing = &locked
			if locked.Requested == declaration.Requested && !refresh[declaration.Source] {
				locks = append(locks, locked)
				continue
			}
		}
		var locked LockedDependency
		var err error
		if declaration.Requested == "latest" {
			var release Release
			release, err = service.resolver.LatestRelease(ctx, declaration.Source)
			if err == nil {
				var decision HoldDecision
				decision, err = service.holds.Resolve(ctx, declaration, existing, release)
				if decision.Notice != "" {
					outcome.notices = append(outcome.notices, decision.Notice)
				}
				if err == nil {
					if decision.Skip && decision.Pin != nil {
						err = fmt.Errorf("hold for %s cannot both skip and pin", declaration.Source)
					}
				}
				if err == nil {
					switch {
					case decision.Skip:
						outcome.held = append(outcome.held, declaration.Source)
						if existing == nil {
							err = fmt.Errorf("hold for %s cannot skip without an existing lock", declaration.Source)
						} else {
							// Retain the resolved lock data but never a stale
							// requested policy from before a re-declaration.
							locked = *existing
							locked.Requested = declaration.Requested
						}
					case decision.Pin != nil:
						outcome.held = append(outcome.held, declaration.Source)
						locked = *decision.Pin
						err = validateHeldPin(declaration, locked)
					default:
						locked, err = service.resolver.ResolveAt(ctx, declaration, release)
					}
				}
			}
		} else {
			locked, err = service.resolver.Resolve(ctx, declaration)
		}
		if err != nil {
			return State{}, resolveOutcome{}, fmt.Errorf("resolve %s@%s: %w", declaration.Source, declaration.Requested, err)
		}
		locks = append(locks, locked)
	}
	state.Lock.Dependencies = locks
	sortState(&state.Project, &state.Lock)
	sort.Strings(outcome.held)
	return state, outcome, nil
}

func validateHeldPin(declaration Declaration, locked LockedDependency) error {
	project := Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{declaration}}
	lock := Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{locked}}
	if err := validateState(project, lock); err != nil {
		return fmt.Errorf("hold returned an invalid pinned lock: %w", err)
	}
	return nil
}

func cloneState(state State) State {
	project := state.Project
	project.Agents = append([]string(nil), state.Project.Agents...)
	project.Dependencies = append([]Declaration(nil), state.Project.Dependencies...)
	for index := range project.Dependencies {
		project.Dependencies[index].Hold = cloneHold(project.Dependencies[index].Hold)
	}
	lock := state.Lock
	lock.Dependencies = append([]LockedDependency(nil), state.Lock.Dependencies...)
	for index := range lock.Dependencies {
		lock.Dependencies[index].Hold = cloneLockHold(lock.Dependencies[index].Hold)
	}
	return State{Project: project, Lock: lock}
}
