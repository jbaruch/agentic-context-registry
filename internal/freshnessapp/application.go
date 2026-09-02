package freshnessapp

import (
	"context"
	"errors"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/realizeapp"
)

// Application adds the session-start freshness command to the shipped
// realization and dependency commands.
type Application struct {
	runner   *Runner
	fallback cli.Application
	setupErr error
}

// NewApplication constructs the complete shipped application boundary.
func NewApplication(github dependency.GitHub) *Application {
	store, err := freshness.DefaultStore()
	resolver := dependency.NewResolver(github)
	service := dependency.NewService(resolver)
	runner := NewRunner(store, time.Now, service).WithInstall(service, realizeapp.NewService(resolver))
	return &Application{runner: runner, fallback: realizeapp.NewApplication(github), setupErr: err}
}

// Execute dispatches freshness and falls back to the remaining CLI commands.
func (application *Application) Execute(ctx context.Context, invocation cli.Invocation) (cli.Result, error) {
	if invocation.Command != cli.CommandFreshness {
		return application.fallback.Execute(ctx, invocation)
	}
	state, err := dependency.LoadState(invocation.ProjectDirectory)
	if err != nil {
		return cli.Result{}, err
	}
	policy, _ := freshness.Resolve(state.Project.Freshness, invocation.Freshness, invocation.FreshnessExplicit)
	if application.setupErr != nil {
		result := Result{
			Policy:   policy,
			Outdated: []dependency.OutdatedDependency{},
			Notices:  []Notice{failureNotice(CodeStateUnwritable, invocation.ProjectDirectory, policy)},
		}
		return cli.Result{Value: result, Notices: freshnessNotices(result.Notices), ExitCode: cli.ExitOperational}, nil
	}
	result, err := application.runner.Run(ctx, invocation.ProjectDirectory, policy)
	cliResult := cli.Result{Value: result, Notices: freshnessNotices(result.Notices)}
	if err == nil {
		return cliResult, nil
	}
	var runErr *RunError
	if errors.As(err, &runErr) {
		cliResult.ExitCode = runErr.ExitCode
		return cliResult, nil
	}
	return cli.Result{}, err
}

func freshnessNotices(notices []Notice) []cli.Notice {
	result := make([]cli.Notice, len(notices))
	for index, notice := range notices {
		result[index] = cli.Notice{Code: notice.Code, Message: notice.Message}
	}
	return result
}
