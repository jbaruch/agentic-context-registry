package tesslplugin

import (
	"errors"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestMapHooksCollapsesMatchingNativeBodies(t *testing.T) {
	t.Parallel()

	plugin := &PluginManifest{
		Hooks: map[string][]HookGroup{
			"SessionStart": {{Hooks: []HookCommand{{
				Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/check.sh", "--fast"},
			}}}},
		},
		NativeHooks: map[string]map[string][]HookGroup{
			"claude-code": {
				"Stop": {{Hooks: []HookCommand{{
					Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"},
				}}}},
			},
			"codex": {
				"Stop": {{Hooks: []HookCommand{{
					Type: "command", Command: `bash "${TESSL_PLUGIN_DIR}/hooks/stop.sh"`,
				}}}},
			},
		},
	}

	hooks, _, err := mapPluginHooks(plugin, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 2 {
		t.Fatalf("hooks = %#v", hooks)
	}
	if hooks[0].Event != manifest.HookSessionStart || hooks[0].Path != "hooks/check.sh" || !stringSlicesEqual(hooks[0].Args, []string{"--fast"}) {
		t.Fatalf("session-start = %#v", hooks[0])
	}
	if hooks[1].Event != manifest.HookStop || hooks[1].Path != "hooks/stop.sh" || hooks[1].Consensus {
		t.Fatalf("stop = %#v", hooks[1])
	}
}

func TestMatcherBlocks(t *testing.T) {
	t.Parallel()

	plugin := &PluginManifest{Hooks: map[string][]HookGroup{
		"Stop": {{Matcher: "Bash", Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}},
	}}
	_, _, err := mapPluginHooks(plugin, false)
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeUnmappedField || conv.Field != "matcher" {
		t.Fatalf("err = %v", err)
	}
}

func TestHookEventOutsideV1Blocks(t *testing.T) {
	t.Parallel()

	plugin := &PluginManifest{Hooks: map[string][]HookGroup{
		"SubagentStart": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}},
	}}
	_, _, err := mapPluginHooks(plugin, false)
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeUnmappedField {
		t.Fatalf("err = %v", err)
	}
}

func TestHookCommandOutsideGrammarBlocks(t *testing.T) {
	t.Parallel()

	plugin := &PluginManifest{Hooks: map[string][]HookGroup{
		"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "python hooks/stop.py"}}}},
	}}
	_, _, err := mapPluginHooks(plugin, false)
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeUnmappedField || !strings.Contains(err.Error(), "outside the closed Tessl grammar; use bash with ${TESSL_PLUGIN_DIR}/ or drop the hook") {
		t.Fatalf("err = %v", err)
	}
}

func TestNativeHookBodyDivergenceBlocks(t *testing.T) {
	t.Parallel()

	plugin := &PluginManifest{NativeHooks: map[string]map[string][]HookGroup{
		"claude-code": {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}}},
		"codex":       {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh", "--strict"}}}}}},
	}}
	_, _, err := mapPluginHooks(plugin, false)
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeUnmappedField {
		t.Fatalf("err = %v", err)
	}
}

func TestAgentWideningBlocksAndFlagAccepts(t *testing.T) {
	t.Parallel()

	plugin := &PluginManifest{NativeHooks: map[string]map[string][]HookGroup{
		"claude-code": {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}}},
		"codex":       {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: `bash "${TESSL_PLUGIN_DIR}/hooks/stop.sh"`}}}}},
	}}

	_, _, err := mapPluginHooks(plugin, false)
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeAgentWidening {
		t.Fatalf("err = %v", err)
	}

	hooks, notes, err := mapPluginHooks(plugin, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 1 || hooks[0].Path != "hooks/stop.sh" {
		t.Fatalf("hooks = %#v", hooks)
	}
	if len(notes) != 1 || notes[0].Reason != "accepted-agent-widening" {
		t.Fatalf("notes = %#v", notes)
	}
}

func TestConsensusHooksAreNotWidening(t *testing.T) {
	t.Parallel()

	plugin := &PluginManifest{
		Hooks: map[string][]HookGroup{
			"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}},
		},
		NativeHooks: map[string]map[string][]HookGroup{
			"claude-code": {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}}},
		},
	}
	hooks, notes, err := mapPluginHooks(plugin, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 1 || !hooks[0].Consensus || len(notes) != 0 {
		t.Fatalf("hooks = %#v notes = %#v", hooks, notes)
	}
}

func TestWidensAgentsByMembership(t *testing.T) {
	t.Parallel()

	three := []string{"claude-code", "codex", "cursor"}
	four := []string{"claude-code", "codex", "cursor", "windsurf"}
	cases := []struct {
		name     string
		got      []string
		universe []string
		want     bool
	}{
		{name: "three proper subset", got: []string{"claude-code", "codex"}, universe: three, want: true},
		{name: "three equal", got: []string{"claude-code", "codex", "cursor"}, universe: three, want: false},
		{name: "three superset", got: []string{"claude-code", "codex", "cursor", "windsurf"}, universe: three, want: false},
		{name: "three equal cardinality foreign", got: []string{"claude-code", "codex", "windsurf"}, universe: three, want: true},
		{name: "three all foreign", got: []string{"aider", "windsurf", "cline"}, universe: three, want: true},
		{name: "four proper subset", got: []string{"claude-code", "codex", "cursor"}, universe: four, want: true},
		{name: "four equal", got: []string{"claude-code", "codex", "cursor", "windsurf"}, universe: four, want: false},
		{name: "four superset", got: []string{"claude-code", "codex", "cursor", "windsurf", "aider"}, universe: four, want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := widensAgents(test.got, test.universe); got != test.want {
				t.Fatalf("widensAgents(%v, %v) = %v, want %v", test.got, test.universe, got, test.want)
			}
		})
	}
}

func TestAgentWideningEqualCardinalityBlocks(t *testing.T) {
	t.Parallel()

	plugin := &PluginManifest{NativeHooks: map[string]map[string][]HookGroup{
		"claude-code": {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}}},
		"codex":       {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}}},
		"windsurf":    {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}}},
	}}
	_, _, err := mapPluginHooks(plugin, false)
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeAgentWidening {
		t.Fatalf("err = %v", err)
	}
}

func TestAgentWideningAllForeignBlocks(t *testing.T) {
	t.Parallel()

	plugin := &PluginManifest{NativeHooks: map[string]map[string][]HookGroup{
		"aider":    {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}}},
		"windsurf": {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}}},
		"cline":    {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}}},
	}}
	_, _, err := mapPluginHooks(plugin, false)
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeAgentWidening {
		t.Fatalf("err = %v", err)
	}
}

func TestNativeHooksForEveryAdapterAreNotWidening(t *testing.T) {
	t.Parallel()

	plugin := &PluginManifest{NativeHooks: map[string]map[string][]HookGroup{
		"claude-code": {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}}},
		"codex":       {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}}},
		"cursor":      {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}}},
	}}
	hooks, notes, err := mapPluginHooks(plugin, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 1 || len(notes) != 0 {
		t.Fatalf("hooks = %#v notes = %#v", hooks, notes)
	}
}
