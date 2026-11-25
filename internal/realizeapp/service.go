// Package realizeapp wires resolved packages, native adapters, preservation,
// and the transactional realization engine.
package realizeapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

type packageLoader interface {
	MaterializeLocked(context.Context, dependency.LockedDependency) (dependency.MaterializedPackage, func() error, error)
}

// stateWriter persists dependency state through one transaction. It is a
// field so a test can inject a failing writer and observe that the engine
// rolls every file operation back.
type stateWriter func(string, dependency.State) error

// stateMarshaler prepares both dependency state files before the journal is
// staged. It is a field so tests can prove a preparation failure writes no
// native output or state.
type stateMarshaler func(dependency.State) ([]byte, []byte, error)

type projectPackageLoader interface {
	MaterializeLockedAt(context.Context, string, dependency.LockedDependency) (dependency.MaterializedPackage, func() error, error)
}

// Service realizes immutable dependency locks through selected native adapters.
type Service struct {
	loader           packageLoader
	engine           *realize.Engine
	writeState       stateWriter
	marshalState     stateMarshaler
	removeVendorTree func(string, realize.VendorTreeRemovalPlan) error
}

// NewService constructs the production realization service.
func NewService(loader packageLoader) *Service {
	return &Service{
		loader: loader, engine: realize.NewEngine(),
		writeState: dependency.WriteState, marshalState: dependency.MarshalState,
		removeVendorTree: realize.ApplyVendorTreeRemoval,
	}
}

// MaterializationError reports a locked dependency that could not be
// downloaded or revalidated. Materialization runs before the root snapshot and
// before any operation is planned, so this error touches no file and neither
// state file. It is typed so uninstall can explain why removing one package
// needed the sources of the packages that remain.
type MaterializationError struct {
	Source string
	Err    error
}

// ConcurrentStateChangeError reports dependency state that changed after the
// caller derived its desired realization but before the apply claim was held.
type ConcurrentStateChangeError struct{}

// Error gives the only safe recovery: rebuild the plan from current state.
func (*ConcurrentStateChangeError) Error() string {
	return "agents.yaml or .agents/registry.lock changed while realization was preparing; retry the command against current project state"
}

// Error keeps the diagnostic the realization path has always emitted.
func (err *MaterializationError) Error() string {
	return fmt.Sprintf("materialize %s: %v", err.Source, err.Err)
}

// Unwrap exposes the loader failure.
func (err *MaterializationError) Unwrap() error {
	return err.Err
}

// Result describes a realization plan without exposing rendered file bodies.
type Result struct {
	Agents  []string         `json:"agents"`
	Plan    realize.Plan     `json:"plan"`
	Notices []adapter.Notice `json:"notices,omitempty"`
}

// Run renders and executes one realization mode against the project's own
// persisted dependency state.
func (service *Service) Run(ctx context.Context, projectDirectory string, selected []string, mode realize.Mode) (Result, error) {
	if mode == realize.ModeApply {
		// Check live and recovered local rows before recovery creates even its
		// claim paths, including individually readable but inconsistent state.
		if err := dependency.AuthorizeLocalRecovery(projectDirectory); err != nil {
			return Result{}, err
		}
		// Valid locked local content still has to match before any mutation.
		if state, loadErr := dependency.LoadState(projectDirectory); loadErr == nil {
			for _, locked := range state.Lock.Dependencies {
				if locked.Kind == dependency.ResolutionLocal {
					loader, ok := service.loader.(projectPackageLoader)
					if !ok {
						return Result{}, &MaterializationError{Source: locked.Source, Err: fmt.Errorf("local sources require a project-aware materializer")}
					}
					_, cleanup, err := loader.MaterializeLockedAt(ctx, projectDirectory, locked)
					if err != nil {
						return Result{}, &MaterializationError{Source: locked.Source, Err: err}
					}
					if err := cleanup(); err != nil {
						return Result{}, err
					}
				}
			}
		}
		if err := realize.RecoverTransactions(projectDirectory); err != nil {
			return Result{}, err
		}
	}
	state, err := dependency.LoadState(projectDirectory)
	if err != nil {
		return Result{}, err
	}
	return service.RunStateFrom(ctx, projectDirectory, state, state, selected, mode)
}

