package codex_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const (
	codexConfigPath  = ".codex/config.toml"
	codexHookCommand = `"$(git rev-parse --show-toplevel)/.codex/hooks/acr__example__all-agents__session-start/start.sh"`
)

func TestWrongNativeEventCasing(t *testing.T) {
	t.Parallel()

	native := codex.New()
	wrong := []byte("[hooks]\nsessionStart = [{ hooks = [{ type = \"command\", command = " + quotedTOML(codexHookCommand) + " }] }]\n")
	err := native.Validate(context.Background(), adapter.ValidateRequest{
		Plan:  codexHookPlan(),
		Files: []adapter.CandidateFile{{Path: codexConfigPath, Content: wrong, Mode: 0o644}},
	})
	if err == nil || !strings.Contains(err.Error(), adapter.CodeInvalidNativeEvent) {
		t.Fatalf("Validate() error = %v, want %s", err, adapter.CodeInvalidNativeEvent)
	}

	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, codexConfigPath, wrong, 0o644)
	before := snapshotTree(t, projectRoot)
	intents, realizeErr := realizeOnly(t, native, projectRoot, []adapter.Package{codexHookPackage(t)})
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

	native := codex.New()
	duplicate := []byte("[[hooks.SessionStart]]\n[[hooks.SessionStart.hooks]]\ntype = \"command\"\ncommand = " + quotedTOML(codexHookCommand) + "\n[[hooks.SessionStart]]\n[[hooks.SessionStart.hooks]]\ntype = \"command\"\ncommand = " + quotedTOML(codexHookCommand) + "\n")
	err := native.Validate(context.Background(), adapter.ValidateRequest{
		Plan:  codexHookPlan(),
		Files: []adapter.CandidateFile{{Path: codexConfigPath, Content: duplicate, Mode: 0o644}},
	})
	if err == nil || !strings.Contains(err.Error(), adapter.CodeDuplicateConfigEntry) {
		t.Fatalf("Validate() error = %v, want %s", err, adapter.CodeDuplicateConfigEntry)
	}

	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, codexConfigPath, duplicate, 0o644)
	before := snapshotTree(t, projectRoot)
	intents, realizeErr := realizeOnly(t, native, projectRoot, []adapter.Package{codexHookPackage(t)})
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
	assertCodexUserConfigPreserved(t)
}

func TestUserConfigKeysPreserved(t *testing.T) {
	t.Parallel()
	assertCodexUserConfigPreserved(t)
}

func TestTOMLTableOrderPreserved(t *testing.T) {
	t.Parallel()
	assertCodexUserConfigPreserved(t)
}

func assertCodexUserConfigPreserved(t *testing.T) {
	t.Helper()
	seed := []byte("# keep this comment\nmodel = \"gpt-5\"\n\n[mcp_servers.tessl]\ncommand = \"tessl\"\n\n[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ntype = \"command\"\ncommand = \"user-command\"\n\n[hooks.state.acr__user]\nlast_run = 123\n")
	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, codexConfigPath, seed, 0o644)
	applyNative(t, codex.New(), projectRoot, []adapter.Package{codexHookPackage(t)})
	got := readProjectFile(t, projectRoot, codexConfigPath)
	if !bytes.HasPrefix(got, seed) {
		t.Fatalf("TOML unowned prefix was rewritten.\n got: %s\nwant prefix: %s", got, seed)
	}
	if !bytes.Contains(got, []byte("command = \"user-command\"")) {
		t.Fatalf("unrelated Stop handler missing: %s", got)
	}
	if !bytes.Contains(got, []byte("[hooks.state.acr__user]")) || !bytes.Contains(got, []byte("[mcp_servers.tessl]")) {
		t.Fatalf("unowned tables missing: %s", got)
	}
	if bytes.Index(got, []byte("[mcp_servers.tessl]")) > bytes.Index(got, []byte("[hooks.state.acr__user]")) {
		t.Fatalf("TOML table order of unowned members changed: %s", got)
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
	applyNative(t, codex.New(), projectRoot, []adapter.Package{pkg})
	assertMode(t, projectRoot, ".codex/skills/acr__example__all-agents__review-change/scripts/check.sh", 0o755)
	assertMode(t, projectRoot, ".codex/skills/acr__example__all-agents__review-change/SKILL.md", 0o644)
	assertMode(t, projectRoot, ".codex/skills/acr__example__all-agents__review-change/references/REFERENCE.md", 0o644)
}

func TestHookCommandQuotesSpaceInPath(t *testing.T) {
	t.Parallel()

	pkg := codexNamedHookPackage(t, "session-start", "hooks/session start.sh")
	projectRoot := t.TempDir()
	intents := mustRealize(t, codex.New(), projectRoot, []adapter.Package{pkg})
	config := string(intentMap(intents)[codexConfigPath].Content)
	if !strings.Contains(config, "session start.sh") {
		t.Fatalf("Codex command missing spaced basename B: %q", config)
	}
	quoted := `command = "\"$(git rev-parse --show-toplevel)/.codex/hooks/acr__example__all-agents__session-start/session start.sh\""`
	if !strings.Contains(config, quoted) {
		t.Fatalf("Codex command is not shell-quoted around B: %q", config)
	}
}

