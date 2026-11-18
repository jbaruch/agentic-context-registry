package tesslplugin

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

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
			"cursor": {
				"Stop": {{Hooks: []HookCommand{{
					Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"},
				}}}},
			},
		},
	}

	hooks, err := mapPluginHooks(plugin)
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

func TestMapHooksCollectsIndependentFailures(t *testing.T) {
	plugin := &PluginManifest{NativeHooks: map[string]map[string][]HookGroup{
		"claude-code": {"Stop": {{Matcher: "Bash", Hooks: []HookCommand{
			{Type: "prompt"},
			{Type: "command", Command: "python hooks/unknown.py"},
			{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/one.sh"}},
			{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/two.sh"}},
		}}}},
	}}
	_, err := mapPluginHooks(plugin)
	findings := ManualMigrationErrors(err)
	if len(findings) != 5 || findings[0].Field != "matcher" || findings[1].Field != "type" || findings[2].Field != "command" || findings[3].Field != "hooks/one.sh" || findings[4].Field != "hooks/two.sh" {
		t.Fatalf("incomplete hook failures: %+v (%v)", findings, err)
	}
}

func TestVendorHooksPreserveFirstError(t *testing.T) {
	var value manifest.Manifest
	err := appendVendorHooks(&value, map[string][]vendorPluginGroup{
		"Stop": {{Hooks: []vendorPluginCommand{{Type: "prompt"}, {Type: "other"}}}},
	})
	if err == nil || err.Error() != `hook type "prompt" is not supported; only type "command" maps onto agent-plugin.yaml` || len(value.Artifacts.Hooks) != 0 {
		t.Fatalf("first vendor hook type: %v", err)
	}
	for _, tc := range []struct {
		events map[string][]vendorPluginGroup
		want   string
	}{
		{map[string][]vendorPluginGroup{"BeforeAll": nil, "Stop": {{Hooks: []vendorPluginCommand{{Type: "prompt"}}}}}, `hook event "BeforeAll" is outside the v1 vocabulary; use a v1 Tessl event or drop the hook`},
		{map[string][]vendorPluginGroup{"Stop": {{Hooks: []vendorPluginCommand{{Command: "python hooks/unknown.py"}, {Type: "prompt"}}}}}, `hook command "python hooks/unknown.py" is outside the closed Tessl grammar; use bash with ${TESSL_PLUGIN_DIR}/ or drop the hook`},
	} {
		err := appendVendorHooks(&value, tc.events)
		if err == nil || err.Error() != tc.want {
			t.Fatalf("first vendor error=%v; want %s", err, tc.want)
		}
	}
	err = appendVendorNativeHooks(&value, map[string]map[string][]vendorPluginGroup{
		"claude-code": {"Stop": {{Hooks: []vendorPluginCommand{
			{Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/one.sh"}},
			{Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/two.sh"}},
		}}}},
	})
	const want = "native hook \"hooks/one.sh\" would widen across agents; move it into hooks or declare it identically for claude-code, codex, cursor"
	if err == nil || err.Error() != want || len(ManualMigrationErrors(err)) != 1 || len(value.Artifacts.Hooks) != 0 {
		t.Fatalf("first vendor widening error: %v", err)
	}
	_, err = SynthesizeVendorManifest(fstest.MapFS{
		".tessl-plugin/plugin.json": {Data: []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"prompt"}]}]},"nativeHooks":{"claude-code":{"Stop":[{"hooks":[{"command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/one.sh"]}]}]}}}`)},
	}, "example/manual", "1.2.3")
	if err == nil || err.Error() != `hook type "prompt" is not supported; only type "command" maps onto agent-plugin.yaml` {
		t.Fatalf("consensus error must precede native processing: %v", err)
	}
}

func TestVendorNativeHooksFindWideningAfterUnsupportedType(t *testing.T) {
	value, err := SynthesizeVendorManifest(fstest.MapFS{
		".tessl-plugin/plugin.json": {Data: []byte(`{"nativeHooks":{"claude-code":{"Stop":[{"hooks":[{"type":"prompt"},{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/one.sh"]}]}]}}}`)},
	}, "example/manual", "1.2.3")
	var conversion *Error
	const want = `native hook "hooks/one.sh" would widen across agents; move it into hooks or declare it identically for claude-code, codex, cursor`
	if !errors.As(err, &conversion) || conversion.Code != CodeAgentWidening || conversion.Field != "hooks/one.sh" || err.Error() != want {
		t.Fatalf("native hook widening after unsupported type: %v", err)
	}
	if len(ManualMigrationErrors(err)) != 1 || len(value.Artifacts.Hooks) != 0 {
		t.Fatalf("vendor refusal emitted hooks or multiple errors: %+v, %v", value, err)
	}
}

func TestMatcherBlocks(t *testing.T) {
	t.Parallel()

	plugin := &PluginManifest{Hooks: map[string][]HookGroup{
		"Stop": {{Matcher: "Bash", Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}},
	}}
	_, err := mapPluginHooks(plugin)
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
	_, err := mapPluginHooks(plugin)
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
	_, err := mapPluginHooks(plugin)
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
	_, err := mapPluginHooks(plugin)
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeUnmappedField {
		t.Fatalf("err = %v", err)
	}
}

func TestAgentWideningBlocks(t *testing.T) {
	t.Parallel()

	plugin := &PluginManifest{NativeHooks: map[string]map[string][]HookGroup{
		"claude-code": {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"}}}}}},
		"codex":       {"Stop": {{Hooks: []HookCommand{{Type: "command", Command: `bash "${TESSL_PLUGIN_DIR}/hooks/stop.sh"`}}}}},
	}}

	_, err := mapPluginHooks(plugin)
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeAgentWidening {
		t.Fatalf("err = %v", err)
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
	hooks, err := mapPluginHooks(plugin)
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 1 || !hooks[0].Consensus {
		t.Fatalf("hooks = %#v", hooks)
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
	_, err := mapPluginHooks(plugin)
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
	_, err := mapPluginHooks(plugin)
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
	hooks, err := mapPluginHooks(plugin)
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 1 {
		t.Fatalf("hooks = %#v", hooks)
	}
}
