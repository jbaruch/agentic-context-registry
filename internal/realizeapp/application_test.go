package realizeapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

type fixtureLoader struct {
	root     string
	manifest manifest.Manifest
}

func (loader fixtureLoader) MaterializeLocked(context.Context, dependency.LockedDependency) (dependency.MaterializedPackage, func() error, error) {
	return dependency.MaterializedPackage{Root: loader.root, Manifest: loader.manifest}, func() error { return nil }, nil
}

func TestApplicationCheckApplyAndPersistedSelection(t *testing.T) {
	t.Parallel()

	projectRoot, packageRoot, state, value := realizationFixture(t)
	state.Project.Agents = []string{"codex"}
	if err := dependency.WriteState(projectRoot, state); err != nil {
		t.Fatal(err)
	}
	application := &Application{service: NewService(fixtureLoader{root: packageRoot, manifest: value}), fallback: cli.UnavailableApplication{}}

	stdout, stderr, exitCode := runCLI(t, application, "realize", "--project", projectRoot, "--dry-run", "--json")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"agents":["codex"]`) {
		t.Fatalf("persisted-agent dry-run exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}

	stdout, stderr, exitCode = runCLI(t, application, "check", "--project", projectRoot, "--agent", "cursor", "--json")
	if exitCode != cli.ExitChanges || stdout != "" || !strings.Contains(stderr, `"code":"realization_changes"`) {
		t.Fatalf("check exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	stdout, stderr, exitCode = runCLI(t, application, "realize", "--project", projectRoot, "--agent", "cursor", "--json")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"agents":["cursor"]`) {
		t.Fatalf("realize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	target := filepath.Join(projectRoot, ".cursor", "rules", "acr__example__all-agents__guidance.mdc")
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "---\nalwaysApply: true\n---\n# Guidance\n" {
		t.Fatalf("realized rule = %q, %v", content, err)
	}
	loaded, err := dependency.LoadState(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := realize.DecodeLedger(loaded.Lock.Realization)
	if err != nil || len(ledger.Targets) != 1 || ledger.Targets[0].Path != ".cursor/rules/acr__example__all-agents__guidance.mdc" {
		t.Fatalf("persisted ledger = %#v, %v", ledger, err)
	}
	if len(loaded.Project.Agents) != 1 || loaded.Project.Agents[0] != "codex" {
		t.Fatalf("flag override mutated persisted agents = %#v", loaded.Project.Agents)
	}
	stdout, stderr, exitCode = runCLI(t, application, "check", "--project", projectRoot, "--agent", "cursor")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, "current for cursor") {
		t.Fatalf("second check exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
}

func TestApplicationRealizeIgnoresOversizedUnrelatedFile(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"claude-code": "CLAUDE.md",
		"codex":       "AGENTS.md",
	}
	for agentID, target := range tests {
		agentID, target := agentID, target
		t.Run(agentID, func(t *testing.T) {
			t.Parallel()
			projectRoot, packageRoot, state, value := realizationFixture(t)
			if err := dependency.WriteState(projectRoot, state); err != nil {
				t.Fatal(err)
			}
			assetPath := filepath.Join(projectRoot, "assets", "demo.mp4")
			if err := os.MkdirAll(filepath.Dir(assetPath), 0o755); err != nil {
				t.Fatal(err)
			}
			asset, err := os.Create(assetPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := asset.Truncate(40 << 20); err != nil {
				t.Fatal(errors.Join(err, asset.Close()))
			}
			if err := asset.Close(); err != nil {
				t.Fatal(err)
			}

			application := &Application{service: NewService(fixtureLoader{root: packageRoot, manifest: value}), fallback: cli.UnavailableApplication{}}
			stdout, stderr, exitCode := runCLI(t, application, "realize", "--project", projectRoot, "--agent", agentID, "--json")
			if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"agents":["`+agentID+`"]`) {
				t.Fatalf("realize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
			}
			if _, err := os.Stat(filepath.Join(projectRoot, target)); err != nil {
				t.Fatalf("realize did not write %s: %v", target, err)
			}
			stdout, stderr, exitCode = runCLI(t, application, "check", "--project", projectRoot, "--agent", agentID, "--json")
			if exitCode != cli.ExitSuccess || stderr != "" {
				t.Fatalf("check exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
			}
		})
	}
}

func TestApplicationDryRunAndConflictExitContracts(t *testing.T) {
	t.Parallel()

	projectRoot, packageRoot, state, value := realizationFixture(t)
	if err := dependency.WriteState(projectRoot, state); err != nil {
		t.Fatal(err)
	}
	application := &Application{service: NewService(fixtureLoader{root: packageRoot, manifest: value}), fallback: cli.UnavailableApplication{}}
	target := filepath.Join(projectRoot, ".cursor", "rules", "acr__example__all-agents__guidance.mdc")

	stdout, stderr, exitCode := runCLI(t, application, "realize", "--project", projectRoot, "--agent", "cursor", "--dry-run", "--json")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"ledgerChanged":true`) {
		t.Fatalf("dry-run exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("dry-run created target: %v", err)
	}
	writeFixture(t, target, []byte("user-owned\n"), 0o644)
	stdout, stderr, exitCode = runCLI(t, application, "realize", "--project", projectRoot, "--agent", "cursor", "--json")
	if exitCode != cli.ExitConflict || stdout != "" || !strings.Contains(stderr, `"code":"realization_conflict"`) {
		t.Fatalf("conflict exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
}

func TestApplicationCursorOwnedVersionSurvivesSecondRunAndCheck(t *testing.T) {
	t.Parallel()

	projectRoot, packageRoot, state, value := realizationFixture(t)
	writeFixture(t, filepath.Join(packageRoot, "hooks", "stop.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	value.Artifacts.Hooks = []manifest.HookArtifact{{ID: "stop", Event: manifest.HookStop, Path: "hooks/stop.sh"}}
	if err := dependency.WriteState(projectRoot, state); err != nil {
		t.Fatal(err)
	}
	application := &Application{service: NewService(fixtureLoader{root: packageRoot, manifest: value}), fallback: cli.UnavailableApplication{}}

	stdout, stderr, exitCode := runCLI(t, application, "realize", "--project", projectRoot, "--agent", "cursor", "--json")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"agents":["cursor"]`) {
		t.Fatalf("first realize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	var hooks struct {
		Version int `json:"version"`
	}
	content := readFile(t, filepath.Join(projectRoot, ".cursor", "hooks.json"))
	if err := json.Unmarshal(content, &hooks); err != nil || hooks.Version != 1 {
		t.Fatalf("first realize hooks.json = %s (decode error %v)", content, err)
	}

	stdout, stderr, exitCode = runCLI(t, application, "realize", "--project", projectRoot, "--agent", "cursor", "--json")
	var second struct {
		Result Result `json:"result"`
	}
	decodeErr := json.Unmarshal([]byte(stdout), &second)
	if exitCode != cli.ExitSuccess || stderr != "" || decodeErr != nil || second.Result.Plan.HasChanges() {
		t.Fatalf("second realize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	stdout, stderr, exitCode = runCLI(t, application, "check", "--project", projectRoot, "--agent", "cursor")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, "current for cursor") {
		t.Fatalf("check after second realize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
}

func realizationFixture(t *testing.T) (string, string, dependency.State, manifest.Manifest) {
	t.Helper()
	projectRoot := t.TempDir()
	packageRoot := t.TempDir()
	writeFixture(t, filepath.Join(packageRoot, "rules", "guidance.md"), []byte("# Guidance\n"), 0o644)
	value := manifest.Manifest{Artifacts: manifest.Artifacts{Rules: []manifest.RuleArtifact{{
		ID: "guidance", Path: "rules/guidance.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways},
	}}}}
	state := dependency.State{
		Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion, Dependencies: []dependency.Declaration{{Source: "github:example/all-agents", Requested: "latest"}}},
		Lock: dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion, Dependencies: []dependency.LockedDependency{{
			Source: "github:example/all-agents", Requested: "latest", Kind: dependency.ResolutionRelease,
			ReleaseID: 1, Tag: "v1.0.0", Commit: strings.Repeat("a", 40), PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("0", 64),
		}}},
	}
	return projectRoot, packageRoot, state, value
}

func runCLI(t *testing.T, application cli.Application, args ...string) (string, string, int) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := cli.New(&stdout, &stderr, application, "test").Run(context.Background(), args)
	return stdout.String(), stderr.String(), exitCode
}

func writeFixture(t *testing.T, filename string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, mode); err != nil {
		t.Fatal(err)
	}
}
