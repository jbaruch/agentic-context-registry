package cursor_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestCursorDescriptorDetectionAndNativeProjection(t *testing.T) {
	t.Parallel()

	packageRoot := t.TempDir()
	writeFixtureFile(t, packageRoot, "rules/always.md", []byte("---\ntitle: discarded\n---\n# Always\n"), 0o644)
	writeFixtureFile(t, packageRoot, "rules/paths.md", []byte("# Paths\n"), 0o644)
	writeFixtureFile(t, packageRoot, "skills/review/SKILL.md", []byte("# Review\n"), 0o644)
	writeFixtureFile(t, packageRoot, "skills/review/references/REFERENCE.md", []byte("Reference\n"), 0o644)
	writeFixtureFile(t, packageRoot, "skills/review/scripts/check.sh", []byte("#!/bin/sh\nexit 0\n"), 0o755)
	writeFixtureFile(t, packageRoot, "scripts/report.sh", []byte("#!/bin/sh\nexit 0\n"), 0o755)
	writeFixtureFile(t, packageRoot, "hooks/start.sh", []byte("#!/bin/sh\nexit 0\n"), 0o755)
	pkg := adapter.Package{
		Source: "github:example/all-agents", Root: os.DirFS(packageRoot),
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{
			Rules: []manifest.RuleArtifact{
				{ID: "always", Path: "rules/always.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}},
				{ID: "paths", Path: "rules/paths.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationPaths, Paths: []string{"**/*.go", "scripts/**"}}},
			},
			Skills:  []manifest.SkillArtifact{{ID: "review", Path: "skills/review"}},
			Scripts: []manifest.ScriptArtifact{{ID: "report", Path: "scripts/report.sh"}},
			Hooks:   []manifest.HookArtifact{{ID: "session-start", Event: manifest.HookSessionStart, Path: "hooks/start.sh", Args: []string{"two words"}}},
		}},
	}

	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, ".cursor/rules/user.mdc", []byte("---\nalwaysApply: true\n---\nUser rule\n"), 0o644)
	existing := []byte("{\n  \"version\": 1,\n  \"unrelated\": {\"keep\": true},\n  \"hooks\": {\"stop\": [{\"command\": \"user-command\"}]}\n}\n")
	writeFixtureFile(t, projectRoot, ".cursor/hooks.json", existing, 0o644)
	snapshot := adapter.NewFSSnapshot(os.DirFS(projectRoot))
	native := cursor.New()
	if got := native.Descriptor(); got.ID != "cursor" || got.Version != "1.0.1" || got.Boundary != adapter.CurrentBoundaryVersion {
		t.Fatalf("Descriptor() = %#v", got)
	}
	detection, err := native.Detect(context.Background(), adapter.DetectRequest{Project: snapshot})
	if err != nil || !detection.Detected || !contains(detection.Evidence, ".cursor/rules/user.mdc") || !contains(detection.Evidence, ".cursor/hooks.json") {
		t.Fatalf("Detect() = %#v, %v", detection, err)
	}

	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), native)
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(context.Background(), snapshot, []adapter.Package{pkg}, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	byPath := intentMap(intents)
	for _, target := range []string{
		".cursor/rules/acr__example__all-agents__always.mdc",
		".cursor/rules/acr__example__all-agents__paths.mdc",
		".cursor/skills/acr__example__all-agents__review/SKILL.md",
		".cursor/skills/acr__example__all-agents__review/references/REFERENCE.md",
		".cursor/skills/acr__example__all-agents__review/scripts/check.sh",
		".cursor/scripts/acr__example__all-agents__report/report.sh",
		".cursor/hooks/acr__example__all-agents__session-start/start.sh",
		".cursor/hooks.json",
	} {
		if _, exists := byPath[target]; !exists {
			t.Errorf("missing intent %q", target)
		}
	}
	if got := byPath[".cursor/skills/acr__example__all-agents__review/scripts/check.sh"].Mode; got != 0o755 {
		t.Fatalf("skill script mode = %04o, want 0755", got)
	}
	if got := string(byPath[".cursor/rules/acr__example__all-agents__always.mdc"].Content); got != "---\nalwaysApply: true\n---\n# Always\n" {
		t.Fatalf("always rule = %q", got)
	}
	if got := string(byPath[".cursor/rules/acr__example__all-agents__paths.mdc"].Content); got != "---\nglobs: [\"**/*.go\", \"scripts/**\"]\nalwaysApply: false\n---\n# Paths\n" {
		t.Fatalf("paths rule = %q", got)
	}
	var hooks struct {
		Version   int             `json:"version"`
		Unrelated json.RawMessage `json:"unrelated"`
		Hooks     map[string][]struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(byPath[".cursor/hooks.json"].Content, &hooks); err != nil {
		t.Fatal(err)
	}
	if hooks.Version != 1 || string(hooks.Unrelated) != `{"keep": true}` {
		t.Fatalf("preserved hooks config = %s", byPath[".cursor/hooks.json"].Content)
	}
	if got := hooks.Hooks["sessionStart"][0].Command; got != ".cursor/hooks/acr__example__all-agents__session-start/start.sh 'two words'" {
		t.Fatalf("Cursor hook command = %q", got)
	}
	for _, entry := range byPath[".cursor/hooks.json"].Entries {
		if entry.ArtifactID == "cursor-hooks-schema" {
			t.Fatal("existing version: 1 became ACR-owned")
		}
	}
}

func TestCursorCreatesOwnedVersionWhenAbsent(t *testing.T) {
	t.Parallel()

	packageRoot := t.TempDir()
	writeFixtureFile(t, packageRoot, "hooks/stop.sh", []byte("#!/bin/sh\nexit 0\n"), 0o755)
	pkg := adapter.Package{Source: "github:example/all-agents", Root: os.DirFS(packageRoot), Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{
		Hooks: []manifest.HookArtifact{{ID: "stop", Event: manifest.HookStop, Path: "hooks/stop.sh"}},
	}}}
	projectRoot := t.TempDir()
	snapshot := adapter.NewFSSnapshot(os.DirFS(projectRoot))
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), cursor.New())
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(context.Background(), snapshot, []adapter.Package{pkg}, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	intent := intentMap(intents)[".cursor/hooks.json"]
	var document struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(intent.Content, &document); err != nil || document.Version != 1 {
		t.Fatalf("hooks config = %s (decode error %v)", intent.Content, err)
	}
	found := false
	for _, entry := range intent.Entries {
		found = found || entry.ArtifactID == "cursor-hooks-schema"
	}
	if !found {
		t.Fatal("new version field has no ownership entry")
	}
}

func TestCursorValidateRejectsWrongCaseDuplicateVersionAndFrontmatter(t *testing.T) {
	t.Parallel()

	native := cursor.New()
	owner := adapter.OwnerRef{Source: "github:example/all-agents", ArtifactID: "session-start", SourcePath: "hooks/start.sh", Kind: adapter.ArtifactHook, Event: manifest.HookSessionStart}
	plan := adapter.NativePlan{Adapter: native.Descriptor(), Items: []adapter.PlanItem{{Owner: owner, Target: ".cursor/hooks.json", Kind: adapter.OutputConfigMerge, Mode: 0o644}}}
	command := ".cursor/hooks/acr__example__all-agents__session-start/start.sh"
	for _, test := range []struct {
		name    string
		content string
		want    string
	}{
		{name: "wrong case", content: `{"version":1,"hooks":{"SessionStart":[{"command":"` + command + `"}]}}`, want: adapter.CodeInvalidNativeEvent},
		{name: "duplicate handler", content: `{"version":1,"hooks":{"sessionStart":[{"command":"` + command + `"},{"command":"` + command + `"}]}}`, want: adapter.CodeDuplicateConfigEntry},
		{name: "duplicate version", content: `{"version":1,"version":1,"hooks":{"sessionStart":[{"command":"` + command + `"}]}}`, want: adapter.CodeDuplicateConfigEntry},
		{name: "wrong version", content: `{"version":2,"hooks":{"sessionStart":[{"command":"` + command + `"}]}}`, want: adapter.CodeInvalidNativeEvent},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := native.Validate(context.Background(), adapter.ValidateRequest{Plan: plan, Files: []adapter.CandidateFile{{Path: ".cursor/hooks.json", Content: []byte(test.content), Mode: 0o644}}})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %s", err, test.want)
			}
		})
	}

	ruleOwner := adapter.OwnerRef{Source: "github:example/all-agents", ArtifactID: "guidance", SourcePath: "rules/guidance.md", Kind: adapter.ArtifactRule}
	rulePlan := adapter.NativePlan{Adapter: native.Descriptor(), Items: []adapter.PlanItem{{Owner: ruleOwner, Target: ".cursor/rules/acr__example__all-agents__guidance.mdc", Kind: adapter.OutputGeneratedFile, Mode: 0o644}}}
	pkg := adapter.Package{Source: ruleOwner.Source, Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Rules: []manifest.RuleArtifact{{ID: ruleOwner.ArtifactID, Path: ruleOwner.SourcePath, Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}}}}}
	err := native.Validate(context.Background(), adapter.ValidateRequest{Packages: []adapter.Package{pkg}, Plan: rulePlan, Files: []adapter.CandidateFile{{Path: rulePlan.Items[0].Target, Content: []byte("---\nalwaysApply: true\n---\n---\nother: true\n---\nbody\n"), Mode: 0o644}}})
	if err == nil || !strings.Contains(err.Error(), adapter.CodeMalformedFrontmatter) {
		t.Fatalf("Validate() malformed frontmatter error = %v", err)
	}
}

