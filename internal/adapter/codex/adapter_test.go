package codex_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestCodexDescriptorDetectionAndNativeProjection(t *testing.T) {
	t.Parallel()

	packageRoot := t.TempDir()
	writeFixtureFile(t, packageRoot, "rules/guidance.md", []byte("# Guidance\n"), 0o644)
	writeFixtureFile(t, packageRoot, "skills/review/SKILL.md", []byte("# Review\n"), 0o644)
	writeFixtureFile(t, packageRoot, "skills/review/scripts/check.sh", []byte("#!/bin/sh\nexit 0\n"), 0o755)
	writeFixtureFile(t, packageRoot, "hooks/start.sh", []byte("#!/bin/sh\nexit 0\n"), 0o755)
	pkg := adapter.Package{
		Source: "github:example/all-agents", Root: os.DirFS(packageRoot),
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{
			Rules:  []manifest.RuleArtifact{{ID: "guidance", Path: "rules/guidance.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}},
			Skills: []manifest.SkillArtifact{{ID: "review", Path: "skills/review"}},
			Hooks:  []manifest.HookArtifact{{ID: "session-start", Event: manifest.HookSessionStart, Path: "hooks/start.sh", Args: []string{"argument with space"}}},
		}},
	}

	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, "AGENTS.md", []byte("User instructions\n"), 0o644)
	snapshot := adapter.NewFSSnapshot(os.DirFS(projectRoot))
	native := codex.New()
	detection, err := native.Detect(context.Background(), adapter.DetectRequest{Project: snapshot})
	if err != nil || !detection.Detected || len(detection.Evidence) != 1 || detection.Evidence[0] != "AGENTS.md" {
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
		"AGENTS.md",
		".codex/skills/acr__example__all-agents__review/SKILL.md",
		".codex/skills/acr__example__all-agents__review/scripts/check.sh",
		".codex/hooks/acr__example__all-agents__session-start/start.sh",
		".codex/config.toml",
	} {
		if _, exists := byPath[target]; !exists {
			t.Errorf("missing intent %q", target)
		}
	}
	if _, exists := byPath[".agents/skills/acr__example__all-agents__review/SKILL.md"]; exists {
		t.Fatal("Codex rendered a reserved .agents/skills target")
	}
	config := string(byPath[".codex/config.toml"].Content)
	if !strings.Contains(config, "[[hooks.SessionStart]]\n[[hooks.SessionStart.hooks]]") {
		t.Fatalf("Codex config does not use native hook array tables: %q", config)
	}
	if !strings.Contains(config, `$(git rev-parse --show-toplevel)/.codex/hooks/acr__example__all-agents__session-start/start.sh`) || !strings.Contains(config, `'argument with space'`) {
		t.Fatalf("Codex config = %q", config)
	}
}

func TestCodexValidateRejectsWrongEventCaseAndDuplicateHandler(t *testing.T) {
	t.Parallel()

	native := codex.New()
	owner := adapter.OwnerRef{Source: "github:example/all-agents", ArtifactID: "session-start", SourcePath: "hooks/start.sh", Kind: adapter.ArtifactHook, Event: manifest.HookSessionStart}
	plan := adapter.NativePlan{Adapter: native.Descriptor(), Items: []adapter.PlanItem{{Owner: owner, Target: ".codex/config.toml", Kind: adapter.OutputConfigMerge, Mode: 0o644}}}
	command := `"$(git rev-parse --show-toplevel)/.codex/hooks/acr__example__all-agents__session-start/start.sh"`
	for _, test := range []struct {
		name    string
		content string
		want    string
	}{
		{name: "wrong case", content: "[hooks]\nsessionStart = [{ hooks = [{ type = \"command\", command = " + quotedTOML(command) + " }] }]\n", want: adapter.CodeInvalidNativeEvent},
		{name: "duplicate", content: "[hooks]\nSessionStart = [{ hooks = [{ type = \"command\", command = " + quotedTOML(command) + " }] }, { hooks = [{ type = \"command\", command = " + quotedTOML(command) + " }] }]\n", want: adapter.CodeDuplicateConfigEntry},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := native.Validate(context.Background(), adapter.ValidateRequest{Plan: plan, Files: []adapter.CandidateFile{{Path: ".codex/config.toml", Content: []byte(test.content), Mode: 0o644}}})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %s", err, test.want)
			}
		})
	}
}

func quotedTOML(value string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`) + `"`
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
