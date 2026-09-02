package migrateapp

import (
	"context"
	"errors"
	"fmt"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/publishapp"
	"github.com/jbaruch/agentic-context-registry/internal/tesslplugin"
)

// Application adds Tessl inventory and producer conversion to the shipped publish,
// freshness, realization, and dependency commands.
type Application struct {
	service  *Service
	fallback cli.Application
}

// NewApplication constructs the complete shipped application boundary.
func NewApplication(client *dependency.GitHubClient, version string) *Application {
	return &Application{service: NewService(), fallback: publishapp.NewApplication(client, version)}
}

// Execute dispatches Tessl migration commands and preserves the shared CLI contract.
func (application *Application) Execute(ctx context.Context, invocation cli.Invocation) (cli.Result, error) {
	if invocation.Command != cli.CommandMigrate {
		return application.fallback.Execute(ctx, invocation)
	}
	if invocation.Subcommand == "tessl-plugin" {
		report, err := application.service.Convert(tesslplugin.Options{
			PackageRoot:         invocation.PublicationPath,
			Repository:          invocation.Repository,
			AcceptAgentWidening: invocation.AcceptAgentWidening,
			DryRun:              invocation.DryRun,
		})
		if err != nil {
			return cli.Result{}, migrateError(err)
		}
		return cli.Result{Message: tesslplugin.FormatText(report), Value: report}, nil
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

func migrateError(err error) error {
	var conv *tesslplugin.Error
	if errors.As(err, &conv) {
		return &cli.Error{ExitCode: cli.ExitOperational, Code: conv.Code, Message: conv.Message, Cause: err}
	}
	return &cli.Error{
		ExitCode: cli.ExitOperational,
		Code:     "migrate_failed",
		Message:  fmt.Sprintf("%v", err),
		Cause:    err,
	}
}
