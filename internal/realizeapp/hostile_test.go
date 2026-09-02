package realizeapp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestCLIRealizeDryRunAndCheckOnTempProject(t *testing.T) {
	t.Parallel()

	projectRoot := t.TempDir()
	packageRoot := t.TempDir()
	writeFixture(t, filepath.Join(packageRoot, "rules", "always.md"), []byte("# Always\n"), 0o644)
	writeFixture(t, filepath.Join(packageRoot, "hooks", "session-start.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	value := manifest.Manifest{Artifacts: manifest.Artifacts{
		Rules: []manifest.RuleArtifact{{ID: "always-rule", Path: "rules/always.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}},
		Hooks: []manifest.HookArtifact{{ID: "session-start", Event: manifest.HookSessionStart, Path: "hooks/session-start.sh"}},
	}}
	userHooks := []byte("{\n  \"version\": 1,\n  \"userSetting\": \"keep\",\n  \"hooks\": {\"stop\": [{\"command\": \"user-command\"}]}\n}\n")
	writeFixture(t, filepath.Join(projectRoot, ".cursor", "hooks.json"), userHooks, 0o644)
	state := dependency.State{
		Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion, Dependencies: []dependency.Declaration{{Source: "github:example/all-agents", Requested: "latest"}}},
		Lock: dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion, Dependencies: []dependency.LockedDependency{{
			Source: "github:example/all-agents", Requested: "latest", Kind: dependency.ResolutionRelease,
			ReleaseID: 1, Tag: "v1.0.0", Commit: strings.Repeat("a", 40), PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("0", 64),
		}}},
	}
	if err := dependency.WriteState(projectRoot, state); err != nil {
		t.Fatal(err)
	}
	application := &Application{service: NewService(fixtureLoader{root: packageRoot, manifest: value}), fallback: cli.UnavailableApplication{}}
	rulePath := filepath.Join(projectRoot, ".cursor", "rules", "acr__example__all-agents__always-rule.mdc")
	hookPath := filepath.Join(projectRoot, ".cursor", "hooks", "acr__example__all-agents__session-start", "session-start.sh")

	stdout, stderr, exitCode := runCLI(t, application, "check", "--project", projectRoot, "--agent", "cursor", "--json")
	if exitCode != cli.ExitChanges || stdout != "" || !strings.Contains(stderr, `"code":"realization_changes"`) {
		t.Fatalf("check before realize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}

	stdout, stderr, exitCode = runCLI(t, application, "realize", "--project", projectRoot, "--agent", "cursor", "--dry-run", "--json")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"ledgerChanged":true`) {
		t.Fatalf("dry-run exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if _, err := os.Stat(rulePath); !os.IsNotExist(err) {
		t.Fatalf("dry-run created rule: %v", err)
	}
	if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run created hook: %v", err)
	}
	if got := readFile(t, filepath.Join(projectRoot, ".cursor", "hooks.json")); !bytes.Equal(got, userHooks) {
		t.Fatalf("dry-run mutated hooks.json: %s", got)
	}

	stdout, stderr, exitCode = runCLI(t, application, "realize", "--project", projectRoot, "--agent", "cursor", "--json")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"agents":["cursor"]`) {
		t.Fatalf("realize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if _, err := os.Stat(rulePath); err != nil {
		t.Fatalf("realize did not write rule: %v", err)
	}
	info, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("realize did not write hook: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("hook mode = %04o, want 0755", info.Mode().Perm())
	}
	got := readFile(t, filepath.Join(projectRoot, ".cursor", "hooks.json"))
	if !bytes.Contains(got, []byte(`"userSetting": "keep"`)) || !bytes.Contains(got, []byte(`"command": "user-command"`)) {
		t.Fatalf("realize dropped user hooks.json bytes: %s", got)
	}
	if !bytes.Contains(got, []byte("acr__example__all-agents__session-start")) {
		t.Fatalf("realize did not append owned sessionStart handler: %s", got)
	}

	stdout, stderr, exitCode = runCLI(t, application, "check", "--project", projectRoot, "--agent", "cursor")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, "current for cursor") {
		t.Fatalf("check after realize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
}

func TestApplicationCursorThreeRealizeThenUserVersion2FailsClosed(t *testing.T) {
	t.Parallel()

	projectRoot, packageRoot, state, value := realizationFixture(t)
	writeFixture(t, filepath.Join(packageRoot, "hooks", "stop.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	value.Artifacts.Hooks = []manifest.HookArtifact{{ID: "stop", Event: manifest.HookStop, Path: "hooks/stop.sh"}}
	if err := dependency.WriteState(projectRoot, state); err != nil {
		t.Fatal(err)
	}
	hooksPath := filepath.Join(projectRoot, ".cursor", "hooks.json")
	if _, err := os.Stat(hooksPath); !os.IsNotExist(err) {
		t.Fatalf("fixture leaked %s: %v", hooksPath, err)
	}
	application := &Application{service: NewService(fixtureLoader{root: packageRoot, manifest: value}), fallback: cli.UnavailableApplication{}}

	for run := 1; run <= 3; run++ {
		stdout, stderr, exitCode := runCLI(t, application, "realize", "--project", projectRoot, "--agent", "cursor", "--json")
		if exitCode != cli.ExitSuccess || stderr != "" {
			t.Fatalf("realize #%d exit = %d, stdout = %q, stderr = %q", run, exitCode, stdout, stderr)
		}
		if run == 1 {
			continue
		}
		var payload struct {
			Result Result `json:"result"`
		}
		if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
			t.Fatalf("realize #%d decode %q: %v", run, stdout, err)
		}
		if payload.Result.Plan.HasChanges() {
			t.Fatalf("realize #%d still has changes: %s", run, stdout)
		}
	}
	stdout, stderr, exitCode := runCLI(t, application, "check", "--project", projectRoot, "--agent", "cursor")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, "current for cursor") {
		t.Fatalf("check after three realize runs exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}

	before := readFile(t, hooksPath)
	edited := bytes.Replace(before, []byte(`"version": 1`), []byte(`"version": 2`), 1)
	if bytes.Equal(edited, before) {
		edited = bytes.Replace(before, []byte(`"version":1`), []byte(`"version":2`), 1)
	}
	if bytes.Equal(edited, before) {
		t.Fatalf("could not rewrite version 1 in hooks.json: %s", before)
	}
	if err := os.WriteFile(hooksPath, edited, 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exitCode = runCLI(t, application, "check", "--project", projectRoot, "--agent", "cursor", "--json")
	if exitCode != cli.ExitConflict || stdout != "" || !strings.Contains(stderr, `"code":"realization_conflict"`) {
		t.Fatalf("check after version 2 exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	stdout, stderr, exitCode = runCLI(t, application, "realize", "--project", projectRoot, "--agent", "cursor", "--json")
	if exitCode != cli.ExitConflict || stdout != "" || !strings.Contains(stderr, `"code":"realization_conflict"`) {
		t.Fatalf("realize after version 2 exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	after := readFile(t, hooksPath)
	if !bytes.Equal(after, edited) {
		t.Fatalf("version 2 edit was rewritten:\n got %s\nwant %s", after, edited)
	}
}

func readFile(t *testing.T, filename string) []byte {
	t.Helper()
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	return content
}
