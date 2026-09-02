// Package publishapp wires immutable package publishing to the CLI boundary.
package publishapp

import (
	"context"
	"errors"
	"fmt"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshnessapp"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/publish"
)

// Application adds publishing to the shipped application stack.
type Application struct {
	service  *Service
	fallback cli.Application
}

// NewApplication constructs publishing with freshness, realization, and dependency fallbacks.
func NewApplication(client *dependency.GitHubClient, version string) *Application {
	return &Application{
		service:  NewService(publish.NewBuilder(version), client),
		fallback: freshnessapp.NewApplication(client),
	}
}

func newApplication(service *Service, fallback cli.Application) *Application {
	return &Application{service: service, fallback: fallback}
}

// Execute dispatches publish and delegates every other command.
func (application *Application) Execute(ctx context.Context, invocation cli.Invocation) (cli.Result, error) {
	if invocation.Command != cli.CommandPublish {
		return application.fallback.Execute(ctx, invocation)
	}
	result, err := application.service.Publish(ctx, invocation.PublicationPath, invocation.DryRun)
	if err != nil {
		return cli.Result{}, publicationError(err)
	}
	message := fmt.Sprintf("Published immutable release %s with %d assets.", result.Tag, len(result.Assets))
	if invocation.DryRun {
		message = fmt.Sprintf("Release %s is publishable with %d assets; rerun without --dry-run to upload it.", result.Tag, len(result.Assets))
	}
	return cli.Result{Message: message, Value: result}, nil
}

func publicationError(err error) error {
	var refusal *publish.Error
	if errors.As(err, &refusal) {
		return &cli.Error{ExitCode: cli.ExitOperational, Code: refusal.Code, Message: refusal.Message, Cause: err}
	}
	var validation *manifest.ValidationErrors
	if errors.As(err, &validation) && len(validation.Issues) != 0 {
		return &cli.Error{ExitCode: cli.ExitOperational, Code: string(validation.Issues[0].Code), Message: validation.Error(), Cause: err}
	}
	return &cli.Error{ExitCode: cli.ExitOperational, Code: "publish_failed", Message: err.Error(), Cause: err}
}
