package setupapp

import (
	"context"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

// Application is the outermost cli.Application decorator: the setup questions
// live here and nowhere else, so no domain service gains an io.Reader or a
// prompt-aware flag.
type Application struct {
	inner    cli.Application
	prompter Prompter
}

// NewApplication wraps the shipped application boundary with the interactive
// setup flow.
func NewApplication(inner cli.Application, prompter Prompter) *Application {
	return &Application{inner: inner, prompter: prompter}
}

// Execute asks whatever the invocation needs, then delegates.
func (application *Application) Execute(ctx context.Context, invocation cli.Invocation) (cli.Result, error) {
	return application.inner.Execute(ctx, invocation)
}

// interactive reports whether this invocation may prompt. --json turns
// prompting off outright so stdout carries exactly one envelope;
// --non-interactive and a non-terminal stdin both mean the typed refusal the
// inner application already returns.
func (application *Application) interactive(invocation cli.Invocation) bool {
	return application.prompter.Interactive() && !invocation.NonInteractive && invocation.Output != cli.OutputJSON
}
