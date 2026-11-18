package adapter_test

import (
	"bytes"
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
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestEmbeddedHookTranslator(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("python3 >= 3.9 is required: ", err)
	}
	for _, test := range []struct {
		name, agent, event, input, stdout, stderr, want, diagnostic string
		exit, wantExit                                              int
	}{
		{name: "codex session-start bracket", agent: "codex", event: "session-start", stdout: "[acr] stale index", want: "[acr] stale index"},
		{name: "codex session-start whitespace", agent: "codex", event: "session-start", stdout: " \t[acr] stale index\n", want: " \t[acr] stale index\n"},
		{name: "codex session-start array", agent: "codex", event: "session-start", stdout: "[\"context\"]", want: "[\"context\"]"},
		{name: "codex user-prompt-submit bracket", agent: "codex", event: "user-prompt-submit", stdout: "[acr] stale index", want: "[acr] stale index"},
		{name: "codex user-prompt-submit whitespace", agent: "codex", event: "user-prompt-submit", stdout: " \t[acr] stale index\n", want: " \t[acr] stale index\n"},
		{name: "codex user-prompt-submit array", agent: "codex", event: "user-prompt-submit", stdout: "[\"context\"]", want: "[\"context\"]"},
		{name: "cursor session-start bracket", agent: "cursor", event: "session-start", stdout: "[acr] stale index", want: "{\"additional_context\":\"[acr] stale index\"}"},
		{name: "cursor session-start whitespace", agent: "cursor", event: "session-start", stdout: " \t[acr] stale index\n", want: "{\"additional_context\":\"[acr] stale index\"}"},
		{name: "cursor session-start array", agent: "cursor", event: "session-start", stdout: "[\"context\"]", want: "{\"additional_context\":\"[\\\"context\\\"]\"}"},
		{name: "cursor user-prompt-submit bracket", agent: "cursor", event: "user-prompt-submit", stdout: "[acr] stale index", want: "{\"continue\":true}", diagnostic: "Cursor beforeSubmitPrompt cannot add model context; dropped 17 characters."},
		{name: "cursor user-prompt-submit whitespace", agent: "cursor", event: "user-prompt-submit", stdout: " \t[acr] stale index\n", want: "{\"continue\":true}", diagnostic: "Cursor beforeSubmitPrompt cannot add model context; dropped 17 characters."},
		{name: "cursor user-prompt-submit array", agent: "cursor", event: "user-prompt-submit", stdout: "[\"context\"]", want: "{\"continue\":true}", diagnostic: "Cursor beforeSubmitPrompt cannot add model context; dropped 11 characters."},
		{name: "codex session JSON unchanged", agent: "codex", event: "session-start", stdout: ` {"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"S"}} `, want: `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"S"}}`},
		{name: "codex prompt unchanged", agent: "codex", event: "user-prompt-submit", stdout: `{"decision":"block","reason":"R"}`, want: `{"decision":"block","reason":"ACR hook hooks/check.sh (user-prompt-submit): blocked the prompt: R\nReview this hook's policy before retrying."}`},
		{name: "codex session end unchanged", agent: "codex", event: "session-end", stdout: "ignored", want: "ignored"},
		{name: "codex allow", agent: "codex", event: "pre-tool-use", stdout: `{"hookSpecificOutput":{"permissionDecision":"allow","updatedInput":{"command":"ls"}}}`, want: `{"hookSpecificOutput":{"permissionDecision":"allow","updatedInput":{"command":"ls"}}}`},
		{name: "codex ask", agent: "codex", event: "pre-tool-use", stdout: `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask","permissionDecisionReason":"R"}}`, want: `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":` + jsonString("ACR hook hooks/check.sh (pre-tool-use): denied the tool call: R\nCodex cannot ask for confirmation here. Ask the user in the conversation and retry if they approve.") + `}}`},
		{name: "codex deny unsupported", agent: "codex", event: "pre-tool-use", stdout: `{"continue":false,"stopReason":"X","suppressOutput":true,"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"R"}}`, want: `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"ACR hook hooks/check.sh (pre-tool-use): denied the tool call: R\nReview this hook's policy before retrying."}}`, diagnostic: "dropped unsupported fields"},
		{name: "codex defer", agent: "codex", event: "pre-tool-use", stdout: `{"hookSpecificOutput":{"permissionDecision":"defer"}}`, want: `{"hookSpecificOutput":{}}`},
		{name: "codex exit two", agent: "codex", event: "pre-tool-use", exit: 2, stderr: "R", wantExit: 2},
		{name: "codex post block", agent: "codex", event: "post-tool-use", stdout: `{"decision":"block","reason":"R","hookSpecificOutput":{"additionalContext":"C","updatedMCPToolOutput":"x","updatedToolOutput":"x"},"suppressOutput":true}`, want: `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"ACR hook hooks/check.sh (post-tool-use): provided feedback after the tool call: R\n\nC\nReview this hook's policy before continuing."}}`, diagnostic: "dropped unsupported fields"},
		{name: "codex post exit two", agent: "codex", event: "post-tool-use", exit: 2, stderr: "R", want: `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"ACR hook hooks/check.sh (post-tool-use): provided feedback after the tool call: R\nReview this hook's policy before continuing."}}`},
		{name: "codex stop plain", agent: "codex", event: "stop", stdout: "stop", want: `{}`, diagnostic: "dropped plain text"},
		{name: "codex stop block", agent: "codex", event: "stop", stdout: `{"decision":"block","reason":"R"}`, want: `{"decision":"block","reason":"ACR hook hooks/check.sh (stop): requested continuation: R\nReview this hook's policy before continuing."}`},
		{name: "codex stop empty object", agent: "codex", event: "stop", stdout: `{}`, want: `{}`},
		{name: "cursor session context", agent: "cursor", event: "session-start", input: `{"session_id":"s1","conversation_id":"s1","hook_event_name":"sessionStart","workspace_roots":["/p"]}`, stdout: `{"hookSpecificOutput":{"additionalContext":"S"}}`, want: `{"additional_context":"S"}`},
		{name: "cursor session plain", agent: "cursor", event: "session-start", stdout: "S", want: `{"additional_context":"S"}`},
		{name: "cursor session empty", agent: "cursor", event: "session-start", want: `{}`},
		{name: "cursor session empty object", agent: "cursor", event: "session-start", stdout: `{}`, want: `{}`},
		{name: "cursor prompt empty object", agent: "cursor", event: "user-prompt-submit", stdout: `{}`, want: `{"continue":true}`},
		{name: "cursor allow empty", agent: "cursor", event: "pre-tool-use", want: `{"permission":"allow"}`},
		{name: "cursor allow rewrite", agent: "cursor", event: "pre-tool-use", stdout: `{"hookSpecificOutput":{"permissionDecision":"allow","updatedInput":{"command":"ls"}}}`, want: `{"permission":"allow","updated_input":{"command":"ls"}}`},
		{name: "cursor deny", agent: "cursor", event: "pre-tool-use", stdout: `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"R"}}`, want: `{"permission":"deny","user_message":"ACR hook hooks/check.sh (pre-tool-use): denied the tool call: R\nReview this hook's policy before retrying.","agent_message":"ACR hook hooks/check.sh (pre-tool-use): denied the tool call: R\nReview this hook's policy before retrying."}`},
		{name: "cursor ask", agent: "cursor", event: "pre-tool-use", stdout: `{"hookSpecificOutput":{"permissionDecision":"ask","permissionDecisionReason":"R"}}`, want: `{"permission":"deny","user_message":` + jsonString("ACR hook hooks/check.sh (pre-tool-use): denied the tool call: R\nCursor cannot ask for confirmation here. Ask the user in the conversation and retry if they approve.") + `,"agent_message":` + jsonString("ACR hook hooks/check.sh (pre-tool-use): denied the tool call: R\nCursor cannot ask for confirmation here. Ask the user in the conversation and retry if they approve.") + `}`},
		{name: "cursor pre exit two", agent: "cursor", event: "pre-tool-use", exit: 2, stderr: "R", want: `{"permission":"deny","user_message":"ACR hook hooks/check.sh (pre-tool-use): denied the tool call: R\nReview this hook's policy before retrying.","agent_message":"ACR hook hooks/check.sh (pre-tool-use): denied the tool call: R\nReview this hook's policy before retrying."}`},
		{name: "cursor post block context", agent: "cursor", event: "post-tool-use", input: `{"tool_name":"Shell","tool_input":{"command":"ls"},"tool_output":"{\"exitCode\":0}"}`, stdout: `{"decision":"block","reason":"R","hookSpecificOutput":{"additionalContext":"C","updatedMCPToolOutput":{"text":"new"},"updatedToolOutput":"lost"}}`, want: `{"additional_context":"ACR hook hooks/check.sh (post-tool-use): provided feedback after the tool call: R\n\nC\nReview this hook's policy before continuing.","updated_mcp_tool_output":{"text":"new"}}`, diagnostic: "dropped unsupported fields"},
		{name: "cursor post raw result", agent: "cursor", event: "post-tool-use", input: `{"tool_output":"not JSON"}`, want: `{}`},
		{name: "cursor prompt block", agent: "cursor", event: "user-prompt-submit", stdout: `{"decision":"block","reason":"R"}`, want: `{"continue":false,"user_message":"ACR hook hooks/check.sh (user-prompt-submit): blocked the prompt: R\nReview this hook's policy before retrying."}`},
		{name: "cursor prompt exit two", agent: "cursor", event: "user-prompt-submit", exit: 2, stderr: "R", want: `{"continue":false,"user_message":"ACR hook hooks/check.sh (user-prompt-submit): blocked the prompt: R\nReview this hook's policy before retrying."}`},
		{name: "cursor prompt drop", agent: "cursor", event: "user-prompt-submit", stdout: `{"hookSpecificOutput":{"additionalContext":"C"}}`, want: `{"continue":true}`, diagnostic: "Cursor beforeSubmitPrompt cannot add model context; dropped 1 characters."},
		{name: "cursor stop block", agent: "cursor", event: "stop", input: `{"loop_count":1}`, stdout: `{"decision":"block","reason":"R"}`, want: `{"followup_message":"ACR hook hooks/check.sh (stop): requested continuation: R\nReview this hook's policy before continuing."}`},
		{name: "cursor stop context", agent: "cursor", event: "stop", stdout: `{"hookSpecificOutput":{"additionalContext":"C"}}`, want: `{"followup_message":"ACR hook hooks/check.sh (stop): requested continuation: C\nReview this hook's policy before continuing."}`},
		{name: "cursor stop exit two", agent: "cursor", event: "stop", exit: 2, stderr: "R", want: `{"followup_message":"ACR hook hooks/check.sh (stop): requested continuation: R\nReview this hook's policy before continuing."}`},
		{name: "cursor stop empty", agent: "cursor", event: "stop", want: `{}`},
		{name: "cursor end discard", agent: "cursor", event: "session-end", stdout: `{"decision":"block","reason":"R"}`, want: `{}`},
		{name: "codex invalid JSON", agent: "codex", event: "stop", stdout: "{broken", diagnostic: "printed invalid JSON"},
		{name: "cursor invalid JSON", agent: "cursor", event: "pre-tool-use", stdout: "{broken", diagnostic: "printed invalid JSON"},
		{name: "hook exit one", agent: "codex", event: "stop", stdout: "must disappear", stderr: "failure", exit: 1, diagnostic: "exited 1"},
		{name: "hook failure", agent: "cursor", event: "stop", stdout: "must disappear", stderr: "failure", exit: 3, diagnostic: "exited 3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			tree := "." + test.agent + "/hooks"
			runtime := filepath.Join(root, tree, adapter.HookTranslatorBasename)
			hookPath := "hooks/check.sh"
			writeTranslatorFixture(t, runtime, adapter.HookTranslator())
			// Plant a shadow module: both -I and the direct-exec preamble must ignore it.
			writeTranslatorFixture(t, filepath.Join(root, tree, "json.py"), []byte("raise RuntimeError('shadowed stdlib')\n"))
			body := "#!/usr/bin/env bash\nset -euo pipefail\ncat >\"$CLAUDE_PROJECT_DIR/seen-input\"\nprintf '%s' \"$CLAUDE_PROJECT_DIR\" >\"$CLAUDE_PROJECT_DIR/seen-root\"\nprintf '%s' \"$0\" >\"$CLAUDE_PROJECT_DIR/seen-hook\"\nprintf '%s' " + adapter.ShellQuote(test.stdout) + "\nprintf '%s' " + adapter.ShellQuote(test.stderr) + " >&2\nexit " + string(rune('0'+test.exit)) + "\n"
			writeTranslatorFixture(t, filepath.Join(root, hookPath), []byte(body))
			input := test.input
			if input == "" {
				input = "{ \"session_id\": \"s\", \"opaque\": true }\n"
			}
			cmd := exec.Command("python3", "-I", runtime, test.agent, test.event, hookPath)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "acr_hook_timeout=86400")
			cmd.Stdin = strings.NewReader(input)
			var out, errOut bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errOut
			err := cmd.Run()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != test.wantExit {
				t.Fatalf("exit = %v; stderr = %s", err, errOut.String())
			}
			want := test.want
			if test.diagnostic == "printed invalid JSON" || (test.exit != 0 && test.exit != 2) {
				message := "ACR hook " + hookPath + " (" + test.event + "): runtime failure: "
				if test.diagnostic == "printed invalid JSON" {
					message += "printed invalid JSON or an invalid hook response"
				} else {
					message += test.diagnostic + "; stdout discarded"
				}
				message += "; Repair this hook and run acr realize before retrying."
				if test.agent == "codex" {
					want = `{"systemMessage":` + jsonString(message) + `}`
				} else if test.event == "pre-tool-use" {
					want = `{"permission":"deny","user_message":` + jsonString(message) + `,"agent_message":` + jsonString(message) + `}`
				} else {
					want = `{}`
				}
			}
			if test.agent == "codex" && strings.HasPrefix(strings.TrimSpace(want), "{") && json.Valid([]byte(want)) {
				var expected map[string]any
				if err := json.Unmarshal([]byte(want), &expected); err != nil {
					t.Fatal(err)
				}
				if specific, ok := expected["hookSpecificOutput"].(map[string]any); ok {
					specific["hookEventName"] = map[string]string{"pre-tool-use": "PreToolUse", "post-tool-use": "PostToolUse", "session-start": "SessionStart"}[test.event]
				}
				encoded, err := json.Marshal(expected)
				if err != nil {
					t.Fatal(err)
				}
				want = string(encoded)
			}
			if strings.HasPrefix(strings.TrimSpace(want), "{") && json.Valid([]byte(want)) {
				assertTranslatorJSON(t, out.String(), want)
			} else if out.String() != want {
				t.Fatalf("stdout bytes = %q, want %q", out.String(), want)
			}
			if strings.HasSuffix(test.name, "empty object") && errOut.Len() != 0 {
				t.Fatalf("no-op JSON produced a diagnostic: %s", errOut.String())
			}
			if !strings.Contains(errOut.String(), test.diagnostic) || !strings.Contains(errOut.String(), test.stderr) {
				t.Fatalf("stderr = %q, want %q and %q", errOut.String(), test.diagnostic, test.stderr)
			}
			seen, err := os.ReadFile(filepath.Join(root, "seen-root"))
			if err != nil || string(seen) != root {
				t.Fatalf("CLAUDE_PROJECT_DIR = %q, %v; want %q", seen, err, root)
			}
			seen, err = os.ReadFile(filepath.Join(root, "seen-input"))
			if err != nil {
				t.Fatal(err)
			}
			if test.agent == "codex" {
				if string(seen) != input {
					t.Fatalf("stdin bytes = %q, want %q", seen, input)
				}
			} else {
				var original, canonical map[string]any
				if err := json.Unmarshal([]byte(input), &original); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(seen, &canonical); err != nil {
					t.Fatal(err)
				}
				for key, value := range original {
					if key != "hook_event_name" && key != "session_id" && key != "cwd" && !reflect.DeepEqual(canonical[key], value) {
						t.Fatalf("lost native field %s", key)
					}
				}
				if canonical["session_id"] != original["session_id"] && canonical["session_id"] != original["conversation_id"] {
					t.Fatalf("session_id: %s", seen)
				}
				if test.event == "session-start" && canonical["source"] != "startup" {
					t.Fatalf("source: %s", seen)
				}
				if test.event == "stop" && canonical["stop_hook_active"] != (original["loop_count"] != nil && original["loop_count"].(float64) > 0) {
					t.Fatalf("stop guard: %s", seen)
				}
				if test.name == "cursor post block context" && !reflect.DeepEqual(canonical["tool_response"], map[string]any{"exitCode": float64(0)}) {
					t.Fatalf("tool_response: %s", seen)
				}
			}
		})
	}
}

