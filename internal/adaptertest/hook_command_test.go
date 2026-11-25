package adaptertest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/antigravity"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestRealizedFreshnessCommandAndTranslatorLifecycle(t *testing.T) {
	for _, native := range []adapter.Adapter{claudecode.New(), codex.New(), cursor.New(), antigravity.Adapter{}} {
		t.Run(native.Descriptor().ID, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "project 'quotes' $literal")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "agents.yaml"), []byte("schemaVersion: 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), native)
			if err != nil {
				t.Fatal(err)
			}
			ledger := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}
			// With no hooks there is no runtime to plan or render.
			intents, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(root)), nil, ledger)
			if err != nil || len(intents) != 0 {
				t.Fatalf("empty packages: %#v, %v", intents, err)
			}
			pkg, ok := freshness.HookPackage(freshness.PolicyOutdated)
			if !ok {
				t.Fatal("missing freshness package")
			}
			intents, err = coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(root)), []adapter.Package{pkg}, ledger)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := realize.NewEngine().Run(root, ledger, intents, realize.ModeApply, func(next realize.Ledger) error { ledger = next; return nil }); err != nil {
				t.Fatal(err)
			}
			id := native.Descriptor().ID
			tree := map[string]string{"claude-code": ".claude", "codex": ".codex", "cursor": ".cursor", "antigravity": ".agent"}[id]
			runtime := filepath.Join(root, tree, "hooks/acr-hook.py")
			if id != "claude-code" {
				info, err := os.Stat(runtime)
				if err != nil || info.Mode().Perm() != 0o755 {
					t.Fatalf("translator mode: %v, %v", info, err)
				}
				owned := 0
				for _, target := range ledger.Targets {
					if target.Path == tree+"/hooks/acr-hook.py" {
						for _, entry := range target.Entries {
							if entry.ArtifactID == "hook-translator" && entry.SourcePath == "internal/adapter/acr-hook.py" {
								owned++
							}
						}
					}
				}
				if owned != 1 {
					t.Fatalf("synthetic ownership count = %d", owned)
				}
			} else if _, err := os.Stat(runtime); !os.IsNotExist(err) {
				t.Fatalf("Claude gained a translator: %v", err)
			}
			fakeACR := filepath.Join(t.TempDir(), "fake acr")
			fakeBody := "#!/usr/bin/env bash\nset -euo pipefail\nprintf '%s\\n' 'freshness_outdated: update available'\n"
			if err := os.WriteFile(fakeACR, []byte(fakeBody), 0o755); err != nil {
				t.Fatal(err)
			}
			var command string
			var args []string
			if id == "codex" {
				content, err := os.ReadFile(filepath.Join(root, ".codex/config.toml"))
				if err != nil {
					t.Fatal(err)
				}
				var document struct {
					Hooks map[string][]struct {
						Hooks []struct {
							Command string `toml:"command"`
						} `toml:"hooks"`
					} `toml:"hooks"`
				}
				if err := adapter.DecodeTOML(content, &document); err != nil {
					t.Fatal(err)
				}
				command = document.Hooks["SessionStart"][0].Hooks[0].Command
			} else if id == "antigravity" {
				name, err := adapter.NativeArtifactName(freshness.Source, freshness.ArtifactID)
				if err != nil {
					t.Fatal(err)
				}
				command = antigravityHandler(t, root, name, "SessionStart").Command
			} else if id == "cursor" {
				content, err := os.ReadFile(filepath.Join(root, ".cursor/hooks.json"))
				if err != nil {
					t.Fatal(err)
				}
				var document struct {
					Hooks map[string][]struct {
						Command string `json:"command"`
					} `json:"hooks"`
				}
				if err := json.Unmarshal(content, &document); err != nil {
					t.Fatal(err)
				}
				command = document.Hooks["sessionStart"][0].Command
			} else {
				content, err := os.ReadFile(filepath.Join(root, ".claude/settings.json"))
				if err != nil {
					t.Fatal(err)
				}
				var document struct {
					Hooks map[string][]struct {
						Hooks []struct {
							Command string   `json:"command"`
							Args    []string `json:"args"`
						} `json:"hooks"`
					} `json:"hooks"`
				}
				if err := json.Unmarshal(content, &document); err != nil {
					t.Fatal(err)
				}
				hook := document.Hooks["SessionStart"][0].Hooks[0]
				command = strings.ReplaceAll(hook.Command, "${CLAUDE_PROJECT_DIR}", root)
				args = hook.Args
			}
			var cmd *exec.Cmd
			if id == "claude-code" {
				cmd = exec.Command(command, args...)
			} else {
				cmd = exec.Command("sh", "-c", command)
			}
			cmd.Dir = root
			if id == "antigravity" {
				cmd.Dir = filepath.Join(root, tree)
			}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Env = append(os.Environ(), "PWD="+root, "ACR_BIN="+fakeACR, "CURSOR_VERSION=test")
			cmd.Stdin = strings.NewReader(`{"hook_event_name":"sessionStart","session_id":"s1","workspace_roots":["/p"]}`)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("freshness command: %v; stderr=%s", err, stderr.String())
			}
			var payload struct {
				InjectSteps []struct {
					SystemMessage struct{ SystemMessage string }
				}
				AdditionalContext string `json:"additional_context"`
				Specific          struct {
					Context string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
				t.Fatal(err, stdout.String())
			}
			message := payload.Specific.Context
			if id == "antigravity" && len(payload.InjectSteps) == 1 {
				message = payload.InjectSteps[0].SystemMessage.SystemMessage
			} else if id == "cursor" {
				message = payload.AdditionalContext
				if payload.Specific.Context != "" {
					t.Fatal("Cursor received canonical reply")
				}
			} else if payload.AdditionalContext != "" {
				t.Fatal("canonical host received Cursor reply")
			}
			if !strings.HasPrefix(message, "Session-start status — ") || !strings.Contains(message, "update available") || stderr.Len() != 0 {
				t.Fatalf("freshness native stdout=%s stderr=%s", stdout.String(), stderr.String())
			}
			if id == "antigravity" {
				for _, tc := range []struct{ body, want string }{
					{"exit 0", "{}"},
					{"echo 'fixture failure' >&2; exit 4", "ACR freshness check failed open"},
				} {
					mustHookWrite(t, fakeACR, "#!/bin/sh\n"+tc.body+"\n", 0755)
					probe := exec.Command("sh", "-c", command)
					probe.Dir = cmd.Dir
					probe.Env = cmd.Env
					probe.Stdin = strings.NewReader("{}")
					out, err := probe.CombinedOutput()
					if err != nil || !strings.Contains(string(out), tc.want) {
						t.Fatalf("native freshness: %s, %v", out, err)
					}
				}
			}
			options := map[string]adapter.TargetOptions{".agent/hooks.json": {ConfigFormat: adapter.ConfigJSON}, ".claude/settings.json": {ConfigFormat: adapter.ConfigJSON}, ".codex/config.toml": {ConfigFormat: adapter.ConfigTOML}, ".cursor/hooks.json": {ConfigFormat: adapter.ConfigJSON}}
			// The same package is current, so repeated planning has no content drift.
			repeated, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(root)), []adapter.Package{pkg}, ledger, options)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := realize.NewEngine().Run(root, ledger, repeated, realize.ModeCheck, func(realize.Ledger) error { return nil }); err != nil {
				t.Fatal(err)
			}
			removal, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(root)), nil, ledger, options)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := realize.NewEngine().Run(root, ledger, removal, realize.ModeApply, func(realize.Ledger) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(runtime); !os.IsNotExist(err) {
				t.Fatalf("translator survives final hook removal: %v", err)
			}
		})
	}
}
