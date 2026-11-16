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
	"syscall"
	"testing"
	"testing/fstest"

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
		{native: claudecode.New(), configPath: ".claude/settings.json", format: adapter.ConfigJSON, hookPath: ".claude/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh"},
		{native: codex.New(), configPath: ".codex/config.toml", format: adapter.ConfigTOML, hookPath: ".codex/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh"},
		{native: cursor.New(), configPath: ".cursor/hooks.json", format: adapter.ConfigJSON, hookPath: ".cursor/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh"},
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

func TestNativeValidationIgnoresPureBytecodeCaches(t *testing.T) {
	cacheDir := "lib/__pycache__"
	cacheFile := "lib/__pycache__/prune_result.cpython-311.pyc"
	rootCache := "__pycache__/skill.cpython-311.pyc"
	for _, test := range []struct {
		name     string
		files    map[string]string
		dirs     []string
		links    map[string]string
		specials []string
		want     string
		wantSub  string
	}{
		{name: "pure cache", files: map[string]string{cacheFile: "pyc", rootCache: "pyc"}},
		{name: "empty cache", dirs: []string{cacheDir}},
		{name: "ancestor cache", files: map[string]string{cacheFile: "pyc"}},
		{name: "non pyc", files: map[string]string{cacheDir + "/notes.txt": "nope"}, want: "unexpected file", wantSub: "notes.txt"},
		{name: "other file", files: map[string]string{"extra.md": "extra"}, want: "unexpected file", wantSub: "extra.md"},
		{name: "loose pyc", files: map[string]string{"loose.pyc": "pyc"}, want: "unexpected file", wantSub: "loose.pyc"},
		{name: "uppercase pyc", files: map[string]string{cacheDir + "/prune_result.cpython-311.PYC": "pyc"}, want: "unexpected file", wantSub: "prune_result.cpython-311.PYC"},
		{name: "pyo", files: map[string]string{cacheDir + "/prune_result.cpython-311.pyo": "pyo"}, want: "unexpected file", wantSub: "prune_result.cpython-311.pyo"},
		{name: "nested cache", files: map[string]string{cacheDir + "/nested/prune_result.pyc": "pyc"}, want: "unexpected file", wantSub: "nested/prune_result.pyc"},
		{name: "symlink cache", links: map[string]string{cacheDir: "missing"}, want: "symlink", wantSub: "__pycache__"},
		{name: "symlink pyc", links: map[string]string{cacheFile: "missing"}, want: "symlink or special", wantSub: "__pycache__"},
		{name: "fifo", specials: []string{"pipe"}, want: "symlink or special", wantSub: "pipe"},
		{name: "fifo in cache", specials: []string{cacheDir + "/pipe.pyc"}, want: "symlink or special", wantSub: "__pycache__"},
		{name: "other empty directory", dirs: []string{"unexpected"}},
	} {
		test := test
		for _, prefix := range []string{".claude", ".codex", ".cursor"} {
			t.Run(test.name+"/"+prefix, func(t *testing.T) {
				includeHelper := test.name != "ancestor cache"
				assertSkillProjection(t, prefix, includeHelper, test.files, test.dirs, test.links, test.specials, test.want, test.wantSub)
			})
		}
	}
	t.Run("declared impure directory does not hide extra bytecode", func(t *testing.T) {
		const root = ".claude/skills/acr__example__policy__release"
		project := t.TempDir()
		var items []adapter.PlanItem
		var files []adapter.CandidateFile
		for _, target := range []string{root + "/SKILL.md", root + "/__pycache__/notes.txt"} {
			writeNativeFixture(t, filepath.Join(project, filepath.FromSlash(target)), []byte("managed\n"), 0o644)
			items = append(items, adapter.PlanItem{
				Owner: adapter.OwnerRef{Kind: adapter.ArtifactSkill}, Target: target,
				Kind: adapter.OutputGeneratedFile, Mode: 0o644,
			})
			files = append(files, adapter.CandidateFile{Path: target, Content: []byte("managed\n"), Mode: 0o644})
		}
		writeNativeFixture(t, filepath.Join(project, filepath.FromSlash(root), "__pycache__", "extra.pyc"), []byte("pyc"), 0o644)
		err := adapter.ValidateFileProjection(adapter.ValidateRequest{
			Project: adapter.NewFSSnapshot(os.DirFS(project)), Plan: adapter.NativePlan{Items: items}, Files: files,
		}, ".claude/skills")
		if err == nil || !strings.Contains(err.Error(), "invalid_skill_tree") || !strings.Contains(err.Error(), "extra.pyc") {
			t.Fatalf("impure declared directory error = %v, want extra.pyc refused", err)
		}
	})
}

