package adaptertest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// These regressions execute the native commands from realized TOML/hooks.json,
// including nested Codex cwd, rather than calling the translation function.
func TestRealizedHookReplyContract(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
		for _, event := range []manifest.HookEvent{manifest.HookPreToolUse, manifest.HookUserPromptSubmit, manifest.HookStop} {
			t.Run(native.Descriptor().ID+"/"+string(event), func(t *testing.T) {
				id := native.Descriptor().ID
				root, command, hook := realizeFailureHook(t, native, event)
				cwd := root
				if id == "codex" {
					cwd = filepath.Join(root, "nested", "session")
					mustHookWrite(t, filepath.Join(cwd, "marker"), "", 0o644)
				}
				path := "." + id + "/hooks/acr__example__hooks__hook/hook.sh"
				action := map[manifest.HookEvent]string{
					manifest.HookPreToolUse:       "denied the tool call",
					manifest.HookUserPromptSubmit: "blocked the prompt",
					manifest.HookStop:             "requested continuation",
				}[event]
				recovery := "Review this hook's policy before retrying."
				if event == manifest.HookStop {
					recovery = "Review this hook's policy before continuing."
				}
				for _, reason := range []string{"R", "", " \t\n "} {
					kinds := []string{"legacy", "exit-two"}
					if event != manifest.HookStop {
						kinds = append(kinds, "continue-false")
					}
					if event == manifest.HookPreToolUse {
						kinds = append(kinds, "permission", "ask")
					}
					for _, kind := range kinds {
						for _, missing := range []bool{false, true} {
							t.Run(fmt.Sprintf("%s/reason=%q/missing=%v", kind, reason, missing), func(t *testing.T) {
								v := map[string]any{"decision": "block", "systemMessage": "old message"}
								if kind == "continue-false" {
									v = map[string]any{"continue": false, "systemMessage": "old message"}
								}
								field, target := "reason", v
								if kind == "permission" || kind == "ask" {
									decision := "deny"
									if kind == "ask" {
										decision = "ask"
									}
									target = map[string]any{"permissionDecision": decision, "updatedInput": map[string]any{"command": "must disappear"}}
									v = map[string]any{"hookSpecificOutput": target, "systemMessage": "old message"}
									field = "permissionDecisionReason"
								}
								if !missing {
									target[field] = reason
								}
								reply, err := json.Marshal(v)
								if err != nil {
									t.Fatal(err)
								}
								body := "#!/bin/sh\nprintf '%s' " + adapter.ShellQuote(string(reply)) + "\n"
								detail := reason
								if missing {
									detail = ""
								}
								if kind == "exit-two" {
									body = "#!/bin/sh\nprintf '%s' " + adapter.ShellQuote(detail) + " >&2\nexit 2\n"
								}
								wantRecovery := recovery
								if kind == "ask" {
									agent := "Codex"
									if id == "cursor" {
										agent = "Cursor"
									}
									wantRecovery = agent + " cannot ask for confirmation here. Ask the user in the conversation and retry if they approve."
								}
								want := "ACR hook " + path + " (" + string(event) + "): " + action + " without providing a reason. " + wantRecovery
								if strings.TrimSpace(detail) != "" {
									want = "ACR hook " + path + " (" + string(event) + "): " + action + ": " + strings.TrimSpace(detail) + "\n" + wantRecovery
								}
								mustHookWrite(t, hook, body, 0o755)
								out, stderr, code := runFailureCommand(t, command, cwd, "{}", os.Getenv("PATH"))
								got := nativeReply(t, id, event, out, stderr, code)
								if kind == "exit-two" && id == "codex" {
									if code != 2 || out != "" || stderr != want+"\n" {
										t.Fatalf("exit-2 bytes: %d %q %q; want %q", code, out, stderr, want+"\n")
									}
									return
								}
								if code != 0 {
									t.Fatalf("exit = %d: %s", code, stderr)
								}
								if id == "codex" {
									if event == manifest.HookPreToolUse {
										specific := got["hookSpecificOutput"].(map[string]any)
										if specific["permissionDecision"] != "deny" || specific["permissionDecisionReason"] != want || specific["updatedInput"] != nil {
											t.Fatalf("deny: %s; want %q", out, want)
										}
									} else if got["decision"] != "block" || got["reason"] != want {
										t.Fatalf("block: %s; want %q", out, want)
									}
									if got["systemMessage"] != want {
										t.Fatalf("systemMessage: %s; want %q", out, want)
									}
								} else {
									field := "user_message"
									if event == manifest.HookStop {
										field = "followup_message"
									}
									if got[field] != want {
										t.Fatalf("message: %s; want %q", out, want)
									}
									if event == manifest.HookPreToolUse && (got["permission"] != "deny" || got["agent_message"] != want || got["updated_input"] != nil) || event == manifest.HookUserPromptSubmit && got["continue"] != false {
										t.Fatalf("gate: %s", out)
									}
								}
							})
						}
					}
				}

				if event == manifest.HookStop {
					for _, continuation := range []string{`"decision":"block"`, `"decision":"block","reason":"do not repeat"`, `"hookSpecificOutput":{"additionalContext":"do not repeat"}`} {
						for _, input := range []string{`{}`, `{"stop_hook_active":true,"loop_count":1}`} {
							mustHookWrite(t, hook, "#!/bin/sh\nprintf '%s' "+adapter.ShellQuote(`{"continue":false,"stopReason":"done",`+continuation+`}`)+"\n", 0o755)
							out, stderr, code := runFailureCommand(t, command, cwd, input, os.Getenv("PATH"))
							got := nativeReply(t, id, event, out, stderr, code)
							if id == "codex" && (got["continue"] != false || got["stopReason"] != "ACR hook .codex/hooks/acr__example__hooks__hook/hook.sh (stop): stopped the response: done\nReview this hook's policy before continuing.") || id == "cursor" && got["followup_message"] != nil {
								t.Fatalf("explicit termination lost: %d %s %s", code, out, stderr)
							}
						}
					}
				}
				if event == manifest.HookPreToolUse {
					for _, reply := range []string{
						`{"hookSpecificOutput":{"permissionDecision":"allow"}}`,
						`{"hookSpecificOutput":{"permissionDecision":"allow","permissionDecisionReason":"allowed"}}`,
						`{"hookSpecificOutput":{"permissionDecision":"defer","permissionDecisionReason":"deferred"}}`,
						`{"decision":"approve","reason":"allowed"}`,
					} {
						mustHookWrite(t, hook, "#!/bin/sh\nprintf '%s' "+adapter.ShellQuote(reply)+"\n", 0o755)
						out, stderr, code := runFailureCommand(t, command, cwd, "{}", os.Getenv("PATH"))
						got := nativeReply(t, id, event, out, stderr, code)
						if id == "codex" {
							if specific, ok := got["hookSpecificOutput"].(map[string]any); ok && (specific["permissionDecision"] != nil || specific["permissionDecisionReason"] != nil) || got["decision"] != nil || got["reason"] != nil {
								t.Fatalf("bare/legacy allow: %s", out)
							}
						} else if got["permission"] != "allow" {
							t.Fatalf("allow: %s", out)
						}
					}
				}
			})
		}
	}
}

