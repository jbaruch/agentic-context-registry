package migrateapp

import (
	"context"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/publishapp"
)

// Application adds Tessl migration commands to the shipped publish,
// freshness, realization, and dependency commands.
type Application struct {
	service  *Service
	fallback cli.Application
}

// NewApplication constructs the complete shipped application boundary.
func NewApplication(github dependency.GitHub, version string) *Application {
	client, _ := github.(*dependency.GitHubClient)
	return &Application{service: NewService(), fallback: publishapp.NewApplication(client, version)}
}

// Execute dispatches Tessl inventory and preserves the shared CLI contract.
func (application *Application) Execute(ctx context.Context, invocation cli.Invocation) (cli.Result, error) {
	if invocation.Command != cli.CommandMigrate {
		return application.fallback.Execute(ctx, invocation)
	}
	if !invocation.DryRun {
		return cli.Result{}, &cli.Error{
			ExitCode: cli.ExitOperational,
			Code:     "not_implemented",
			Message:  "acr migrate tessl apply is not implemented yet; rerun with --dry-run to inventory the Tessl installation without writing files, or see https://github.com/jbaruch/agentic-context-registry/issues/2",
		}
	}
	report, err := application.service.Inventory(invocation.ProjectDirectory)
	if err != nil {
		return cli.Result{}, &cli.Error{
			ExitCode: cli.ExitOperational,
			Code:     "migrate_failed",
			Message:  err.Error(),
			Cause:    err,
		}
	}
	return cli.Result{Message: migrate.FormatText(report), Value: report}, nil
}