func TestCursorRenderRejectsUnclosedAndAdjacentSourceFrontmatter(t *testing.T) {
	t.Parallel()

	for _, content := range []string{"---\ntitle: open\n", "---\ntitle: one\n---\n---\ntitle: two\n---\nbody\n"} {
		packageRoot := t.TempDir()
		writeFixtureFile(t, packageRoot, "rules/guidance.md", []byte(content), 0o644)
		pkg := adapter.Package{Source: "github:example/all-agents", Root: os.DirFS(packageRoot), Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Rules: []manifest.RuleArtifact{{ID: "guidance", Path: "rules/guidance.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}}}}}
		plan, err := cursor.New().Plan(context.Background(), adapter.PlanRequest{Project: adapter.NewFSSnapshot(os.DirFS(t.TempDir())), Packages: []adapter.Package{pkg}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = cursor.New().Render(context.Background(), adapter.RenderRequest{Packages: []adapter.Package{pkg}, Plan: plan})
		if err == nil || !strings.Contains(err.Error(), adapter.CodeMalformedFrontmatter) {
			t.Fatalf("Render() error = %v", err)
		}
	}
}

func writeFixtureFile(t *testing.T, root, relative string, content []byte, mode os.FileMode) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
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

func intentMap(intents []realize.Intent) map[string]realize.Intent {
	result := make(map[string]realize.Intent, len(intents))
	for _, intent := range intents {
		result[intent.Path] = intent
	}
	return result
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