// RunState renders and executes one realization mode against a caller-supplied
// dependency state, which need not be the state on disk: acr uninstall hands
// in the pruned state so the ordinary realization pass removes what the prune
// no longer wants. In apply mode the supplied state is what the transactional
// finalizer persists, alongside the next ownership ledger.
func (service *Service) RunState(ctx context.Context, projectDirectory string, state dependency.State, selected []string, mode realize.Mode) (result Result, err error) {
	expected, err := dependency.LoadState(projectDirectory)
	if err != nil {
		return Result{}, err
	}
	return service.RunStateFrom(ctx, projectDirectory, expected, state, selected, mode)
}

// RunStateFrom realizes desired state derived from expected project state. In
// apply mode expected is re-read under the mutation claim before the journal
// accepts current state files as its before-image.
func (service *Service) RunStateFrom(ctx context.Context, projectDirectory string, expected, state dependency.State, selected []string, mode realize.Mode) (Result, error) {
	return service.runStateFrom(ctx, projectDirectory, expected, state, selected, mode, true, nil)
}

// ApplyStateFromWithVendorTrees applies migration state and the new vendor
// packages it declares in the same journal. planVendors observes the project
// under the mutation claim, after recovery.
func (service *Service) ApplyStateFromWithVendorTrees(ctx context.Context, projectDirectory string, expected, state dependency.State, selected []string, planVendors func() (realize.VendorTreeChanges, error)) (Result, error) {
	return service.runStateFrom(ctx, projectDirectory, expected, state, selected, realize.ModeApply, true, planVendors)
}

// RestoreStateFrom restores native outputs and dependency state after a prior
// successful realization. Checkout-local evidence is left untouched.
func (service *Service) RestoreStateFrom(ctx context.Context, projectDirectory string, expected, state dependency.State, selected []string) (Result, error) {
	return service.runStateFrom(ctx, projectDirectory, expected, state, selected, realize.ModeApply, false, nil)
}