func TestRealizedHookDetailBoundary(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
		t.Run(native.Descriptor().ID, func(t *testing.T) {
			id := native.Descriptor().ID
			root, command, hook := realizeFailureHook(t, native, manifest.HookPreToolUse)
			for _, tc := range []struct {
				name   string
				reason string
				want   string
			}{
				{"hostile-controls", "before\x1b[2Jafter\rFORGED\x07\x00\x08\x0b\x0c\x7f\u0080\u0085\u009f\u202e", `before\u001B[2Jafter\u000DFORGED\u0007\u0000\u0008\u000B\u000C\u007F\u0080\u0085\u009F\u202E`},
				{"one-MiB", strings.Repeat("x", 1<<20), strings.Repeat("x", 16384) + "\n[hook detail truncated]"},
				{"exact-budget", strings.Repeat("x", 16384), strings.Repeat("x", 16384)},
				{"two-byte-overflow", strings.Repeat("x", 16383) + "éTAIL", strings.Repeat("x", 16383) + "\n[hook detail truncated]"},
				{"three-byte-overflow", strings.Repeat("x", 16383) + "界TAIL", strings.Repeat("x", 16383) + "\n[hook detail truncated]"},
				{"four-byte-overflow", strings.Repeat("x", 16382) + "😀TAIL", strings.Repeat("x", 16382) + "\n[hook detail truncated]"},
				{"four-byte-fit", strings.Repeat("x", 16380) + "😀TAIL", strings.Repeat("x", 16380) + "😀\n[hook detail truncated]"},
				{"escape-overflow", strings.Repeat("x", 16379) + "\x1b", strings.Repeat("x", 16379) + "\n[hook detail truncated]"},
				{"multiline", "Policy failed:\n\tRun tests.\nThen retry.", "Policy failed:\n\tRun tests.\nThen retry."},
			} {
				t.Run(tc.name, func(t *testing.T) {
					reply, err := json.Marshal(map[string]any{"decision": "block", "reason": tc.reason, "systemMessage": "old message"})
					if err != nil {
						t.Fatal(err)
					}
					mustHookWrite(t, hook, "#!/bin/sh\nprintf '%s' "+adapter.ShellQuote(string(reply))+"\n", 0o755)
					out, stderr, code := runFailureCommand(t, command, root, "{}", os.Getenv("PATH"))
					wantStderr := ""
					if id == "cursor" {
						wantStderr = "ACR hook .cursor/hooks/acr__example__hooks__hook/hook.sh: dropped unsupported fields\n"
					}
					if code != 0 || stderr != wantStderr {
						t.Fatalf("denial exit=%d, stderr=%.200q", code, stderr)
					}
					got := nativeReply(t, id, manifest.HookPreToolUse, out, stderr, code)
					want := "ACR hook ." + id + "/hooks/acr__example__hooks__hook/hook.sh (pre-tool-use): denied the tool call: " + tc.want + "\nReview this hook's policy before retrying."
					if id == "codex" {
						specific := got["hookSpecificOutput"].(map[string]any)
						if specific["permissionDecision"] != "deny" || specific["permissionDecisionReason"] != want || got["systemMessage"] != want {
							t.Fatalf("Codex denial differs from exact message (%d bytes); output %.200q", len(want), out)
						}
					} else if got["permission"] != "deny" || got["user_message"] != want || got["agent_message"] != want {
						t.Fatalf("Cursor denial differs from exact message (%d bytes); output %.200q", len(want), out)
					}
				})
			}
		})
	}
	for _, event := range []manifest.HookEvent{manifest.HookPreToolUse, manifest.HookUserPromptSubmit, manifest.HookStop} {
		t.Run("codex/exit-two/"+string(event), func(t *testing.T) {
			root, command, hook := realizeFailureHook(t, codex.New(), event)
			for _, newline := range []string{"", "\n"} {
				body := "#!/bin/sh\nprintf '\\377{bad'\nprintf 'before\\033[2Jafter\\rFORGED\\007\\000\\377" + newline + "' >&2\nexit 2\n"
				mustHookWrite(t, hook, body, 0o755)
				out, stderr, code := runFailureCommand(t, command, root, "{}", os.Getenv("PATH"))
				action := map[manifest.HookEvent]string{
					manifest.HookPreToolUse:       "denied the tool call",
					manifest.HookUserPromptSubmit: "blocked the prompt",
					manifest.HookStop:             "requested continuation",
				}[event]
				recovery := "Review this hook's policy before retrying."
				if event == manifest.HookStop {
					recovery = "Review this hook's policy before continuing."
				}
				want := "ACR hook .codex/hooks/acr__example__hooks__hook/hook.sh (" + string(event) + "): " + action + ": before\\u001B[2Jafter\\u000DFORGED\\u0007\\u0000�\n" + recovery + "\n"
				if code != 2 || out != "\xff{bad" || stderr != want {
					t.Fatalf("exit-2 byte preservation: exit=%d stdout=%.200q stderr=%.400q; want %.400q", code, out, stderr, want)
				}
			}
		})
	}
}

