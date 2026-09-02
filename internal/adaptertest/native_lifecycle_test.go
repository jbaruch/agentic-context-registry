package adaptertest

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestFreshnessSessionStartCleanupPreservesUserState(t *testing.T) {
	fixture := filepath.Join("testdata", "freshness-session-start")
	loaded, err := manifest.Load(filepath.Join(fixture, "package"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		native     adapter.Adapter
		configPath string
		format     adapter.ConfigFormat
		hookPath   string
	}{
		{native: claudecode.New(), configPath: ".claude/settings.json", format: adapter.ConfigJSON, hookPath: ".claude/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/hook.go"},
		{native: codex.New(), configPath: ".codex/config.toml", format: adapter.ConfigTOML, hookPath: ".codex/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/hook.go"},
		{native: cursor.New(), configPath: ".cursor/hooks.json", format: adapter.ConfigJSON, hookPath: ".cursor/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/hook.go"},
	} {
		test := test
		t.Run(test.native.Descriptor().ID, func(t *testing.T) {
			projectRoot := t.TempDir()
			if err := copyTree(filepath.Join(fixture, "project"), projectRoot); err != nil {
				t.Fatal(err)
			}
			pkg := adapter.Package{Source: "github:" + loaded.Name, Root: os.DirFS(filepath.Join(fixture, "package")), Manifest: loaded}
			coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), test.native)
			if err != nil {
				t.Fatal(err)
			}
			snapshot := adapter.NewFSSnapshot(os.DirFS(projectRoot))
			ledger := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}
			intents, err := coordinator.Realize(context.Background(), snapshot, []adapter.Package{pkg}, ledger)
			if err != nil {
				t.Fatal(err)
			}
			_, err = realize.NewEngine().Run(projectRoot, ledger, intents, realize.ModeApply, func(next realize.Ledger) error {
				ledger = next
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			unchanged, err := coordinator.Realize(context.Background(), snapshot, []adapter.Package{pkg}, ledger)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := realize.NewEngine().Run(projectRoot, ledger, unchanged, realize.ModeCheck, nil)
			if err != nil || plan.HasChanges() {
				t.Fatalf("idempotent check = %#v, %v", plan, err)
			}
			intents, err = coordinator.Realize(context.Background(), snapshot, nil, ledger, map[string]adapter.TargetOptions{
				test.configPath: {ConfigFormat: test.format},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = realize.NewEngine().Run(projectRoot, ledger, intents, realize.ModeApply, func(next realize.Ledger) error {
				ledger = next
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(ledger.Targets) != 0 {
				t.Fatalf("cleanup ledger = %#v", ledger)
			}
			if _, err := os.Stat(filepath.Join(projectRoot, filepath.FromSlash(test.hookPath))); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("cleanup retained generated hook: %v", err)
			}
			config, err := os.ReadFile(filepath.Join(projectRoot, filepath.FromSlash(test.configPath)))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(config), "user-command") || strings.Contains(string(config), "freshness-session-start") {
				t.Fatalf("cleanup config = %s", config)
			}
		})
	}
}

func TestNativeEventVocabulary(t *testing.T) {
	events := []manifest.HookEvent{
		manifest.HookSessionStart, manifest.HookSessionEnd, manifest.HookUserPromptSubmit,
		manifest.HookPreToolUse, manifest.HookPostToolUse, manifest.HookStop,
	}
	expected := map[string][]string{
		"claude-code": {"SessionStart", "SessionEnd", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"},
		"codex":       {"SessionStart", "SessionEnd", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"},
		"cursor":      {"sessionStart", "sessionEnd", "beforeSubmitPrompt", "preToolUse", "postToolUse", "stop"},
	}
	packageRoot := t.TempDir()
	hooks := make([]manifest.HookArtifact, len(events))
	for index, event := range events {
		name := string(event) + ".sh"
		writeNativeFixture(t, filepath.Join(packageRoot, "hooks", name), []byte("#!/bin/sh\nexit 0\n"), 0o755)
		hooks[index] = manifest.HookArtifact{ID: string(event), Event: event, Path: "hooks/" + name}
	}
	pkg := adapter.Package{Source: "github:example/events", Root: os.DirFS(packageRoot), Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Hooks: hooks}}}
	for _, native := range []adapter.Adapter{claudecode.New(), codex.New(), cursor.New()} {
		native := native
		t.Run(native.Descriptor().ID, func(t *testing.T) {
			projectRoot := t.TempDir()
			coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), native)
			if err != nil {
				t.Fatal(err)
			}
			intents, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(projectRoot)), []adapter.Package{pkg}, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion})
			if err != nil {
				t.Fatal(err)
			}
			config := configIntent(t, intents).Content
			for _, spelling := range expected[native.Descriptor().ID] {
				if native.Descriptor().ID == "codex" {
					if !strings.Contains(string(config), "[[hooks."+spelling+"]]\n") {
						t.Errorf("config misses event %q: %s", spelling, config)
					}
					continue
				}
				var document struct {
					Hooks map[string]json.RawMessage `json:"hooks"`
				}
				if err := json.Unmarshal(config, &document); err != nil {
					t.Fatal(err)
				}
				if _, exists := document.Hooks[spelling]; !exists {
					t.Errorf("config misses event %q: %s", spelling, config)
				}
			}
		})
	}
}

