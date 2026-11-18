package adapter

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// HookTimeoutSeconds makes the native deadline explicit, including Cursor's
// unspecified platform default. Codex documents 600 seconds, or 1 for SessionEnd
// with a maximum of 3; use 3 to leave time for the SessionEnd watchdog:
// https://learn.chatgpt.com/docs/hooks#configuration
func HookTimeoutSeconds(agent string, event manifest.HookEvent) int {
	if agent == "codex" && event == manifest.HookSessionEnd {
		return 3
	}
	return 600
}

//go:embed acr-hook.py
var hookTranslator []byte

// HookTranslator returns the co-shipped canonical-to-native command hook translator.
func HookTranslator() []byte { return append([]byte(nil), hookTranslator...) }

const HookTranslatorBasename = "acr-hook.py"

// HookTranslatorOwner identifies the adapter-owned runtime independently of packages.
var HookTranslatorOwner = OwnerRef{
	Source: "github:jbaruch/agentic-context-registry", ArtifactID: "hook-translator",
	SourcePath: "internal/adapter/acr-hook.py", Kind: ArtifactHook,
}

// HookFailureCommand protects native gates even when Python cannot start.
func HookFailureCommand(agent string, event manifest.HookEvent, message string) string {
	command := "printf '%s\\n' " + ShellQuote(message) + " >&2\n"
	reply := map[string]any{}
	if event == manifest.HookPreToolUse || event == manifest.HookUserPromptSubmit {
		if agent == "codex" {
			return command + "exit 2"
		}
		reply["user_message"] = message
		if event == manifest.HookPreToolUse {
			reply["permission"] = "deny"
			reply["agent_message"] = message
		} else {
			reply["continue"] = false
		}
	} else if agent == "codex" && event != manifest.HookSessionEnd {
		reply["systemMessage"] = message
	} else if agent == "codex" {
		return command + "exit 0"
	}
	// Strings and booleans are always JSON encodable. Quote the serialized object
	// separately so diagnostics remain literal through both JSON and the shell.
	encoded, _ := json.Marshal(reply)
	return command + "printf '%s\\n' " + ShellQuote(string(encoded)) + "\nexit 0"
}

// TranslatedHookCommand handles missing interpreters and translator startup failures.
func TranslatedHookCommand(runtime, agent string, event manifest.HookEvent, hookPath string, args []string) string {
	missing := HookFailureCommand(agent, event, fmt.Sprintf("ACR hook %s (%s): could not run because Python >= 3.9 is unavailable; Install Python >= 3.9 on the agent PATH and retry.", hookPath, event))
	failed := HookFailureCommand(agent, event, fmt.Sprintf("ACR hook %s (%s): runtime failure: translator failed; Check Python >= 3.9 on the agent PATH and run acr realize before retrying.", hookPath, event))
	accepted := "0"
	if agent == "codex" {
		accepted = "0|2" // Preserve Codex's exit-2 status and ignored stdout.
	}

	return fmt.Sprintf("acr_hook_timeout=%d\n", HookTimeoutSeconds(agent, event)) +
		"command -v python3 >/dev/null || {\n" + missing + "\n}\n" +
		"[ -f " + runtime + " ] && [ -r " + runtime + " ] || {\n" + failed + "\n}\n" +
		// Buffer stdout so a failing interpreter cannot leak a partial native reply.
		// Caught traps protect both shells from translator group cleanup. Exec
		// resets caught dispositions, so package hooks still receive SIGTERM.
		"trap ':' TERM\nacr_hook_output=$(\ntrap ':' TERM\n" +
		ShellJoin("acr_hook_timeout=\"$acr_hook_timeout\" python3 -I "+runtime, append([]string{agent, string(event), hookPath}, args...)) + "\n" +
		"acr_hook_status=$?\n" +
		// A sentinel preserves trailing newlines through command substitution.
		"printf '.'\nexit \"$acr_hook_status\"\n)\nacr_hook_status=$?\n" +
		"case $acr_hook_status in\n" + accepted + ") trap '' PIPE; printf '%s' \"${acr_hook_output%.}\" 2>/dev/null || exit 141; exit \"$acr_hook_status\" ;;\n129|130|141|143) exit \"$acr_hook_status\" ;;\n*)\n" + failed + "\n;;\nesac"
}
