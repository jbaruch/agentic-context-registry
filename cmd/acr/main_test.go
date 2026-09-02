package main

import (
	"bytes"
	"runtime/debug"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(&stdout, &stderr, []string{"version"})

	if exitCode != 0 {
		t.Fatalf("run(version) exit code = %d, want 0", exitCode)
	}
	if got := strings.TrimSpace(stdout.String()); got != version {
		t.Fatalf("run(version) stdout = %q, want %q", got, version)
	}
	if stderr.Len() != 0 {
		t.Fatalf("run(version) stderr = %q, want empty", stderr.String())
	}
}

func TestRunFreshnessThroughPublishApplication(t *testing.T) {
	t.Setenv("ACR_STATE_HOME", t.TempDir())
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(&stdout, &stderr, []string{"freshness", "run", "--project", t.TempDir(), "--policy", "none", "--json"})

	if exitCode != 0 {
		t.Fatalf("run(freshness none) exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, `"command":"freshness"`) || !strings.Contains(got, `"policy":"none"`) {
		t.Fatalf("run(freshness none) stdout = %q, want freshness result", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("run(freshness none) stderr = %q, want empty", stderr.String())
	}
}

func TestResolveVersionUsesInstalledModuleTag(t *testing.T) {
	t.Parallel()

	got := resolveVersion("dev", func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, true
	})
	if got != "v1.2.3" {
		t.Fatalf("resolveVersion() = %q", got)
	}
	linked := resolveVersion("v2.0.0", func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, true
	})
	if linked != "v2.0.0" {
		t.Fatalf("linked resolveVersion() = %q", linked)
	}
}

func TestRunHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(&stdout, &stderr, []string{"help"})

	if exitCode != 0 {
		t.Fatalf("run(help) exit code = %d, want 0", exitCode)
	}
	if got := stdout.String(); !strings.Contains(got, "Usage:\n  acr") {
		t.Fatalf("run(help) stdout = %q, want usage", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("run(help) stderr = %q, want empty", stderr.String())
	}
}

func TestRunWithoutCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(&stdout, &stderr, nil)

	if exitCode != 0 {
		t.Fatalf("run() exit code = %d, want 0", exitCode)
	}
	if got := stdout.String(); !strings.Contains(got, "Agentic Context Registry") {
		t.Fatalf("run() stdout = %q, want product name", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("run() stderr = %q, want empty", stderr.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(&stdout, &stderr, []string{"missing"})

	if exitCode != 2 {
		t.Fatalf("run(missing) exit code = %d, want 2", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("run(missing) stdout = %q, want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "unknown command") {
		t.Fatalf("run(missing) stderr = %q, want unknown-command diagnostic", got)
	}
	if got := stderr.String(); !strings.Contains(got, "acr help") {
		t.Fatalf("run(missing) stderr = %q, want recovery guidance", got)
	}
}

func TestRunRealizeRequiresAnAgentSelection(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(&stdout, &stderr, []string{"realize", "--json"})

	if exitCode != 1 {
		t.Fatalf("run(realize --json) exit code = %d, want 1", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("run(realize --json) stdout = %q, want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, `"code":"realization_failed"`) || !strings.Contains(got, "no agent adapters selected") {
		t.Fatalf("run(realize --json) stderr = %q, want selection diagnostic", got)
	}
	if got := stderr.String(); !strings.Contains(got, "https://github.com/jbaruch/agentic-context-registry/issues") {
		t.Fatalf("run(realize --json) stderr = %q, want implementation-status guidance", got)
	}
}

func TestRunListEmptyProject(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(&stdout, &stderr, []string{"list", "--project", t.TempDir(), "--json"})

	if exitCode != 0 {
		t.Fatalf("run(list --json) exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, `"dependencies":[]`) {
		t.Fatalf("run(list --json) stdout = %q, want empty dependency list", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("run(list --json) stderr = %q, want empty", stderr.String())
	}
}
