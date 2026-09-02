package dependency

import (
	"context"
	"fmt"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
)

// Application wires issue #5 dependency commands to the CLI boundary.
type Application struct {
	service  *Service
	fallback cli.Application
}

// NewApplication constructs the dependency application with production
// fallbacks for commands owned by later implementation issues.
func NewApplication(github GitHub) *Application {
	return &Application{service: NewService(NewResolver(github)), fallback: cli.UnavailableApplication{}}
}

// Execute dispatches dependency-state commands and preserves the shared CLI contract.
func (application *Application) Execute(ctx context.Context, invocation cli.Invocation) (cli.Result, error) {
	switch invocation.Command {
	case cli.CommandInstall:
		var result ChangeResult
		var err error
		if invocation.Reconcile {
			result, err = application.service.Reconcile(ctx, invocation.ProjectDirectory, invocation.DryRun)
		} else {
			result, err = application.service.Install(ctx, invocation.ProjectDirectory, invocation.Source, invocation.RequestedVersion, invocation.DryRun)
		}
		if err != nil {
			return cli.Result{}, dependencyError(err)
		}
		if !invocation.DryRun {
			if err := persistFreshness(invocation); err != nil {
				return cli.Result{}, dependencyError(err)
			}
		}
		return cli.Result{Message: changeMessage("install", result, invocation.DryRun), Value: result, Notices: dependencyNotices(result.Notices)}, nil
	case cli.CommandList:
		statuses, err := application.service.List(invocation.ProjectDirectory)
		if err != nil {
			return cli.Result{}, dependencyError(err)
		}
		return cli.Result{Message: listMessage(statuses), Value: map[string]any{"dependencies": statuses}}, nil
	case cli.CommandOutdated:
		outdated, err := application.service.Outdated(ctx, invocation.ProjectDirectory)
		if err != nil {
			return cli.Result{}, dependencyError(err)
		}
		message := "All latest dependencies are current."
		if len(outdated) != 0 {
			message = fmt.Sprintf("%d latest dependencies are outdated.", len(outdated))
		}
		return cli.Result{Message: message, Value: map[string]any{"outdated": outdated}}, nil
	case cli.CommandUpdate:
		result, err := application.service.Update(ctx, invocation.ProjectDirectory, invocation.Source, invocation.DryRun)
		if err != nil {
			return cli.Result{}, dependencyError(err)
		}
		return cli.Result{Message: changeMessage("update", result, invocation.DryRun), Value: result, Notices: dependencyNotices(result.Notices)}, nil
	default:
		return application.fallback.Execute(ctx, invocation)
	}
}

func persistFreshness(invocation cli.Invocation) error {
	state, err := LoadState(invocation.ProjectDirectory)
	if err != nil {
		return err
	}
	policy, persist := freshness.Resolve(
		state.Project.Freshness,
		invocation.Freshness,
		invocation.FreshnessExplicit,
	)
	if !persist {
		return nil
	}
	state.Project.Freshness = string(policy)
	return WriteState(invocation.ProjectDirectory, state)
}

func dependencyNotices(notices []string) []cli.Notice {
	result := make([]cli.Notice, len(notices))
	for index, notice := range notices {
		result[index] = cli.Notice{Code: "dependency_hold", Message: notice}
	}
	return result
}

func dependencyError(err error) error {
	return &cli.Error{ExitCode: cli.ExitOperational, Code: "dependency_operation_failed", Message: err.Error(), Cause: err}
}

func changeMessage(command string, result ChangeResult, dryRun bool) string {
	if !result.Changed {
		return "Dependency state is already current."
	}
	if dryRun {
		return fmt.Sprintf("%s would update dependency state; rerun without --dry-run to write %s and %s.", command, ProjectFilename, LockFilename)
	}
	return fmt.Sprintf("Dependency state updated in %s and %s; run 'acr realize' to materialize locked artifacts.", ProjectFilename, LockFilename)
}

func listMessage(statuses []DependencyStatus) string {
	if len(statuses) == 0 {
		return "No dependencies declared."
	}
	var builder strings.Builder
	for index, status := range statuses {
		if index != 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(status.Declaration.Source)
		builder.WriteByte('@')
		builder.WriteString(status.Declaration.Requested)
		if status.Locked == nil {
			builder.WriteString(" (not locked; run 'acr install')")
			continue
		}
		builder.WriteString(" -> ")
		builder.WriteString(status.Locked.Commit)
	}
	return builder.String()
}
