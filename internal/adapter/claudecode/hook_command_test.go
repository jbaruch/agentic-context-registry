package claudecode_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// NUL separators distinguish empty arguments and embedded newlines.
const hookProbe = "#!/bin/bash\nset -euo pipefail\nprintf 'ran\\0'\nfor argument in \"$@\"; do printf '%s\\0' \"$argument\"; done\n"

// Exercise the generated settings with Claude's documented invocation contract,
// including the exec-form switch on an explicitly empty args array (#103).
func TestRealizedHookCommandRunsUnderAProjectPathWithSpaces(t *testing.T) {
	t.Parallel()
	for _, relative := range []string{
		"hooks/session-start.sh",
		"hooks/session start.sh",
		"hooks/session 'quoted' \"double\" $dollar `backtick` ; & (check) [glob].sh",
	} {
		for _, args := range [][]string{
			nil,
			{},
			{"--policy", "outdated"},
			{"", "two words", "line\nbreak", "tab\there", "'single'", `"double"`, "$HOME", "${UNEXPANDED}", "$(printf injected)", "`printf injected`", "a;b&c|d>e<f", "*?[abc]", `back\slash`, ""},
		} {
			t.Run(relative+"/"+strings.Join(args, ","), func(t *testing.T) {
				t.Parallel()
				project := spacedProjectDir(t)
				pkg := hookProbePackage(t, relative)
				pkg.Manifest.Artifacts.Hooks[0].Args = args
				applyNative(t, claudecode.New(), project, []adapter.Package{pkg})
				realized := ".claude/hooks/acr__example__all-agents__session-start/" + filepath.Base(relative)
				assertMode(t, project, realized, 0o755)
				if got := string(readProjectFile(t, project, realized)); got != hookProbe {
					t.Fatalf("hook payload changed: %q", got)
				}
				command, gotArgs := sessionStartHook(t, readProjectFile(t, project, claudeSettingsPath))
				if gotArgs == nil {
					t.Fatal("missing or null args cannot select valid native exec form")
				}
				if !slices.Equal(gotArgs, args) {
					t.Fatalf("args = %#v, want %#v", gotArgs, args)
				}
				assertProbeRuns(t, project, command, gotArgs, args)
			})
		}
	}
}

// The unquoted command emitted before 0.1.4 is already valid exec form when
// it has arguments. Re-realization must retain one working handler and its
// foreign settings while updating the older ledger version.
func TestRealizeRetainsAnExistingUnquotedExecHook(t *testing.T) {
	t.Parallel()

	const relative = "hooks/session-start.sh"
	realized := ".claude/hooks/acr__example__all-agents__session-start/session-start.sh"
	unquoted := "${CLAUDE_PROJECT_DIR}/" + realized

	project := spacedProjectDir(t)
	previous := seedOldMaterialization(t, project, unquoted)

	native := claudecode.New()
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), native)
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(project)), []adapter.Package{hookProbePackage(t, relative)}, previous)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := realize.NewEngine().Run(project, previous, intents, realize.ModeApply, func(realize.Ledger) error { return nil }); err != nil {
		t.Fatal(err)
	}

	settings := readProjectFile(t, project, claudeSettingsPath)
	if !strings.Contains(string(settings), `"tessl": {"enabled": true}`) {
		t.Fatalf("upgrade dropped foreign settings: %s", settings)
	}
	command, args := sessionStartHook(t, settings)
	if want := `${CLAUDE_PROJECT_DIR}/` + realized; command != want {
		t.Fatalf("upgraded command = %q, want %q", command, want)
	}
	assertProbeRuns(t, project, command, args, []string{"--mode", "two words"})
}

// seedOldMaterialization writes the settings file an older ACR would have
// left, with the same foreign keys around it, and returns the ledger that ACR
// would have recorded for it. The managed hash comes from the shipped
// preservation compiler rather than from a literal, so the seeded entry is
// owned by exactly the identity the current run reconciles against.
func seedOldMaterialization(t *testing.T, project, command string) realize.Ledger {
	t.Helper()

	owner := adapter.OwnerRef{Source: "github:example/all-agents", ArtifactID: "session-start", SourcePath: "hooks/session-start.sh", Kind: adapter.ArtifactHook, Event: manifest.HookSessionStart}
	encoded, err := json.Marshal(map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": command, "args": []string{"--mode", "two words"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	foreign := []byte("{\n  \"tessl\": {\"enabled\": true}\n}\n")
	compilation, err := preserve.NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{
			Path:     claudeSettingsPath,
			Observed: &adapter.ObservedFile{Path: claudeSettingsPath, Content: foreign, Mode: 0o644, Hash: sha256Hash(foreign)},
		},
		Format: adapter.ConfigJSON,
		Desired: []adapter.ConfigEntry{{
			Owner: owner, Container: []string{"hooks", "SessionStart"}, Kind: adapter.ConfigElement,
			Key: adapter.CanonicalConfigOwnerKey(owner, "claude-code", claudeSettingsPath, "SessionStart"), EncodedValue: encoded,
			AdapterID: "claude-code",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compilation.Candidate == nil {
		t.Fatal("the preservation compiler returned no candidate for the seeded materialization")
	}
	writeFixtureFile(t, project, claudeSettingsPath, compilation.Candidate.Content, 0o644)

	entries := make([]realize.Entry, 0, len(compilation.Managed))
	for _, managed := range compilation.Managed {
		entries = append(entries, realize.Entry{
			Source: managed.Owner.Source, ArtifactID: managed.Owner.ArtifactID, ArtifactKind: managed.Kind,
			SourcePath: managed.Owner.SourcePath, Adapter: "claude-code", AdapterVersion: "1.0.1", ManagedHash: managed.ManagedHash,
		})
	}
	return realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{{
		Path: claudeSettingsPath, Mode: 0o644, Ownership: realize.OwnershipShared,
		OutputHash: sha256Hash(compilation.Candidate.Content), Entries: entries,
	}}}
}

// hookProbePackage is one package whose single SessionStart hook is the
// executable probe, carried at relative so a caller can choose whether the
// filename itself contains a space.
func hookProbePackage(t *testing.T, relative string) adapter.Package {
	t.Helper()
	packageRoot := t.TempDir()
	writeFixtureFile(t, packageRoot, relative, []byte(hookProbe), 0o755)
	return adapter.Package{
		Source: "github:example/all-agents", Root: os.DirFS(packageRoot),
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Hooks: []manifest.HookArtifact{{
			ID: "session-start", Event: manifest.HookSessionStart, Path: relative, Args: []string{"--mode", "two words"},
		}}}},
	}
}

