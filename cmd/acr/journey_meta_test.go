package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

// metaJourneys covers the two commands that read no project state. Both run
// through the compiled shipped executable, which is the lane that proves what
// a user's shell actually gets.
func metaJourneys() []journeyCase {
	return []journeyCase{
		{leaf: "help", name: "aliases-and-commands", kind: journeySuccess, run: journeyHelpSuccess},
		{leaf: "help", name: "unknown-and-extra-arguments", kind: journeyRefusal, run: journeyHelpRefusals},
		{leaf: "version", name: "aliases-and-json", kind: journeySuccess, run: journeyVersionSuccess},
		{leaf: "version", name: "unknown-argument", kind: journeyRefusal, run: journeyVersionRefusals},
	}
}

// journeyHelpSuccess proves help reaches every executable leaf, agrees across
// its aliases, and touches neither the project nor the machine state.
func journeyHelpSuccess(t *testing.T) int {
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	before := project.snapshot()

	root := project.runBinaryBare(binary, 0, "help")
	if root.stderr != "" {
		t.Fatalf("acr help wrote %q to stderr", root.stderr)
	}
	for _, alias := range [][]string{{"--help"}, {"-h"}, {}} {
		aliased := project.runBinaryBare(binary, 0, alias...)
		if aliased.stdout != root.stdout {
			t.Fatalf("acr %v help text differs from acr help", alias)
		}
	}

	proven := 0
	for _, leaf := range cli.Leaves() {
		if !strings.Contains(root.stdout, "  "+leaf.Command+" ") {
			t.Errorf("root help does not list %q as a command", leaf.Command)
		}
		if occurrences := strings.Count(root.stdout, "  "+leaf.Command+" "); occurrences != 1 && leaf.Subcommand == "" {
			t.Errorf("root help lists %q %d times, want once", leaf.Command, occurrences)
		}
		// help takes one command name; a subcommand's usage is reached through
		// its parent's help and through the leaf's own --help.
		perCommand := project.runBinaryBare(binary, 0, "help", leaf.Command)
		if !strings.Contains(perCommand.stdout, "Usage:") || !strings.Contains(perCommand.stdout, "acr "+leaf.Command) {
			t.Errorf("acr help %s = %q, want its usage", leaf.Command, perCommand.stdout)
		}
		if leaf.Subcommand != "" && !strings.Contains(perCommand.stdout, leaf.Command+" "+leaf.Subcommand) {
			t.Errorf("acr help %s does not document the %q subcommand: %q", leaf.Command, leaf.Subcommand, perCommand.stdout)
		}
		flagForm := project.runBinaryBare(binary, 0, append(leaf.Args(), "--help")...)
		if flagForm.stdout != perCommand.stdout {
			t.Errorf("acr %s --help differs from acr help %s", leaf.String(), leaf.Command)
		}
		proven++
	}
	if proven != len(cli.Leaves()) {
		t.Fatalf("help proved %d leaves, want %d", proven, len(cli.Leaves()))
	}
	project.assertUnchanged(before, "acr help")
	assertStateHomeEmpty(t, project)
	return proven
}

func journeyHelpRefusals(t *testing.T) int {
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	before := project.snapshot()

	refusals := []struct {
		args  []string
		names string
	}{
		{args: []string{"help", "missing"}, names: `unknown command "missing"`},
		{args: []string{"help", "install", "extra"}, names: "usage: acr help [COMMAND]"},
		{args: []string{"missing"}, names: `unknown command "missing"`},
		{args: []string{"migrate"}, names: "acr migrate tessl"},
		{args: []string{"freshness"}, names: "acr freshness run"},
		{args: []string{"migrate", "tessl-plug"}, names: `unsupported migration target "tessl-plug"`},
		{args: []string{"freshness", "walk"}, names: `unsupported freshness subcommand "walk"`},
	}
	for _, refusal := range refusals {
		run := project.runBinaryBare(binary, 2, refusal.args...)
		if run.stdout != "" {
			t.Errorf("acr %v wrote %q to stdout", refusal.args, run.stdout)
		}
		if !strings.Contains(run.stderr, refusal.names) {
			t.Errorf("acr %v refusal = %q, want it to name %q", refusal.args, run.stderr, refusal.names)
		}
	}
	project.assertUnchanged(before, "a help refusal")
	assertStateHomeEmpty(t, project)
	return len(refusals)
}

// journeyVersionSuccess builds an executable with a known identity and proves
// every alias reports exactly that identity, in text and in one JSON envelope.
func journeyVersionSuccess(t *testing.T) int {
	binary := journeyBinaryWithIdentity(t, "9.8.7", "c0ffee1")
	project := newJourneyProject(t, nil)
	before := project.snapshot()

	text := project.runBinaryBare(binary, 0, "version")
	if strings.TrimSpace(text.stdout) != "9.8.7 (c0ffee1)" || text.stderr != "" {
		t.Fatalf("acr version stdout = %q, stderr = %q", text.stdout, text.stderr)
	}
	for _, alias := range []string{"-v", "--version"} {
		aliased := project.runBinaryBare(binary, 0, alias)
		if aliased.stdout != text.stdout {
			t.Fatalf("acr %s = %q, want %q", alias, aliased.stdout, text.stdout)
		}
	}
	structured := project.runBinaryBare(binary, 0, "version", "--json")
	result := journeyResult(t, structured.stdout)
	if result["version"] != "9.8.7" || result["commit"] != "c0ffee1" {
		t.Fatalf("acr version --json result = %#v", result)
	}
	if structured.stderr != "" {
		t.Fatalf("acr version --json wrote %q to stderr", structured.stderr)
	}
	project.assertUnchanged(before, "acr version")
	assertStateHomeEmpty(t, project)
	return 3
}

func journeyVersionRefusals(t *testing.T) int {
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	before := project.snapshot()

	refusals := [][]string{{"version", "--bogus"}, {"version", "extra"}}
	for _, args := range refusals {
		run := project.runBinaryBare(binary, 2, args...)
		if run.stdout != "" || !strings.Contains(run.stderr, "version") {
			t.Errorf("acr %v stdout = %q, stderr = %q", args, run.stdout, run.stderr)
		}
	}
	project.assertUnchanged(before, "a version refusal")
	return len(refusals)
}

// journeyBinaryWithIdentity compiles the shipped executable with the release
// linker values, so a version assertion checks a supplied identity rather than
// whatever the working tree happened to embed.
func journeyBinaryWithIdentity(t *testing.T, version, commit string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "acr")
	command := exec.Command("go", "build",
		"-ldflags", "-X main.version="+version+" -X main.commit="+commit,
		"-o", binary, "./cmd/acr")
	command.Dir = filepath.Join("..", "..")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build acr with a fixed identity: %v\n%s", err, output)
	}
	return binary
}

// assertStateHomeEmpty proves a command that reads no project state also wrote
// no machine-local state.
func assertStateHomeEmpty(t *testing.T, project *journeyProject) {
	t.Helper()
	entries, err := os.ReadDir(project.stateHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("machine state directory is not empty: %v", names)
	}
}
