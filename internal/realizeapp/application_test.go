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
	if err != nil || !ledgerHasTarget(ledger, ".cursor/rules/acr__example__all-agents__guidance.mdc") ||
		!ledgerHasTarget(ledger, ".cursor/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh") {
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

func ledgerHasTarget(ledger realize.Ledger, path string) bool {
	for _, target := range ledger.Targets {
		if target.Path == path {
			return true
		}
	}
	return false
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

func TestApplicationGraphErrorsUseConflictExit(t *testing.T) {
	t.Parallel()

	projectRoot, packageRoot, state, value := realizationFixture(t)
	if err := dependency.WriteState(projectRoot, state); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(projectRoot, "AGENTS.md"), []byte("@notes.md\n@notes.md\n"), 0o644)
	writeFixture(t, filepath.Join(projectRoot, "notes.md"), []byte("# Notes\n"), 0o644)
	application := &Application{service: NewService(fixtureLoader{root: packageRoot, manifest: value}), fallback: cli.UnavailableApplication{}}

	for _, command := range []string{"realize", "check"} {
		stdout, stderr, exitCode := runCLI(t, application, command, "--project", projectRoot, "--agent", "codex", "--json")
		if exitCode != cli.ExitConflict || stdout != "" || !strings.Contains(stderr, `"code":"realization_conflict"`) || !strings.Contains(stderr, "duplicate_include") || !strings.Contains(stderr, "resolve the conflicting managed content") {
			t.Fatalf("%s graph conflict exit = %d, stdout = %q, stderr = %q", command, exitCode, stdout, stderr)
		}
	}
}

func TestApplicationMixedIncludeGuidance(t *testing.T) {
	t.Parallel()

	projectRoot, packageRoot, state, value := realizationFixture(t)
	if err := dependency.WriteState(projectRoot, state); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(projectRoot, "AGENTS.md"), []byte("# Instructions\n@notes.md\n@notes.md\n@missing.md\n@../outside.md\n"), 0o644)
	writeFixture(t, filepath.Join(projectRoot, "notes.md"), []byte("# Notes\n"), 0o644)
	application := &Application{service: NewService(fixtureLoader{root: packageRoot, manifest: value}), fallback: cli.UnavailableApplication{}}
	const evidenceAndRepairs = `plan shared instruction hosts: duplicate_include: AGENTS.md:3: include "notes.md" duplicates line 2; ` +
		"invalid_include: AGENTS.md:5: include path escapes the project; " +
		`unresolved_include: AGENTS.md:4: include "missing.md" does not resolve to a regular file; ` +
		"create the missing file or any missing directories in its path, or correct or remove the unresolved import; " +
		"point the invalid import at a regular file inside the project or remove it; " +
		"resolve the conflicting managed content"

	before := reverifyGuardTree(t, projectRoot)
	for _, test := range []struct {
		name, command string
		flags         []string
	}{
		{name: "check", command: "check"},
		{name: "realize", command: "realize"},
		{name: "realize dry run", command: "realize", flags: []string{"--dry-run"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			wantMessage := evidenceAndRepairs
			if test.command == "realize" {
				wantMessage += ", then retry your original 'acr realize' command"
			}
			wantMessage += ", then run 'acr check'"
			args := append([]string{test.command, "--project", projectRoot, "--agent", "codex"}, test.flags...)
			stdout, stderr, exitCode := runCLI(t, application, args...)
			if exitCode != cli.ExitConflict || stdout != "" || stderr != "acr "+test.command+": "+wantMessage+"\n" {
				t.Errorf("text conflict exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
			}

			stdout, stderr, exitCode = runCLI(t, application, append(args, "--json")...)
			if exitCode != cli.ExitConflict || stdout != "" {
				t.Fatalf("JSON conflict exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
			}
			var envelope map[string]any
			if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
				t.Fatalf("decode JSON conflict: %v", err)
			}
			diagnostic, _ := envelope["error"].(map[string]any)
			if len(envelope) != 3 || envelope["ok"] != false || envelope["command"] != test.command ||
				len(diagnostic) != 2 || diagnostic["code"] != "realization_conflict" || diagnostic["message"] != wantMessage {
				t.Errorf("unexpected JSON conflict: %s", stderr)
			}
			if after := reverifyGuardTree(t, projectRoot); !reverifyTreesEqual(before, after) {
				t.Fatal("refusal changed project bytes or modes")
			}
		})
	}
}

func TestApplicationIncludeNormalizationFailures(t *testing.T) {
	t.Parallel()
	guidance := map[string]string{
		"unresolved_include": "create the missing file or any missing directories in its path, or correct or remove the unresolved import",
		"invalid_include":    "point the invalid import at a regular file inside the project or remove it",
	}
	for _, test := range []struct {
		name, token, code, reason string
	}{
		{name: "missing file", token: "rules/missing.md", code: "unresolved_include", reason: `include "rules/missing.md" does not resolve to a regular file`},
		{name: "relative missing traversal", token: "new/../rules/style.md", code: "unresolved_include", reason: "include does not resolve to a regular file"},
		{name: "absolute missing traversal", token: "{root}/new/../rules/style.md", code: "unresolved_include", reason: "include does not resolve to a regular file"},
		{name: "double leading separator missing traversal", token: "/{root}/new/../rules/style.md", code: "unresolved_include", reason: "include does not resolve to a regular file"},
		{name: "leading dot missing traversal", token: "/.{root}/new/../rules/style.md", code: "unresolved_include", reason: "include does not resolve to a regular file"},
		{name: "absolute NUL", token: "{root}/rules/\x00style.md", code: "invalid_include", reason: "include path is empty or contains NUL"},
		{name: "relative escape", token: "../outside.md", code: "invalid_include", reason: "include path escapes the project"},
		{name: "absolute escape", token: "{root}/../outside.md", code: "invalid_include", reason: "include path escapes the project"},
		{name: "double leading separator escape", token: "/{root}/../outside.md", code: "invalid_include", reason: "include path escapes the project"},
		{name: "symlink escape", token: "linked.md", code: "invalid_include", reason: "include path escapes the project"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			projectRoot, packageRoot, state, value := realizationFixture(t)
			if err := dependency.WriteState(projectRoot, state); err != nil {
				t.Fatal(err)
			}
			token := strings.ReplaceAll(test.token, "{root}", filepath.ToSlash(projectRoot))
			writeFixture(t, filepath.Join(projectRoot, "AGENTS.md"), []byte("# Instructions\n@"+token+"\n"), 0o644)
			writeFixture(t, filepath.Join(projectRoot, "rules/style.md"), []byte("# Style\n"), 0o644)
			writeFixture(t, filepath.Join(projectRoot, "../outside.md"), []byte("# Outside\n"), 0o644)
			if err := os.Symlink("../outside.md", filepath.Join(projectRoot, "linked.md")); err != nil {
				t.Fatal(err)
			}
			canonicalRoot, err := filepath.EvalSymlinks(projectRoot)
			if err != nil {
				t.Fatal(err)
			}
			before := reverifyGuardTree(t, projectRoot)
			application := &Application{service: NewService(fixtureLoader{root: packageRoot, manifest: value}), fallback: cli.UnavailableApplication{}}
			for _, command := range []string{"realize", "check"} {
				wantGuidance := guidance[test.code]
				if command == "realize" {
					wantGuidance += ", then retry your original 'acr realize' command"
				}
				wantGuidance += ", then run 'acr check'"
				for _, format := range []string{"text", "json"} {
					t.Run(command+"/"+format, func(t *testing.T) {
						args := []string{command, "--project", projectRoot, "--agent", "codex"}
						if format == "json" {
							args = append(args, "--json")
						}
						stdout, stderr, exitCode := runCLI(t, application, args...)
						if exitCode != cli.ExitConflict || stdout != "" {
							t.Fatalf("exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
						}
						message := stderr
						if format == "json" {
							var envelope struct {
								OK      bool   `json:"ok"`
								Command string `json:"command"`
								Error   struct {
									Code    string `json:"code"`
									Message string `json:"message"`
								} `json:"error"`
							}
							if err := json.Unmarshal([]byte(stderr), &envelope); err != nil || envelope.OK || envelope.Command != command || envelope.Error.Code != "realization_conflict" {
								t.Fatalf("JSON conflict = %q, decode error = %v", stderr, err)
							}
							message = envelope.Error.Message
						}
						for _, want := range []string{test.code + ": AGENTS.md:2: " + test.reason, wantGuidance} {
							if !strings.Contains(message, want) {
								t.Errorf("diagnostic = %q, want %q", message, want)
							}
						}
						if strings.Contains(message, "resolve the conflicting managed content") {
							t.Errorf("diagnostic includes generic guidance: %q", message)
						}
						for _, prefix := range []string{filepath.Dir(projectRoot), filepath.Dir(canonicalRoot)} {
							if strings.Contains(stderr, filepath.ToSlash(prefix)) {
								t.Fatalf("diagnostic exposes machine prefix: %q", stderr)
							}
						}
						if after := reverifyGuardTree(t, projectRoot); !reverifyTreesEqual(before, after) {
							t.Fatalf("refusal changed project bytes or modes: before=%v, after=%v", before, after)
						}
					})
				}
			}
		})
	}
}

func TestRealizationErrorMapsTesslOwnedTargetToConflict(t *testing.T) {
	t.Parallel()
	converted := realizationError(&realize.TesslOwnedTargetError{Path: ".tessl/plugins/example/alpha/rules/always.md"}, cli.CommandRealize)
	var cliErr *cli.Error
	if !errors.As(converted, &cliErr) || cliErr.ExitCode != cli.ExitConflict || cliErr.Code != "tessl_owned_target" {
		t.Fatalf("realizationError() = %#v, want tessl_owned_target conflict", converted)
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
	exitCode := cli.New(&stdout, &stderr, application, cli.Build{Version: "test"}).Run(context.Background(), args)
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
