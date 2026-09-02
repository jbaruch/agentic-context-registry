package realize

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"sort"
)

// Planner produces a deterministic, precondition-bound realization plan.
type Planner struct {
	git gitInspector
}

// NewPlanner constructs the production planner.
func NewPlanner() *Planner {
	return &Planner{git: commandGitInspector{}}
}

func newPlanner(git gitInspector) *Planner {
	return &Planner{git: git}
}

// Plan compares complete adapter intents with the ownership ledger and project.
// Intents are the complete desired target set; omitted generated-only targets are
// removed, while omitted shared targets conflict rather than risking user data.
//
// retained is variadic so every existing caller keeps compiling unchanged: omit
// it for the default (nothing retained), or pass exactly one ledger of targets
// owned outside this invocation — an --agent subset's omitted agents. A
// retained target is never planned, never removed, and never enters the next
// ledger; it participates only in the local Git-exclusion set, so scoping the
// ledger cannot un-exclude another agent's generated outputs. Passing more than
// one ledger is rejected before any planning, rather than silently discarding
// every ledger after the first.
func (planner *Planner) Plan(projectDirectory string, current Ledger, intents []Intent, retained ...Ledger) (Plan, error) {
	current = canonicalLedger(current)
	if err := ValidateLedger(current); err != nil {
		return Plan{}, err
	}
	retainedLedger, err := resolveRetained(retained)
	if err != nil {
		return Plan{}, err
	}
	projectRoot, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return Plan{}, fmt.Errorf("open project directory %q: %w", projectDirectory, err)
	}
	defer projectRoot.Close()

	intentByPath := make(map[string]Intent, len(intents))
	paths := make([]string, 0, len(intents)+len(current.Targets))
	for _, intent := range intents {
		if err := ValidateTargetPath(intent.Path); err != nil {
			return Plan{}, fmt.Errorf("intent path: %w", err)
		}
		if _, exists := intentByPath[intent.Path]; exists {
			return Plan{}, fmt.Errorf("adapter emitted target %q more than once; combine its owned entries before planning", intent.Path)
		}
		intentByPath[intent.Path] = intent
		paths = append(paths, intent.Path)
	}
	currentByPath := make(map[string]Target, len(current.Targets))
	for _, target := range current.Targets {
		currentByPath[target.Path] = target
		if _, exists := intentByPath[target.Path]; !exists {
			paths = append(paths, target.Path)
		}
	}
	retainedPaths := make([]string, 0, len(retainedLedger.Targets))
	for _, target := range retainedLedger.Targets {
		if _, planned := intentByPath[target.Path]; planned {
			return Plan{}, fmt.Errorf("retained target %q is also an adapter intent; scope the ledger before planning", target.Path)
		}
		if _, owned := currentByPath[target.Path]; owned {
			return Plan{}, fmt.Errorf("retained target %q is also in the compared ownership ledger; scope the ledger before planning", target.Path)
		}
		retainedPaths = append(retainedPaths, target.Path)
	}
	sort.Strings(paths)
	inspected := paths
	if len(retainedPaths) != 0 {
		inspected = append(append([]string(nil), paths...), retainedPaths...)
		sort.Strings(inspected)
	}
	gitState, err := planner.git.Inspect(projectDirectory, inspected)
	if err != nil {
		return Plan{}, err
	}

	plan := Plan{NextLedger: Ledger{SchemaVersion: CurrentLedgerSchemaVersion}}
	seen := make(map[string]struct{}, len(intents))
	for _, targetPath := range paths {
		if _, already := seen[targetPath]; already {
			continue
		}
		seen[targetPath] = struct{}{}
		intent, wanted := intentByPath[targetPath]
		previous, owned := currentByPath[targetPath]
		snapshot, err := snapshotFile(projectRoot, targetPath)
		if err != nil {
			return Plan{}, err
		}
		if !wanted {
			planner.planOmitted(&plan, previous, snapshot, gitState.tracked[targetPath])
			continue
		}
		action := intent.Action
		if action == "" {
			action = ActionEnsure
		}
		switch action {
		case ActionPreserve:
			if owned {
				plan.NextLedger.Targets = append(plan.NextLedger.Targets, previous)
				if !snapshot.exists || snapshot.hash != previous.OutputHash || uint32(snapshot.mode.Perm()) != previous.Mode {
					plan.addConflict(targetPath, previous.Ownership, "cannot preserve an owned target that is missing or changed since the ledger was written")
					continue
				}
			}
			plan.Operations = append(plan.Operations, Operation{
				Kind: OperationPreserve, Path: targetPath, BeforeHash: snapshot.hash,
				OwnershipBefore: ownershipBefore(owned, previous), OwnershipAfter: ownershipBefore(owned, previous),
				Reason: "adapter explicitly preserved the current target",
			})
		case ActionRemove:
			if !owned {
				if snapshot.exists {
					plan.addConflict(targetPath, OwnershipUnmanaged, "refusing to remove a target absent from the ownership ledger")
				}
				continue
			}
			if previous.Ownership == OwnershipShared {
				planner.planSharedRemoval(&plan, previous, intent, snapshot)
			} else {
				planner.planRemoval(&plan, previous, intent, snapshot, gitState.tracked[targetPath])
			}
		case ActionEnsure:
			planner.planEnsure(&plan, previous, owned, intent, snapshot)
		default:
			return Plan{}, fmt.Errorf("intent for %q has unsupported action %q", targetPath, action)
		}
	}

	plan.NextLedger = canonicalLedger(plan.NextLedger)
	if err := planner.planGitExclusions(projectRoot, gitState, &plan, retainedLedger); err != nil {
		return Plan{}, err
	}
	plan.NextLedger = canonicalLedger(plan.NextLedger)
	if err := ValidateLedger(plan.NextLedger); err != nil {
		return Plan{}, fmt.Errorf("planned ownership ledger is invalid: %w", err)
	}
	plan.LedgerChanged = !reflect.DeepEqual(current, plan.NextLedger)
	sort.SliceStable(plan.Operations, func(left, right int) bool {
		if plan.Operations[left].Path == plan.Operations[right].Path {
			return plan.Operations[left].Kind < plan.Operations[right].Kind
		}
		return plan.Operations[left].Path < plan.Operations[right].Path
	})
	return plan, nil
}