func (service *Service) runStateFrom(ctx context.Context, projectDirectory string, expected, state dependency.State, selected []string, mode realize.Mode, maintainApplied bool, planVendors func() (realize.VendorTreeChanges, error)) (result Result, err error) {
	agentIDs := append([]string(nil), selected...)
	if len(agentIDs) == 0 {
		agentIDs = append(agentIDs, state.Project.Agents...)
	}
	adapters, agentIDs, err := selectAdapters(agentIDs)
	if err != nil {
		return Result{}, err
	}
	if len(state.Project.Dependencies) != len(state.Lock.Dependencies) {
		return Result{}, fmt.Errorf("not every declared dependency is locked; run 'acr install' before realization")
	}
	policy, persistPolicy := freshness.Resolve(state.Project.Freshness, "", false)

	committed, err := realize.DecodeLedger(state.Lock.Realization)
	if err != nil {
		return Result{}, err
	}
	var appliedBefore, ignoreBefore realize.StateFile
	var appliedExists, ignoreExists bool
	var receiptProblem string
	if maintainApplied {
		if err := refuseTrackedApplied(projectDirectory); err != nil {
			return Result{}, err
		}
		appliedBefore, appliedExists, err = realize.ReadStateFile(projectDirectory, appliedPath)
		if err != nil {
			var oversized *realize.FileTooLargeError
			if !errors.As(err, &oversized) {
				return Result{}, err
			}
			if mode != realize.ModeCheck && mode != realize.ModeDryRun {
				return Result{}, fmt.Errorf("%w; back up/remove the named local receipt and retry 'acr realize'; preserve .agents/registry.lock", err)
			}
			receiptProblem = fmt.Sprintf("%s is unusable last-applied evidence (%v)", appliedPath, err)
		}
		ignoreBefore, ignoreExists, err = realize.ReadStateFile(projectDirectory, appliedIgnorePath)
		if err != nil {
			return Result{}, err
		}
	}
	applied := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}
	if appliedExists {
		if decoded, decodeErr := boundApplied(projectDirectory, appliedBefore.Content); decodeErr == nil {
			_, eligible, evidenceErr := realize.ComparisonLedger(projectDirectory, committed, decoded)
			if evidenceErr == nil {
				applied = eligible
			} else {
				receiptProblem = fmt.Sprintf("%s is unusable last-applied evidence (%v)", appliedPath, evidenceErr)
			}
		} else {
			receiptProblem = fmt.Sprintf("%s is unusable last-applied evidence (%v)", appliedPath, decodeErr)
		}
	} else if maintainApplied && receiptProblem == "" {
		receiptProblem = appliedPath + " is missing last-applied evidence"
	}
	if receiptProblem != "" {
		receiptProblem += "; after a pulled lock bump, review/back up edits, delete only the reported generated-only files, and run 'acr realize'; preserve shared files and .agents/registry.lock (see docs/troubleshooting.md)"
	}
	defer func() {
		var conflict *realize.ConflictError
		if receiptProblem != "" && errors.As(err, &conflict) {
			for _, operation := range conflict.Operations {
				if operation.OwnershipBefore == realize.OwnershipGenerated {
					err = fmt.Errorf("%w; %s", err, receiptProblem)
					break
				}
			}
		}
	}()
	// An explicit --agent list is a non-persisting subset override, so the
	// agents it omits keep their outputs and their ledger entries. An empty
	// list means the persisted selection, which is the project's whole desired
	// set: deselecting an agent there still removes what it left behind.
	//
	// The shared skill surface follows the same rule at target granularity:
	// an invocation that covers the whole configured selection owns it, and a
	// strict subset leaves it exactly as it is.
	ownsShared := coversConfiguredAgents(agentIDs, state.Project.Agents)
	scoped, carried := committed, realize.Ledger{SchemaVersion: committed.SchemaVersion}
	appliedCarried := realize.Ledger{SchemaVersion: applied.SchemaVersion}
	if len(selected) != 0 {
		scoped, carried, err = splitLedger(committed, agentIDs, ownsShared)
		if err != nil {
			return Result{}, err
		}
		_, appliedCarried, err = splitLedger(applied, agentIDs, ownsShared)
		if err != nil {
			return Result{}, err
		}
	}

	packages := make([]adapter.Package, 0, len(state.Lock.Dependencies))
	var cleanups []func() error
	defer func() {
		for index := len(cleanups) - 1; index >= 0; index-- {
			err = errors.Join(err, cleanups[index]())
		}
	}()
	for _, locked := range state.Lock.Dependencies {
		var materialized dependency.MaterializedPackage
		var cleanup func() error
		var loadErr error
		if loader, ok := service.loader.(projectPackageLoader); ok {
			materialized, cleanup, loadErr = loader.MaterializeLockedAt(ctx, projectDirectory, locked)
		} else {
			materialized, cleanup, loadErr = service.loader.MaterializeLocked(ctx, locked)
		}
		if loadErr != nil {
			return Result{}, &MaterializationError{Source: locked.Source, Err: loadErr}
		}
		cleanups = append(cleanups, cleanup)
		packages = append(packages, adapter.Package{Source: locked.Source, Root: os.DirFS(materialized.Root), Manifest: materialized.Manifest})
	}
	if hookPackage, present := freshness.HookPackage(policy); present {
		packages = append(packages, hookPackage)
	}

	snapshot, err := adapter.NewRootSnapshot(projectDirectory)
	if err != nil {
		return Result{}, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), adapters...)
	if err != nil {
		return Result{}, err
	}
	comparison, _, err := realize.ComparisonLedger(projectDirectory, scoped, applied)
	if err != nil {
		return Result{}, err
	}
	intents, notices, err := coordinator.RealizeWithNotices(ctx, snapshot, packages, comparison, priorConfigOptions(scoped))
	for _, declaration := range state.Project.Dependencies {
		if declaration.Requested == dependency.RequestedLocal {
			notices = append(notices, adapter.Notice{Code: "local_dependency", Message: dependency.LocalNotice(projectDirectory, declaration)})
		}
	}
	if err != nil {
		return Result{}, err
	}
	if state.Project.SharedSkills && ownsShared {
		shared, err := adapter.SharedSkillIntents(packages)
		if err != nil {
			return Result{}, err
		}
		intents = append(intents, shared...)
		sort.Slice(intents, func(left, right int) bool { return intents[left].Path < intents[right].Path })
	}
	// An upgrade on a still-enabled surface must retain ownership
	// of an omitted tracked whole file. Standalone scripts, uninstall and surface
	// retirement use the planner's ordinary tracked-file ownership release.
	byPath := make(map[string]bool, len(intents))
	for _, intent := range intents {
		byPath[intent.Path] = true
	}
	desiredSources := make(map[string]bool, len(state.Lock.Dependencies))
	for _, locked := range state.Lock.Dependencies {
		desiredSources[locked.Source] = true
	}
	enabledAgents := make(map[string]bool, len(agentIDs))
	for _, agent := range agentIDs {
		enabledAgents[agent] = true
	}
	var candidates []realize.Target
	var paths []string
	for _, target := range scoped.Targets {
		if byPath[target.Path] || target.Ownership != realize.OwnershipGenerated ||
			len(target.Entries) != 1 || target.Entries[0].ArtifactKind != realize.ArtifactFile {
			continue
		}
		entry := target.Entries[0]
		if strings.HasPrefix(target.Path, ".claude/scripts/") ||
			strings.HasPrefix(target.Path, ".codex/scripts/") ||
			strings.HasPrefix(target.Path, ".cursor/scripts/") {
			continue
		}
		if !desiredSources[entry.Source] ||
			(entry.Source == freshness.Source && entry.ArtifactID == freshness.ArtifactID && entry.SourcePath == freshness.SourcePath) {
			continue
		}
		if target.Owner == realize.OwnerCoordinator {
			if !state.Project.SharedSkills || !ownsShared {
				continue
			}
		} else if !enabledAgents[entry.Adapter] {
			continue
		}
		if _, err := snapshot.ReadFile(target.Path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return Result{}, fmt.Errorf("inspect omitted managed file %q: %w", target.Path, err)
		}
		candidates = append(candidates, target)
		paths = append(paths, target.Path)
	}
	if len(paths) != 0 {
		tracked, err := realize.TrackedPaths(projectDirectory, paths)
		if err != nil {
			return Result{}, fmt.Errorf("inspect Git-tracked omitted files: %w", err)
		}
		var conflicts []realize.Operation
		for _, target := range candidates {
			if tracked[target.Path] {
				conflicts = append(conflicts, realize.Operation{
					Kind: realize.OperationConflict, Path: target.Path,
					Reason: fmt.Sprintf("%q is tracked by Git and was dropped by an upgraded package; its recorded managedHash is %s; review the file, remove it from Git and the working tree, and retry", target.Path, target.Entries[0].ManagedHash),
				})
			}
		}
		if len(conflicts) != 0 {
			return Result{}, &realize.ConflictError{Operations: conflicts}
		}
	}
	finalize := func(next realize.Ledger) ([]realize.StateFile, error) {
		live, files, err := dependency.LoadStateFiles(projectDirectory)
		if err != nil {
			return nil, err
		}
		if mode == realize.ModeApply {
			// Recheck file-incarnation evidence under the mutation claim. A
			// change during compilation requires a fresh committed comparison.
			if len(applied.Targets) != 0 {
				liveApplied, err := boundApplied(projectDirectory, appliedBefore.Content)
				if err != nil {
					return nil, err
				}
				_, eligible, err := realize.ComparisonLedger(projectDirectory, committed, liveApplied)
				if err != nil {
					return nil, err
				}
				if !reflect.DeepEqual(applied, eligible) {
					return nil, fmt.Errorf("last-applied file metadata changed while realization was preparing; retry against current checkout state")
				}
			}
			if !reflect.DeepEqual(expected, live) {
				return nil, &ConcurrentStateChangeError{}
			}
			if maintainApplied {
				if err := refuseTrackedApplied(projectDirectory); err != nil {
					return nil, err
				}
			}
		}
		merged, err := realize.MergeLedgers(next, carried)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(committed, merged) {
			encoded, err := realize.EncodeLedger(merged)
			if err != nil {
				return nil, err
			}
			state.Lock.Realization = encoded
		}
		if persistPolicy {
			state.Project.Freshness = string(policy)
		}
		projectData, lockData, err := service.marshalState(state)
		if err != nil {
			return nil, err
		}
		files[0].Content, files[0].Mode = projectData, 0o644
		files[1].Content, files[1].Mode = lockData, 0o644
		if mode != realize.ModeApply || !maintainApplied {
			return files, nil
		}
		nextApplied, err := realize.MergeLedgers(generatedLedger(next), appliedCarried)
		if err != nil {
			return nil, err
		}
		return append(files,
			appliedIgnore(ignoreBefore, ignoreExists),
			realize.StateFile{Path: appliedPath, Mode: 0o644, BeforeExists: appliedBefore.BeforeExists, BeforeContent: appliedBefore.BeforeContent, BeforeMode: appliedBefore.BeforeMode, AfterApply: func() ([]byte, error) {
				return encodeApplied(projectDirectory, nextApplied)
			}},
		), nil
	}
	var plan realize.Plan
	var runErr error
	if planVendors != nil {
		plan, runErr = service.engine.ApplyStateFilesWithVendorTrees(projectDirectory, scoped, applied, intents, finalize, planVendors, carried)
	} else {
		plan, runErr = service.engine.RunStateFiles(projectDirectory, scoped, applied, intents, mode, finalize, carried)
	}
	// Auxiliary evidence is journaled with product state, but does not represent
	// realization drift, a native change, or a reason to restart an agent.
	operations := plan.Operations[:0]
	for _, operation := range plan.Operations {
		if operation.Path != appliedPath && operation.Path != appliedIgnorePath {
			operations = append(operations, operation)
		}
	}
	plan.Operations = operations
	if mode == realize.ModeApply && runErr == nil && receiptProblem != "" && appliedExists {
		notices = append(notices, adapter.Notice{Code: "applied_evidence_unavailable", Message: receiptProblem})
	}
	return Result{Agents: agentIDs, Plan: plan, Notices: notices}, runErr
}

