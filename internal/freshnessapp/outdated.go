// Package freshnessapp executes throttled session-start freshness policies.
package freshnessapp

import (
	"context"
	"fmt"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

// outdatedChecker is deliberately read-only: outdated mode cannot reach a
// dependency mutator through this interface.
type outdatedChecker interface {
	Outdated(context.Context, string) ([]dependency.OutdatedDependency, error)
}

type outdatedExecutor struct {
	checker outdatedChecker
}

func (executor outdatedExecutor) execute(ctx context.Context, root string) (Result, error) {
	outdated, err := executor.checker.Outdated(ctx, root)
	if err != nil {
		return Result{}, err
	}
	// A held steady state is not a session-start finding: the barrier is
	// working as declared, so only actionable rows reach the hook.
	result := Result{Outdated: []dependency.OutdatedDependency{}}
	for _, item := range outdated {
		if item.Actionable() {
			result.Outdated = append(result.Outdated, item)
		}
	}
	for _, item := range result.Outdated {
		if item.Notice != "" {
			result.Notices = append(result.Notices, Notice{Code: dependency.NoticeCodeHoldResumable, Message: item.Notice})
			continue
		}
		result.Notices = append(result.Notices, Notice{
			Code: CodeOutdated,
			Message: fmt.Sprintf("%s is outdated: %s (%s) -> %s (%s); run 'acr install --project %s' to apply latest dependencies.",
				item.Source, item.CurrentTag, item.CurrentCommit, item.LatestTag, item.LatestCommit, root),
		})
	}
	return result, nil
}
