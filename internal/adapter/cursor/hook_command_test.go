package cursor_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestTranslatedHookCommands(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		event                                  manifest.HookEvent
		native, input, reply, want, diagnostic string
		code                                   int
	}{
		{"session", manifest.HookSessionStart, "sessionStart", `{"session_id":"s1","conversation_id":"s1","hook_event_name":"sessionStart","workspace_roots":["/p"]}`, `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"S"}}`, `{"additional_context":"S"}`, "", 0},
		{"deny", manifest.HookPreToolUse, "preToolUse", `{"tool_name":"Shell","tool_input":{"command":"ls"},"tool_use_id":"t1"}`, `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"R"}}`, `{"permission":"deny","user_message":"ACR hook .cursor/hooks/acr__example__all-agents__hook/hook 'quote' $literal.sh (pre-tool-use): denied the tool call: R\nReview this hook's policy before retrying.","agent_message":"ACR hook .cursor/hooks/acr__example__all-agents__hook/hook 'quote' $literal.sh (pre-tool-use): denied the tool call: R\nReview this hook's policy before retrying."}`, "", 0},
		{"exit two", manifest.HookPreToolUse, "preToolUse", `{}`, ``, `{"permission":"deny","user_message":"ACR hook .cursor/hooks/acr__example__all-agents__hook/hook 'quote' $literal.sh (pre-tool-use): denied the tool call: R\nReview this hook's policy before retrying.","agent_message":"ACR hook .cursor/hooks/acr__example__all-agents__hook/hook 'quote' $literal.sh (pre-tool-use): denied the tool call: R\nReview this hook's policy before retrying."}`, "R", 2},
		{"ask", manifest.HookPreToolUse, "preToolUse", `{}`, `{"hookSpecificOutput":{"permissionDecision":"ask","permissionDecisionReason":"R"}}`, `{"permission":"deny","user_message":"ACR hook .cursor/hooks/acr__example__all-agents__hook/hook 'quote' $literal.sh (pre-tool-use): denied the tool call: R\nCursor cannot ask for confirmation here. Ask the user in the conversation and retry if they approve.","agent_message":"ACR hook .cursor/hooks/acr__example__all-agents__hook/hook 'quote' $literal.sh (pre-tool-use): denied the tool call: R\nCursor cannot ask for confirmation here. Ask the user in the conversation and retry if they approve."}`, "", 0},
		{"empty", manifest.HookPreToolUse, "preToolUse", `{}`, ``, `{"permission":"allow"}`, "", 0},
		{"allow", manifest.HookPreToolUse, "preToolUse", `{}`, `{"hookSpecificOutput":{"permissionDecision":"allow"}}`, `{"permission":"allow"}`, "", 0},
		{"rewrite", manifest.HookPreToolUse, "preToolUse", `{}`, `{"hookSpecificOutput":{"updatedInput":{"command":"ls"}}}`, `{"permission":"allow","updated_input":{"command":"ls"}}`, "", 0},
		{"post", manifest.HookPostToolUse, "postToolUse", `{"tool_output":"{\"exitCode\":0}"}`, `{"decision":"block","reason":"R","hookSpecificOutput":{"additionalContext":"C"}}`, `{"additional_context":"ACR hook .cursor/hooks/acr__example__all-agents__hook/hook 'quote' $literal.sh (post-tool-use): provided feedback after the tool call: R\n\nC\nReview this hook's policy before continuing."}`, "", 0},
		{"prompt block", manifest.HookUserPromptSubmit, "beforeSubmitPrompt", `{}`, `{"decision":"block","reason":"R"}`, `{"continue":false,"user_message":"ACR hook .cursor/hooks/acr__example__all-agents__hook/hook 'quote' $literal.sh (user-prompt-submit): blocked the prompt: R\nReview this hook's policy before retrying."}`, "", 0},
		{"prompt context", manifest.HookUserPromptSubmit, "beforeSubmitPrompt", `{}`, `{"hookSpecificOutput":{"additionalContext":"C"}}`, `{"continue":true}`, "Cursor beforeSubmitPrompt cannot add model context; dropped 1 characters.", 0},
		{"stop", manifest.HookStop, "stop", `{"loop_count":1}`, `{"decision":"block","reason":"R"}`, `{"followup_message":"ACR hook .cursor/hooks/acr__example__all-agents__hook/hook 'quote' $literal.sh (stop): requested continuation: R\nReview this hook's policy before continuing."}`, "", 0},
		{"end", manifest.HookSessionEnd, "sessionEnd", `{}`, `{"anything":"ignored"}`, `{}`, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "project 'quoted' $literal")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			packageRoot := t.TempDir()
			hookPath := "hooks/hook 'quote' $literal.sh"
			args := []string{"", "two words", "one'quote", "$literal", "`literal`", "back\\slash", "semi;colon"}
			body := "#!/usr/bin/env bash\nset -euo pipefail\ncat >\"$CLAUDE_PROJECT_DIR/input\"\nprintf '%s' \"$CLAUDE_PROJECT_DIR\" >\"$CLAUDE_PROJECT_DIR/root\"\npwd -P >\"$CLAUDE_PROJECT_DIR/cwd\"\nprintf '%s\\n' \"$@\" >\"$CLAUDE_PROJECT_DIR/args\"\nprintf '%s' " + adapter.ShellQuote(tc.reply) + "\n"
			if tc.code == 2 {
				body += "printf R >&2\nexit 2\n"
			}
			writeFixtureFile(t, packageRoot, hookPath, []byte(body), 0o755)
			pkg := adapter.Package{Source: "github:example/all-agents", Root: os.DirFS(packageRoot), Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Hooks: []manifest.HookArtifact{{ID: "hook", Event: tc.event, Path: hookPath, Args: args}}}}}
			applyNative(t, cursor.New(), root, []adapter.Package{pkg})
			var document struct {
				Hooks map[string][]struct {
					Command string `json:"command"`
				} `json:"hooks"`
			}
			if err := json.Unmarshal(readProjectFile(t, root, cursorHooksPath), &document); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", document.Hooks[tc.native][0].Command)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR=/wrong")
			cmd.Stdin = strings.NewReader(tc.input)
			var out, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("command: %v; stderr %s", err, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.diagnostic) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.diagnostic)
			}
			if tc.want == "" {
				if out.Len() != 0 {
					t.Fatalf("stdout = %q", out.String())
				}
			} else {
				var actual, expected any
				if err := json.Unmarshal(out.Bytes(), &actual); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(tc.want), &expected); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(actual, expected) {
					t.Fatalf("stdout = %s, want %s", out.String(), tc.want)
				}
			}
			physical, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(readProjectFile(t, root, "root")); got != physical {
				t.Fatalf("root = %q, want %q", got, physical)
			}
			if got := strings.TrimSpace(string(readProjectFile(t, root, "cwd"))); got != physical {
				t.Fatalf("cwd = %q, want %q", got, physical)
			}
			if got := string(readProjectFile(t, root, "args")); got != strings.Join(args, "\n")+"\n" {
				t.Fatalf("argv = %q, want %q", got, args)
			}
			var input map[string]any
			if err := json.Unmarshal(readProjectFile(t, root, "input"), &input); err != nil {
				t.Fatal(err)
			}
			if input["hook_event_name"] != map[manifest.HookEvent]string{manifest.HookSessionStart: "SessionStart", manifest.HookPreToolUse: "PreToolUse", manifest.HookPostToolUse: "PostToolUse", manifest.HookStop: "Stop", manifest.HookSessionEnd: "SessionEnd", manifest.HookUserPromptSubmit: "UserPromptSubmit"}[tc.event] {
				t.Fatalf("event = %v", input["hook_event_name"])
			}
			switch tc.name {
			case "session":
				if input["session_id"] != "s1" || input["cwd"] != "/p" || input["source"] != "startup" {
					t.Fatalf("session input = %#v", input)
				}
			case "post":
				if !reflect.DeepEqual(input["tool_response"], map[string]any{"exitCode": float64(0)}) {
					t.Fatalf("tool response = %#v", input)
				}
			case "stop":
				if input["stop_hook_active"] != true {
					t.Fatalf("stop input = %#v", input)
				}
			case "deny":
				if input["tool_name"] != "Shell" || !reflect.DeepEqual(input["tool_input"], map[string]any{"command": "ls"}) || input["tool_use_id"] != "t1" {
					t.Fatalf("tool vocabulary changed: %#v", input)
				}
			}
		})
	}
}