func (planner *Planner) planEnsure(plan *Plan, previous Target, owned bool, intent Intent, snapshot fileSnapshot) {
	if len(intent.Content) > maxTargetBytes {
		plan.addConflict(intent.Path, ownershipBefore(owned, previous), fmt.Sprintf("rendered target exceeds %d MiB", maxTargetBytes>>20))
		if owned {
			plan.NextLedger.Targets = append(plan.NextLedger.Targets, previous)
		}
		return
	}
	mode := intent.Mode
	if mode == 0 {
		mode = 0o644
		if snapshot.exists {
			mode = uint32(snapshot.mode.Perm())
		}
	}
	next := Target{
		Path: intent.Path, Mode: mode, Ownership: intent.Ownership,
		OutputHash: contentHash(intent.Content), Entries: append([]Entry(nil), intent.Entries...),
	}
	if owned && next.Ownership == OwnershipGenerated {
		next.Excluded = previous.Excluded
	}
	nextLedger := canonicalLedger(Ledger{SchemaVersion: CurrentLedgerSchemaVersion, Targets: []Target{next}})
	next = nextLedger.Targets[0]
	if err := ValidateLedger(nextLedger); err != nil {
		plan.addConflict(intent.Path, ownershipBefore(owned, previous), err.Error())
		if owned {
			plan.NextLedger.Targets = append(plan.NextLedger.Targets, previous)
		}
		return
	}
	if !snapshot.exists {
		if next.Ownership == OwnershipShared {
			plan.addConflict(intent.Path, ownershipBefore(owned, previous), "a missing target has no unmanaged content and must be generated-only")
			if owned {
				plan.NextLedger.Targets = append(plan.NextLedger.Targets, previous)
			}
			return
		}
		plan.NextLedger.Targets = append(plan.NextLedger.Targets, next)
		plan.Operations = append(plan.Operations, mutation(OperationCreate, previous, owned, snapshot, next, intent.Content, "create missing managed output"))
		return
	}

	if !owned {
		if next.Ownership != OwnershipShared || !intent.ManagedIntact || intent.ObservedHash != snapshot.hash || !preserves(intent.Content, intent.PreservedContent) || hasUnpreservedContent(snapshot, intent) {
			plan.addConflict(intent.Path, OwnershipUnmanaged, "existing unmanaged target requires a shared merge bound to its observed hash and preserved unmanaged content")
			return
		}
		plan.NextLedger.Targets = append(plan.NextLedger.Targets, next)
		plan.Operations = append(plan.Operations, mutation(OperationMerge, previous, false, snapshot, next, intent.Content, "merge managed entries into an existing unmanaged target"))
		return
	}

	currentChanged := snapshot.hash != previous.OutputHash || uint32(snapshot.mode.Perm()) != previous.Mode
	if previous.Ownership == OwnershipShared {
		if !intent.ManagedIntact || intent.ObservedHash != snapshot.hash || !preserves(intent.Content, intent.PreservedContent) {
			plan.addConflict(intent.Path, previous.Ownership, "shared target merge is not bound to intact managed content and the exact observed file")
			plan.NextLedger.Targets = append(plan.NextLedger.Targets, previous)
			return
		}
		if next.Ownership == OwnershipGenerated && !intent.ExplicitDemotion {
			plan.addConflict(intent.Path, previous.Ownership, "shared ownership is sticky; request explicit demotion only after confirming no unmanaged content remains")
			plan.NextLedger.Targets = append(plan.NextLedger.Targets, previous)
			return
		}
		if next.Ownership == OwnershipGenerated && hasNonEmptyFragment(intent.PreservedContent) {
			plan.addConflict(intent.Path, previous.Ownership, "explicit demotion requires zero unmanaged content")
			plan.NextLedger.Targets = append(plan.NextLedger.Targets, previous)
			return
		}
		if next.Ownership == OwnershipShared && hasUnpreservedContent(snapshot, intent) {
			plan.addConflict(intent.Path, previous.Ownership, "shared merge must preserve at least one fragment of the observed unmanaged content")
			plan.NextLedger.Targets = append(plan.NextLedger.Targets, previous)
			return
		}
	}
	if previous.Ownership == OwnershipGenerated && currentChanged {
		if !intent.ManagedIntact {
			plan.addConflict(intent.Path, previous.Ownership, "managed generated output was modified; resolve the conflict before realizing")
			plan.NextLedger.Targets = append(plan.NextLedger.Targets, previous)
			return
		}
		if next.Ownership != OwnershipShared || intent.ObservedHash != snapshot.hash || !preserves(intent.Content, intent.PreservedContent) || hasUnpreservedContent(snapshot, intent) {
			plan.addConflict(intent.Path, previous.Ownership, "changed generated output must be promoted through a hash-bound merge that preserves unmanaged content")
			plan.NextLedger.Targets = append(plan.NextLedger.Targets, previous)
			return
		}
	}

	operationKind := OperationUpdate
	reason := "update generated managed output"
	switch {
	case previous.Ownership == OwnershipGenerated && next.Ownership == OwnershipShared:
		operationKind = OperationPromote
		reason = "promote generated-only output after unmanaged content was detected"
	case previous.Ownership == OwnershipShared && next.Ownership == OwnershipGenerated:
		operationKind = OperationDemote
		reason = "explicitly demote shared output after unmanaged content was removed"
	case previous.Ownership == OwnershipShared:
		operationKind = OperationMerge
		reason = "merge managed entries while preserving unmanaged content"
	}
	plan.NextLedger.Targets = append(plan.NextLedger.Targets, next)
	if snapshot.hash == next.OutputHash && uint32(snapshot.mode.Perm()) == next.Mode && reflect.DeepEqual(previous, next) {
		return
	}
	plan.Operations = append(plan.Operations, mutation(operationKind, previous, true, snapshot, next, intent.Content, reason))
}

