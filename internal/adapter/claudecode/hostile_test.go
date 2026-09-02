package claudecode_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const (
	claudeSettingsPath = ".claude/settings.json"
	claudeHookCommand  = "${CLAUDE_PROJECT_DIR}/.claude/hooks/acr__example__all-agents__session-start/start.sh"
)

func TestWrongNativeEventCasing(t *testing.T) {
	t.Parallel()

	native := claudecode.New()
	wrong := []byte(`{"hooks":{"sessionStart":[{"hooks":[{"type":"command","command":"` + claudeHookCommand + `"}]}]}}`)
	err := native.Validate(context.Background(), adapter.ValidateRequest{
		Plan:  claudeHookPlan(),
		Files: []adapter.CandidateFile{{Path: claudeSettingsPath, Content: wrong, Mode: 0o644}},
	})
	if err == nil || !strings.Contains(err.Error(), adapter.CodeInvalidNativeEvent) {
		t.Fatalf("Validate() error = %v, want %s", err, adapter.CodeInvalidNativeEvent)
	}

	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, claudeSettingsPath, wrong, 0o644)
	writeFixtureFile(t, projectRoot, "sentinel.txt", []byte("untouched\n"), 0o644)
	before := snapshotTree(t, projectRoot)
	intents, realizeErr := realizeOnly(t, native, projectRoot, []adapter.Package{claudeHookPackage(t)})
	if realizeErr == nil || !strings.Contains(realizeErr.Error(), adapter.CodeInvalidNativeEvent) {
		t.Fatalf("Realize() error = %v, want %s", realizeErr, adapter.CodeInvalidNativeEvent)
	}
	if len(intents) != 0 {
		t.Fatalf("Realize() leaked intents after validation failure: %#v", intents)
	}
	if got := snapshotTree(t, projectRoot); !reflect.DeepEqual(got, before) {
		t.Fatalf("tree changed after invalid_native_event:\n got %#v\nwant %#v", got, before)
	}
}