// persistState writes state with the resolved freshness policy applied. It is
// the path for a realization pass that plans no change and therefore returns
// before the engine's transactional finalizer ever runs, leaving a
// caller-supplied state that must still land.
func (service *Service) persistState(projectDirectory string, state dependency.State) error {
	if policy, persist := freshness.Resolve(state.Project.Freshness, "", false); persist {
		state.Project.Freshness = string(policy)
	}
	return service.writeState(projectDirectory, state)
}

// coversConfiguredAgents reports whether this invocation's selection includes
// every agent agents.yaml configures. An --agent list that omits one is a
// temporary subset: the agents it omits keep their outputs, and so does the
// shared skill surface they contribute to.
func coversConfiguredAgents(selected, configured []string) bool {
	chosen := make(map[string]struct{}, len(selected))
	for _, agentID := range selected {
		chosen[agentID] = struct{}{}
	}
	for _, agentID := range configured {
		if _, ok := chosen[agentID]; !ok {
			return false
		}
	}
	return true
}

func selectAdapters(agentIDs []string) ([]adapter.Adapter, []string, error) {
	if len(agentIDs) == 0 {
		return nil, nil, fmt.Errorf("no agent adapters selected; set agents in %s or pass --agent", dependency.ProjectFilename)
	}
	ids := append([]string(nil), agentIDs...)
	sort.Strings(ids)
	var adapters []adapter.Adapter
	for index, id := range ids {
		if index != 0 && ids[index-1] == id {
			return nil, nil, fmt.Errorf("agent adapter %q is selected more than once", id)
		}
		switch id {
		case "claude-code":
			adapters = append(adapters, claudecode.New())
		case "codex":
			adapters = append(adapters, codex.New())
		case "cursor":
			adapters = append(adapters, cursor.New())
		default:
			return nil, nil, fmt.Errorf("unsupported agent adapter %q; use claude-code, codex, or cursor", id)
		}
	}
	return adapters, ids, nil
}