func TestRealizedHookPlainContext(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
		for _, event := range []manifest.HookEvent{manifest.HookSessionStart, manifest.HookUserPromptSubmit} {
			t.Run(native.Descriptor().ID+"/"+string(event), func(t *testing.T) {
				root, command, hook := realizeFailureHook(t, native, event)
				for _, text := range []string{"[acr] stale index", " \t[acr] stale index\n", `["context"]`, "notice\x1b[2J\r\x07\x7f\u0085", strings.Repeat("x", 1<<20)} {
					mustHookWrite(t, hook, "#!/bin/sh\nprintf '%s' "+adapter.ShellQuote(text)+"\n", 0o755)
					out, stderr, code := runFailureCommand(t, command, root, "{}", os.Getenv("PATH"))
					if code != 0 {
						t.Fatalf("plain context rejected: %d %s %s", code, out, stderr)
					}
					if native.Descriptor().ID == "codex" {
						if out != text || stderr != "" {
							t.Fatalf("plain context changed: %.200q %.200q", out, stderr)
						}
					} else {
						var got map[string]any
						if err := json.Unmarshal([]byte(out), &got); err != nil {
							t.Fatal(err)
						}
						if event == manifest.HookSessionStart {
							if got["additional_context"] != strings.TrimSpace(text) || stderr != "" {
								t.Fatalf("session context lost: %.200q %.200q", out, stderr)
							}
						} else if got["continue"] != true || !strings.Contains(stderr, fmt.Sprintf("cannot add model context; dropped %d characters", len(strings.TrimSpace(text)))) {
							t.Fatalf("prompt context handling: %s %s", out, stderr)
						}
					}
				}
			})
		}
	}
}

func TestRealizedHookInvalidOutput(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
		for _, event := range adapter.SortedEvents() {
			t.Run(native.Descriptor().ID+"/"+string(event), func(t *testing.T) {
				id := native.Descriptor().ID
				root, command, hook := realizeFailureHook(t, native, event)
				replies := []string{
					`{broken`, `  {broken`, `{"systemMessage":42}`, `{"continue":"false"}`, `{"reason":{}}`,
					`{"hookSpecificOutput":{"additionalContext":[]}}`,
				}
				if id == "cursor" && event == manifest.HookPreToolUse {
					replies = append(replies, `{"hookSpecificOutput":{"permissionDecision":"allow","updatedInput":[]}}`)
				}
				if id == "cursor" && event == manifest.HookPostToolUse {
					replies = append(replies, `{"hookSpecificOutput":{"updatedMCPToolOutput":"invalid object"}}`)
				}
				if event == manifest.HookPreToolUse {
					replies = append(replies, `{"hookSpecificOutput":{"permissionDecision":"allow","updatedInput":{"value":NaN}}}`)
				}
				for _, reply := range replies {
					mustHookWrite(t, hook, "#!/bin/sh\nprintf '%s' "+adapter.ShellQuote(reply)+"\n", 0o755)
					out, stderr, code := runFailureCommand(t, command, root, "{}", os.Getenv("PATH"))
					if !strings.Contains(stderr, "ACR hook") {
						t.Fatalf("missing invalid-reply diagnostic: %d %s %s", code, out, stderr)
					}
					gating := event == manifest.HookPreToolUse || event == manifest.HookUserPromptSubmit
					if gating || event == manifest.HookStop {
						got := nativeReply(t, id, event, out, stderr, code)
						if event == manifest.HookStop {
							if got["decision"] == "block" || got["followup_message"] != nil {
								t.Fatalf("invalid output trapped Stop: %s", out)
							}
						} else if id == "codex" && code != 2 || id == "cursor" && event == manifest.HookPreToolUse && got["permission"] != "deny" || id == "cursor" && event == manifest.HookUserPromptSubmit && got["continue"] != false {
							t.Fatalf("invalid reply failed open: %d %s %s", code, out, stderr)
						}
					} else if code != 0 || out != "" && id != "codex" && !json.Valid([]byte(out)) {
						t.Fatalf("native failure reply invalid: %d %s %s", code, out, stderr)
					}
				}
			})
		}
	}
}

