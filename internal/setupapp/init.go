package setupapp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/setup"
)

// detector reports which agents a project already uses. It is a field on
// Application so a test drives the selection without building an agent tree.
type detector func(context.Context, string) ([]string, error)

// initialize answers the setup questions, writes the selections, and renders
// the result.
func (application *Application) initialize(ctx context.Context, invocation cli.Invocation) (cli.Result, error) {
	result, err := application.runSetup(ctx, invocation)
	if err != nil {
		return cli.Result{}, setupError(err)
	}
	return cli.Result{Message: initMessage(result, invocation.DryRun), Value: result}, nil
}

// runSetup answers the agent and freshness questions and applies them. It is
// shared by acr init and by the first acr install of a project that has no
// agents.yaml yet.
func (application *Application) runSetup(ctx context.Context, invocation cli.Invocation) (setup.Result, error) {
	configured, err := setup.Configured(invocation.ProjectDirectory)
	if err != nil {
		return setup.Result{}, err
	}
	stored, err := setup.Stored(invocation.ProjectDirectory)
	if err != nil {
		return setup.Result{}, err
	}
	agents, err := application.chooseAgents(ctx, invocation, configured, stored)
	if err != nil {
		return setup.Result{}, err
	}
	policy, err := application.chooseFreshness(ctx, invocation, stored)
	if err != nil {
		return setup.Result{}, err
	}
	return setup.Apply(invocation.ProjectDirectory, setup.Selection{Agents: agents, Freshness: string(policy)}, invocation.DryRun)
}

// chooseAgents resolves the agent selection. A repeated --agent wins outright
// and replaces a stored selection, which is the only way to narrow one. On a
// project that already has an agents.yaml the stored selection is
// pre-selected and detection only contributes candidates, so nothing is
// overwritten silently.
func (application *Application) chooseAgents(ctx context.Context, invocation cli.Invocation, configured bool, stored setup.Selection) ([]string, error) {
	if len(invocation.Agents) != 0 {
		return supportedAgents(invocation.Agents)
	}
	detected, err := application.detect(ctx, invocation.ProjectDirectory)
	if err != nil {
		return nil, err
	}
	preselected := detected
	if configured {
		preselected = stored.Agents
	}
	if !application.interactive(invocation) {
		return refuseEmptySelection(preselected)
	}
	answer, err := application.prompter.Ask(ctx, agentQuestion(detected, preselected))
	if err != nil {
		return nil, err
	}
	if answer.Cancelled {
		return refuseEmptySelection(nil)
	}
	return supportedAgents(answer.Values)
}

// chooseFreshness resolves the session-start policy. An explicit --freshness
// wins, then the stored value, and only an unconfigured project is asked; its
// empty answer is outdated.
func (application *Application) chooseFreshness(ctx context.Context, invocation cli.Invocation, stored setup.Selection) (freshness.Policy, error) {
	policy, _ := freshness.Resolve(stored.Freshness, string(invocation.Freshness), invocation.FreshnessExplicit)
	if invocation.FreshnessExplicit || stored.Freshness != "" || !application.interactive(invocation) {
		return policy, nil
	}
	answer, err := application.prompter.Ask(ctx, freshnessQuestion())
	if err != nil {
		return "", err
	}
	if answer.Cancelled || len(answer.Values) != 1 {
		return freshness.PolicyOutdated, nil
	}
	return freshness.Policy(answer.Values[0]), nil
}

// detectProject runs every adapter's detection over a root-confined snapshot.
func detectProject(ctx context.Context, projectDirectory string) (detected []string, err error) {
	snapshot, err := adapter.NewRootSnapshot(projectDirectory)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	return setup.Detect(ctx, snapshot)
}

func agentQuestion(detected, preselected []string) Question {
	question := Question{
		ID:     "agents",
		Prompt: "Which agents should this project realize context for?",
		Kind:   QuestionMultipleChoice,
	}
	for _, agentID := range setup.SupportedAgents() {
		label := agentID
		if contains(detected, agentID) {
			label += " (detected)"
		}
		question.Options = append(question.Options, Option{Value: agentID, Label: label, Selected: contains(preselected, agentID)})
	}
	return question
}

func freshnessQuestion() Question {
	return Question{
		ID:     "freshness",
		Prompt: "What should ACR do at session start?",
		Kind:   QuestionSingleChoice,
		Options: []Option{
			{Value: string(cli.FreshnessOutdated), Label: "outdated — report newer stable releases", Selected: true},
			{Value: string(cli.FreshnessInstall), Label: "install — reconcile latest dependencies and realize them"},
			{Value: string(cli.FreshnessNone), Label: "none — install no session-start hook"},
		},
	}
}

// supportedAgents validates and canonicalizes one selection.
func supportedAgents(values []string) ([]string, error) {
	agents := make([]string, 0, len(values))
	for _, value := range values {
		if !contains(setup.SupportedAgents(), value) {
			return nil, &cli.Error{
				ExitCode: cli.ExitUsage,
				Code:     "usage",
				Message:  fmt.Sprintf("unsupported agent %q; pass --agent with claude-code, codex, or cursor", value),
			}
		}
		if !contains(agents, value) {
			agents = append(agents, value)
		}
	}
	sort.Strings(agents)
	return refuseEmptySelection(agents)
}

// refuseEmptySelection rejects a selection of nothing in every mode: an
// agents.yaml that selects no adapter cannot realize anything.
func refuseEmptySelection(agents []string) ([]string, error) {
	if len(agents) == 0 {
		return nil, &cli.Error{
			ExitCode: cli.ExitUsage,
			Code:     "no_agent_selected",
			Message: "no agent adapter selected, and an " + dependency.ProjectFilename +
				" that selects none cannot realize anything; rerun with --agent claude-code, --agent codex, or --agent cursor",
		}
	}
	return agents, nil
}

func setupError(err error) error {
	var commandErr *cli.Error
	if errors.As(err, &commandErr) {
		return err
	}
	return &cli.Error{ExitCode: cli.ExitOperational, Code: "setup_failed", Message: err.Error(), Cause: err}
}

func initMessage(result setup.Result, dryRun bool) string {
	selection := strings.Join(result.Agents, ", ")
	switch {
	case !result.Changed:
		return fmt.Sprintf("%s already selects %s with freshness %s; run 'acr install SOURCE' to add a package.",
			dependency.ProjectFilename, selection, result.Freshness)
	case dryRun:
		return fmt.Sprintf("Would select %s with freshness %s in %s; rerun without --dry-run to write it.",
			selection, result.Freshness, dependency.ProjectFilename)
	default:
		return fmt.Sprintf("Selected %s with freshness %s in %s; run 'acr install SOURCE' to add a package.",
			selection, result.Freshness, dependency.ProjectFilename)
	}
}
