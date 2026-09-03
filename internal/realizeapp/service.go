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
func (service *Service) RunStateFrom(ctx context.Context, projectDirectory string, expected, state dependency.State, selected []string, mode realize.Mode) (result Result, err error) {
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

	previous, err := realize.DecodeLedger(state.Lock.Realization)
	if err != nil {
		return Result{}, err
	}
	// An explicit --agent list is a non-persisting subset override, so the
	// agents it omits keep their outputs and their ledger entries. An empty
	// list means the persisted selection, which is the project's whole desired
	// set: deselecting an agent there still removes what it left behind.
	scoped, carried := previous, realize.Ledger{SchemaVersion: previous.SchemaVersion}
	if len(selected) != 0 {
		scoped, carried, err = splitLedger(previous, agentIDs)
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
	intents, notices, err := coordinator.RealizeWithNotices(ctx, snapshot, packages, scoped, priorConfigOptions(scoped))
	if err != nil {
		return Result{}, err
	}
	finalize := func(next realize.Ledger) ([]realize.StateFile, error) {
		if mode == realize.ModeApply {
			live, err := dependency.LoadState(projectDirectory)
			if err != nil {
				return nil, err
			}
			if !reflect.DeepEqual(expected, live) {
				return nil, &ConcurrentStateChangeError{}
			}
		}
		merged, err := realize.MergeLedgers(next, carried)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(previous, merged) {
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
		return []realize.StateFile{
			{Path: dependency.ProjectFilename, Content: projectData, Mode: 0o644},
			{Path: dependency.LockFilename, Content: lockData, Mode: 0o644},
		}, nil
	}
	plan, runErr := service.engine.RunStateFiles(projectDirectory, scoped, intents, mode, finalize, carried)
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