func TestRealizedCodexOrphanReason(t *testing.T) {
	for _, event := range adapter.SortedEvents() {
		t.Run(string(event), func(t *testing.T) {
			root, command, hook := realizeFailureHook(t, codex.New(), event)
			cwd := filepath.Join(root, "nested", "session")
			mustHookWrite(t, filepath.Join(cwd, "marker"), "", 0o644)
			for _, reason := range []string{"note", " \t "} {
				for _, context := range []string{"", "important context"} {
					reply := map[string]any{"reason": reason}
					if context != "" {
						reply["hookSpecificOutput"] = map[string]any{"additionalContext": context}
					}
					encoded, err := json.Marshal(reply)
					if err != nil {
						t.Fatal(err)
					}
					mustHookWrite(t, hook, "#!/bin/sh\nprintf '%s' "+adapter.ShellQuote(string(encoded))+"\n", 0o755)
					out, stderr, code := runFailureCommand(t, command, cwd, "{}", os.Getenv("PATH"))
					if code != 0 {
						t.Fatalf("orphan reason failed: %d %s %s", code, out, stderr)
					}
					if event == manifest.HookSessionEnd {
						if out != "" {
							t.Fatalf("session end emitted unsupported JSON: %s", out)
						}
						continue
					}
					var got map[string]any
					if err := json.Unmarshal([]byte(out), &got); err != nil {
						t.Fatal(err)
					}
					if got["reason"] != nil && got["decision"] != "block" && got["continue"] != false {
						t.Fatalf("native parser rejects orphan reason: %s", out)
					}
					if context != "" && event != manifest.HookStop {
						specific, _ := got["hookSpecificOutput"].(map[string]any)
						if specific["additionalContext"] != context {
							t.Fatalf("additionalContext lost: %s", out)
						}
					}
				}
			}
		})
	}
}

func TestRealizedCodexPostToolContinueReason(t *testing.T) {
	root, command, hook := realizeFailureHook(t, codex.New(), manifest.HookPostToolUse)
	for _, continuation := range []string{"", `"continue":true,`, `"continue":false,`} {
		for _, context := range []string{"", "important context"} {
			reply := `{` + continuation + `"reason":"post-tool hook says stop"`
			if context != "" {
				reply += `,"hookSpecificOutput":{"additionalContext":"` + context + `"}`
			}
			reply += `}`
			mustHookWrite(t, hook, "#!/bin/sh\nprintf '%s' "+adapter.ShellQuote(reply)+"\n", 0o755)
			out, stderr, code := runFailureCommand(t, command, root, "{}", os.Getenv("PATH"))
			got := nativeReply(t, "codex", manifest.HookPostToolUse, out, stderr, code)
			wantMessage := "ACR hook .codex/hooks/acr__example__hooks__hook/hook.sh (post-tool-use): provided feedback after the tool call: post-tool hook says stop"
			if context != "" {
				wantMessage += "\n\n" + context
			}
			wantMessage += "\nReview this hook's policy before continuing."
			if continuation == `"continue":false,` {
				if got["continue"] != false || got["reason"] != wantMessage {
					t.Fatalf("continue:false lost its reason: %s", out)
				}
			} else if got["reason"] != nil {
				t.Fatalf("native parser rejects orphan reason: %s", out)
			}
			if context != "" {
				specific, _ := got["hookSpecificOutput"].(map[string]any)
				wantContext := context
				if continuation == `"continue":false,` {
					wantContext = wantMessage
				}
				if specific["additionalContext"] != wantContext {
					t.Fatalf("additional context lost: %s", out)
				}
			}
		}
	}
}

func TestRealizedHookJobControl(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
		t.Run(native.Descriptor().ID, func(t *testing.T) {
			root, command, hook := realizeFailureHook(t, native, manifest.HookPreToolUse)
			mustHookWrite(t, hook, "#!/bin/sh\nprintf ran >\"$CLAUDE_PROJECT_DIR/ran\"\nprintf '{}'\n", 0o755)
			out, stderr, code := runFailureCommand(t, "/bin/bash -m -c "+adapter.ShellQuote(command), root, "{}", os.Getenv("PATH"))
			got := nativeReply(t, native.Descriptor().ID, manifest.HookPreToolUse, out, stderr, code)
			if code != 0 || got["permission"] == "deny" {
				t.Fatalf("job control failed to start hook: %d %s %s", code, out, stderr)
			}
			if _, err := os.Stat(filepath.Join(root, "ran")); err != nil {
				t.Fatalf("job control skipped package hook: %v", err)
			}
		})
	}
}

func TestRealizedCodexJSONLimits(t *testing.T) {
	root, command, hook := realizeFailureHook(t, codex.New(), manifest.HookPreToolUse)
	for _, test := range []struct {
		name, value string
		deny        bool
	}{
		{"401-digit-integer", strings.Repeat("9", 401), true},
		{"integer-above-u64", "18446744073709551616", true},
		{"integer-below-i64", "-9223372036854775809", true},
		{"infinite-float", "1e400", true},
		{"negative-infinite-float", "-1e400", true},
		{"140-level-object", strings.Repeat(`{"v":`, 140) + "0" + strings.Repeat("}", 140), true},
		// The reply envelope has three containers; serde_json rejects entry 128.
		{"128th-container", strings.Repeat("[", 125) + "0" + strings.Repeat("]", 125), true},
		{"127-containers", strings.Repeat("[", 124) + "0" + strings.Repeat("]", 124), false},
		{"max-u64", "18446744073709551615", false},
		{"min-i64", "-9223372036854775808", false},
		{"finite-f64", "1.7976931348623157e308", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			reply := `{"hookSpecificOutput":{"permissionDecision":"allow","updatedInput":{"command":"echo safe-rewrite","value":` + test.value + `}}}`
			mustHookWrite(t, hook, "#!/bin/sh\nprintf '%s' "+adapter.ShellQuote(reply)+"\n", 0o755)
			out, stderr, code := runFailureCommand(t, command, root, "{}", os.Getenv("PATH"))
			got := nativeReply(t, "codex", manifest.HookPreToolUse, out, stderr, code)
			if test.deny {
				if code != 2 || out != "" || stderr != "ACR hook .codex/hooks/acr__example__hooks__hook/hook.sh (pre-tool-use): runtime failure: printed invalid JSON or an invalid hook response; Repair this hook and run acr realize before retrying.\n" {
					t.Fatalf("unrepresentable rewrite failed open: %d %s %s", code, out, stderr)
				}
			} else {
				specific, _ := got["hookSpecificOutput"].(map[string]any)
				updated, _ := specific["updatedInput"].(map[string]any)
				if code != 0 || specific["permissionDecision"] != "allow" || updated["command"] != "echo safe-rewrite" {
					t.Fatalf("valid rewrite lost: %d %s %s", code, out, stderr)
				}
			}
		})
	}
}