func TestDuplicateHookEntryInNativeConfig(t *testing.T) {
	t.Parallel()

	native := claudecode.New()
	duplicate := []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"` + claudeHookCommand + `"}]},{"hooks":[{"type":"command","command":"` + claudeHookCommand + `"}]}]}}`)
	err := native.Validate(context.Background(), adapter.ValidateRequest{
		Plan:  claudeHookPlan(),
		Files: []adapter.CandidateFile{{Path: claudeSettingsPath, Content: duplicate, Mode: 0o644}},
	})
	if err == nil || !strings.Contains(err.Error(), adapter.CodeDuplicateConfigEntry) {
		t.Fatalf("Validate() error = %v, want %s", err, adapter.CodeDuplicateConfigEntry)
	}

	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, claudeSettingsPath, duplicate, 0o644)
	before := snapshotTree(t, projectRoot)
	intents, realizeErr := realizeOnly(t, native, projectRoot, []adapter.Package{claudeHookPackage(t)})
	if realizeErr == nil || !strings.Contains(realizeErr.Error(), adapter.CodeDuplicateConfigEntry) {
		t.Fatalf("Realize() error = %v, want %s", realizeErr, adapter.CodeDuplicateConfigEntry)
	}
	if len(intents) != 0 {
		t.Fatalf("Realize() leaked intents after duplicate_config_entry: %#v", intents)
	}
	if got := snapshotTree(t, projectRoot); !reflect.DeepEqual(got, before) {
		t.Fatalf("tree changed after duplicate_config_entry:\n got %#v\nwant %#v", got, before)
	}
}

func TestUnrelatedHookBytesPreserved(t *testing.T) {
	t.Parallel()
	assertClaudeUserConfigPreserved(t)
}

func TestUserConfigKeysPreserved(t *testing.T) {
	t.Parallel()
	assertClaudeUserConfigPreserved(t)
}

func TestJSONKeyOrderPreserved(t *testing.T) {
	t.Parallel()
	assertClaudeUserConfigPreserved(t)
}

func assertClaudeUserConfigPreserved(t *testing.T) {
	t.Helper()
	userStop := []byte(`{"hooks": [{"type": "command", "command": "user-command"}]}`)
	seed := []byte("{\n  \"zzz\": {\"keep\": true},\n  \"tessl\": {\"enabled\": true},\n  \"permissions\": {\"allow\": [\"Read\"]},\n  \"hooks\": {\"Stop\": [" + string(userStop) + "]}\n}\n")
	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, claudeSettingsPath, seed, 0o644)
	applyNative(t, claudecode.New(), projectRoot, []adapter.Package{claudeHookPackage(t)})
	got := readProjectFile(t, projectRoot, claudeSettingsPath)
	if !bytes.Contains(got, userStop) {
		t.Fatalf("user Stop matcher was rewritten: %s", got)
	}
	if !bytes.Contains(got, []byte(`"zzz": {"keep": true}`)) || !bytes.Contains(got, []byte(`"tessl": {"enabled": true}`)) {
		t.Fatalf("user keys were rewritten: %s", got)
	}
	if bytes.Index(got, []byte(`"zzz"`)) > bytes.Index(got, []byte(`"permissions"`)) {
		t.Fatalf("JSON key order of unowned members changed: %s", got)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".claude", "settings.local.json")); !os.IsNotExist(err) {
		t.Fatalf("wrote settings.local.json: %v", err)
	}
}

func TestSkillScriptExecuteBitSurvives(t *testing.T) {
	t.Parallel()

	packageRoot := t.TempDir()
	writeFixtureFile(t, packageRoot, "skills/review-change/SKILL.md", []byte("# Review\n"), 0o644)
	writeFixtureFile(t, packageRoot, "skills/review-change/references/REFERENCE.md", []byte("Reference\n"), 0o644)
	writeFixtureFile(t, packageRoot, "skills/review-change/scripts/check.sh", []byte("#!/bin/sh\nexit 0\n"), 0o755)
	pkg := adapter.Package{
		Source: "github:example/all-agents", Root: os.DirFS(packageRoot),
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Skills: []manifest.SkillArtifact{{ID: "review-change", Path: "skills/review-change"}}}},
	}
	projectRoot := t.TempDir()
	applyNative(t, claudecode.New(), projectRoot, []adapter.Package{pkg})
	assertMode(t, projectRoot, ".claude/skills/acr__example__all-agents__review-change/scripts/check.sh", 0o755)
	assertMode(t, projectRoot, ".claude/skills/acr__example__all-agents__review-change/SKILL.md", 0o644)
	assertMode(t, projectRoot, ".claude/skills/acr__example__all-agents__review-change/references/REFERENCE.md", 0o644)
}

func TestHookCommandQuotesSpaceInPath(t *testing.T) {
	t.Parallel()

	pkg := claudeNamedHookPackage(t, "session-start", "hooks/session start.sh")
	projectRoot := t.TempDir()
	intents := mustRealize(t, claudecode.New(), projectRoot, []adapter.Package{pkg})
	content := intentMap(intents)[claudeSettingsPath].Content
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(content, &settings); err != nil {
		t.Fatal(err)
	}
	command := settings.Hooks["SessionStart"][0].Hooks[0].Command
	if command != `"${CLAUDE_PROJECT_DIR}/.claude/hooks/acr__example__all-agents__session-start/session start.sh"` {
		t.Fatalf("Claude command = %q, want a quoted path that keeps the space in B", command)
	}
}

func TestNativePathIncludesOwnerRepo(t *testing.T) {
	t.Parallel()

	one := skillPackage(t, "github:acme/one", []byte("# One\n"))
	two := skillPackage(t, "github:acme/two", []byte("# Two\n"))
	projectRoot := t.TempDir()
	intents := mustRealize(t, claudecode.New(), projectRoot, []adapter.Package{two, one})
	byPath := intentMap(intents)
	first := ".claude/skills/acr__acme__one__review-change/SKILL.md"
	second := ".claude/skills/acr__acme__two__review-change/SKILL.md"
	if _, ok := byPath[first]; !ok {
		t.Fatalf("missing %s in %#v", first, byPath)
	}
	if _, ok := byPath[second]; !ok {
		t.Fatalf("missing %s in %#v", second, byPath)
	}
	if first == second {
		t.Fatal("native skill paths collided")
	}
}

func TestUninstallLeavesUnrelatedHooks(t *testing.T) {
	t.Parallel()

	userStop := []byte(`{"hooks": [{"type": "command", "command": "user-command"}]}`)
	seed := []byte("{\n  \"tessl\": {\"enabled\": true},\n  \"hooks\": {\"Stop\": [" + string(userStop) + "]}\n}\n")
	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, claudeSettingsPath, seed, 0o644)
	native := claudecode.New()
	pkg := claudeHookPackage(t)
	ledger := applyNative(t, native, projectRoot, []adapter.Package{pkg})
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), native)
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(projectRoot)), nil, ledger, map[string]adapter.TargetOptions{
		claudeSettingsPath: {ConfigFormat: adapter.ConfigJSON},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = realize.NewEngine().Run(projectRoot, ledger, intents, realize.ModeApply, func(realize.Ledger) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	got := readProjectFile(t, projectRoot, claudeSettingsPath)
	if !bytes.Contains(got, userStop) || !bytes.Contains(got, []byte(`"tessl": {"enabled": true}`)) {
		t.Fatalf("uninstall rewrote unrelated config: %s", got)
	}
	if bytes.Contains(got, []byte("acr__example__all-agents")) {
		t.Fatalf("uninstall left owned handler: %s", got)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".claude", "hooks", "acr__example__all-agents__session-start", "start.sh")); !os.IsNotExist(err) {
		t.Fatalf("uninstall left owned hook file: %v", err)
	}
}

func TestHookOrderingAcrossPackagesShuffled(t *testing.T) {
	t.Parallel()

	alpha := claudeSourceHookPackage(t, "github:acme/alpha", "stop-a", "hooks/a.sh")
	beta := claudeSourceHookPackage(t, "github:acme/beta", "stop-b", "hooks/b.sh")
	forward := configBytes(t, claudecode.New(), []adapter.Package{alpha, beta})
	shuffled := configBytes(t, claudecode.New(), []adapter.Package{beta, alpha})
	if !bytes.Equal(forward, shuffled) {
		t.Fatalf("native hook order depends on package input order\nforward: %s\nshuffled: %s", forward, shuffled)
	}
}

func TestDetectCreatesNothing(t *testing.T) {
	t.Parallel()

	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, "CLAUDE.md", []byte("@AGENTS.md\n"), 0o644)
	writeFixtureFile(t, projectRoot, claudeSettingsPath, []byte("{}\n"), 0o644)
	before := snapshotTree(t, projectRoot)
	detection, err := claudecode.New().Detect(context.Background(), adapter.DetectRequest{Project: adapter.NewFSSnapshot(os.DirFS(projectRoot))})
	if err != nil || !detection.Detected {
		t.Fatalf("Detect() = %#v, %v", detection, err)
	}
	if got := snapshotTree(t, projectRoot); !reflect.DeepEqual(got, before) {
		t.Fatalf("Detect() wrote files:\n got %#v\nwant %#v", got, before)
	}
}

func claudeHookPlan() adapter.NativePlan {
	owner := adapter.OwnerRef{Source: "github:example/all-agents", ArtifactID: "session-start", SourcePath: "hooks/start.sh", Kind: adapter.ArtifactHook, Event: manifest.HookSessionStart}
	return adapter.NativePlan{Adapter: claudecode.New().Descriptor(), Items: []adapter.PlanItem{{Owner: owner, Target: claudeSettingsPath, Kind: adapter.OutputConfigMerge, Mode: 0o644}}}
}

func claudeHookPackage(t *testing.T) adapter.Package {
	t.Helper()
	return claudeNamedHookPackage(t, "session-start", "hooks/start.sh")
}

func claudeNamedHookPackage(t *testing.T, id, relative string) adapter.Package {
	t.Helper()
	return claudeSourceHookPackage(t, "github:example/all-agents", id, relative)
}

func claudeSourceHookPackage(t *testing.T, source, id, relative string) adapter.Package {
	t.Helper()
	packageRoot := t.TempDir()
	writeFixtureFile(t, packageRoot, relative, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	return adapter.Package{
		Source: source, Root: os.DirFS(packageRoot),
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Hooks: []manifest.HookArtifact{{ID: id, Event: manifest.HookSessionStart, Path: relative}}}},
	}
}

func skillPackage(t *testing.T, source string, body []byte) adapter.Package {
	t.Helper()
	packageRoot := t.TempDir()
	writeFixtureFile(t, packageRoot, "skills/review-change/SKILL.md", body, 0o644)
	return adapter.Package{
		Source: source, Root: os.DirFS(packageRoot),
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Skills: []manifest.SkillArtifact{{ID: "review-change", Path: "skills/review-change"}}}},
	}
}

func realizeOnly(t *testing.T, native adapter.Adapter, projectRoot string, packages []adapter.Package) ([]realize.Intent, error) {
	t.Helper()
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), native)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(projectRoot)), packages, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion})
}

func mustRealize(t *testing.T, native adapter.Adapter, projectRoot string, packages []adapter.Package) []realize.Intent {
	t.Helper()
	intents, err := realizeOnly(t, native, projectRoot, packages)
	if err != nil {
		t.Fatal(err)
	}
	return intents
}

func applyNative(t *testing.T, native adapter.Adapter, projectRoot string, packages []adapter.Package) realize.Ledger {
	t.Helper()
	intents := mustRealize(t, native, projectRoot, packages)
	ledger := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}
	_, err := realize.NewEngine().Run(projectRoot, ledger, intents, realize.ModeApply, func(next realize.Ledger) error {
		ledger = next
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func configBytes(t *testing.T, native adapter.Adapter, packages []adapter.Package) []byte {
	t.Helper()
	intents := mustRealize(t, native, t.TempDir(), packages)
	for _, intent := range intents {
		if strings.HasSuffix(intent.Path, ".json") || strings.HasSuffix(intent.Path, ".toml") {
			return append([]byte(nil), intent.Content...)
		}
	}
	t.Fatal("no config intent")
	return nil
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(filename string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		tree[filepath.ToSlash(relative)] = string(content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func readProjectFile(t *testing.T, root, relative string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func assertMode(t *testing.T, root, relative string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", relative, got, want)
	}
}
