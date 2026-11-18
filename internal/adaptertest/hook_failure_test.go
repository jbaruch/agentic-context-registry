package adaptertest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// Execute the actual TOML/hooks.json commands. Every failure is tested at every
// event, so repairing a permission gate cannot accidentally create a Stop loop.
func TestRealizedHookFailures(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
		for _, event := range adapter.SortedEvents() {
			t.Run(native.Descriptor().ID+"/"+string(event), func(t *testing.T) {
				id := native.Descriptor().ID
				root, command, hook := realizeFailureHook(t, native, event)
				runtime := filepath.Join(root, "."+id, "hooks", adapter.HookTranslatorBasename)
				nested := filepath.Join(root, "nested", "session")
				mustHookWrite(t, filepath.Join(nested, "marker"), "", 0o644)
				cwd := root
				if id == "codex" {
					cwd = nested
				}
				for _, scenario := range []string{"missing-python", "python-startup", "missing-translator", "broken-translator", "internal-exception", "missing-hook", "unrunnable-hook", "invalid-json", "invalid-shape", "unexpected-exit", "absolute-path", "parent-path", "symlink-escape", "invalid-stdin"} {
					t.Run(scenario, func(t *testing.T) {
						mustHookWrite(t, runtime, string(adapter.HookTranslator()), 0o755)
						mustHookWrite(t, hook, "#!/bin/sh\nprintf '{}'\n", 0o755)
						cmdText, input, pathEnv := command, "{}", os.Getenv("PATH")
						actualPath := "." + id + "/hooks/acr__example__hooks__hook/hook.sh"
						switch scenario {
						case "missing-python":
							pathEnv = t.TempDir()
						case "python-startup":
							pathEnv = t.TempDir()
							mustHookWrite(t, filepath.Join(pathEnv, "python3"), "#!/bin/sh\nprintf 'interpreter failed\\n' >&2\nexit 1\n", 0o755)
						case "missing-translator":
							if err := os.Remove(runtime); err != nil {
								t.Fatal(err)
							}
						case "broken-translator":
							mustHookWrite(t, runtime, "invalid syntax !\n", 0o755)
						case "internal-exception":
							mustHookWrite(t, runtime, strings.Replace(string(adapter.HookTranslator()), "    agent, event, path = sys.argv[1:4]", "    raise RuntimeError('injected translator failure')", 1), 0o755)
						case "missing-hook":
							if err := os.Remove(hook); err != nil {
								t.Fatal(err)
							}
						case "unrunnable-hook":
							if err := os.Chmod(hook, 0o644); err != nil {
								t.Fatal(err)
							}
						case "invalid-json":
							mustHookWrite(t, hook, "#!/bin/sh\nprintf '{broken'\n", 0o755)
						case "invalid-shape":
							mustHookWrite(t, hook, "#!/bin/sh\nprintf '%s' '{\"hookSpecificOutput\":[]}'\n", 0o755)
						case "unexpected-exit":
							mustHookWrite(t, hook, "#!/bin/sh\nprintf 'discard this'\nexit 7\n", 0o755)
						case "absolute-path", "parent-path", "symlink-escape":
							outside := filepath.Join(t.TempDir(), "outside.sh")
							// If containment fails this writes a marker, proving unintended execution.
							mustHookWrite(t, outside, "#!/bin/sh\nprintf ran >"+adapter.ShellQuote(filepath.Join(root, "escaped"))+"\nprintf '{}'\n", 0o755)
							relative, err := filepath.Rel(root, hook)
							if err != nil {
								t.Fatal(err)
							}
							replacement := outside
							if scenario == "parent-path" {
								replacement = "../" + filepath.Base(root) + "/" + filepath.ToSlash(relative)
							}
							if scenario == "symlink-escape" {
								if err := os.Remove(hook); err != nil {
									t.Fatal(err)
								}
								if err := os.Symlink(outside, hook); err != nil {
									t.Fatal(err)
								}
								t.Cleanup(func() {
									if err := os.Remove(hook); err != nil {
										t.Error(err)
									}
								})
								replacement = filepath.ToSlash(relative)
							}
							actualPath = replacement
							invocation := " " + id + " " + string(event) + " " + adapter.ShellQuote(filepath.ToSlash(relative))
							cmdText = strings.Replace(command, invocation, " "+id+" "+string(event)+" "+adapter.ShellQuote(replacement), 1)
							if cmdText == command && scenario != "symlink-escape" {
								t.Fatal("did not replace invocation")
							}
						case "invalid-stdin":
							if id == "codex" { // Codex forwards stdin unchanged; provoke a canonical-hook parse failure.
								mustHookWrite(t, hook, "#!/bin/sh\nexit 1\n", 0o755)
							}
							input = "{broken"
						}
						out, stderr, code := runFailureCommand(t, cmdText, cwd, input, pathEnv)
						gating := event == manifest.HookPreToolUse || event == manifest.HookUserPromptSubmit
						wantCode := 0
						if gating {
							if id == "codex" {
								wantCode = 2
							} else {
								wantCode = 0
							}
						}
						if code != wantCode {
							t.Fatalf("exit %d want %d; stdout %q; stderr %q", code, wantCode, out, stderr)
						}
						prefix := "ACR hook " + actualPath + " (" + string(event) + "): "
						diagnostic := map[string]string{
							"internal-exception": "translator failed (RuntimeError)",
							"missing-hook":       "cannot run hook (FileNotFoundError)",
							"unrunnable-hook":    "cannot run hook (PermissionError)",
							"invalid-json":       "printed invalid JSON or an invalid hook response",
							"invalid-shape":      "printed invalid JSON or an invalid hook response",
							"unexpected-exit":    "exited 7; stdout discarded",
							"absolute-path":      "hook path must be project-relative without .. segments",
							"parent-path":        "hook path must be project-relative without .. segments",
							"symlink-escape":     "hook path resolves outside the realized project root",
							"invalid-stdin":      "invalid native stdin (JSONDecodeError)",
						}[scenario]
						if scenario == "invalid-stdin" && id == "codex" {
							diagnostic = "exited 1; stdout discarded"
						}
						message := prefix + "runtime failure: " + diagnostic + "; Repair this hook and run acr realize before retrying."
						shellFailure := scenario == "missing-python" || scenario == "python-startup" || scenario == "missing-translator" || scenario == "broken-translator"
						if shellFailure {
							message = prefix + "runtime failure: translator failed; Check Python >= 3.9 on the agent PATH and run acr realize before retrying."
							if scenario == "missing-python" {
								message = prefix + "could not run because Python >= 3.9 is unavailable; Install Python >= 3.9 on the agent PATH and retry."
							}
						}
						assertNativeFailureMessage(t, id, event, out, stderr, code, message, shellFailure)
						interpreterStderr := strings.TrimSuffix(stderr, message+"\n")
						switch scenario {
						case "python-startup":
							if interpreterStderr != "interpreter failed\n" {
								t.Fatalf("interpreter stderr = %q", interpreterStderr)
							}
						case "broken-translator":
							if !strings.Contains(interpreterStderr, "SyntaxError: invalid syntax") || strings.Contains(interpreterStderr, "ACR hook") {
								t.Fatalf("interpreter stderr = %q", interpreterStderr)
							}
						default:
							if interpreterStderr != "" {
								t.Fatalf("extra failure diagnostics: %q", interpreterStderr)
							}
						}

						if strings.Contains(out, "discard this") {
							t.Fatalf("failed stdout leaked: %q", out)
						}
						if gating && id == "cursor" {
							var v map[string]any
							if err := json.Unmarshal([]byte(out), &v); err != nil {
								t.Fatal(err, out)
							}
							if event == manifest.HookPreToolUse && v["permission"] != "deny" || event == manifest.HookUserPromptSubmit && v["continue"] != false {
								t.Fatalf("gate allowed: %s", out)
							}
							if _, exists := v["updated_input"]; exists {
								t.Fatal("deny includes rewrite")
							}
						}
						if event == manifest.HookStop && (strings.Contains(out, `"block"`) || strings.Contains(out, "followup_message")) {
							t.Fatalf("failure continues Stop: %s", out)
						}
						if _, err := os.Stat(filepath.Join(root, "escaped")); !os.IsNotExist(err) {
							t.Fatalf("outside hook executed: %v", err)
						}
					})
				}
			})
		}
	}
}