func priorConfigOptions(previous realize.Ledger) map[string]adapter.TargetOptions {
	formats := map[string]adapter.ConfigFormat{
		".claude/settings.json": adapter.ConfigJSON,
		".codex/config.toml":    adapter.ConfigTOML,
		".cursor/hooks.json":    adapter.ConfigJSON,
	}
	options := make(map[string]adapter.TargetOptions)
	for _, target := range previous.Targets {
		format, known := formats[target.Path]
		if !known || !hasStructuredEntry(target.Entries) {
			continue
		}
		options[target.Path] = adapter.TargetOptions{ConfigFormat: format}
	}
	return options
}

func hasStructuredEntry(entries []realize.Entry) bool {
	for _, entry := range entries {
		if entry.ArtifactKind == realize.ArtifactStructuredEntry {
			return true
		}
	}
	return false
}

func changeCount(plan realize.Plan) int {
	count := 0
	if plan.LedgerChanged {
		count++
	}
	for _, operation := range plan.Operations {
		if operation.Kind != realize.OperationPreserve {
			count++
		}
	}
	return count
}

func realizationMessage(mode realize.Mode, result Result) string {
	changes := changeCount(result.Plan)
	switch mode {
	case realize.ModeDryRun:
		return fmt.Sprintf("Realization would apply %d change(s) for %s.", changes, strings.Join(result.Agents, ", "))
	case realize.ModeCheck:
		return fmt.Sprintf("Realization is current for %s.", strings.Join(result.Agents, ", "))
	default:
		if changes == 0 {
			return fmt.Sprintf("Realization is already current for %s.", strings.Join(result.Agents, ", "))
		}
		return fmt.Sprintf("Applied %d realization change(s) for %s.", changes, strings.Join(result.Agents, ", "))
	}
}