// Assert the fields consumed by the target parsers, independently of the Python
// filter. Codex output schemas: github.com/openai/codex/codex-rs/hooks/schema/generated;
// Cursor event contracts: https://cursor.com/docs/hooks#reference.
func nativeReply(t *testing.T, id string, event manifest.HookEvent, out, stderr string, code int) map[string]any {
	t.Helper()
	if id == "codex" && code == 2 {
		if strings.TrimSpace(stderr) == "" {
			t.Fatal("Codex exit-2 requires a non-empty trimmed stderr reason")
		}
		return nil
	}
	if code != 0 {
		t.Fatalf("native parser would fail: exit %d; %s %s", code, out, stderr)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(out), &value); err != nil {
		t.Fatalf("native JSON: %v; %s %s", err, out, stderr)
	}
	allowed := map[string]string{
		"codex/session-start":       "continue stopReason suppressOutput systemMessage hookSpecificOutput",
		"codex/pre-tool-use":        "systemMessage decision reason hookSpecificOutput",
		"codex/post-tool-use":       "continue stopReason systemMessage decision reason hookSpecificOutput",
		"codex/user-prompt-submit":  "continue stopReason suppressOutput systemMessage decision reason hookSpecificOutput",
		"codex/stop":                "continue stopReason suppressOutput systemMessage decision reason",
		"cursor/session-start":      "additional_context",
		"cursor/session-end":        "",
		"cursor/post-tool-use":      "additional_context updated_mcp_tool_output",
		"cursor/pre-tool-use":       "permission user_message agent_message updated_input",
		"cursor/user-prompt-submit": "continue user_message",
		"cursor/stop":               "followup_message",
	}[id+"/"+string(event)]
	for key := range value {
		if !strings.Contains(" "+allowed+" ", " "+key+" ") {
			t.Fatalf("unsupported %s %s output field %q: %s", id, event, key, out)
		}
	}
	if id == "codex" {
		if value["reason"] != nil && value["decision"] != "block" && value["continue"] != false {
			t.Fatalf("invalid Codex orphan reason: %s", out)
		}
		if value["decision"] == "block" {
			reason, _ := value["reason"].(string)
			if strings.TrimSpace(reason) == "" {
				t.Fatalf("invalid Codex block reason: %s", out)
			}
		}
		if specific, ok := value["hookSpecificOutput"].(map[string]any); ok {
			if specific["permissionDecision"] == "deny" {
				reason, _ := specific["permissionDecisionReason"].(string)
				if strings.TrimSpace(reason) == "" {
					t.Fatalf("invalid Codex deny reason: %s", out)
				}
			}
			if specific["permissionDecision"] == "allow" && specific["updatedInput"] == nil {
				t.Fatalf("unsupported Codex bare allow: %s", out)
			}
		}
	}
	return value
}