func TestRealizedHookControlAndBytes(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
		for _, event := range adapter.SortedEvents() {
			t.Run(native.Descriptor().ID+"/"+string(event), func(t *testing.T) {
				id := native.Descriptor().ID
				root, command, hook := realizeFailureHook(t, native, event)
				cwd := root
				if id == "codex" {
					cwd = filepath.Join(root, "nested")
					mustHookWrite(t, filepath.Join(cwd, "marker"), "", 0o644)
				}
				t.Run("exit-two-non-utf8", func(t *testing.T) {
					mustHookWrite(t, hook, "#!/bin/sh\nprintf '\\377{bad'\nprintf 'blocked\\377' >&2\nexit 2\n", 0o755)
					out, stderr, code := runFailureCommand(t, command, cwd, "{}", os.Getenv("PATH"))
					action := map[manifest.HookEvent]string{
						manifest.HookPreToolUse:       "denied the tool call",
						manifest.HookUserPromptSubmit: "blocked the prompt",
						manifest.HookStop:             "requested continuation",
						manifest.HookPostToolUse:      "provided feedback after the tool call",
						manifest.HookSessionStart:     "provided session feedback",
						manifest.HookSessionEnd:       "provided session feedback",
					}[event]
					recovery := "Review this hook's policy before continuing."
					if event == manifest.HookPreToolUse || event == manifest.HookUserPromptSubmit {
						recovery = "Review this hook's policy before retrying."
					}
					want := "ACR hook ." + id + "/hooks/acr__example__hooks__hook/hook.sh (" + string(event) + "): " + action + ": blocked�\n" + recovery + "\n"
					if stderr != want {
						t.Fatalf("prepared diagnostic = %q; want %q", stderr, want)
					}
					wantCode := 0
					if id == "codex" && (event == manifest.HookPreToolUse || event == manifest.HookUserPromptSubmit || event == manifest.HookStop) {
						wantCode = 2
					}
					if code != wantCode {
						t.Fatalf("exit %d want %d; %s", code, wantCode, stderr)
					}
					if wantCode == 2 && out != string([]byte{255, '{', 'b', 'a', 'd'}) {
						t.Fatalf("exit-two stdout bytes: %q", out)
					}
					if id == "cursor" && event == manifest.HookPreToolUse && !strings.Contains(out, `"deny"`) {
						t.Fatal(out)
					}
					if id == "cursor" && event == manifest.HookUserPromptSubmit && !strings.Contains(out, `false`) {
						t.Fatal(out)
					}
				})
				t.Run("unknown-fields", func(t *testing.T) {
					reply := `{"unknown":true,"additionalContext":"unsupported","hookSpecificOutput":{"additionalContext":"C","unknown":true}}`
					mustHookWrite(t, hook, "#!/bin/sh\nprintf '%s' "+adapter.ShellQuote(reply)+"\n", 0o755)
					out, stderr, code := runFailureCommand(t, command, cwd, "{}", os.Getenv("PATH"))
					if code != 0 {
						t.Fatalf("%d %s", code, stderr)
					}
					if strings.Contains(out, "unknown") || strings.Contains(out, "unsupported") {
						t.Fatalf("unknown fields leaked: %s", out)
					}
					if id == "codex" && event == manifest.HookStop {
						var v map[string]any
						if err := json.Unmarshal([]byte(out), &v); err != nil {
							t.Fatal(err)
						}
						if v["decision"] != "block" || v["reason"] != "ACR hook .codex/hooks/acr__example__hooks__hook/hook.sh (stop): requested continuation: C\nReview this hook's policy before continuing." || v["hookSpecificOutput"] != nil {
							t.Fatalf("Stop schema: %s", out)
						}
					}
				})
				if event == manifest.HookPreToolUse {
					for _, decision := range []string{"ask", "deny"} {
						t.Run(decision+"-rewrite", func(t *testing.T) {
							reply := `{"hookSpecificOutput":{"permissionDecision":"` + decision + `","permissionDecisionReason":"R","updatedInput":{"command":"rewritten"},"updatedToolOutput":"drop"}}`
							mustHookWrite(t, hook, "#!/bin/sh\nprintf '%s' "+adapter.ShellQuote(reply)+"\n", 0o755)
							out, stderr, code := runFailureCommand(t, command, cwd, "{}", os.Getenv("PATH"))
							if code != 0 || !strings.Contains(out, `"deny"`) || strings.Contains(out, "rewritten") || strings.Contains(out, "updatedToolOutput") {
								t.Fatalf("deny/rewrite: %d %s %s", code, out, stderr)
							}
						})
					}
					t.Run("legacy-block", func(t *testing.T) {
						mustHookWrite(t, hook, "#!/bin/sh\nprintf '%s' '{\"decision\":\"block\",\"reason\":\"R\"}'\n", 0o755)
						out, stderr, code := runFailureCommand(t, command, cwd, "{}", os.Getenv("PATH"))
						if code != 0 || !strings.Contains(out, `"deny"`) {
							t.Fatalf("legacy block: %d %s %s", code, out, stderr)
						}
					})
				}
				if id == "codex" && (event == manifest.HookSessionStart || event == manifest.HookSessionEnd || event == manifest.HookUserPromptSubmit) {
					t.Run("plain-non-utf8", func(t *testing.T) {
						mustHookWrite(t, hook, "#!/bin/sh\nprintf 'context\\377'\n", 0o755)
						out, stderr, code := runFailureCommand(t, command, cwd, "{}", os.Getenv("PATH"))
						if code != 0 || out != string([]byte{'c', 'o', 'n', 't', 'e', 'x', 't', 255}) {
							t.Fatalf("plain bytes: %d %q %s", code, out, stderr)
						}
					})
				}
			})
		}
	}
}

