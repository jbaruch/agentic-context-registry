package migrateapp

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependencytest"
	"github.com/jbaruch/agentic-context-registry/internal/setupapp"
)

// The owner's three-file consumer has installed package content but none of
// the native evidence that migration uses to select an agent.
func noAgentConsumer(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeJSON(t, root, "tessl.json", map[string]any{"dependencies": map[string]any{"example/alpha": map[string]string{"version": "1.0.0"}}})
	writeJSON(t, root, ".tessl/plugins/example/alpha/.tessl-plugin/plugin.json", map[string]any{"name": "example/alpha", "version": "1.0.0", "rules": []string{"rules"}})
	writeFile(t, root, ".tessl/plugins/example/alpha/rules/always.md", []byte("Always.\n"), 0o644)
	return root
}

func TestMigrationNoAgentsExplainsNativeEvidenceRecovery(t *testing.T) {
	for _, fixture := range []string{"no-native-output", "agents-yaml-only", "uncovered-agent-only"} {
		t.Run(fixture, func(t *testing.T) {
			root := noAgentConsumer(t)
			app := NewApplication(dependencytest.NewRemote(), "test")
			switch fixture {
			case "agents-yaml-only":
				writeFile(t, root, "agents.yaml", []byte("schemaVersion: 2\nagents: [claude-code]\n"), 0o644)
				requireMigrationSuccess(t, app, "list", "--json", "--project", root)
			case "uncovered-agent-only":
				writeFile(t, root, ".gemini/settings.json", []byte("{}\n"), 0o644)
			}
			writeFile(t, root, "AGENTS.md", []byte("# Caller instructions\nKeep my project intact.\n"), 0o640)
			before := hashTreeWithModes(t, root)
			for _, dryRun := range []bool{true, false} {
				for _, jsonOutput := range []bool{false, true} {
					args := []string{"migrate", "tessl", "--vendor-unmapped", "--non-interactive", "--project", root}
					if dryRun {
						args = append(args, "--dry-run")
					}
					if jsonOutput {
						args = append(args, "--json")
					}
					stdout, stderr, exit := runCLI(t, app, args...)
					t.Logf("dryRun=%t json=%t exit=%d stdout=%s stderr=%s", dryRun, jsonOutput, exit, stdout, stderr)
					if exit != cli.ExitOperational || stdout != "" {
						t.Fatalf("wrong refusal: %d %s %s", exit, stdout, stderr)
					}
					diagnostic := stderr
					if jsonOutput {
						var envelope struct {
							Error struct{ Code, Message, Remedy string } `json:"error"`
						}
						if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
							t.Fatal(err)
						}
						if envelope.Error.Code != "migrate_failed" || envelope.Error.Remedy == "" {
							t.Errorf("wrong diagnostic envelope: %s", stderr)
						}
						diagnostic = envelope.Error.Message + " " + envelope.Error.Remedy
					}
					for _, want := range []string{"no supported agent output", "Tessl inventory", "claude-code", "codex", "cursor", "restore or generate", ".claude/skills", ".codex/skills", ".cursor/skills", "acr migrate tessl"} {
						if !strings.Contains(diagnostic, want) {
							t.Errorf("missing %q: %s", want, diagnostic)
						}
					}
					for _, wrong := range []string{"pass --agent", "set agents in agents.yaml"} {
						if strings.Contains(diagnostic, wrong) {
							t.Errorf("unsupported recovery %q: %s", wrong, diagnostic)
						}
					}
					if !mapsEqual(before, hashTreeWithModes(t, root)) {
						t.Fatal("refusal changed tree bytes, modes, paths or links")
					}
				}
			}
			for _, agent := range []string{"claude-code", "claude", "codex", "cursor"} {
				stdout, stderr, exit := runCLI(t, app, "migrate", "tessl", "--vendor-unmapped", "--dry-run", "--json", "--project", root, "--agent", agent)
				if exit != cli.ExitUsage || stdout != "" || !strings.Contains(stderr, `"code":"usage"`) {
					t.Fatalf("consumer --agent changed: %d %s %s", exit, stdout, stderr)
				}
				if !mapsEqual(before, hashTreeWithModes(t, root)) {
					t.Fatal("unsupported flag changed tree")
				}
			}
		})
	}
}

