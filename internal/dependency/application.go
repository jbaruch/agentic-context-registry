package dependency

import (
	"context"
	"errors"
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
			choice, choiceErr := downgradeChoice(invocation.Downgrade)
			if choiceErr != nil {
				return cli.Result{}, choiceErr
			}
			result, err = application.service.Install(ctx, invocation.ProjectDirectory, invocation.Source, invocation.RequestedVersion, choice, invocation.DryRun)
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
		return cli.Result{Message: outdatedMessage(outdated), Value: map[string]any{"outdated": outdated}}, nil
	case cli.CommandUpdate:
		result, err := application.service.Update(ctx, invocation.ProjectDirectory, invocation.Source, invocation.DryRun)
		if err != nil {
			return cli.Result{}, dependencyError(err)
		}
		return cli.Result{Message: changeMessage("update", result, invocation.DryRun), Value: result, Notices: dependencyNotices(result.Notices)}, nil
	case cli.CommandResume:
		result, err := application.service.Resume(ctx, invocation.ProjectDirectory, invocation.Source, invocation.DryRun)
		if err != nil {
			return cli.Result{}, dependencyError(err)
		}
		return cli.Result{Message: changeMessage("resume", result, invocation.DryRun), Value: result, Notices: dependencyNotices(result.Notices)}, nil
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
		string(invocation.Freshness),
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
		result[index] = cli.Notice{Code: NoticeCodeHoldResumable, Message: notice}
	}
	return result
}

func downgradeChoice(choice cli.DowngradeChoice) (DowngradeChoice, error) {
	switch choice {
	case cli.DowngradeUnset:
		return DowngradeUnset, nil
	case cli.DowngradeHold:
		return DowngradeHold, nil
	case cli.DowngradePin:
		return DowngradePin, nil
	default:
		return DowngradeUnset, &cli.Error{
			ExitCode: cli.ExitUsage, Code: "usage",
			Message: fmt.Sprintf("unsupported downgrade choice %q; use --hold or --pin", choice),
		}
	}
}

func dependencyError(err error) error {
	var downgrade *DowngradeRequiredError
	if errors.As(err, &downgrade) {
		return &cli.Error{ExitCode: cli.ExitUsage, Code: "downgrade_choice_required", Message: err.Error(), Cause: err}
	}
	return &cli.Error{ExitCode: cli.ExitOperational, Code: "dependency_operation_failed", Message: err.Error(), Cause: err}
}

// outdatedMessage counts the rows an ordinary reconcile would act on and lists
// rollback barriers separately, because a barrier is never an ordinary update.
func outdatedMessage(outdated []OutdatedDependency) string {
	actionable := 0
	var barriers []string
	for _, item := range outdated {
		if item.Actionable() {
			actionable++
		}
		if item.Status == OutdatedBeyondBarrier {
			barriers = append(barriers, fmt.Sprintf("%s (barrier %s, candidate %s; run '%s')", item.Source, item.Hold.Rejected, item.LatestTag, item.ResumeCommand))
		}
	}
	message := "All latest dependencies are current."
	if actionable != 0 {
		message = fmt.Sprintf("%d latest dependencies are outdated.", actionable)
	}
	if len(barriers) != 0 {
		message += "\nBeyond a rollback barrier:\n" + strings.Join(barriers, "\n")
	}
	return message
}

func changeMessage(command string, result ChangeResult, dryRun bool) string {
	message := "Dependency state is already current."
	switch {
	case result.Changed && dryRun:
		message = fmt.Sprintf("%s would update dependency state; rerun without --dry-run to write %s and %s.", command, ProjectFilename, LockFilename)
	case result.Changed:
		message = fmt.Sprintf("Dependency state updated in %s and %s; run 'acr realize' to materialize locked artifacts.", ProjectFilename, LockFilename)
	}
	if len(result.Resumed) != 0 {
		if dryRun {
			message += fmt.Sprintf("\nWould resume latest for %s and retire its rollback barrier.", strings.Join(result.Resumed, ", "))
		} else {
			message += fmt.Sprintf("\nResumed latest for %s; its rollback barrier is retired.", strings.Join(result.Resumed, ", "))
		}
	}
	if len(result.Held) != 0 {
		message += fmt.Sprintf("\nHeld behind a rollback barrier: %s.", strings.Join(result.Held, ", "))
	}
	return message
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
		if hold := status.Declaration.Hold; hold != nil {
			fmt.Fprintf(&builder, " [held %s, barrier %s]", hold.Pin, hold.Rejected)
		}
		if status.Locked == nil {
			builder.WriteString(" (not locked; run 'acr install')")
			continue
		}
		builder.WriteString(" -> ")
		builder.WriteString(status.Locked.Commit)
	}
	return builder.String()
}