func TestHookTranslatorFailureAndIsolation(t *testing.T) {
	root := t.TempDir()
	runtime := filepath.Join(root, ".cursor/hooks/acr-hook.py")
	writeTranslatorFixture(t, runtime, adapter.HookTranslator())
	writeTranslatorFixture(t, filepath.Join(root, ".cursor/hooks/json.py"), []byte("raise RuntimeError('shadowed stdlib')\n"))
	for _, args := range [][]string{nil, {"other", "stop", "missing"}, {"cursor", "other", "missing"}, {"cursor", "stop", "missing"}, {"codex", "stop", "missing"}} {
		cmd := exec.Command(runtime, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdin = strings.NewReader("{}")
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		wantExit := 1
		if len(args) == 3 && (args[0] == "codex" || args[0] == "cursor") && args[1] == "stop" {
			wantExit = 0
		}
		err := cmd.Run()
		if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != wantExit || strings.Contains(stderr.String(), "shadowed stdlib") {
			t.Fatalf("args %q: error %v, stdout %q, stderr %q", args, err, out.String(), stderr.String())
		}
		if len(args) == 3 && (args[0] == "cursor" || args[0] == "codex") && args[1] == "stop" && !strings.Contains(stderr.String(), "run acr realize") {
			t.Fatalf("missing-hook diagnostic: %s", stderr.String())
		}
	}
}

func writeTranslatorFixture(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
}

func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func assertTranslatorJSON(t *testing.T, got, want string) {
	t.Helper()
	if want == "" {
		if got != "" {
			t.Fatalf("stdout = %q, want empty", got)
		}
		return
	}
	var actual, expected any
	if err := json.Unmarshal([]byte(got), &actual); err != nil {
		t.Fatal(err, got)
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err, want)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("stdout = %s, want %s", got, want)
	}
}

