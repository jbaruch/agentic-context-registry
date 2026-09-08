package claudecode_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// hookProbe reports that it ran and echoes each argument on its own line, so
// an argument carrying a space proves it survived as one word rather than as
// two.
const hookProbe = "#!/bin/sh\nprintf 'ran\\n'\nfor argument in \"$@\"; do printf 'arg=%s\\n' \"$argument\"; done\n"

// TestRealizedHookCommandRunsUnderAProjectPathWithSpaces executes the command
// the adapter actually generated, the way Claude Code runs it: the command
// string through a shell, with the project root in CLAUDE_PROJECT_DIR and the
// hook's own arguments as its argument vector.
//
// Issue #100: quoting keyed on the relative target, so an ordinary hook
// filename produced a bare `${CLAUDE_PROJECT_DIR}/…` and a project directory
// with a space split the executable path. The relative target the adapter can
// see is never the half that decides, so both filename shapes run the probe
// from the same spaced project root. The shell's working directory is
// somewhere else entirely, so only the expanded absolute path can resolve.
func TestRealizedHookCommandRunsUnderAProjectPathWithSpaces(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		relative string
	}{
		{name: "ordinary filename", relative: "hooks/session-start.sh"},
		{name: "space in the filename", relative: "hooks/session start.sh"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			project := spacedProjectDir(t)
			applyNative(t, claudecode.New(), project, []adapter.Package{hookProbePackage(t, test.relative)})

			realized := path.Join(".claude/hooks/acr__example__all-agents__session-start", filepath.Base(test.relative))
			assertMode(t, project, realized, 0o755)

			command, args := sessionStartHook(t, readProjectFile(t, project, claudeSettingsPath))
			assertProbeRuns(t, project, command, args)
			if want := `"${CLAUDE_PROJECT_DIR}/` + realized + `"`; command != want {
				t.Fatalf("generated command = %q, want %q", command, want)
			}
		})
	}
}

// TestRealizeUpgradesAnExistingUnquotedHookCommand realizes over the
// materialization an older ACR left behind: the same owned entry carrying the
// bare `${CLAUDE_PROJECT_DIR}/…` command, recorded in the ledger as owned. The
// upgrade has to replace that element in place, leaving neither the old
// command behind nor a second handler beside the new one, and the foreign
// settings around it have to survive.
func TestRealizeUpgradesAnExistingUnquotedHookCommand(t *testing.T) {
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
	if strings.Contains(string(settings), `"`+unquoted+`"`) {
		t.Fatalf("upgrade left the unquoted command behind: %s", settings)
	}
	if !strings.Contains(string(settings), `"tessl": {"enabled": true}`) {
		t.Fatalf("upgrade dropped foreign settings: %s", settings)
	}
	command, args := sessionStartHook(t, settings)
	if want := `"${CLAUDE_PROJECT_DIR}/` + realized + `"`; command != want {
		t.Fatalf("upgraded command = %q, want %q", command, want)
	}
	assertProbeRuns(t, project, command, args)
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
	project := filepath.Join(t.TempDir(), "project with spaces")
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

// assertProbeRuns runs the generated command the way Claude Code's shell form
// does — the command string through a shell, the hook's arguments as its
// argument vector — and holds the result to the probe's contract. The command
// names the probe alone, so the executable bit and the probe's own shebang are
// what run it; no interpreter is prefixed.
func assertProbeRuns(t *testing.T, project, command string, args []string) {
	t.Helper()
	shell := exec.Command("sh", append([]string{"-c", command + ` "$@"`, "sh"}, args...)...)
	shell.Dir = t.TempDir()
	shell.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR="+project)
	output, err := shell.CombinedOutput()
	if err != nil {
		t.Fatalf("run %s: %v\n%s", command, err, output)
	}
	want := "ran\narg=--mode\narg=two words\n"
	if string(output) != want {
		t.Fatalf("run %s output = %q, want %q", command, output, want)
	}
}

func sha256Hash(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}
