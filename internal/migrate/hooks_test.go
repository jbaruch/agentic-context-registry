package migrate

import (
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/tesslplugin"
)

func TestHookCommandGrammar(t *testing.T) {
	t.Parallel()

	t.Run("formArgs", func(t *testing.T) {
		t.Parallel()
		parsed, err := tesslplugin.ParseHookCommand("bash", []string{"${TESSL_PLUGIN_DIR}/hooks/session-start.sh"})
		if err != nil || parsed.Path != "hooks/session-start.sh" {
			t.Fatalf("form 1 = %+v", parsed)
		}
	})

	t.Run("formString", func(t *testing.T) {
		t.Parallel()
		parsed, err := tesslplugin.ParseHookCommand(`bash "${TESSL_PLUGIN_DIR}/hooks/stop.sh"`, nil)
		if err != nil || parsed.Path != "hooks/stop.sh" {
			t.Fatalf("form 2 = %+v", parsed)
		}
		argsForm, err := tesslplugin.ParseHookCommand("bash", []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"})
		if err != nil {
			t.Fatal(err)
		}
		if !equalStrings(parsed.Argv, argsForm.Argv) {
			t.Fatalf("forms must normalize to the same argv: %v vs %v", parsed.Argv, argsForm.Argv)
		}
	})

	t.Run("outsideGrammar", func(t *testing.T) {
		t.Parallel()
		if _, err := tesslplugin.ParseHookCommand("python", []string{"hooks/session-start.py"}); err == nil {
			t.Fatal("command outside the closed grammar must be rejected")
		}
		if _, err := tesslplugin.ParseHookCommand("bash hooks/session-start.sh", nil); err == nil {
			t.Fatal("unquoted relative command must be rejected")
		}
	})

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	plugin := alphaPlugin(true, []string{"skills/review-change"}, "")
	plugin["nativeHooks"] = map[string]any{
		"claude-code": map[string]any{
			"Stop": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"},
			}}}},
		},
		"codex": map[string]any{
			"Stop": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": `bash "${TESSL_PLUGIN_DIR}/hooks/stop.sh"`,
			}}}},
		},
	}
	seedAlpha(t, root, plugin)

	hooks := normalizeTestHooks(t, root, "example/alpha")
	if len(hooks) != 2 {
		t.Fatalf("collapsed hooks = %#v, want session-start + stop", hooks)
	}
	var stop NormalizedHook
	for _, hook := range hooks {
		if hook.ID == "stop" {
			stop = hook
		}
		if hook.Unsupported || hook.Ambiguous {
			t.Fatalf("canonical grammar must be migratable: %+v", hook)
		}
	}
	if stop.Event != manifest.HookStop || stop.Digest == "" {
		t.Fatalf("stop hook = %+v", stop)
	}
}

func TestHookCommandDivergenceIsAmbiguous(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	plugin := alphaPlugin(true, []string{"skills/review-change"}, "")
	plugin["nativeHooks"] = map[string]any{
		"claude-code": map[string]any{
			"Stop": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"},
			}}}},
		},
		"codex": map[string]any{
			"Stop": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh", "--other"},
			}}}},
		},
	}
	seedAlpha(t, root, plugin)

	var stop NormalizedHook
	for _, hook := range normalizeTestHooks(t, root, "example/alpha") {
		if hook.ID == "stop" {
			stop = hook
		}
	}
	if !stop.Ambiguous || stop.Reason != reasonHookDivergence {
		t.Fatalf("per-agent command divergence = %+v", stop)
	}
}

func TestUnsupportedHookEvent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	plugin := alphaPlugin(false, []string{"skills/review-change"}, "")
	plugin["hooks"] = map[string]any{
		"OnSession": []any{map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/session-start.sh"},
		}}}},
	}
	seedAlpha(t, root, plugin)
	writeHookScript(t, root, "example/alpha", "session-start.sh", "#!/bin/sh\necho start\n")

	hooks := normalizeTestHooks(t, root, "example/alpha")
	if len(hooks) != 1 || !hooks[0].Unsupported || hooks[0].Reason != reasonBadEvent {
		t.Fatalf("event outside v1 = %#v", hooks)
	}
}

func normalizeTestHooks(t *testing.T, root, identity string) []NormalizedHook {
	t.Helper()
	install := installByIdentity(t, loadTestInstalls(t, root), identity)
	hooks, err := NormalizeHooks(openSnapshot(t, root), install)
	if err != nil {
		t.Fatal(err)
	}
	return hooks
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