func TestHookTranslatorBrokenPipe(t *testing.T) {
	for _, agent := range []string{"codex", "cursor"} {
		for _, stage := range []string{"buffered-stdout", "stderr", "failure-reply", "exception-reply"} {
			t.Run(agent+"/"+stage, func(t *testing.T) {
				root := t.TempDir()
				runtime := filepath.Join(root, "."+agent, "hooks", adapter.HookTranslatorBasename)
				source := string(adapter.HookTranslator())
				hook := "." + agent + "/hooks/h.sh"
				body := "#!/bin/sh\nprintf '{}'\n"
				event := "pre-tool-use"
				if stage == "stderr" {
					body = "#!/bin/sh\nprintf '{}'\nprintf reason >&2\n"
				} else if stage == "failure-reply" {
					body = "#!/bin/sh\nprintf '{broken'\n"
				} else if stage == "exception-reply" {
					source = strings.Replace(source, "    agent, event, path = sys.argv[1:4]", "    raise RuntimeError('injected')", 1)
				}
				if stage == "buffered-stdout" {
					// The small reply is buffered; only the final flush sees EPIPE.
					event = "stop"
				}
				writeTranslatorFixture(t, runtime, []byte(source))
				writeTranslatorFixture(t, filepath.Join(root, hook), []byte(body))
				read, write, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := read.Close(); err != nil {
					t.Fatal(err)
				}
				defer write.Close()
				cmd := exec.Command("python3", "-I", runtime, agent, event, hook)
				cmd.Env = append(os.Environ(), "acr_hook_timeout=86400")
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				cmd.Stdin = strings.NewReader("{}") // Ordinary EOF completes input.
				var out, stderr bytes.Buffer
				if stage == "buffered-stdout" {
					cmd.Stdout, cmd.Stderr = write, &stderr
				} else {
					cmd.Stdout, cmd.Stderr = &out, write
				}
				err = cmd.Run()
				if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 141 || out.Len() != 0 || stderr.Len() != 0 {
					t.Fatalf("EPIPE: %v, status=%v stdout=%q stderr=%q", err, cmd.ProcessState, out.String(), stderr.String())
				}
			})
		}
	}
}