func (planner *Planner) planOmitted(plan *Plan, previous Target, snapshot fileSnapshot, tracked bool) {
	if previous.Ownership == OwnershipShared {
		plan.addConflict(previous.Path, previous.Ownership, "shared target removal requires an adapter-rendered intent that removes only recorded entries")
		plan.NextLedger.Targets = append(plan.NextLedger.Targets, previous)
		return
	}
	planner.planRemoval(plan, previous, Intent{}, snapshot, tracked)
}

func (planner *Planner) planRemoval(plan *Plan, previous Target, intent Intent, snapshot fileSnapshot, tracked bool) {
	if !snapshot.exists {
		return
	}
	if tracked && snapshot.hash == previous.OutputHash && uint32(snapshot.mode.Perm()) == previous.Mode {
		retained := snapshot.content
		reason := "drop ownership while retaining a tracked target"
		if intent.Action == ActionRemove {
			if len(intent.Entries) != 0 ||
				(intent.Ownership != OwnershipShared && intent.Ownership != OwnershipUnmanaged) ||
				!intent.ManagedIntact || intent.ObservedHash != snapshot.hash ||
				!hasNonEmptyFragment(intent.PreservedContent) ||
				!preserves(intent.Content, intent.PreservedContent) || hasUnpreservedContent(snapshot, intent) ||
				(intent.Mode != 0 && intent.Mode != uint32(snapshot.mode.Perm())) {
				plan.addConflict(previous.Path, previous.Ownership, "tracked generated target removal requires a hash-bound nonempty preservation proof")
				plan.NextLedger.Targets = append(plan.NextLedger.Targets, previous)
				return
			}
			retained = intent.Content
			reason = "remove managed content while retaining a tracked target"
		}
		plan.Operations = append(plan.Operations, Operation{
			Kind: OperationRemove, Path: previous.Path, OwnershipBefore: previous.Ownership,
			OwnershipAfter: OwnershipUnmanaged, BeforeHash: snapshot.hash, AfterHash: contentHash(retained),
			Reason: reason, Mode: uint32(snapshot.mode.Perm()),
			content: append([]byte(nil), retained...), beforeExists: true, beforeMode: uint32(snapshot.mode.Perm()),
		})
		return
	}
	if snapshot.hash != previous.OutputHash || uint32(snapshot.mode.Perm()) != previous.Mode {
		if intent.Action == ActionRemove && len(intent.Entries) == 0 &&
			(intent.Ownership == OwnershipShared || intent.Ownership == OwnershipUnmanaged) &&
			intent.ManagedIntact && intent.ObservedHash == snapshot.hash &&
			hasNonEmptyFragment(intent.PreservedContent) &&
			preserves(intent.Content, intent.PreservedContent) && !hasUnpreservedContent(snapshot, intent) &&
			(intent.Mode == 0 || intent.Mode == uint32(snapshot.mode.Perm())) {
			plan.Operations = append(plan.Operations, Operation{
				Kind: OperationRemove, Path: previous.Path, OwnershipBefore: previous.Ownership,
				OwnershipAfter: OwnershipUnmanaged, BeforeHash: snapshot.hash, AfterHash: contentHash(intent.Content),
				Reason: "remove managed entries while retaining newly added unmanaged content", Mode: uint32(snapshot.mode.Perm()),
				content: append([]byte(nil), intent.Content...), beforeExists: true, beforeMode: uint32(snapshot.mode.Perm()),
			})
			return
		}
		plan.addConflict(previous.Path, previous.Ownership, "refusing to remove modified managed output")
		plan.NextLedger.Targets = append(plan.NextLedger.Targets, previous)
		return
	}
	plan.Operations = append(plan.Operations, Operation{
		Kind: OperationRemove, Path: previous.Path, OwnershipBefore: previous.Ownership,
		OwnershipAfter: OwnershipUnmanaged, BeforeHash: snapshot.hash, Reason: "remove output proven to be wholly tool-owned",
		remove: true, beforeExists: true, beforeMode: uint32(snapshot.mode.Perm()),
	})
}

