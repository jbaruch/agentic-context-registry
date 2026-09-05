//go:build darwin || linux

package main

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
)

// terminalJourneys covers the questions acr asks on a real terminal. The
// answers travel through an actual pseudo-terminal, so the interactive path
// is proven by the same probe that decides it in production.
func terminalJourneys() []journeyCase {
	return []journeyCase{
		{leaf: "init", name: "pty-selection-and-reprompt", kind: journeySuccess, run: journeyInitTerminalSuccess},
		{leaf: "init", name: "pty-cancel", kind: journeyRefusal, run: journeyInitTerminalCancel},
	}
}

// journeyInitTerminalSuccess answers the setup questions over a terminal, with
// one unparsable answer first, and proves the selection that reaches
// agents.yaml is the one that was typed.
func journeyInitTerminalSuccess(t *testing.T) int {
	project := newJourneyProject(t, nil)
	master, slave := openTerminalPair(t)
	if _, err := master.WriteString("not-an-agent\nclaude-code cursor\nnone\n"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	exit := runWith(project.composedClient(), slave, &stdout, &stderr, []string{"init", "--project", project.root})
	if exit != 0 {
		t.Fatalf("interactive init exit = %d\nstdout: %s\nstderr: %s", exit, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "Which agents") {
		t.Fatalf("the agent question was never asked: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "not one of the options") {
		t.Fatalf("an unparsable answer was accepted silently: %q", stderr.String())
	}
	state := loadJourneyState(t, project)
	if !reflect.DeepEqual(state.Project.Agents, []string{"claude-code", "cursor"}) {
		t.Fatalf("agents = %v, want the typed selection", state.Project.Agents)
	}
	if state.Project.Freshness != "none" {
		t.Fatalf("freshness = %q, want the typed policy", state.Project.Freshness)
	}
	return len(state.Project.Agents)
}

// journeyInitTerminalCancel proves an empty answer to a question with no
// default declines rather than choosing something nobody asked for.
func journeyInitTerminalCancel(t *testing.T) int {
	project := newJourneyProject(t, nil)
	master, slave := openTerminalPair(t)
	if _, err := master.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	before := project.snapshot()

	// --json is itself a decision not to ask, so the declined answer is only
	// reachable in text mode on a terminal.
	var stdout, stderr bytes.Buffer
	exit := runWith(project.composedClient(), slave, &stdout, &stderr, []string{"init", "--project", project.root})
	if exit != 2 {
		t.Fatalf("a declined setup exit = %d\nstdout: %s\nstderr: %s", exit, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "setup cancelled") {
		t.Fatalf("a declined setup = %q, want the cancellation refusal", stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("a declined setup wrote %q to stdout", stdout.String())
	}
	project.assertUnchanged(before, "a declined interactive setup")
	if _, err := os.Stat(project.path("agents.yaml")); err == nil {
		t.Fatal("a declined setup wrote agents.yaml")
	}
	return 1
}