// spacedProjectDir is a project root whose own path carries a space, which is
// the half of the command CLAUDE_PROJECT_DIR expands to.
func spacedProjectDir(t *testing.T) string {
	t.Helper()
	project := filepath.Join(t.TempDir(), "project with spaces 'quote' \"double\" $dollar `backtick`")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	return project
}

// sessionStartHook returns the single SessionStart command and its arguments
// from a realized settings document.
func sessionStartHook(t *testing.T, settings []byte) (string, []string) {
	t.Helper()
	var decoded struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(settings, &decoded); err != nil {
		t.Fatalf("decode %s: %v\n%s", claudeSettingsPath, err, settings)
	}
	groups := decoded.Hooks["SessionStart"]
	if len(groups) != 1 || len(groups[0].Hooks) != 1 {
		t.Fatalf("SessionStart handlers = %#v, want exactly one: %s", groups, settings)
	}
	return groups[0].Hooks[0].Command, groups[0].Hooks[0].Args
}

// Claude substitutes known path placeholders as strings, then directly spawns
// command when args is present. No shell parses or appends exec-form arguments.
// The shell branch models legacy settings with no args; it cannot mask the
// quoted-executable ENOENT reproduced in #103.
func assertProbeRuns(t *testing.T, project, command string, args, wantArgs []string) {
	t.Helper()
	command = strings.ReplaceAll(command, "${CLAUDE_PROJECT_DIR}", project)
	var process *exec.Cmd
	if args == nil {
		process = exec.Command("/bin/sh", "-c", command)
	} else {
		process = exec.Command(command, args...)
	}
	process.Dir = t.TempDir()
	process.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR="+project)
	output, err := process.CombinedOutput()
	if err != nil {
		t.Fatalf("run %s: %v\n%s", command, err, output)
	}
	want := "ran\x00"
	for _, arg := range wantArgs {
		want += arg + "\x00"
	}
	if string(output) != want {
		t.Fatalf("run %s output = %q, want %q", command, output, want)
	}
}

func sha256Hash(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func TestRealizedZeroArgumentHookPreservesProcessStreams(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, body, stdout, stderr string
		exit                       int
	}{
		{name: "success", body: "printf 'hook succeeded\\n'", stdout: "hook succeeded\n"},
		{name: "absence", body: "exit 0"},
		{name: "stderr failure", body: "printf 'hook diagnostic\\n' >&2\nexit 2", stderr: "hook diagnostic\n", exit: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			pkg := claudeHookPackage(t)
			root := t.TempDir()
			writeFixtureFile(t, root, "hooks/start.sh", []byte("#!/bin/bash\nset -euo pipefail\n"+test.body+"\n"), 0o755)
			pkg.Root = os.DirFS(root)
			project := spacedProjectDir(t)
			applyNative(t, claudecode.New(), project, []adapter.Package{pkg})
			command, args := sessionStartHook(t, readProjectFile(t, project, claudeSettingsPath))
			if args == nil || len(args) != 0 {
				t.Fatalf("zero-argument hook argv = %#v", args)
			}
			process := exec.Command(strings.ReplaceAll(command, "${CLAUDE_PROJECT_DIR}", project), args...)
			var stdout, stderr bytes.Buffer
			process.Stdout, process.Stderr = &stdout, &stderr
			err := process.Run()
			exit := 0
			if err != nil {
				var failure *exec.ExitError
				if !errors.As(err, &failure) {
					t.Fatalf("hook did not start: %v", err)
				}
				exit = failure.ExitCode()
			}
			if exit != test.exit || stdout.String() != test.stdout || stderr.String() != test.stderr {
				t.Fatalf("hook outcome = (%d, %q, %q), want (%d, %q, %q)", exit, stdout.String(), stderr.String(), test.exit, test.stdout, test.stderr)
			}
		})
	}
}