func realizeFailureHook(t *testing.T, native adapter.Adapter, event manifest.HookEvent, fixtureTimeout ...int) (string, string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "project 'quoted' $literal")
	mustHookWrite(t, filepath.Join(root, "agents.yaml"), "schemaVersion: 1\n", 0o644)
	pkgRoot := t.TempDir()
	mustHookWrite(t, filepath.Join(pkgRoot, "hook.sh"), "#!/bin/sh\nprintf '{}'\n", 0o755)
	pkg := adapter.Package{Source: "github:example/hooks", Root: os.DirFS(pkgRoot), Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Hooks: []manifest.HookArtifact{{ID: "hook", Event: event, Path: "hook.sh"}}}}}
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), native)
	if err != nil {
		t.Fatal(err)
	}
	ledger := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}
	intents, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(root)), []adapter.Package{pkg}, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := realize.NewEngine().Run(root, ledger, intents, realize.ModeApply, func(realize.Ledger) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var command string
	id := native.Descriptor().ID
	hookPath := filepath.Join(root, "."+id, "hooks/acr__example__hooks__hook/hook.sh")
	// Fresh executable startup on macOS can consume Codex SessionEnd's entire
	// native budget. Prepare the inert fixture before testing response bytes;
	// watchdog tests separately exercise slow package hooks and launchers.
	if out, err := exec.Command(hookPath).CombinedOutput(); err != nil {
		t.Fatalf("prepare hook fixture: %v; %s", err, out)
	}
	if len(fixtureTimeout) != 0 {
		config := filepath.Join(root, "."+id, "hooks.json")
		if id == "codex" {
			config = filepath.Join(root, ".codex", "config.toml")
		}
		content, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		// Lower the real native fixture deadline and the command's matching deadline.
		// There is no test-only runtime flag or environment override.
		old := adapter.HookTimeoutSeconds(id, event)
		text := strings.ReplaceAll(string(content), fmt.Sprintf("acr_hook_timeout=%d", old), fmt.Sprintf("acr_hook_timeout=%d", fixtureTimeout[0]))
		text = strings.ReplaceAll(text, fmt.Sprintf("timeout = %d", old), fmt.Sprintf("timeout = %d", fixtureTimeout[0]))
		text = strings.ReplaceAll(text, fmt.Sprintf(`"timeout": %d`, old), fmt.Sprintf(`"timeout": %d`, fixtureTimeout[0]))
		text = strings.ReplaceAll(text, fmt.Sprintf(`"timeout":%d`, old), fmt.Sprintf(`"timeout":%d`, fixtureTimeout[0]))
		mustHookWrite(t, config, text, 0o644)
	}
	if id == "codex" {
		content, err := os.ReadFile(filepath.Join(root, ".codex/config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Command string `toml:"command"`
					Timeout int    `toml:"timeout"`
				} `toml:"hooks"`
			} `toml:"hooks"`
		}
		if err := adapter.DecodeTOML(content, &document); err != nil {
			t.Fatal(err)
		}
		for _, groups := range document.Hooks {
			command = groups[0].Hooks[0].Command
			if len(fixtureTimeout) != 0 && groups[0].Hooks[0].Timeout != fixtureTimeout[0] {
				t.Fatal("fixture native deadline was not lowered")
			}
		}
	} else {
		content, err := os.ReadFile(filepath.Join(root, ".cursor/hooks.json"))
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Hooks map[string][]struct {
				Command    string `json:"command"`
				Timeout    int    `json:"timeout"`
				FailClosed bool   `json:"failClosed"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(content, &document); err != nil {
			t.Fatal(err)
		}
		for _, hooks := range document.Hooks {
			command = hooks[0].Command
			if len(fixtureTimeout) != 0 && hooks[0].Timeout != fixtureTimeout[0] {
				t.Fatal("fixture native deadline was not lowered")
			}
			gating := event == manifest.HookPreToolUse || event == manifest.HookUserPromptSubmit
			if hooks[0].FailClosed != gating {
				t.Fatalf("failClosed = %v for %s", hooks[0].FailClosed, event)
			}
		}
	}
	if command == "" {
		t.Fatal("no realized command")
	}
	return root, command, hookPath
}

func mustHookWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile preserves an existing file's permissions across scenarios.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func runFailureCommand(t *testing.T, command, cwd, input, pathEnv string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = cwd
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "PATH=") && !strings.HasPrefix(entry, "PWD=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "PATH="+pathEnv, "PWD="+cwd)
	cmd.Stdin = strings.NewReader(input)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	if cmd.ProcessState == nil {
		t.Fatalf("start command: %v", err)
	}
	return out.String(), stderr.String(), cmd.ProcessState.ExitCode()
}

// Both failure matrices use the independently specified native message routes.
func assertNativeFailureMessage(t *testing.T, id string, event manifest.HookEvent, out, stderr string, code int, message string, shellFailure bool) {
	t.Helper()
	if !strings.HasSuffix(stderr, message+"\n") || strings.Count(stderr, message+"\n") != 1 {
		t.Fatalf("generated stderr = %q; want one final %q", stderr, message+"\n")
	}
	if id == "codex" && event == manifest.HookSessionEnd {
		want := message
		if shellFailure {
			want = ""
		}
		if code != 0 || out != want {
			t.Fatalf("session-end: %d %q; want %q", code, out, want)
		}
		return
	}
	got := nativeReply(t, id, event, out, stderr, code)
	if id == "codex" && (event == manifest.HookPreToolUse || event == manifest.HookUserPromptSubmit) {
		if code != 2 || out != "" {
			t.Fatalf("failure gate: %d %q", code, out)
		}
		return
	}
	want := map[string]any{}
	if id == "codex" {
		want["systemMessage"] = message
	} else {
		switch event {
		case manifest.HookPreToolUse:
			want = map[string]any{"permission": "deny", "user_message": message, "agent_message": message}
		case manifest.HookUserPromptSubmit:
			want = map[string]any{"continue": false, "user_message": message}
		case manifest.HookSessionStart, manifest.HookPostToolUse:
			if !shellFailure {
				want["additional_context"] = message
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("native failure = %s; want %#v", out, want)
	}
}