func TestRealizedHookFeedbackMessages(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
		for _, event := range []manifest.HookEvent{manifest.HookPostToolUse, manifest.HookSessionStart, manifest.HookSessionEnd} {
			t.Run(native.Descriptor().ID+"/"+string(event), func(t *testing.T) {
				id := native.Descriptor().ID
				root, command, hook := realizeFailureHook(t, native, event)
				if event == manifest.HookSessionStart {
					for _, tc := range []struct {
						name    string
						context string
						want    string
					}{
						{"context-only", "index is stale", "index is stale"},
						{"context-block", "index is stale", "index is stale"},
						{"context-controls", "index\x1b[2J\r\u202e is stale", `index\u001B[2J\u000D\u202E is stale`},
						{"context-truncated", strings.Repeat("x", 1<<20), strings.Repeat("x", 16384) + "\n[hook detail truncated]"},
					} {
						t.Run(tc.name, func(t *testing.T) {
							specific := map[string]any{"additionalContext": tc.context}
							v := map[string]any{"continue": false, "hookSpecificOutput": specific}
							if tc.name == "context-block" {
								delete(v, "continue")
								v["decision"] = "block"
							}
							reply, err := json.Marshal(v)
							if err != nil {
								t.Fatal(err)
							}
							mustHookWrite(t, hook, "#!/bin/sh\nprintf '%s' "+adapter.ShellQuote(string(reply))+"\n", 0o755)
							out, stderr, code := runFailureCommand(t, command, root, "{}", os.Getenv("PATH"))
							if code != 0 {
								t.Fatalf("session feedback exit=%d stderr=%.200q", code, stderr)
							}
							got := nativeReply(t, id, event, out, stderr, code)
							message := "ACR hook ." + id + "/hooks/acr__example__hooks__hook/hook.sh (session-start): provided session feedback: " + tc.want + "\nReview this hook's policy before continuing."
							want := map[string]any{"additional_context": message}
							wantStderr := ""
							if id == "codex" {
								specific["additionalContext"] = message
								specific["hookEventName"] = "SessionStart"
								want = map[string]any{"continue": false, "systemMessage": message, "hookSpecificOutput": specific}
								if tc.name == "context-block" {
									delete(want, "continue")
									wantStderr = "ACR hook .codex/hooks/acr__example__hooks__hook/hook.sh: dropped unsupported fields\n"
								}
							} else if tc.name != "context-block" {
								wantStderr = "ACR hook .cursor/hooks/acr__example__hooks__hook/hook.sh: dropped unsupported fields\n"
							}
							if !reflect.DeepEqual(got, want) || stderr != wantStderr {
								t.Fatalf("session context differs from exact reply: stdout %.200q stderr %.200q", out, stderr)
							}
						})
					}
				}
				for _, detail := range []string{"R", "", " \t\n "} {
					for _, kind := range []string{"legacy", "missing-reason", "exit-two", "continue-false"} {
						t.Run(fmt.Sprintf("%s/%q", kind, detail), func(t *testing.T) {
							v := map[string]any{"decision": "block", "reason": detail}
							if kind == "missing-reason" {
								delete(v, "reason")
							}
							if kind == "continue-false" {
								delete(v, "decision")
								v["continue"] = false
							}
							encoded, err := json.Marshal(v)
							if err != nil {
								t.Fatal(err)
							}
							body := "#!/bin/sh\nprintf '%s' " + adapter.ShellQuote(string(encoded)) + "\n"
							if kind == "exit-two" {
								body = "#!/bin/sh\nprintf '%s' " + adapter.ShellQuote(detail) + " >&2\nexit 2\n"
							}
							action := "provided session feedback"
							if event == manifest.HookPostToolUse {
								action = "provided feedback after the tool call"
							}
							want := "ACR hook ." + id + "/hooks/acr__example__hooks__hook/hook.sh (" + string(event) + "): " + action
							if strings.TrimSpace(detail) != "" && kind != "missing-reason" {
								want += ": " + strings.TrimSpace(detail) + "\nReview this hook's policy before continuing."
							} else {
								want += " without providing a reason. Review this hook's policy before continuing."
							}
							mustHookWrite(t, hook, body, 0o755)
							out, stderr, code := runFailureCommand(t, command, root, "{}", os.Getenv("PATH"))
							if code != 0 || strings.Contains(stderr, "runtime failure") {
								t.Fatalf("feedback treated as failure: %d %s %s", code, out, stderr)
							}
							if id == "codex" && event == manifest.HookSessionEnd {
								if out != want {
									t.Fatalf("session feedback = %q; want %q", out, want)
								}
								return
							}
							got := nativeReply(t, id, event, out, stderr, code)
							if id == "codex" && event == manifest.HookPostToolUse {
								if got["hookSpecificOutput"].(map[string]any)["additionalContext"] != want {
									t.Fatalf("feedback: %s; want %q", out, want)
								}
								if kind == "continue-false" && got["reason"] != want {
									t.Fatalf("continued reason: %s; want %q", out, want)
								}
							} else if id == "codex" {
								if got["systemMessage"] != want {
									t.Fatalf("session feedback: %s; want %q", out, want)
								}
							} else if event == manifest.HookSessionEnd {
								if len(got) != 0 {
									t.Fatalf("unsupported session-end feedback: %s", out)
								}
							} else if got["additional_context"] != want {
								t.Fatalf("feedback: %s; want %q", out, want)
							}
						})
					}
				}
			})
		}
	}
}

