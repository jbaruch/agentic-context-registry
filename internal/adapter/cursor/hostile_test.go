package cursor_test

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
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const (
	cursorHooksPath   = ".cursor/hooks.json"
	cursorHookCommand = ".cursor/hooks/acr__example__all-agents__session-start/start.sh"
)

func TestWrongNativeEventCasing(t *testing.T) {
	t.Parallel()

	native := cursor.New()
	wrong := []byte(`{"version":1,"hooks":{"SessionStart":[{"command":"` + cursorHookCommand + `"}]}}`)
	err := native.Validate(context.Background(), adapter.ValidateRequest{
		Plan:  cursorHookPlan(),
		Files: []adapter.CandidateFile{{Path: cursorHooksPath, Content: wrong, Mode: 0o644}},
	})
	if err == nil || !strings.Contains(err.Error(), adapter.CodeInvalidNativeEvent) {
		t.Fatalf("Validate() error = %v, want %s", err, adapter.CodeInvalidNativeEvent)
	}

	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, cursorHooksPath, wrong, 0o644)
	before := snapshotTree(t, projectRoot)
	intents, realizeErr := realizeOnly(t, native, projectRoot, []adapter.Package{cursorHookPackage(t)})
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

	native := cursor.New()
	duplicate := []byte(`{"version":1,"hooks":{"sessionStart":[{"command":"` + cursorHookCommand + `"},{"command":"` + cursorHookCommand + `"}]}}`)
	err := native.Validate(context.Background(), adapter.ValidateRequest{
		Plan:  cursorHookPlan(),
		Files: []adapter.CandidateFile{{Path: cursorHooksPath, Content: duplicate, Mode: 0o644}},
	})
	if err == nil || !strings.Contains(err.Error(), adapter.CodeDuplicateConfigEntry) {
		t.Fatalf("Validate() error = %v, want %s", err, adapter.CodeDuplicateConfigEntry)
	}

	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, cursorHooksPath, duplicate, 0o644)
	before := snapshotTree(t, projectRoot)
	intents, realizeErr := realizeOnly(t, native, projectRoot, []adapter.Package{cursorHookPackage(t)})
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
	assertCursorUserConfigPreserved(t)
}

func TestUserConfigKeysPreserved(t *testing.T) {
	t.Parallel()
	assertCursorUserConfigPreserved(t)
}

func TestJSONKeyOrderPreserved(t *testing.T) {
	t.Parallel()
	assertCursorUserConfigPreserved(t)
}

func assertCursorUserConfigPreserved(t *testing.T) {
	t.Helper()
	userStop := []byte(`{"command": "user-command"}`)
	seed := []byte("{\n  \"version\": 1,\n  \"zzz\": {\"keep\": true},\n  \"tessl\": {\"enabled\": true},\n  \"userSetting\": \"keep\",\n  \"hooks\": {\"stop\": [" + string(userStop) + "]}\n}\n")
	mcp := []byte("{\n  \"mcpServers\": {\"user\": {\"command\": \"keep\"}}\n}\n")
	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, cursorHooksPath, seed, 0o644)
	writeFixtureFile(t, projectRoot, ".cursor/mcp.json", mcp, 0o644)
	ledger := applyNative(t, cursor.New(), projectRoot, []adapter.Package{cursorHookPackage(t)})
	got := readProjectFile(t, projectRoot, cursorHooksPath)
	if !bytes.Contains(got, userStop) {
		t.Fatalf("user stop handler was rewritten: %s", got)
	}
	if !bytes.Contains(got, []byte(`"zzz": {"keep": true}`)) || !bytes.Contains(got, []byte(`"tessl": {"enabled": true}`)) {
		t.Fatalf("user keys were rewritten: %s", got)
	}
	if bytes.Index(got, []byte(`"zzz"`)) > bytes.Index(got, []byte(`"userSetting"`)) {
		t.Fatalf("JSON key order of unowned members changed: %s", got)
	}
	if !bytes.Equal(readProjectFile(t, projectRoot, ".cursor/mcp.json"), mcp) {
		t.Fatalf("mcp.json was rewritten: %s", readProjectFile(t, projectRoot, ".cursor/mcp.json"))
	}
	for _, target := range ledger.Targets {
		if target.Path != cursorHooksPath {
			continue
		}
		for _, entry := range target.Entries {
			if entry.ArtifactID == "cursor-hooks-schema" {
				t.Fatal("existing version: 1 became ACR-owned")
			}
		}
	}
}

