// Package realizeapp wires resolved packages, native adapters, preservation,
// and the transactional realization engine.
package realizeapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

type packageLoader interface {
	MaterializeLocked(context.Context, dependency.LockedDependency) (dependency.MaterializedPackage, func() error, error)
}

// Service realizes immutable dependency locks through selected native adapters.
type Service struct {
	loader packageLoader
	engine *realize.Engine
}

// NewService constructs the production realization service.
func NewService(loader packageLoader) *Service {
	return &Service{loader: loader, engine: realize.NewEngine()}
}

// Result describes a realization plan without exposing rendered file bodies.
type Result struct {
	Agents  []string         `json:"agents"`
	Plan    realize.Plan     `json:"plan"`
	Notices []adapter.Notice `json:"notices,omitempty"`
}

// Run renders and executes one realization mode.
func (service *Service) Run(ctx context.Context, projectDirectory string, selected []string, mode realize.Mode) (result Result, err error) {
	state, err := dependency.LoadState(projectDirectory)
	if err != nil {
		return Result{}, err
	}
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

	packages := make([]adapter.Package, 0, len(state.Lock.Dependencies))
	var cleanups []func() error
	defer func() {
		for index := len(cleanups) - 1; index >= 0; index-- {
			err = errors.Join(err, cleanups[index]())
		}
	}()
	for _, locked := range state.Lock.Dependencies {
		materialized, cleanup, loadErr := service.loader.MaterializeLocked(ctx, locked)
		if loadErr != nil {
			return Result{}, fmt.Errorf("materialize %s: %w", locked.Source, loadErr)
		}
		cleanups = append(cleanups, cleanup)
		packages = append(packages, adapter.Package{Source: locked.Source, Root: os.DirFS(materialized.Root), Manifest: materialized.Manifest})
	}

	previous, err := realize.DecodeLedger(state.Lock.Realization)
	if err != nil {
		return Result{}, err
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
	intents, notices, err := coordinator.RealizeWithNotices(ctx, snapshot, packages, previous, priorConfigOptions(previous))
	if err != nil {
		return Result{}, err
	}
	finalize := realize.Finalizer(nil)
	if mode == realize.ModeApply {
		finalize = func(next realize.Ledger) error {
			encoded, err := realize.EncodeLedger(next)
			if err != nil {
				return err
			}
			state.Lock.Realization = encoded
			return dependency.WriteState(projectDirectory, state)
		}
	}
	plan, runErr := service.engine.Run(projectDirectory, previous, intents, mode, finalize)
	return Result{Agents: agentIDs, Plan: plan, Notices: notices}, runErr
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