func TestNativePathIncludesOwnerRepo(t *testing.T) {
	t.Parallel()

	one := skillPackage(t, "github:acme/one", []byte("# One\n"))
	two := skillPackage(t, "github:acme/two", []byte("# Two\n"))
	projectRoot := t.TempDir()
	intents := mustRealize(t, codex.New(), projectRoot, []adapter.Package{two, one})
	byPath := intentMap(intents)
	first := ".codex/skills/acr__acme__one__review-change/SKILL.md"
	second := ".codex/skills/acr__acme__two__review-change/SKILL.md"
	if _, ok := byPath[first]; !ok {
		t.Fatalf("missing %s in %#v", first, byPath)
	}
	if _, ok := byPath[second]; !ok {
		t.Fatalf("missing %s in %#v", second, byPath)
	}
	if _, exists := byPath[".agents/skills/acr__acme__one__review-change/SKILL.md"]; exists {
		t.Fatal("Codex rendered a reserved .agents/skills target")
	}
}

func TestUninstallLeavesUnrelatedHooks(t *testing.T) {
	t.Parallel()

	seed := []byte("model = \"gpt-5\"\n\n[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ntype = \"command\"\ncommand = \"user-command\"\n\n[hooks.state.acr__user]\nlast_run = 123\n")
	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, codexConfigPath, seed, 0o644)
	native := codex.New()
	ledger := applyNative(t, native, projectRoot, []adapter.Package{codexHookPackage(t)})
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), native)
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(projectRoot)), nil, ledger, map[string]adapter.TargetOptions{
		codexConfigPath: {ConfigFormat: adapter.ConfigTOML},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = realize.NewEngine().Run(projectRoot, ledger, intents, realize.ModeApply, func(realize.Ledger) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	got := readProjectFile(t, projectRoot, codexConfigPath)
	if !bytes.Contains(got, []byte("command = \"user-command\"")) || !bytes.Contains(got, []byte("[hooks.state.acr__user]")) {
		t.Fatalf("uninstall rewrote unrelated config: %s", got)
	}
	if bytes.Contains(got, []byte("acr__example__all-agents")) {
		t.Fatalf("uninstall left owned handler: %s", got)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".codex", "hooks", "acr__example__all-agents__session-start", "start.sh")); !os.IsNotExist(err) {
		t.Fatalf("uninstall left owned hook file: %v", err)
	}
}

func TestHookOrderingAcrossPackagesShuffled(t *testing.T) {
	t.Parallel()

	alpha := codexSourceHookPackage(t, "github:acme/alpha", "stop-a", "hooks/a.sh")
	beta := codexSourceHookPackage(t, "github:acme/beta", "stop-b", "hooks/b.sh")
	forward := configBytes(t, codex.New(), []adapter.Package{alpha, beta})
	shuffled := configBytes(t, codex.New(), []adapter.Package{beta, alpha})
	if !bytes.Equal(forward, shuffled) {
		t.Fatalf("native hook order depends on package input order\nforward: %s\nshuffled: %s", forward, shuffled)
	}
}

func TestDetectCreatesNothing(t *testing.T) {
	t.Parallel()

	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, "AGENTS.md", []byte("User instructions\n"), 0o644)
	writeFixtureFile(t, projectRoot, codexConfigPath, []byte("model = \"gpt-5\"\n"), 0o644)
	before := snapshotTree(t, projectRoot)
	detection, err := codex.New().Detect(context.Background(), adapter.DetectRequest{Project: adapter.NewFSSnapshot(os.DirFS(projectRoot))})
	if err != nil || !detection.Detected {
		t.Fatalf("Detect() = %#v, %v", detection, err)
	}
	if got := snapshotTree(t, projectRoot); !reflect.DeepEqual(got, before) {
		t.Fatalf("Detect() wrote files:\n got %#v\nwant %#v", got, before)
	}
}

func codexHookPlan() adapter.NativePlan {
	owner := adapter.OwnerRef{Source: "github:example/all-agents", ArtifactID: "session-start", SourcePath: "hooks/start.sh", Kind: adapter.ArtifactHook, Event: manifest.HookSessionStart}
	return adapter.NativePlan{Adapter: codex.New().Descriptor(), Items: []adapter.PlanItem{{Owner: owner, Target: codexConfigPath, Kind: adapter.OutputConfigMerge, Mode: 0o644}}}
}

func codexHookPackage(t *testing.T) adapter.Package {
	t.Helper()
	return codexNamedHookPackage(t, "session-start", "hooks/start.sh")
}

func codexNamedHookPackage(t *testing.T, id, relative string) adapter.Package {
	t.Helper()
	return codexSourceHookPackage(t, "github:example/all-agents", id, relative)
}

func codexSourceHookPackage(t *testing.T, source, id, relative string) adapter.Package {
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