func TestCursorSecondFrontmatterConflicts(t *testing.T) {
	t.Parallel()

	native := cursor.New()
	twoBlocks := []byte("---\nalwaysApply: true\n---\n---\nother: true\n---\nbody\n")
	ruleOwner := adapter.OwnerRef{Source: "github:example/all-agents", ArtifactID: "guidance", SourcePath: "rules/guidance.md", Kind: adapter.ArtifactRule}
	rulePlan := adapter.NativePlan{Adapter: native.Descriptor(), Items: []adapter.PlanItem{{Owner: ruleOwner, Target: ".cursor/rules/acr__example__all-agents__guidance.mdc", Kind: adapter.OutputGeneratedFile, Mode: 0o644}}}
	pkg := adapter.Package{Source: ruleOwner.Source, Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Rules: []manifest.RuleArtifact{{ID: ruleOwner.ArtifactID, Path: ruleOwner.SourcePath, Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}}}}}
	err := native.Validate(context.Background(), adapter.ValidateRequest{Packages: []adapter.Package{pkg}, Plan: rulePlan, Files: []adapter.CandidateFile{{Path: rulePlan.Items[0].Target, Content: twoBlocks, Mode: 0o644}}})
	if err == nil || !strings.Contains(err.Error(), adapter.CodeMalformedFrontmatter) {
		t.Fatalf("Validate() error = %v, want %s", err, adapter.CodeMalformedFrontmatter)
	}

	packageRoot := t.TempDir()
	writeFixtureFile(t, packageRoot, "rules/guidance.md", twoBlocks, 0o644)
	source := adapter.Package{Source: "github:example/all-agents", Root: os.DirFS(packageRoot), Manifest: pkg.Manifest}
	plan, err := native.Plan(context.Background(), adapter.PlanRequest{Project: adapter.NewFSSnapshot(os.DirFS(t.TempDir())), Packages: []adapter.Package{source}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = native.Render(context.Background(), adapter.RenderRequest{Packages: []adapter.Package{source}, Plan: plan})
	if err == nil || !strings.Contains(err.Error(), adapter.CodeMalformedFrontmatter) {
		t.Fatalf("Render() error = %v, want %s", err, adapter.CodeMalformedFrontmatter)
	}

	projectRoot := t.TempDir()
	userRule := []byte("---\nalwaysApply: true\n---\n---\ntessl: true\n---\nUser rule\n")
	writeFixtureFile(t, projectRoot, ".cursor/rules/user.mdc", userRule, 0o644)
	applyNative(t, native, projectRoot, []adapter.Package{cursorAlwaysRulePackage(t)})
	if got := readProjectFile(t, projectRoot, ".cursor/rules/user.mdc"); !bytes.Equal(got, userRule) {
		t.Fatalf("unrelated two-block .mdc was rewritten: %s", got)
	}
}

func TestCursorPathsGlobsFrontmatter(t *testing.T) {
	t.Parallel()

	packageRoot := t.TempDir()
	writeFixtureFile(t, packageRoot, "rules/always.md", []byte("---\ntitle: discarded\n---\n# Always\n"), 0o644)
	writeFixtureFile(t, packageRoot, "rules/paths.md", []byte("# Paths\n"), 0o644)
	pkg := adapter.Package{
		Source: "github:example/all-agents", Root: os.DirFS(packageRoot),
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Rules: []manifest.RuleArtifact{
			{ID: "always-rule", Path: "rules/always.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}},
			{ID: "go-paths-rule", Path: "rules/paths.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationPaths, Paths: []string{"**/*.go", "scripts/**"}}},
		}}},
	}
	projectRoot := t.TempDir()
	intents := mustRealize(t, cursor.New(), projectRoot, []adapter.Package{pkg})
	byPath := intentMap(intents)
	always := string(byPath[".cursor/rules/acr__example__all-agents__always-rule.mdc"].Content)
	paths := string(byPath[".cursor/rules/acr__example__all-agents__go-paths-rule.mdc"].Content)
	if always != "---\nalwaysApply: true\n---\n# Always\n" {
		t.Fatalf("always rule = %q", always)
	}
	if paths != "---\nglobs: [\"**/*.go\", \"scripts/**\"]\nalwaysApply: false\n---\n# Paths\n" {
		t.Fatalf("paths rule = %q", paths)
	}
	if strings.Count(always, "---") != 2 || strings.Count(paths, "---") != 2 {
		t.Fatalf("frontmatter is not exactly one document: always=%q paths=%q", always, paths)
	}
	if strings.Contains(always, "title:") {
		t.Fatalf("source YAML leaked into Cursor frontmatter: %q", always)
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
	applyNative(t, cursor.New(), projectRoot, []adapter.Package{pkg})
	assertMode(t, projectRoot, ".cursor/skills/acr__example__all-agents__review-change/scripts/check.sh", 0o755)
	assertMode(t, projectRoot, ".cursor/skills/acr__example__all-agents__review-change/SKILL.md", 0o644)
	assertMode(t, projectRoot, ".cursor/skills/acr__example__all-agents__review-change/references/REFERENCE.md", 0o644)
}

func TestHookCommandQuotesSpaceInPath(t *testing.T) {
	t.Parallel()

	pkg := cursorNamedHookPackage(t, "session-start", "hooks/session start.sh")
	projectRoot := t.TempDir()
	intents := mustRealize(t, cursor.New(), projectRoot, []adapter.Package{pkg})
	content := intentMap(intents)[cursorHooksPath].Content
	var document struct {
		Hooks map[string][]struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	command := document.Hooks["sessionStart"][0].Command
	if command != `'.cursor/hooks/acr__example__all-agents__session-start/session start.sh'` && command != `".cursor/hooks/acr__example__all-agents__session-start/session start.sh"` {
		t.Fatalf("Cursor command = %q, want a quoted path that keeps the space in B", command)
	}
}

func TestHookCommandQuotesQuoteAndDollarInPath(t *testing.T) {
	t.Parallel()

	pkg := cursorNamedHookPackage(t, "session-start", `hooks/say"$x".sh`)
	projectRoot := t.TempDir()
	intents := mustRealize(t, cursor.New(), projectRoot, []adapter.Package{pkg})
	content := intentMap(intents)[cursorHooksPath].Content
	var document struct {
		Hooks map[string][]struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	command := document.Hooks["sessionStart"][0].Command
	want := `'.cursor/hooks/acr__example__all-agents__session-start/say"$x".sh'`
	if command != want {
		t.Fatalf("Cursor command = %q, want %q so B keeps the quote and $ unexpanded", command, want)
	}
}

func TestNativePathIncludesOwnerRepo(t *testing.T) {
	t.Parallel()

	one := skillPackage(t, "github:acme/one", []byte("# One\n"))
	two := skillPackage(t, "github:acme/two", []byte("# Two\n"))
	projectRoot := t.TempDir()
	intents := mustRealize(t, cursor.New(), projectRoot, []adapter.Package{two, one})
	byPath := intentMap(intents)
	first := ".cursor/skills/acr__acme__one__review-change/SKILL.md"
	second := ".cursor/skills/acr__acme__two__review-change/SKILL.md"
	if _, ok := byPath[first]; !ok {
		t.Fatalf("missing %s in %#v", first, byPath)
	}
	if _, ok := byPath[second]; !ok {
		t.Fatalf("missing %s in %#v", second, byPath)
	}
}

func TestUninstallLeavesUnrelatedHooks(t *testing.T) {
	t.Parallel()

	userStop := []byte(`{"command": "user-command"}`)
	seed := []byte("{\n  \"version\": 1,\n  \"tessl\": {\"enabled\": true},\n  \"hooks\": {\"stop\": [" + string(userStop) + "]}\n}\n")
	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, cursorHooksPath, seed, 0o644)
	native := cursor.New()
	ledger := applyNative(t, native, projectRoot, []adapter.Package{cursorHookPackage(t)})
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), native)
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(projectRoot)), nil, ledger, map[string]adapter.TargetOptions{
		cursorHooksPath: {ConfigFormat: adapter.ConfigJSON},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = realize.NewEngine().Run(projectRoot, ledger, intents, realize.ModeApply, func(realize.Ledger) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	got := readProjectFile(t, projectRoot, cursorHooksPath)
	if !bytes.Contains(got, userStop) || !bytes.Contains(got, []byte(`"version": 1`)) || !bytes.Contains(got, []byte(`"tessl": {"enabled": true}`)) {
		t.Fatalf("uninstall rewrote unrelated config: %s", got)
	}
	if bytes.Contains(got, []byte("acr__example__all-agents")) {
		t.Fatalf("uninstall left owned handler: %s", got)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".cursor", "hooks", "acr__example__all-agents__session-start", "start.sh")); !os.IsNotExist(err) {
		t.Fatalf("uninstall left owned hook file: %v", err)
	}
}

func TestHookOrderingAcrossPackagesShuffled(t *testing.T) {
	t.Parallel()

	alpha := cursorSourceHookPackage(t, "github:acme/alpha", "stop-a", "hooks/a.sh")
	beta := cursorSourceHookPackage(t, "github:acme/beta", "stop-b", "hooks/b.sh")
	forward := configBytes(t, cursor.New(), []adapter.Package{alpha, beta})
	shuffled := configBytes(t, cursor.New(), []adapter.Package{beta, alpha})
	if !bytes.Equal(forward, shuffled) {
		t.Fatalf("native hook order depends on package input order\nforward: %s\nshuffled: %s", forward, shuffled)
	}
}

func TestDetectCreatesNothing(t *testing.T) {
	t.Parallel()

	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, cursorHooksPath, []byte("{\"version\":1}\n"), 0o644)
	writeFixtureFile(t, projectRoot, ".cursor/rules/user.mdc", []byte("---\nalwaysApply: true\n---\nUser\n"), 0o644)
	before := snapshotTree(t, projectRoot)
	detection, err := cursor.New().Detect(context.Background(), adapter.DetectRequest{Project: adapter.NewFSSnapshot(os.DirFS(projectRoot))})
	if err != nil || !detection.Detected {
		t.Fatalf("Detect() = %#v, %v", detection, err)
	}
	if got := snapshotTree(t, projectRoot); !reflect.DeepEqual(got, before) {
		t.Fatalf("Detect() wrote files:\n got %#v\nwant %#v", got, before)
	}
}

func cursorHookPlan() adapter.NativePlan {
	owner := adapter.OwnerRef{Source: "github:example/all-agents", ArtifactID: "session-start", SourcePath: "hooks/start.sh", Kind: adapter.ArtifactHook, Event: manifest.HookSessionStart}
	return adapter.NativePlan{Adapter: cursor.New().Descriptor(), Items: []adapter.PlanItem{{Owner: owner, Target: cursorHooksPath, Kind: adapter.OutputConfigMerge, Mode: 0o644}}}
}

func cursorHookPackage(t *testing.T) adapter.Package {
	t.Helper()
	return cursorNamedHookPackage(t, "session-start", "hooks/start.sh")
}

func cursorNamedHookPackage(t *testing.T, id, relative string) adapter.Package {
	t.Helper()
	return cursorSourceHookPackage(t, "github:example/all-agents", id, relative)
}

func cursorSourceHookPackage(t *testing.T, source, id, relative string) adapter.Package {
	t.Helper()
	packageRoot := t.TempDir()
	writeFixtureFile(t, packageRoot, relative, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	return adapter.Package{
		Source: source, Root: os.DirFS(packageRoot),
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Hooks: []manifest.HookArtifact{{ID: id, Event: manifest.HookSessionStart, Path: relative}}}},
	}
}

func cursorAlwaysRulePackage(t *testing.T) adapter.Package {
	t.Helper()
	packageRoot := t.TempDir()
	writeFixtureFile(t, packageRoot, "rules/always.md", []byte("# Always\n"), 0o644)
	return adapter.Package{
		Source: "github:example/all-agents", Root: os.DirFS(packageRoot),
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Rules: []manifest.RuleArtifact{{ID: "always-rule", Path: "rules/always.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}}}},
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
