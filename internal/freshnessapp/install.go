package freshnessapp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"github.com/jbaruch/agentic-context-registry/internal/realizeapp"
)

const CodeRestartRequired = "restart_required"

type dependencyReconciler interface {
	Reconcile(context.Context, string, bool) (dependency.ChangeResult, error)
}

type realizationService interface {
	Run(context.Context, string, []string, realize.Mode) (realizeapp.Result, error)
}

type installExecutor struct {
	reconciler dependencyReconciler
	realizer   realizationService
}

func (executor installExecutor) execute(ctx context.Context, root string) (Result, error) {
	state, err := dependency.LoadState(root)
	if err != nil {
		return Result{}, err
	}
	previous, err := realize.DecodeLedger(state.Lock.Realization)
	if err != nil {
		return Result{}, err
	}
	reconciled, err := executor.reconciler.Reconcile(ctx, root, false)
	if err != nil {
		return Result{}, fmt.Errorf("reconcile latest dependencies: %w", err)
	}
	realized, err := executor.realizer.Run(ctx, root, nil, realize.ModeApply)
	if err != nil {
		return Result{}, fmt.Errorf("realize reconciled dependencies: %w", err)
	}
	result := Result{Agents: append([]string(nil), realized.Agents...), Outdated: []dependency.OutdatedDependency{}}
	for _, notice := range reconciled.Notices {
		result.Notices = append(result.Notices, Notice{Code: dependency.NoticeCodeHoldResumable, Message: notice})
	}
	for _, notice := range realized.Notices {
		result.Notices = append(result.Notices, Notice{Code: notice.Code, Message: notice.Message})
	}
	affected := affectedAgents(realized.Plan, previous)
	if len(affected) != 0 {
		result.RestartRequired = true
		result.RestartAgents = affected
		result.Notices = append(result.Notices, Notice{
			Code:    CodeRestartRequired,
			Message: fmt.Sprintf("Restart affected agents to load freshness changes: %s.", strings.Join(affected, ", ")),
		})
	}
	return result, nil
}

func affectedAgents(plan realize.Plan, previous realize.Ledger) []string {
	set := make(map[string]struct{})
	for _, operation := range plan.Operations {
		if operation.Kind == realize.OperationPreserve {
			continue
		}
		if agent := nativePathAgent(operation.Path); agent != "" {
			set[agent] = struct{}{}
			continue
		}
		addLedgerAgents(set, plan.NextLedger, operation.Path)
		addLedgerAgents(set, previous, operation.Path)
	}
	agents := make([]string, 0, len(set))
	for agent := range set {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	return agents
}

func nativePathAgent(path string) string {
	for prefix, agent := range map[string]string{
		".claude/": "claude-code",
		".codex/":  "codex",
		".cursor/": "cursor",
	} {
		if strings.HasPrefix(path, prefix) {
			return agent
		}
	}
	return ""
}

func addLedgerAgents(set map[string]struct{}, ledger realize.Ledger, path string) {
	for _, target := range ledger.Targets {
		if target.Path != path {
			continue
		}
		for _, entry := range target.Entries {
			set[entry.Adapter] = struct{}{}
		}
	}
}
