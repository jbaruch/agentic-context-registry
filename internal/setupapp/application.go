package setupapp

import (
	"context"
	"errors"

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
	result, err := application.inner.Execute(ctx, invocation)
	if invocation.Command != cli.CommandInstall || !downgradeChoiceRequired(err) || !application.interactive(invocation) {
		return result, err
	}
	return application.chooseDowngrade(ctx, invocation)
}

// chooseDowngrade asks the three-option rollback question and re-invokes the
// inner application exactly once. The first attempt cost nothing: the install
// service refuses before it resolves anything and before it writes either
// state file, so the question is free. A second downgrade_choice_required
// propagates unchanged rather than looping.
func (application *Application) chooseDowngrade(ctx context.Context, invocation cli.Invocation) (cli.Result, error) {
	answer, err := application.prompter.Ask(ctx, downgradeQuestion(invocation.Source, invocation.RequestedVersion))
	if err != nil {
		return cli.Result{}, err
	}
	if answer.Cancelled || len(answer.Values) != 1 {
		return cli.Result{}, &cli.Error{
			ExitCode: cli.ExitUsage,
			Code:     "downgrade_cancelled",
			Message: "rollback cancelled; nothing was installed; rerun 'acr install " + invocation.Source + "@" + invocation.RequestedVersion +
				"' with --hold to roll back temporarily or --pin to replace latest with a permanent pin",
		}
	}
	invocation.Downgrade = cli.DowngradeChoice(answer.Values[0])
	return application.inner.Execute(ctx, invocation)
}

// downgradeQuestion offers the two recorded outcomes plus cancel. There is no
// default: a rollback is never chosen by pressing Enter.
func downgradeQuestion(source, requested string) Question {
	return Question{
		ID:     "downgrade",
		Prompt: "Installing " + source + "@" + requested + " rolls a latest dependency backwards. Record it as:",
		Kind:   QuestionSingleChoice,
		Cancel: "cancel",
		Options: []Option{
			{Value: string(cli.DowngradeHold), Label: "hold — keep requesting latest behind a resume barrier"},
			{Value: string(cli.DowngradePin), Label: "pin — replace latest with a permanent pin"},
			{Value: "cancel", Label: "cancel — install nothing"},
		},
	}
}

func downgradeChoiceRequired(err error) bool {
	var commandErr *cli.Error
	return errors.As(err, &commandErr) && commandErr.Code == cli.CodeDowngradeChoiceRequired
}

// interactive reports whether this invocation may prompt. --json turns
// prompting off outright so stdout carries exactly one envelope;
// --non-interactive and a non-terminal stdin both mean the typed refusal the
// inner application already returns.
func (application *Application) interactive(invocation cli.Invocation) bool {
	return application.prompter.Interactive() && !invocation.NonInteractive && invocation.Output != cli.OutputJSON
}