func TestHookWrapperCancellationAndPublication(t *testing.T) {
	for _, agent := range []string{"codex", "cursor"} {
		for _, status := range []string{"129", "130", "141", "143", "0"} {
			t.Run(agent+"/"+status, func(t *testing.T) {
				root := t.TempDir()
				runtime := filepath.Join(root, "translator.py")
				// Partial output must be discarded even if the runtime failed while
				// publishing; 0 instead exercises the shell's final printf EPIPE.
				writeTranslatorFixture(t, runtime, []byte("import sys\nprint('partial')\nsys.exit("+status+")\n"))
				command := adapter.TranslatedHookCommand(adapter.ShellQuote(runtime), agent, manifest.HookPreToolUse, "unused", nil)
				cmd := exec.Command("/bin/sh", "-c", command)
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				var out, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &out, &stderr
				want := status
				if status == "0" {
					read, write, err := os.Pipe()
					if err != nil {
						t.Fatal(err)
					}
					if err := read.Close(); err != nil {
						t.Fatal(err)
					}
					defer write.Close()
					cmd.Stdout = write
					want = "141"
				}
				err := cmd.Run()
				if cmd.ProcessState == nil || fmt.Sprint(cmd.ProcessState.ExitCode()) != want || out.Len() != 0 || strings.Contains(stderr.String(), "ACR hook") {
					t.Fatalf("wrapper: %v status=%v stdout=%q stderr=%q", err, cmd.ProcessState, out.String(), stderr.String())
				}
			})
		}
	}
}

