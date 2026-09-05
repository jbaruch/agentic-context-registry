package main

import (
	"os"
	"strings"
	"testing"
)

// streamJourneys covers the process contract itself: the composed-subprocess
// lane runs the shipped composition with real file descriptors, a real argv
// and a real exit status, so the JSON envelope, the stream separation and
// every exit code are checked where a caller sees them.
func streamJourneys() []journeyCase {
	return []journeyCase{
		{leaf: "install", name: "subprocess-streams-and-exits", kind: journeySuccess, run: journeySubprocessStreams},
	}
}

func journeySubprocessStreams(t *testing.T) int {
	github := newJourneyGitHub(t)
	alpha := newJourneyPackage(t, "example/alpha", "1.0.0")
	github.SeedRelease(alpha.fullName, alpha.tag, alpha.commit, alpha.archive)

	project := newJourneyProject(t, github)
	project.runSubprocess(0, "init", "--agent", "claude-code", "--freshness", "none", "--non-interactive")

	// Exit 0: one success envelope on stdout, nothing on stderr.
	install := project.runSubprocess(0, "install", alpha.source, "--non-interactive", "--json")
	if install.stderr != "" {
		t.Fatalf("a successful install wrote %q to stderr", install.stderr)
	}
	result := journeyResult(t, install.stdout)
	dependencies, ok := result["dependencies"].([]any)
	if !ok || len(dependencies) != 1 {
		t.Fatalf("install result = %#v, want one locked dependency", result)
	}
	assertNoCredentialLeak(t, install)
	locked := loadJourneyState(t, project).Lock.Dependencies
	if len(locked) != 1 || locked[0].Commit != alpha.commit {
		t.Fatalf("the subprocess locked %#v, want %s", locked, alpha.commit)
	}
	project.runSubprocess(0, "realize")
	project.runSubprocess(0, "check")

	// Exit 2: usage refusals write one error envelope on stderr and nothing on
	// stdout.
	usage := project.runSubprocess(2, "install", "--hold", "--non-interactive", "--json")
	if usage.stdout != "" {
		t.Fatalf("a usage refusal wrote %q to stdout", usage.stdout)
	}
	if journeyError(t, usage.stderr)["code"] == "" {
		t.Fatalf("usage refusal = %q, want a machine-readable code", usage.stderr)
	}

	// Exit 1: an operational failure keeps stdout clean too.
	operational := project.runSubprocess(1, "install", "github:example/absent", "--non-interactive", "--json")
	if operational.stdout != "" {
		t.Fatalf("an operational failure wrote %q to stdout", operational.stdout)
	}

	// Exit 3: check reports unapplied changes and names them.
	removed := nativeSkillDirectory(".claude", alpha.fullName, "advocate") + "/SKILL.md"
	if err := os.Remove(project.path(removed)); err != nil {
		t.Fatal(err)
	}
	changes := project.runSubprocess(3, "check", "--json")
	if !strings.Contains(journeyError(t, changes.stderr)["message"].(string), removed) {
		t.Fatalf("check --json = %q, want it to name %s", changes.stderr, removed)
	}
	project.runSubprocess(0, "realize")

	// Exit 4: an ownership refusal changes nothing.
	reverify2Put(t, project.root, removed, "# Edited by the operator\n", 0o644)
	before := project.snapshot()
	conflict := project.runSubprocess(4, "uninstall", alpha.source, "--json")
	if journeyError(t, conflict.stderr)["code"] != "realization_conflict" {
		t.Fatalf("uninstall over an edited output = %q", conflict.stderr)
	}
	project.assertUnchanged(before, "a refused uninstall in a separate process")
	return len(dependencies) + 4
}