func TestRealizedIntentionalFeedbackFields(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
		for _, event := range adapter.SortedEvents() {
			t.Run(native.Descriptor().ID+"/"+string(event), func(t *testing.T) {
				id := native.Descriptor().ID
				root, command, hook := realizeFailureHook(t, native, event)
				path := "." + id + "/hooks/acr__example__hooks__hook/hook.sh"
				note := "ACR hook " + path + ": dropped unsupported fields\n"
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
				for _, payload := range []struct{ name, raw, prepared string }{
					{"controls", "before\x1b[2Jafter\rFORGED\x07\x00\x08\x0b\x0c\x7f\u0080\u0085\u009f\u202e", `before\u001B[2Jafter\u000DFORGED\u0007\u0000\u0008\u000B\u000C\u007F\u0080\u0085\u009F\u202E`},
					{"one-MiB", strings.Repeat("x", 1<<20), strings.Repeat("x", 16384) + "\n[hook detail truncated]"},
					{"rendered-budget", strings.Repeat("x", 16379) + "\x1b", strings.Repeat("x", 16379) + "\n[hook detail truncated]"},
				} {
					kinds := []string{"legacy", "continue-false", "exit-two"}
					if event == manifest.HookPreToolUse {
						kinds = append(kinds, "deny", "ask")
					}
					if event == manifest.HookStop {
						kinds = append(kinds, "context-only", "termination")
					}
					for _, kind := range kinds {
						fields := []string{"reason", "systemMessage", "stopReason", "additionalContext"}
						if kind == "deny" || kind == "ask" {
							fields = append(fields, "permissionDecisionReason")
						}
						if kind == "exit-two" {
							fields = []string{"stderr"}
						} else if kind == "context-only" {
							fields = []string{"additionalContext"}
						} else if kind == "termination" {
							fields = []string{"stopReason"}
						}
						for _, field := range fields {
							t.Run(payload.name+"/"+kind+"/"+field, func(t *testing.T) {
								specific := map[string]any{}
								v := map[string]any{"decision": "block", "reason": "R", "systemMessage": "old", "stopReason": "old"}
								if kind == "continue-false" || kind == "termination" {
									delete(v, "decision")
									v["continue"] = false
								}
								if kind == "deny" || kind == "ask" {
									delete(v, "decision")
									delete(v, "reason")
									decision := "deny"
									if kind == "ask" {
										decision = "ask"
									}
									specific["permissionDecision"] = decision
									specific["permissionDecisionReason"] = "R"
								}
								if kind == "context-only" {
									v = map[string]any{}
								}
								if kind == "termination" {
									v = map[string]any{"continue": false}
								}
								if field == "additionalContext" || field == "permissionDecisionReason" {
									specific[field] = payload.raw
								} else if field != "stderr" {
									v[field] = payload.raw
								}
								if len(specific) > 0 {
									v["hookSpecificOutput"] = specific
								}
								detail := "R"
								if field == "reason" || field == "permissionDecisionReason" || field == "stderr" || kind == "context-only" {
									detail = payload.prepared
								} else if field == "additionalContext" && event != manifest.HookSessionEnd {
									detail = "R\n\n" + payload.prepared
									// R and its separator consume three rendered bytes first.
									if payload.name == "one-MiB" {
										detail = "R\n\n" + strings.Repeat("x", 16381) + "\n[hook detail truncated]"
									}
								}
								wantAction := action
								if event == manifest.HookStop && (kind == "continue-false" || kind == "termination") {
									wantAction = "stopped the response"
									detail = "old"
									if field == "stopReason" {
										detail = payload.prepared
									}
								}
								wantRecovery := recovery
								if kind == "ask" {
									wantRecovery = strings.ToUpper(id[:1]) + id[1:] + " cannot ask for confirmation here. Ask the user in the conversation and retry if they approve."
								}
								message := "ACR hook " + path + " (" + string(event) + "): " + wantAction + ": " + detail + "\n" + wantRecovery
								encoded, err := json.Marshal(v)
								if err != nil {
									t.Fatal(err)
								}
								// Files keep one-MiB data out of argv and shell command limits.
								mustHookWrite(t, filepath.Join(root, "reply"), string(encoded), 0o644)
								mustHookWrite(t, filepath.Join(root, "diagnostic"), payload.raw, 0o644)
								body := "#!/bin/sh\ncat \"$CLAUDE_PROJECT_DIR/reply\"\ncat \"$CLAUDE_PROJECT_DIR/diagnostic\" >&2\n"
								if kind == "exit-two" {
									body += "exit 2\n"
								}
								mustHookWrite(t, hook, body, 0o755)
								out, stderr, code := runFailureCommand(t, command, root, "{}", os.Getenv("PATH"))
								if kind == "exit-two" && id == "codex" && (event == manifest.HookPreToolUse || event == manifest.HookUserPromptSubmit || event == manifest.HookStop) {
									if code != 2 || out != string(encoded) || stderr != message+"\n" {
										t.Fatalf("exit-2: code=%d stdout=%.180q stderr=%.180q", code, out, stderr)
									}
									return
								}
								want := map[string]any{}
								wantStderr := message + "\n"
								if id == "codex" {
									if event == manifest.HookSessionEnd {
										if code != 0 || out != message || stderr != wantStderr {
											t.Fatalf("session end: %d %.180q %.180q", code, out, stderr)
										}
										return
									}
									if kind != "exit-two" && kind != "context-only" {
										want["systemMessage"], want["stopReason"] = message, message
										if kind == "continue-false" || kind == "termination" {
											want["continue"] = false
										}
										if kind == "legacy" || kind == "continue-false" {
											want["reason"] = message
										}
										if kind == "legacy" {
											want["decision"] = "block"
										}
									}
									if kind == "termination" {
										delete(want, "systemMessage")
									}
									if event == manifest.HookStop && len(specific) > 0 {
										wantStderr += note
									}
									wantSpecific := map[string]any{"hookEventName": string(map[manifest.HookEvent]string{
										manifest.HookSessionStart: "SessionStart", manifest.HookPreToolUse: "PreToolUse", manifest.HookUserPromptSubmit: "UserPromptSubmit", manifest.HookPostToolUse: "PostToolUse",
									}[event])}
									if field == "additionalContext" {
										wantSpecific["additionalContext"] = message
									}
									switch event {
									case manifest.HookSessionStart:
										want["systemMessage"] = message
										delete(want, "reason")
										delete(want, "decision")
										if kind == "legacy" || kind == "continue-false" {
											wantStderr += note
										}
									case manifest.HookPreToolUse:
										wantSpecific["permissionDecision"], wantSpecific["permissionDecisionReason"] = "deny", message
										delete(want, "reason")
										delete(want, "decision")
										delete(want, "stopReason")
										delete(want, "continue")
										wantStderr += note
									case manifest.HookUserPromptSubmit:
										want["decision"], want["reason"] = "block", message
									case manifest.HookPostToolUse:
										delete(want, "decision")
										if kind != "continue-false" {
											delete(want, "reason")
										}
										wantSpecific["additionalContext"] = message
									case manifest.HookStop:
										if kind != "continue-false" && kind != "termination" {
											want["decision"], want["reason"] = "block", message
										}
									}
									if event != manifest.HookStop && (len(specific) > 0 || event == manifest.HookPreToolUse || event == manifest.HookPostToolUse) {
										want["hookSpecificOutput"] = wantSpecific
									}
								} else {
									if kind != "exit-two" && kind != "context-only" {
										wantStderr += note
									}
									switch event {
									case manifest.HookSessionStart, manifest.HookPostToolUse:
										want["additional_context"] = message
									case manifest.HookPreToolUse:
										want = map[string]any{"permission": "deny", "user_message": message, "agent_message": message}
									case manifest.HookUserPromptSubmit:
										want = map[string]any{"continue": false, "user_message": message}
									case manifest.HookStop:
										if kind != "continue-false" && kind != "termination" {
											want["followup_message"] = message
										}
									}
									if field == "additionalContext" && (event == manifest.HookPreToolUse || event == manifest.HookUserPromptSubmit) {
										eventName := "preToolUse"
										if event == manifest.HookUserPromptSubmit {
											eventName = "beforeSubmitPrompt"
										}
										wantStderr += fmt.Sprintf("ACR hook %s: Cursor %s cannot add model context; dropped %d characters.\n", path, eventName, len([]rune(payload.raw)))
									}
								}
								got := nativeReply(t, id, event, out, stderr, code)
								if !reflect.DeepEqual(got, want) || stderr != wantStderr {
									t.Fatalf("exact feedback differs: code=%d stdout=%.180q stderr=%.180q; want field count=%v stderr=%.180q", code, out, stderr, len(want), wantStderr)
								}
							})
						}
					}
				}
			})
		}
	}
}

