package realizeapp

import (
	"context"
	"errors"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// Application adds realization commands to the dependency application.
type Application struct {
	service  *Service
	fallback cli.Application
}

// NewApplication constructs the complete shipped application boundary.
func NewApplication(github dependency.GitHub) *Application {
	resolver := dependency.NewResolver(github)
	return &Application{service: NewService(resolver), fallback: dependency.NewApplication(github)}
}

// Execute dispatches realization and dependency commands.
func (application *Application) Execute(ctx context.Context, invocation cli.Invocation) (cli.Result, error) {
	var mode realize.Mode
	switch invocation.Command {
	case cli.CommandRealize:
		mode = realize.ModeApply
		if invocation.DryRun {
			mode = realize.ModeDryRun
		}
	case cli.CommandCheck:
		mode = realize.ModeCheck
	default:
		return application.fallback.Execute(ctx, invocation)
	}
	result, err := application.service.Run(ctx, invocation.ProjectDirectory, invocation.Agents, mode)
	if err != nil {
		return cli.Result{}, realizationError(err)
	}
	return cli.Result{Message: realizationMessage(mode, result), Value: result}, nil
}

func realizationError(err error) error {
	var changes *realize.ChangesError
	if errors.As(err, &changes) {
		return &cli.Error{ExitCode: cli.ExitChanges, Code: "realization_changes", Message: err.Error(), Cause: err}
	}
	var engineConflict *realize.ConflictError
	var preserveConflict *preserve.ConflictError
	var malformed *adapter.MalformedOutputError
	var duplicate *adapter.DuplicateEntryError
	var native *adapter.NativeValidationError
	if errors.As(err, &engineConflict) || errors.As(err, &preserveConflict) || errors.As(err, &malformed) || errors.As(err, &duplicate) || errors.As(err, &native) {
		return &cli.Error{ExitCode: cli.ExitConflict, Code: "realization_conflict", Message: err.Error(), Cause: err}
	}
	return &cli.Error{ExitCode: cli.ExitOperational, Code: "realization_failed", Message: err.Error(), Cause: err}
}