func (planner *Planner) planSharedRemoval(plan *Plan, previous Target, intent Intent, snapshot fileSnapshot) {
	conflict := func(reason string) {
		plan.addConflict(previous.Path, previous.Ownership, reason)
		plan.NextLedger.Targets = append(plan.NextLedger.Targets, previous)
	}
	if !snapshot.exists {
		conflict("cannot remove managed entries from a missing shared target")
		return
	}
	if len(intent.Content) > maxTargetBytes {
		conflict(fmt.Sprintf("rendered target exceeds %d MiB", maxTargetBytes>>20))
		return
	}
	if len(intent.Entries) != 0 {
		conflict("final shared-target removal must not retain ownership entries")
		return
	}
	if intent.Ownership != "" && intent.Ownership != OwnershipUnmanaged {
		conflict("final shared-target removal must transition to unmanaged ownership")
		return
	}
	mode := uint32(snapshot.mode.Perm())
	if intent.Mode != 0 && intent.Mode != mode {
		conflict("final shared-target removal cannot change unmanaged file permissions")
		return
	}
	if !intent.ManagedIntact || intent.ObservedHash != snapshot.hash || !preserves(intent.Content, intent.PreservedContent) {
		conflict("shared target removal must be bound to the exact observed file, intact managed entries, and preserved unmanaged content")
		return
	}
	plan.Operations = append(plan.Operations, Operation{
		Kind: OperationRemove, Path: previous.Path, OwnershipBefore: previous.Ownership,
		OwnershipAfter: OwnershipUnmanaged, BeforeHash: snapshot.hash, AfterHash: contentHash(intent.Content),
		Reason: "remove final managed entries while preserving unmanaged content", Mode: mode,
		content: append([]byte(nil), intent.Content...), beforeExists: true, beforeMode: mode,
	})
}

// resolveRetained accepts the optional retained ledger described on Plan.
func resolveRetained(retained []Ledger) (Ledger, error) {
	switch len(retained) {
	case 0:
		return Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil
	case 1:
		ledger := canonicalLedger(retained[0])
		if err := ValidateLedger(ledger); err != nil {
			return Ledger{}, fmt.Errorf("retained ownership ledger is invalid: %w", err)
		}
		return ledger, nil
	default:
		return Ledger{}, fmt.Errorf("planning accepts at most one retained ownership ledger, got %d", len(retained))
	}
}