func TestRealizedSuccessfulFeedbackPassthrough(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
		for _, event := range []manifest.HookEvent{manifest.HookSessionStart, manifest.HookPreToolUse, manifest.HookUserPromptSubmit, manifest.HookPostToolUse, manifest.HookSessionEnd} {
			t.Run(native.Descriptor().ID+"/"+string(event), func(t *testing.T) {
				id := native.Descriptor().ID
				if id == "cursor" && (event == manifest.HookPreToolUse || event == manifest.HookUserPromptSubmit) {
					return
				} // Cursor has no successful context route for these events.
				root, command, hook := realizeFailureHook(t, native, event)
				for _, payload := range []struct{ name, text string }{
					{"controls", "notice\x1b[2J\r\x07\x7f\u0080\u0085\u009f\u202e"},
					{"one-MiB", strings.Repeat("x", 1<<20)},
				} {
					t.Run(payload.name, func(t *testing.T) {
						specific := map[string]any{"additionalContext": payload.text}
						v := map[string]any{"hookSpecificOutput": specific}
						want := map[string]any{"additional_context": payload.text}
						if id == "codex" {
							v["systemMessage"] = payload.text
							if event == manifest.HookPreToolUse {
								specific["permissionDecision"], specific["permissionDecisionReason"] = "allow", payload.text
								specific["updatedInput"] = map[string]any{"command": "echo permitted"}
							}
						}
						encoded, err := json.Marshal(v)
						if err != nil {
							t.Fatal(err)
						}
						mustHookWrite(t, filepath.Join(root, "reply"), string(encoded), 0o644)
						diagnostic := payload.text + "\xff\n"
						mustHookWrite(t, filepath.Join(root, "diagnostic"), diagnostic, 0o644)
						body := "#!/bin/sh\ncat \"$CLAUDE_PROJECT_DIR/reply\"\ncat \"$CLAUDE_PROJECT_DIR/diagnostic\" >&2\n"
						if event == manifest.HookSessionEnd {
							mustHookWrite(t, filepath.Join(root, "reply"), payload.text, 0o644)
						}
						mustHookWrite(t, hook, body, 0o755)
						out, stderr, code := runFailureCommand(t, command, root, "{}", os.Getenv("PATH"))
						if code != 0 || stderr != diagnostic {
							t.Fatalf("successful stderr changed: exit=%d stderr=%.180q", code, stderr)
						}
						if id == "codex" && event == manifest.HookSessionEnd {
							if out != payload.text {
								t.Fatalf("successful advisory changed: %.180q", out)
							}
							return
						}
						if id == "codex" {
							specific["hookEventName"] = map[manifest.HookEvent]string{
								manifest.HookSessionStart: "SessionStart", manifest.HookPreToolUse: "PreToolUse", manifest.HookUserPromptSubmit: "UserPromptSubmit", manifest.HookPostToolUse: "PostToolUse",
							}[event]
							want = v
						} else if event == manifest.HookSessionEnd {
							want = map[string]any{}
						}
						got := nativeReply(t, id, event, out, stderr, code)
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("successful payload changed: %.180q", out)
						}
					})
				}
			})
		}
	}
}

func TestRealizedHookFilteringAndFailureDiagnostics(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
		t.Run(native.Descriptor().ID, func(t *testing.T) {
			id := native.Descriptor().ID
			root, command, hook := realizeFailureHook(t, native, manifest.HookPreToolUse)
			path := "." + id + "/hooks/acr__example__hooks__hook/hook.sh"
			for _, key := range []string{"unknown\x1b[2J\rFORGED\x07\x7f\u0085", strings.Repeat("x", 1<<20)} {
				v := map[string]any{key: true, "hookSpecificOutput": map[string]any{"permissionDecision": "deny", "permissionDecisionReason": "R"}}
				encoded, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				mustHookWrite(t, filepath.Join(root, "reply"), string(encoded), 0o644)
				mustHookWrite(t, hook, "#!/bin/sh\ncat \"$CLAUDE_PROJECT_DIR/reply\"\n", 0o755)
				out, stderr, code := runFailureCommand(t, command, root, "{}", os.Getenv("PATH"))
				got := nativeReply(t, id, manifest.HookPreToolUse, out, stderr, code)
				message := "ACR hook " + path + " (pre-tool-use): denied the tool call: R\nReview this hook's policy before retrying."
				want := map[string]any{"permission": "deny", "user_message": message, "agent_message": message}
				if id == "codex" {
					want = map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": message}}
				}
				if !reflect.DeepEqual(got, want) || stderr != "ACR hook "+path+": dropped unsupported fields\n" {
					t.Fatalf("filtering diagnostic: %.180q %.180q", out, stderr)
				}
			}
			mustHookWrite(t, hook, "#!/bin/sh\nprintf 'ignored'\nprintf 'bad\\033[2J\\r\\377' >&2\nexit 3\n", 0o755)
			out, stderr, code := runFailureCommand(t, command, root, "{}", os.Getenv("PATH"))
			message := "ACR hook " + path + " (pre-tool-use): runtime failure: exited 3; stdout discarded; Repair this hook and run acr realize before retrying."
			got := nativeReply(t, id, manifest.HookPreToolUse, out, stderr, code)
			want := map[string]any{"permission": "deny", "user_message": message, "agent_message": message}
			if id == "codex" {
				if code != 2 || out != "" {
					t.Fatalf("failure gate: %d %.180q", code, out)
				}
			} else if !reflect.DeepEqual(got, want) {
				t.Fatalf("failure gate: %.180q", out)
			}
			if stderr != message+"\n" {
				t.Fatalf("failure leaked child stderr: %.180q", stderr)
			}
		})
	}
}