func TestHookTranslatorASCIILocale(t *testing.T) {
	for _, agent := range []string{"codex", "cursor"} {
		for _, scenario := range []string{"plain-session", "json-session", "denial", "failure"} {
			t.Run(agent+"/"+scenario, func(t *testing.T) {
				root := t.TempDir()
				runtime := filepath.Join(root, "."+agent, "hooks", adapter.HookTranslatorBasename)
				// -I ignores PYTHONIOENCODING. Reconfigure the fixture's actual
				// text streams so UTF-8 output cannot pass by relying on the locale.
				source := strings.Replace(string(adapter.HookTranslator()), "import posix", "import posix\nsys.stdout.reconfigure(encoding='ascii')\nsys.stderr.reconfigure(encoding='ascii')", 1)
				writeTranslatorFixture(t, runtime, []byte(source))
				hookPath := "hooks/chécḱ 'quoted' \\ $literal `literal`.sh"
				event, reply := "session-start", "Session-start status — ready"
				want := reply
				if scenario == "json-session" {
					reply = `{"hookSpecificOutput":{"additionalContext":"Session-start status — ready"}}`
					want = `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"Session-start status — ready"}}`
				}
				if agent == "cursor" {
					want = `{"additional_context":"Session-start status — ready"}`
				}
				if scenario == "denial" {
					event = "pre-tool-use"
					reply = `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"révision — 必須"}}`
					message := "ACR hook " + hookPath + " (pre-tool-use): denied the tool call: révision — 必須\nReview this hook's policy before retrying."
					want = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":` + jsonString(message) + `}}`
					if agent == "cursor" {
						want = `{"permission":"deny","user_message":` + jsonString(message) + `,"agent_message":` + jsonString(message) + `}`
					}
				}
				if scenario == "failure" {
					event, reply = "stop", "{broken"
					message := "ACR hook " + hookPath + " (stop): runtime failure: printed invalid JSON or an invalid hook response; Repair this hook and run acr realize before retrying."
					want = `{"systemMessage":` + jsonString(message) + `}`
					if agent == "cursor" {
						want = `{}`
					}
				}
				writeTranslatorFixture(t, filepath.Join(root, hookPath), []byte("#!/bin/sh\nprintf '%s' "+adapter.ShellQuote(reply)+"\n"))
				cmd := exec.Command("python3", "-I", runtime, agent, event, hookPath)
				cmd.Env = append(os.Environ(), "acr_hook_timeout=86400")
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				cmd.Stdin = strings.NewReader("{}")
				var out, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &out, &stderr
				if err := cmd.Run(); err != nil {
					t.Fatalf("ASCII streams: %v; %s", err, stderr.String())
				}
				if scenario == "failure" {
					message := "ACR hook " + hookPath + " (stop): runtime failure: printed invalid JSON or an invalid hook response; Repair this hook and run acr realize before retrying.\n"
					if stderr.String() != message {
						t.Fatalf("stderr = %q; want %q", stderr.String(), message)
					}
				} else if stderr.Len() != 0 {
					t.Fatalf("valid Unicode reply rejected: %s", stderr.String())
				}
				if agent == "codex" && scenario == "plain-session" {
					if out.String() != want {
						t.Fatalf("context = %q; want %q", out.String(), want)
					}
				} else {
					assertTranslatorJSON(t, out.String(), want)
				}
			})
		}
	}
}