func (plan *Plan) addConflict(targetPath string, before Ownership, reason string) {
	plan.Operations = append(plan.Operations, Operation{
		Kind: OperationConflict, Path: targetPath, OwnershipBefore: before,
		OwnershipAfter: before, Reason: reason,
	})
}

func (planner *Planner) planGitExclusions(root *os.Root, gitState gitContext, plan *Plan, retained Ledger) error {
	if !gitState.enabled {
		for index := range plan.NextLedger.Targets {
			plan.NextLedger.Targets[index].Excluded = false
		}
		return nil
	}
	var excluded []string
	for index := range plan.NextLedger.Targets {
		target := &plan.NextLedger.Targets[index]
		target.Excluded = target.Ownership == OwnershipGenerated && !gitState.tracked[target.Path]
		if target.Excluded {
			excluded = append(excluded, target.Path)
		}
	}
	// A retained target is owned outside this invocation and keeps its
	// exclusion: rewriting the block from the scoped ledger alone would expose
	// another agent's generated outputs as untracked files.
	for _, target := range retained.Targets {
		if target.Ownership == OwnershipGenerated && !gitState.tracked[target.Path] {
			excluded = append(excluded, target.Path)
		}
	}
	excludeRoot := root
	excludePath := gitExcludePath
	if gitState.excludeRoot != "" {
		if gitState.excludePath == "" {
			return fmt.Errorf("resolved Git exclusion root %q has no file path", gitState.excludeRoot)
		}
		opened, err := os.OpenRoot(gitState.excludeRoot)
		if err != nil {
			return fmt.Errorf("open Git exclusion directory %q: %w", gitState.excludeRoot, err)
		}
		defer opened.Close()
		excludeRoot = opened
		excludePath = gitState.excludePath
	}
	snapshot, err := snapshotFile(excludeRoot, excludePath)
	if err != nil {
		return err
	}
	updated, err := rewriteGitExclude(snapshot.content, excluded)
	if err != nil {
		plan.addConflict(gitExcludePath, OwnershipShared, err.Error())
		return nil
	}
	if bytes.Equal(snapshot.content, updated) {
		return nil
	}
	mode := uint32(0o644)
	if snapshot.exists {
		mode = uint32(snapshot.mode.Perm())
	}
	plan.Operations = append(plan.Operations, Operation{
		Kind: OperationMerge, Path: gitExcludePath, OwnershipBefore: OwnershipShared,
		OwnershipAfter: OwnershipShared, BeforeHash: snapshot.hash, AfterHash: contentHash(updated),
		Reason: "synchronize local Git exclusions with generated-only ownership", GitExclusion: true,
		Mode: mode, content: updated, beforeExists: snapshot.exists, beforeMode: uint32(snapshot.mode.Perm()),
		physicalRoot: gitState.excludeRoot, physicalPath: gitState.excludePath,
	})
	return nil
}

func mutation(kind OperationKind, previous Target, owned bool, snapshot fileSnapshot, next Target, content []byte, reason string) Operation {
	return Operation{
		Kind: kind, Path: next.Path, OwnershipBefore: ownershipBefore(owned, previous),
		OwnershipAfter: next.Ownership, BeforeHash: snapshot.hash, AfterHash: next.OutputHash,
		Reason: reason, Mode: next.Mode, content: append([]byte(nil), content...),
		beforeExists: snapshot.exists, beforeMode: uint32(snapshot.mode.Perm()),
	}
}

func ownershipBefore(owned bool, previous Target) Ownership {
	if owned {
		return previous.Ownership
	}
	return OwnershipUnmanaged
}

func preserves(rendered []byte, fragments [][]byte) bool {
	for _, fragment := range fragments {
		if len(fragment) != 0 && !bytes.Contains(rendered, fragment) {
			return false
		}
	}
	return true
}

// hasUnpreservedContent closes the gap where preserves is vacuously true for
// an empty (or all-empty) PreservedContent: a non-empty observed file being
// merged into shared ownership must bind at least one non-empty preserved
// fragment, or an adapter could whole-file-replace a shared target while
// still satisfying every other merge precondition.
func hasUnpreservedContent(snapshot fileSnapshot, intent Intent) bool {
	if !snapshot.exists || len(snapshot.content) == 0 {
		return false
	}
	for _, fragment := range intent.PreservedContent {
		if len(fragment) != 0 {
			return false
		}
	}
	return true
}

func hasNonEmptyFragment(fragments [][]byte) bool {
	for _, fragment := range fragments {
		if len(fragment) != 0 {
			return true
		}
	}
	return false
}
