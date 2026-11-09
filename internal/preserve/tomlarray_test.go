package preserve

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

// A managed native hook, as the Codex adapter realizes one.
func managedTOMLHook() adapter.ConfigEntry {
	entry := testConfigEntry(adapter.ConfigTOML, []string{"hooks", "SessionStart"}, adapter.ConfigElement, "owner-key", `{ hooks = [{ type = "command", command = "echo ready" }] }`)
	entry.Representation = adapter.ConfigEntryTOMLHookTables
	return entry
}

func compileObservedTOML(content []byte) (adapter.SharedCompilation, error) {
	return NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target:  adapter.SharedTarget{Path: "config.toml", Observed: ptrObserved("config.toml", content)},
		Format:  adapter.ConfigTOML,
		Desired: []adapter.ConfigEntry{managedTOMLHook()},
	})
}

// Issue #167: valid foreign TOML that Tessl and Codex write into .codex/config.toml.
func TestTOMLPreservesValidForeignArrayTablesAndTablesAfterNativeHooks(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"repeated-array-of-tables":  "[[t.\"p/q\"]]\nx = 1\n\n[[t.\"p/q\"]]\nx = 2\n",
		"nested-arrays-per-element": "[[t.p]]\nx = 1\n[[t.p.h]]\ny = 1\n\n[[t.p]]\nx = 2\n[[t.p.h]]\ny = 2\n",
		"subtable-per-element":      "[[t]]\nx = 1\n[t.sub]\ny = 1\n\n[[t]]\nx = 2\n[t.sub]\ny = 2\n",
		"tables-after-leading-native-hook-group": "[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ntype = \"command\"\ncommand = \"x\"\n\n" +
			"[hooks.state.\"/p/.codex/config.toml:stop:0:0\"]\nenabled = true\n\n" +
			"[hooks.state.\"/p/.codex/config.toml:stop:1:0\"]\nenabled = true\n",
		"tessl-and-codex-trust-state-together": "[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ntype = \"command\"\ncommand = \"x\"\n\n" +
			"[hooks.state.\"a\"]\nenabled = true\ntrusted_hash = \"sha256:1\"\n\n[hooks.state.\"b\"]\nenabled = true\ntrusted_hash = \"sha256:2\"\n\n" +
			"[[tessl.native.\"jbaruch/coding-policy\"]]\nevent = \"Stop\"\n[[tessl.native.\"jbaruch/coding-policy\".hookEntry.hooks]]\ncommand = \"a\"\n\n" +
			"[[tessl.native.\"jbaruch/coding-policy\"]]\nevent = \"SessionEnd\"\n[[tessl.native.\"jbaruch/coding-policy\".hookEntry.hooks]]\ncommand = \"b\"\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			compiled, err := compileObservedTOML([]byte(content))
			if err != nil {
				t.Fatalf("valid foreign TOML refused: %v", err)
			}
			if got := compiled.Candidate.Content; !bytes.Contains(got, []byte(content)) ||
				!bytes.Contains(got, []byte("command = \"echo ready\"")) {
				t.Fatalf("foreign TOML not preserved beside the managed hook:\n%s", got)
			}
		})
	}
}

func TestTOMLStillRefusesDuplicatesAroundArraysOfTables(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"key-twice-in-one-element":     "[[t]]\nx = 1\nx = 2\n",
		"plain-table-twice":            "[t]\nk = 1\n[t]\nj = 1\n",
		"table-then-array":             "[t]\nk = 1\n[[t]]\nk = 2\n",
		"array-then-table":             "[[t]]\nk = 1\n[t]\nk = 2\n",
		"key-shadows-array":            "[[t.p]]\nx = 1\n[t]\np = 1\n",
		"array-shadows-key":            "[t]\np = 1\n[[t.p]]\nx = 1\n",
		"subtable-twice-in-an-element": "[[t]]\n[t.sub]\ny = 1\n[t.sub]\ny = 2\n",
		"table-twice-after-native-hook-group": "[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ntype = \"command\"\ncommand = \"x\"\n\n" +
			"[a]\nk = 1\n[a]\nj = 2\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := compileObservedTOML([]byte(content)); err == nil || !strings.Contains(err.Error(), CodeDuplicateConfigEntry) {
				t.Fatalf("duplicate TOML error = %v", err)
			}
		})
	}
}
