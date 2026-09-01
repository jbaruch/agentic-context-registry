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
}

// NewService constructs dependency project operations.
func NewService(resolver *Resolver) *Service {
	return &Service{resolver: resolver}
}

// ChangeResult describes a mutating dependency operation.
type ChangeResult struct {
	Changed      bool               `json:"changed"`
	Dependencies []LockedDependency `json:"dependencies"`
}

// DependencyStatus pairs requested policy with its optional immutable lock.
type DependencyStatus struct {
	Declaration Declaration       `json:"declaration"`
	Locked      *LockedDependency `json:"locked,omitempty"`
}

// OutdatedDependency reports one latest declaration that has advanced.
type OutdatedDependency struct {
	Source        string `json:"source"`
	CurrentTag    string `json:"currentTag,omitempty"`
	CurrentCommit string `json:"currentCommit,omitempty"`
	LatestTag     string `json:"latestTag"`
	LatestCommit  string `json:"latestCommit"`
}

// Install adds or changes one declaration and resolves it.
func (service *Service) Install(ctx context.Context, root, source, requested string, dryRun bool) (ChangeResult, error) {
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
		refresh = refresh || state.Project.Dependencies[index].Requested != requested
		declaration.Extra = state.Project.Dependencies[index].Extra
		state.Project.Dependencies[index] = declaration
	} else {
		refresh = true
		state.Project.Dependencies = append(state.Project.Dependencies, declaration)
	}
	state, err = service.resolveState(ctx, state, map[string]bool{source: refresh})
	if err != nil {
		return ChangeResult{}, err
	}
	changed := !reflect.DeepEqual(before, state)
	if changed && !dryRun {
		if err := WriteState(root, state); err != nil {
			return ChangeResult{}, err
		}
	}
	return ChangeResult{Changed: changed, Dependencies: state.Lock.Dependencies}, nil
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
	state, err = service.resolveState(ctx, state, refresh)
	if err != nil {
		return ChangeResult{}, err
	}
	changed := !reflect.DeepEqual(before, state)
	if changed && !dryRun {
		if err := WriteState(root, state); err != nil {
			return ChangeResult{}, err
		}
	}
	return ChangeResult{Changed: changed, Dependencies: state.Lock.Dependencies}, nil
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
	state, err = service.resolveState(ctx, state, refresh)
	if err != nil {
		return ChangeResult{}, err
	}
	changed := !reflect.DeepEqual(before, state)
	if changed && !dryRun {
		if err := WriteState(root, state); err != nil {
			return ChangeResult{}, err
		}
	}
	return ChangeResult{Changed: changed, Dependencies: state.Lock.Dependencies}, nil
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
		outdated := OutdatedDependency{Source: declaration.Source, LatestTag: release.Tag, LatestCommit: commit}
		if index, exists := findLock(state.Lock.Dependencies, declaration.Source); exists {
			outdated.CurrentTag = state.Lock.Dependencies[index].Tag
			outdated.CurrentCommit = state.Lock.Dependencies[index].Commit
			if outdated.CurrentCommit == outdated.LatestCommit {
				continue
			}
		}
		result = append(result, outdated)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Source < result[right].Source })
	return result, nil
}

func (service *Service) resolveState(ctx context.Context, state State, refresh map[string]bool) (State, error) {
	locks := make([]LockedDependency, 0, len(state.Project.Dependencies))
	for _, declaration := range state.Project.Dependencies {
		if index, exists := findLock(state.Lock.Dependencies, declaration.Source); exists {
			existing := state.Lock.Dependencies[index]
			if existing.Requested == declaration.Requested && !refresh[declaration.Source] {
				locks = append(locks, existing)
				continue
			}
		}
		locked, err := service.resolver.Resolve(ctx, declaration)
		if err != nil {
			return State{}, fmt.Errorf("resolve %s@%s: %w", declaration.Source, declaration.Requested, err)
		}
		locks = append(locks, locked)
	}
	state.Lock.Dependencies = locks
	sortState(&state.Project, &state.Lock)
	return state, nil
}

func cloneState(state State) State {
	project := state.Project
	project.Dependencies = append([]Declaration(nil), state.Project.Dependencies...)
	lock := state.Lock
	lock.Dependencies = append([]LockedDependency(nil), state.Lock.Dependencies...)
	return State{Project: project, Lock: lock}
}