func TestMigrationNoAgentsRecoversFromInstalledTesslSkillLinks(t *testing.T) {
	for _, agent := range []struct{ id, dir string }{{"claude-code", ".claude/skills"}, {"codex", ".codex/skills"}, {"cursor", ".cursor/skills"}} {
		t.Run(agent.id, func(t *testing.T) {
			root := noAgentConsumer(t)
			writeJSON(t, root, ".tessl/plugins/example/alpha/.tessl-plugin/plugin.json", map[string]any{"name": "example/alpha", "version": "1.0.0", "rules": []string{"rules"}, "skills": []string{"skills/review"}})
			writeFile(t, root, ".tessl/plugins/example/alpha/skills/review/SKILL.md", []byte("# Review\nReview changes carefully.\n"), 0o644)
			// This is the other half of the owner's failed remedy: a valid agents.yaml
			// by itself does not provide Tessl-native evidence.
			writeFile(t, root, "agents.yaml", []byte("schemaVersion: 2\nfreshness: none\nagents: ["+agent.id+"]\n"), 0o644)
			app := NewApplication(dependencytest.NewRemote(), "test")
			args := []string{"migrate", "tessl", "--vendor-unmapped", "--json", "--project", root}
			before := hashTreeWithModes(t, root)
			_, stderr, exit := runCLI(t, app, append(append([]string(nil), args...), "--dry-run")...)
			t.Logf("before native recovery: exit=%d stderr=%s", exit, stderr)
			if exit != cli.ExitOperational {
				t.Fatalf("missing native evidence unexpectedly accepted: %d %s", exit, stderr)
			}
			if !mapsEqual(before, hashTreeWithModes(t, root)) {
				t.Fatal("preview refusal changed tree")
			}
			// Restore the real native link to this installed skill, as the remedy
			// requests. No Tessl command, remote registry or fabricated package is used.
			if err := os.MkdirAll(filepath.Join(root, agent.dir), 0o755); err != nil {
				t.Fatal(err)
			}
			native := filepath.Join(agent.dir, "tessl__review")
			target := "../../.tessl/plugins/example/alpha/skills/review"
			if err := os.Symlink(target, filepath.Join(root, native)); err != nil {
				t.Fatal(err)
			}
			inventory, err := NewService().Inventory(root)
			if err != nil {
				t.Fatal(err)
			}
			if got := selectedAgents(inventory); !reflect.DeepEqual(got, []string{agent.id}) {
				t.Fatalf("detected agents=%v", got)
			}
			before = hashTreeWithModes(t, root)
			requireMigrationSuccess(t, app, append(append([]string(nil), args...), "--dry-run")...)
			if !mapsEqual(before, hashTreeWithModes(t, root)) {
				t.Fatal("accepted preview changed tree")
			}
			requireMigrationSuccess(t, app, args...)
			if got := loadRemigrationState(t, root).Project.Agents; !reflect.DeepEqual(got, []string{agent.id}) {
				t.Fatalf("realized agents=%v", got)
			}
			if got, err := os.Readlink(filepath.Join(root, native)); err != nil || got != target {
				t.Fatalf("Tessl link changed: %s %v", got, err)
			}
			before = hashTreeWithModes(t, root)
			output := requireMigrationSuccess(t, app, args...)
			if !strings.Contains(output, `"wrote":false`) || !mapsEqual(before, hashTreeWithModes(t, root)) {
				t.Fatal("repeat changed recovered project")
			}
		})
	}
}

func TestMigrationNoAgentsKeepsOtherCommandSelectionGuidance(t *testing.T) {
	for _, command := range []string{"realize", "check"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			app := NewApplication(dependencytest.NewRemote(), "test")
			args := []string{command, "--project", root, "--json"}
			if command == "realize" {
				args = append(args, "--dry-run")
			}
			_, stderr, exit := runCLI(t, app, args...)
			if exit != cli.ExitOperational || !strings.Contains(stderr, "no agent adapters selected; set agents in agents.yaml or pass --agent") {
				t.Fatalf("%s guidance changed: %d %s", command, exit, stderr)
			}
			stdout, stderr, exit := runCLI(t, app, append(args, "--agent", "codex")...)
			if exit != cli.ExitSuccess && exit != cli.ExitChanges {
				t.Fatalf("supported flag failed: %d %s %s", exit, stdout, stderr)
			}
			writeFile(t, root, "agents.yaml", []byte("schemaVersion: 2\nagents: [codex]\nfreshness: none\n"), 0o644)
			stdout, stderr, exit = runCLI(t, app, args...)
			if exit != cli.ExitSuccess && exit != cli.ExitChanges {
				t.Fatalf("supported YAML recovery failed: %d %s %s", exit, stdout, stderr)
			}
		})
	}
	for _, command := range []string{"init", "install"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			app := setupapp.NewApplication(NewApplication(remigrationRemote(t), "test"), setupapp.NewTerminalPrompter(strings.NewReader(""), io.Discard, false))
			args := []string{command}
			if command == "install" {
				args = append(args, "github:example/alpha@v1.0.0")
			}
			args = append(args, "--project", root, "--agent", "codex", "--freshness", "none", "--non-interactive", "--json")
			requireMigrationSuccess(t, app, args...)
			if got := loadRemigrationState(t, root).Project.Agents; !reflect.DeepEqual(got, []string{"codex"}) {
				t.Fatalf("%s --agent selected %v", command, got)
			}
		})
	}
}

func TestMigrationNoAgentsRecoversFromInstalledCursorRule(t *testing.T) {
	root := noAgentConsumer(t)
	app := NewApplication(dependencytest.NewRemote(), "test")
	args := []string{"migrate", "tessl", "--vendor-unmapped", "--json", "--project", root}
	_, stderr, exit := runCLI(t, app, append(append([]string(nil), args...), "--dry-run")...)
	if exit != cli.ExitOperational || !strings.Contains(stderr, "no supported agent output") {
		t.Fatalf("missing evidence: %d %s", exit, stderr)
	}
	const native = ".cursor/rules/tessl__rule__example__alpha__always.mdc"
	content := []byte("---\nalwaysApply: true\n---\nAlways.\n")
	writeFile(t, root, native, content, 0o644)
	before := hashTreeWithModes(t, root)
	requireMigrationSuccess(t, app, append(append([]string(nil), args...), "--dry-run")...)
	if !mapsEqual(before, hashTreeWithModes(t, root)) {
		t.Fatal("accepted preview changed tree")
	}
	requireMigrationSuccess(t, app, args...)
	if got := loadRemigrationState(t, root).Project.Agents; !reflect.DeepEqual(got, []string{"cursor"}) {
		t.Fatalf("selected agents=%v", got)
	}
	if got, err := os.ReadFile(filepath.Join(root, native)); err != nil || string(got) != string(content) {
		t.Fatalf("Tessl rule changed: %q %v", got, err)
	}
}
