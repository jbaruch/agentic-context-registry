package claudecode_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestClaudeCodeDescriptorDetectionAndNativeProjection(t *testing.T) {
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
			Hooks:  []manifest.HookArtifact{{ID: "session-start", Event: manifest.HookSessionStart, Path: "hooks/start.sh", Args: []string{"--quiet"}}},
		}},
	}

	projectRoot := t.TempDir()
	writeFixtureFile(t, projectRoot, "CLAUDE.md", []byte("User instructions\n"), 0o644)
	snapshot := adapter.NewFSSnapshot(os.DirFS(projectRoot))
	native := claudecode.New()
	if got := native.Descriptor(); got.ID != "claude-code" || got.Version != "1.0.1" || got.Boundary != adapter.CurrentBoundaryVersion {
		t.Fatalf("Descriptor() = %#v", got)
	}
	detection, err := native.Detect(context.Background(), adapter.DetectRequest{Project: snapshot})
	if err != nil || !detection.Detected || len(detection.Evidence) != 1 || detection.Evidence[0] != "CLAUDE.md" {
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
		"CLAUDE.md",
		".claude/skills/acr__example__all-agents__review/SKILL.md",
		".claude/skills/acr__example__all-agents__review/scripts/check.sh",
		".claude/hooks/acr__example__all-agents__session-start/start.sh",
		".claude/settings.json",
	} {
		if _, exists := byPath[target]; !exists {
			t.Errorf("missing intent %q", target)
		}
	}
	if got := byPath[".claude/skills/acr__example__all-agents__review/scripts/check.sh"].Mode; got != 0o755 {
		t.Fatalf("skill script mode = %04o, want 0755", got)
	}
	if !strings.Contains(string(byPath["CLAUDE.md"].Content), "User instructions\n") || !strings.Contains(string(byPath["CLAUDE.md"].Content), "# Guidance") {
		t.Fatalf("CLAUDE.md candidate = %q", byPath["CLAUDE.md"].Content)
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string   `json:"type"`
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(byPath[".claude/settings.json"].Content, &settings); err != nil {
		t.Fatal(err)
	}
	hook := settings.Hooks["SessionStart"][0].Hooks[0]
	if hook.Type != "command" || !strings.Contains(hook.Command, "${CLAUDE_PROJECT_DIR}/.claude/hooks/") || len(hook.Args) != 1 || hook.Args[0] != "--quiet" {
		t.Fatalf("Claude hook = %#v", hook)
	}
}

func TestClaudeCodeValidateRejectsWrongEventCaseAndDuplicateHandler(t *testing.T) {
	t.Parallel()

	native := claudecode.New()
	owner := adapter.OwnerRef{Source: "github:example/all-agents", ArtifactID: "session-start", SourcePath: "hooks/start.sh", Kind: adapter.ArtifactHook, Event: manifest.HookSessionStart}
	plan := adapter.NativePlan{Adapter: native.Descriptor(), Items: []adapter.PlanItem{{Owner: owner, Target: ".claude/settings.json", Kind: adapter.OutputConfigMerge, Mode: 0o644}}}
	command := "${CLAUDE_PROJECT_DIR}/.claude/hooks/acr__example__all-agents__session-start/start.sh"
	for _, test := range []struct {
		name    string
		content string
		want    string
	}{
		{name: "wrong case", content: `{"hooks":{"sessionStart":[{"hooks":[{"type":"command","command":"` + command + `"}]}]}}`, want: adapter.CodeInvalidNativeEvent},
		{name: "duplicate", content: `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"` + command + `"}]},{"hooks":[{"type":"command","command":"` + command + `"}]}]}}`, want: adapter.CodeDuplicateConfigEntry},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := native.Validate(context.Background(), adapter.ValidateRequest{Plan: plan, Files: []adapter.CandidateFile{{Path: ".claude/settings.json", Content: []byte(test.content), Mode: 0o644}}})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %s", err, test.want)
			}
		})
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