func TestNativeValidationRejectsProjectionDamage(t *testing.T) {
	for _, test := range []struct {
		native adapter.Adapter
		prefix string
	}{
		{native: claudecode.New(), prefix: ".claude"},
		{native: codex.New(), prefix: ".codex"},
		{native: cursor.New(), prefix: ".cursor"},
	} {
		test := test
		t.Run(test.native.Descriptor().ID, func(t *testing.T) {
			owner := adapter.OwnerRef{Source: "github:example/all-agents", ArtifactID: "review", SourcePath: "skills/review/SKILL.md", Kind: adapter.ArtifactSkill}
			target := test.prefix + "/skills/acr__example__all-agents__review/SKILL.md"
			plan := adapter.NativePlan{Adapter: test.native.Descriptor(), Items: []adapter.PlanItem{{Owner: owner, Target: target, Kind: adapter.OutputGeneratedFile, Mode: 0o644}}}
			err := test.native.Validate(context.Background(), adapter.ValidateRequest{Plan: plan})
			if err == nil || !strings.Contains(err.Error(), adapter.CodeInvalidSkillTree) {
				t.Fatalf("missing skill error = %v", err)
			}

			projectRoot := t.TempDir()
			writeNativeFixture(t, filepath.Join(projectRoot, filepath.FromSlash(target)), []byte("# Skill\n"), 0o644)
			writeNativeFixture(t, filepath.Join(projectRoot, filepath.FromSlash(filepath.Dir(target)), "extra.md"), []byte("extra\n"), 0o644)
			err = test.native.Validate(context.Background(), adapter.ValidateRequest{
				Project: adapter.NewFSSnapshot(os.DirFS(projectRoot)), Plan: plan,
				Files: []adapter.CandidateFile{{Path: target, Content: []byte("# Skill\n"), Mode: 0o644}},
			})
			if err == nil || !strings.Contains(err.Error(), adapter.CodeInvalidSkillTree) {
				t.Fatalf("extra skill error = %v", err)
			}

			symlinkRoot := t.TempDir()
			symlinkTarget := filepath.Join(symlinkRoot, filepath.FromSlash(target))
			if err := os.MkdirAll(filepath.Dir(symlinkTarget), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("missing.md", symlinkTarget); err != nil {
				t.Fatal(err)
			}
			err = test.native.Validate(context.Background(), adapter.ValidateRequest{
				Project: adapter.NewFSSnapshot(os.DirFS(symlinkRoot)), Plan: plan,
				Files: []adapter.CandidateFile{{Path: target, Content: []byte("# Skill\n"), Mode: 0o644}},
			})
			if err == nil || !strings.Contains(err.Error(), adapter.CodeInvalidSkillTree) {
				t.Fatalf("symlink skill error = %v", err)
			}

			scriptOwner := adapter.OwnerRef{Source: owner.Source, ArtifactID: "script", SourcePath: "scripts/run.sh", Kind: adapter.ArtifactScript}
			scriptPlan := adapter.NativePlan{Adapter: test.native.Descriptor(), Items: []adapter.PlanItem{{Owner: scriptOwner, Target: test.prefix + "/scripts/name/run.sh", Kind: adapter.OutputGeneratedFile, Mode: 0o755}}}
			err = test.native.Validate(context.Background(), adapter.ValidateRequest{Plan: scriptPlan, Files: []adapter.CandidateFile{{Path: scriptPlan.Items[0].Target, Mode: 0o644}}})
			if err == nil || !strings.Contains(err.Error(), adapter.CodeInvalidExecutableMode) {
				t.Fatalf("non-executable script error = %v", err)
			}
		})
	}
}

func TestNativeRenderingIsOrderIndependent(t *testing.T) {
	packageRoot := t.TempDir()
	for _, name := range []string{"a.sh", "b.sh", "c.sh"} {
		writeNativeFixture(t, filepath.Join(packageRoot, "hooks", name), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	}
	build := func(order []string) adapter.Package {
		hooks := make([]manifest.HookArtifact, len(order))
		for index, id := range order {
			hooks[index] = manifest.HookArtifact{ID: id, Event: manifest.HookStop, Path: "hooks/" + id + ".sh"}
		}
		return adapter.Package{Source: "github:example/order", Root: os.DirFS(packageRoot), Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Hooks: hooks}}}
	}
	for _, native := range []adapter.Adapter{claudecode.New(), codex.New(), cursor.New()} {
		native := native
		t.Run(native.Descriptor().ID, func(t *testing.T) {
			render := func(pkg adapter.Package) []realize.Intent {
				coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), native)
				if err != nil {
					t.Fatal(err)
				}
				intents, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(t.TempDir())), []adapter.Package{pkg}, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion})
				if err != nil {
					t.Fatal(err)
				}
				return intents
			}
			forward := render(build([]string{"a", "b", "c"}))
			shuffled := render(build([]string{"c", "a", "b"}))
			if !reflect.DeepEqual(forward, shuffled) {
				t.Fatalf("render depends on input order:\nforward: %#v\nshuffled: %#v", forward, shuffled)
			}
		})
	}
}

func configIntent(t *testing.T, intents []realize.Intent) realize.Intent {
	t.Helper()
	for _, intent := range intents {
		if strings.HasSuffix(intent.Path, ".json") || strings.HasSuffix(intent.Path, ".toml") {
			return intent
		}
	}
	t.Fatal("no config intent")
	return realize.Intent{}
}

func writeNativeFixture(t *testing.T, filename string, content []byte, mode os.FileMode) {
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