func TestHookFailureCommandLiteralMessage(t *testing.T) {
	message := "ACR hook hooks/check 'quote' \\ $literal `literal`.sh (pre-tool-use): runtime failure: \"quoted\"\nrévision — 必須; Repair this hook and run acr realize before retrying."
	for _, agent := range []string{"codex", "cursor"} {
		for _, event := range adapter.SortedEvents() {
			t.Run(agent+"/"+string(event), func(t *testing.T) {
				cmd := exec.Command("/bin/sh", "-c", adapter.HookFailureCommand(agent, event, message))
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				cmd.Dir = t.TempDir()
				var out, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &out, &stderr
				err := cmd.Run()
				wantExit := 0
				if agent == "codex" && (event == manifest.HookPreToolUse || event == manifest.HookUserPromptSubmit) {
					wantExit = 2
				}
				if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != wantExit || stderr.String() != message+"\n" {
					t.Fatalf("literal diagnostic: %v %q %q", err, out.String(), stderr.String())
				}
				want := map[string]any{}
				if agent == "cursor" && event == manifest.HookPreToolUse {
					want = map[string]any{"permission": "deny", "user_message": message, "agent_message": message}
				}
				if agent == "cursor" && event == manifest.HookUserPromptSubmit {
					want = map[string]any{"continue": false, "user_message": message}
				}
				if agent == "codex" {
					if wantExit == 2 || event == manifest.HookSessionEnd {
						if out.Len() != 0 {
							t.Fatalf("unexpected stdout: %q", out.String())
						}
						return
					}
					want["systemMessage"] = message
				}
				encoded, err := json.Marshal(want)
				if err != nil {
					t.Fatal(err)
				}
				assertTranslatorJSON(t, out.String(), string(encoded))
			})
		}
	}
}