func assertSkillProjection(t *testing.T, prefix string, includeHelper bool, files map[string]string, dirs []string, links map[string]string, specials []string, want, wantSub string) {
	t.Helper()
	const source = "github:example/policy"
	name := "acr__example__policy__release"
	root := prefix + "/skills/" + name
	target := root + "/SKILL.md"
	helper := root + "/lib/prune_result.py"
	owner := adapter.OwnerRef{Source: source, ArtifactID: "release", SourcePath: "skills/release/SKILL.md", Kind: adapter.ArtifactSkill}
	items := []adapter.PlanItem{{Owner: owner, Target: target, Kind: adapter.OutputGeneratedFile, Mode: 0o644}}
	candidates := []adapter.CandidateFile{{Path: target, Content: []byte("# Skill\n"), Mode: 0o644}}
	if includeHelper {
		helperOwner := owner
		helperOwner.SourcePath = "skills/release/lib/prune_result.py"
		items = append(items, adapter.PlanItem{Owner: helperOwner, Target: helper, Kind: adapter.OutputGeneratedFile, Mode: 0o644})
		candidates = append(candidates, adapter.CandidateFile{Path: helper, Content: []byte("def prune():\n    return None\n"), Mode: 0o644})
	}
	projectRoot := t.TempDir()
	writeNativeFixture(t, filepath.Join(projectRoot, filepath.FromSlash(target)), []byte("# Skill\n"), 0o644)
	if includeHelper {
		writeNativeFixture(t, filepath.Join(projectRoot, filepath.FromSlash(helper)), []byte("def prune():\n    return None\n"), 0o644)
	}
	for relative, content := range files {
		writeNativeFixture(t, filepath.Join(projectRoot, filepath.FromSlash(root), filepath.FromSlash(relative)), []byte(content), 0o644)
	}
	for _, relative := range dirs {
		if err := os.MkdirAll(filepath.Join(projectRoot, filepath.FromSlash(root), filepath.FromSlash(relative)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for relative, linkTarget := range links {
		link := filepath.Join(projectRoot, filepath.FromSlash(root), filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(linkTarget, link); err != nil {
			t.Fatal(err)
		}
	}
	for _, relative := range specials {
		special := filepath.Join(projectRoot, filepath.FromSlash(root), filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(special), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(special, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	request := adapter.ValidateRequest{
		Project: adapter.NewFSSnapshot(os.DirFS(projectRoot)),
		Plan:    adapter.NativePlan{Items: items},
		Files:   candidates,
	}
	var err error
	switch prefix {
	case ".claude":
		err = claudecode.New().Validate(context.Background(), request)
	case ".codex":
		err = codex.New().Validate(context.Background(), request)
	case ".cursor":
		err = cursor.New().Validate(context.Background(), request)
	default:
		err = adapter.ValidateFileProjection(request, prefix+"/skills")
	}
	if want == "" {
		if err != nil {
			t.Fatalf("validate %s: %v", prefix, err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), adapter.CodeInvalidSkillTree) || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), wantSub) {
		t.Fatalf("validate %s error = %v, want %s %q and %q", prefix, err, adapter.CodeInvalidSkillTree, want, wantSub)
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

func TestNativeValidationHistoricalLeavesAndDirectories(t *testing.T) {
	for _, native := range []adapter.Adapter{claudecode.New(), codex.New(), cursor.New()} {
		t.Run(native.Descriptor().ID, func(t *testing.T) {
			prefix := map[string]string{"claude-code": ".claude", "codex": ".codex", "cursor": ".cursor"}[native.Descriptor().ID]
			root := prefix + "/skills/acr__example__pkg__review"
			target, leaf := root+"/SKILL.md", root+"/old/nested/notes.md"
			entry := realize.Entry{Adapter: native.Descriptor().ID, ArtifactKind: realize.ArtifactFile}
			for _, scenario := range []struct {
				name      string
				mode      fs.FileMode
				ownership realize.Ownership
				entries   []realize.Entry
				want      string
			}{
				{name: "nested empty scaffolding", mode: fs.ModeDir | 0o700},
				{name: "unknown descendant", mode: 0o644, want: "unexpected file"},
				{name: "historical whole file", mode: 0o644, ownership: realize.OwnershipGenerated, entries: []realize.Entry{entry}},
				{name: "other adapter", mode: 0o644, ownership: realize.OwnershipGenerated, entries: []realize.Entry{{Adapter: "other", ArtifactKind: realize.ArtifactFile}}, want: "unexpected file"},
				{name: "shared ownership", mode: 0o644, ownership: realize.OwnershipShared, entries: []realize.Entry{entry}, want: "unexpected file"},
				{name: "multiple entries", mode: 0o644, ownership: realize.OwnershipGenerated, entries: []realize.Entry{entry, entry}, want: "unexpected file"},
				{name: "managed span", mode: 0o644, ownership: realize.OwnershipGenerated, entries: []realize.Entry{{Adapter: native.Descriptor().ID, ArtifactKind: realize.ArtifactManagedBlock}}, want: "unexpected file"},
				{name: "owned symlink descendant", mode: fs.ModeSymlink | 0o777, ownership: realize.OwnershipGenerated, entries: []realize.Entry{entry}, want: "symlink or special entry"},
				{name: "owned special descendant", mode: fs.ModeNamedPipe | 0o600, ownership: realize.OwnershipGenerated, entries: []realize.Entry{entry}, want: "symlink or special entry"},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					project := fstest.MapFS{target: {Data: []byte("# Skill\n"), Mode: 0o644}, leaf: {Mode: scenario.mode}}
					plan := adapter.NativePlan{Adapter: native.Descriptor(), Items: []adapter.PlanItem{{
						Owner: adapter.OwnerRef{Kind: adapter.ArtifactSkill}, Target: target, Kind: adapter.OutputGeneratedFile, Mode: 0o644,
					}}}
					err := native.Validate(context.Background(), adapter.ValidateRequest{
						Project: adapter.NewFSSnapshot(project), Plan: plan,
						Files:    []adapter.CandidateFile{{Path: target, Content: []byte("# Skill\n"), Mode: 0o644}},
						Previous: realize.Ledger{Targets: []realize.Target{{Path: leaf, Ownership: scenario.ownership, Entries: scenario.entries}}},
					})
					if scenario.want == "" {
						if err != nil {
							t.Fatalf("validation: %v", err)
						}
					} else if err == nil || !strings.Contains(err.Error(), scenario.want) || !strings.Contains(err.Error(), leaf) {
						t.Fatalf("validation = %v, want %q naming %s", err, scenario.want, leaf)
					}
				})
			}
			plan := adapter.NativePlan{Adapter: native.Descriptor(), Items: []adapter.PlanItem{{
				Owner: adapter.OwnerRef{Kind: adapter.ArtifactSkill}, Target: target, Kind: adapter.OutputGeneratedFile, Mode: 0o644,
			}}}
			err := native.Validate(context.Background(), adapter.ValidateRequest{
				Project: failingSkillDirectory{FSSnapshot: adapter.NewFSSnapshot(fstest.MapFS{
					target: {Data: []byte("# Skill\n"), Mode: 0o644}, root + "/old": {Mode: fs.ModeDir | 0o700},
				}), directory: root + "/old"}, Plan: plan,
				Files: []adapter.CandidateFile{{Path: target, Content: []byte("# Skill\n"), Mode: 0o644}},
			})
			if err == nil || !strings.Contains(err.Error(), "permission denied") || !strings.Contains(err.Error(), root+"/old") {
				t.Fatalf("descendant inspection failure = %v", err)
			}
		})
	}
}

// A nested inspection failure must not be mistaken for empty scaffolding.
type failingSkillDirectory struct {
	adapter.FSSnapshot
	directory string
}

func (snapshot failingSkillDirectory) ReadDir(directory string) ([]adapter.ObservedEntry, error) {
	if directory == snapshot.directory {
		return nil, &fs.PathError{Op: "readdir", Path: directory, Err: fs.ErrPermission}
	}
	return snapshot.FSSnapshot.ReadDir(directory)
}
