package preserve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestHostileMarkdownBytePreservation(t *testing.T) {
	t.Parallel()

	t.Run("CRLF-only", func(t *testing.T) {
		t.Parallel()
		original := []byte("# user\r\nkeep me\r\n")
		compiled := compileObservedMarkdown(t, "AGENTS.md", original, testMarkdownInsertion("rule-a", "managed\n"))
		if !bytes.HasPrefix(compiled.Candidate.Content, original) {
			t.Fatalf("CRLF prefix changed: %q", compiled.Candidate.Content)
		}
		if bytes.Contains(bytes.ReplaceAll(compiled.Candidate.Content, []byte("\r\n"), nil), []byte{'\n'}) {
			t.Fatalf("introduced bare LF: %q", compiled.Candidate.Content)
		}
		restored := removeMarkdown(t, "AGENTS.md", compiled)
		if !bytes.Equal(restored.Candidate.Content, original) {
			t.Fatalf("CRLF round-trip = %q, want %q", restored.Candidate.Content, original)
		}
	})

	t.Run("mixed-eol", func(t *testing.T) {
		t.Parallel()
		prefix := []byte("# user\n")
		suffix := []byte("tail\r\n")
		initial := compileMissingMarkdown(t, testMarkdownInsertion("rule-a", "v1\n"))
		content := append(append(append([]byte(nil), prefix...), initial.Candidate.Content...), suffix...)
		updated := updateMarkdown(t, "AGENTS.md", content, initial, testMarkdownInsertion("rule-a", "v2\n"))
		if !bytes.HasPrefix(updated.Candidate.Content, prefix) || !bytes.HasSuffix(updated.Candidate.Content, suffix) {
			t.Fatalf("mixed EOL regions changed: %q", updated.Candidate.Content)
		}
		if !bytes.Equal(updated.Proof.PreservedContent[0], prefix) || !bytes.Equal(updated.Proof.PreservedContent[len(updated.Proof.PreservedContent)-1], suffix) {
			t.Fatalf("proof = %q", updated.Proof.PreservedContent)
		}
	})

	t.Run("no-trailing-newline", func(t *testing.T) {
		t.Parallel()
		original := []byte("user text")
		compiled := compileObservedMarkdown(t, "notes.md", original, testMarkdownInsertion("rule-a", "managed\n"))
		if len(compiled.Proof.PreservedContent) != 1 || !bytes.Equal(compiled.Proof.PreservedContent[0], original) {
			t.Fatalf("unmanaged proof = %q, want the original missing-NL bytes", compiled.Proof.PreservedContent)
		}
		if !bytes.Contains(compiled.Candidate.Content, []byte("prefix=lf")) {
			t.Fatalf("owned separator was not recorded: %q", compiled.Candidate.Content)
		}
		restored := removeMarkdown(t, "notes.md", compiled)
		if !bytes.Equal(restored.Candidate.Content, original) {
			t.Fatalf("missing-NL restore = %q", restored.Candidate.Content)
		}
	})

	t.Run("utf8-bom", func(t *testing.T) {
		t.Parallel()
		bom := []byte{0xef, 0xbb, 0xbf}
		original := append(append([]byte(nil), bom...), []byte("# user 🎉\n")...)
		compiled := compileObservedMarkdown(t, "AGENTS.md", original, testMarkdownInsertion("rule-a", "managed\n"))
		if !bytes.HasPrefix(compiled.Candidate.Content, bom) {
			t.Fatalf("BOM was not first three bytes: %q", compiled.Candidate.Content)
		}
		if !bytes.HasPrefix(compiled.Candidate.Content, original) {
			t.Fatalf("BOM prefix changed: %q", compiled.Candidate.Content)
		}
		restored := removeMarkdown(t, "AGENTS.md", compiled)
		if !bytes.Equal(restored.Candidate.Content, original) {
			t.Fatalf("BOM round-trip = %q", restored.Candidate.Content)
		}
	})

	t.Run("block-position", func(t *testing.T) {
		t.Parallel()
		block := compileMissingMarkdown(t, testMarkdownInsertion("rule-a", "managed\n"))
		cases := map[string]struct{ prefix, suffix []byte }{
			"start":  {nil, []byte("user tail\n")},
			"middle": {[]byte("head ✨\n"), []byte("tail\n")},
			"end":    {[]byte("head\n"), nil},
		}
		for name, test := range cases {
			test := test
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				content := append(append(append([]byte(nil), test.prefix...), block.Candidate.Content...), test.suffix...)
				updated := updateMarkdown(t, "AGENTS.md", content, block, testMarkdownInsertion("rule-a", "managed-v2\n"))
				if len(test.prefix) > 0 && !bytes.HasPrefix(updated.Candidate.Content, test.prefix) {
					t.Fatalf("prefix changed: %q", updated.Candidate.Content)
				}
				if len(test.suffix) > 0 && !bytes.HasSuffix(updated.Candidate.Content, test.suffix) {
					t.Fatalf("suffix changed: %q", updated.Candidate.Content)
				}
				if bytes.Contains(updated.Candidate.Content, []byte("managed\n")) && !bytes.Contains(updated.Candidate.Content, []byte("managed-v2\n")) {
					t.Fatalf("block body not replaced: %q", updated.Candidate.Content)
				}
			})
		}
	})

	t.Run("two-package-blocks", func(t *testing.T) {
		t.Parallel()
		first := testMarkdownInsertion("rule-a", "alpha\n")
		second := testMarkdownInsertion("rule-b", "beta\n")
		if first.BlockID == second.BlockID {
			t.Fatal("package blocks share a BlockID")
		}
		user := []byte("# keep\n")
		compiled := compileObservedMarkdown(t, "AGENTS.md", user, second, first)
		if !bytes.HasPrefix(compiled.Candidate.Content, user) || !bytes.Contains(compiled.Candidate.Content, first.Body) || !bytes.Contains(compiled.Candidate.Content, second.Body) {
			t.Fatalf("two-package splice = %q", compiled.Candidate.Content)
		}
		if len(compiled.Managed) != 2 {
			t.Fatalf("managed = %#v", compiled.Managed)
		}
		dropped := updateMarkdown(t, "AGENTS.md", compiled.Candidate.Content, compiled, first)
		if bytes.Contains(dropped.Candidate.Content, second.Body) || !bytes.Contains(dropped.Candidate.Content, first.Body) || !bytes.HasPrefix(dropped.Candidate.Content, user) {
			t.Fatalf("one-package removal = %q", dropped.Candidate.Content)
		}
	})

	t.Run("edited-managed-block", func(t *testing.T) {
		t.Parallel()
		initial := compileMissingMarkdown(t, testMarkdownInsertion("rule-a", "managed\n"))
		edited := bytes.Replace(initial.Candidate.Content, []byte("managed"), []byte("tampered"), 1)
		_, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
			Target: adapter.SharedTarget{
				Path: "AGENTS.md", Observed: ptrObserved("AGENTS.md", edited),
				Previous: ptrTarget(markdownTarget("AGENTS.md", initial.Candidate.Content, realize.OwnershipGenerated, initial.Managed)),
				Force:    true,
			},
			Desired: []adapter.MarkdownInsertion{testMarkdownInsertion("rule-a", "managed\n")},
		})
		if err == nil || !strings.Contains(err.Error(), "was edited") {
			t.Fatalf("edited block error = %v", err)
		}
	})

	t.Run("copied-marker-text", func(t *testing.T) {
		t.Parallel()
		initial := compileMissingMarkdown(t, testMarkdownInsertion("rule-a", "managed\n"))
		userPrefix := []byte("# notes\n")
		exactCopy := append(append([]byte(nil), userPrefix...), initial.Candidate.Content...)
		exactCopy = append(exactCopy, initial.Candidate.Content...)
		_, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
			Target:  adapter.SharedTarget{Path: "AGENTS.md", Observed: ptrObserved("AGENTS.md", exactCopy)},
			Desired: []adapter.MarkdownInsertion{testMarkdownInsertion("rule-a", "managed\n")},
		})
		if err == nil || !strings.Contains(err.Error(), CodeMarkerConflict) {
			t.Fatalf("copied exact markers error = %v, want marker_conflict", err)
		}

		indented := append(append([]byte(nil), userPrefix...), []byte("  ")...)
		indented = append(indented, bytes.ReplaceAll(initial.Candidate.Content, []byte("\n"), []byte("\n  "))...)
		compiled := compileObservedMarkdown(t, "AGENTS.md", indented, testMarkdownInsertion("rule-b", "other\n"))
		if !bytes.HasPrefix(compiled.Candidate.Content, indented) {
			t.Fatalf("indented copied marker text was rewritten: %q", compiled.Candidate.Content)
		}

		ambiguous := []byte("user\n<!-- acr:begin copied by user -->\n")
		_, err = NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
			Target:  adapter.SharedTarget{Path: "AGENTS.md", Observed: ptrObserved("AGENTS.md", ambiguous), Force: true},
			Desired: []adapter.MarkdownInsertion{testMarkdownInsertion("rule-a", "managed\n")},
		})
		if err == nil || !strings.Contains(err.Error(), CodeMarkerConflict) {
			t.Fatalf("ambiguous marker error = %v", err)
		}
	})
}

func TestHostileIncludeGraph(t *testing.T) {
	t.Parallel()

	t.Run("nested-reuse-claude-to-agents", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeGraphFile(t, root, "CLAUDE.md", "\ufeff# Claude\r\n@AGENTS.md follow instructions\r\n")
		writeGraphFile(t, root, "AGENTS.md", "@.tessl/RULES.md follow rules\n")
		writeGraphFile(t, root, ".tessl/RULES.md", "# Rules\n")
		graph, err := DiscoverIncludeGraph(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := graph.ValidateSelected([]string{"CLAUDE.md"}); err != nil {
			t.Fatal(err)
		}
		if !graph.Reachable("CLAUDE.md", "AGENTS.md") || !graph.Reachable("CLAUDE.md", ".tessl/RULES.md") {
			t.Fatalf("reachable failed: %#v", graph)
		}
		if host, ok := graph.DeepestSharedHost([]string{"CLAUDE.md"}); !ok || host != ".tessl/RULES.md" {
			t.Fatalf("DeepestSharedHost() = %q, %t", host, ok)
		}
	})

	t.Run("duplicate-cycle-unresolved", func(t *testing.T) {
		t.Parallel()
		cases := map[string]struct {
			files map[string]string
			code  string
		}{
			"duplicate":  {map[string]string{"CLAUDE.md": "@AGENTS.md\n@AGENTS.md again\n", "AGENTS.md": "# agents\n"}, CodeDuplicateInclude},
			"cycle":      {map[string]string{"CLAUDE.md": "@AGENTS.md\n", "AGENTS.md": "@CLAUDE.md\n"}, CodeIncludeCycle},
			"self-cycle": {map[string]string{"CLAUDE.md": "@CLAUDE.md\n"}, CodeIncludeCycle},
			"unresolved": {map[string]string{"CLAUDE.md": "@missing.md\n"}, CodeUnresolvedInclude},
		}
		for name, test := range cases {
			test := test
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				for filename, content := range test.files {
					writeGraphFile(t, root, filename, content)
				}
				graph, err := DiscoverIncludeGraph(root)
				if err != nil {
					t.Fatal(err)
				}
				var graphErr *GraphError
				if err := graph.ValidateSelected([]string{"CLAUDE.md"}); !errors.As(err, &graphErr) {
					t.Fatalf("ValidateSelected() error = %v", err)
				}
				found := false
				for _, diagnostic := range graphErr.Diagnostics {
					if diagnostic.Code == test.code {
						found = true
					}
				}
				if !found {
					t.Fatalf("diagnostics = %#v, want %s", graphErr.Diagnostics, test.code)
				}
			})
		}
	})

	t.Run("symlink-not-followed", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeGraphFile(t, root, "real.md", "# real\n")
		if err := os.Symlink(filepath.Join(root, "real.md"), filepath.Join(root, "AGENTS.md")); err != nil {
			t.Fatal(err)
		}
		writeGraphFile(t, root, "CLAUDE.md", "@AGENTS.md\n")
		graph, err := DiscoverIncludeGraph(root)
		if err != nil {
			t.Fatal(err)
		}
		var graphErr *GraphError
		if err := graph.ValidateSelected([]string{"CLAUDE.md"}); !errors.As(err, &graphErr) {
			t.Fatalf("symlink include error = %v", err)
		}
		found := false
		for _, diagnostic := range graphErr.Diagnostics {
			if diagnostic.Code == CodeUnresolvedInclude {
				found = true
			}
		}
		if !found {
			t.Fatalf("symlink diagnostics = %#v", graphErr.Diagnostics)
		}
		for _, rootName := range graph.Roots {
			if rootName == "AGENTS.md" {
				t.Fatalf("symlinked AGENTS.md became a root: %#v", graph.Roots)
			}
		}
	})
}

func TestHostileJSONMatrix(t *testing.T) {
	t.Parallel()

	t.Run("key-order-and-unowned", func(t *testing.T) {
		t.Parallel()
		entry := testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigField, "managed", `{"v":1}`)
		initial := compileMissingConfig(t, adapter.ConfigJSON, entry)
		content := []byte("{\r\n  \"z\": {\"nested\": [1, 2]},\r\n  \"a\": true,\r\n  \"managed\": {\"v\":1},\r\n  \"tail\": null\r\n}\r\n")
		previous := configTarget("mcp", content, realize.OwnershipShared, initial.Managed)
		observed := observedFile("mcp", content)
		entry.EncodedValue = []byte(`{"v":2}`)
		compiled, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
			Target: adapter.SharedTarget{Path: "mcp", Observed: &observed, Previous: &previous},
			Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
		})
		if err != nil {
			t.Fatal(err)
		}
		got := compiled.Candidate.Content
		z := bytes.Index(got, []byte(`"z"`))
		a := bytes.Index(got, []byte(`"a"`))
		tail := bytes.Index(got, []byte(`"tail"`))
		if z < 0 || a < 0 || tail < 0 || z > a || a > tail {
			t.Fatalf("unowned key order changed: %s", got)
		}
		if !bytes.Contains(got, []byte(`{"nested": [1, 2]}`)) || !bytes.Contains(got, []byte(`true`)) || !bytes.Contains(got, []byte(`null`)) {
			t.Fatalf("unowned values changed: %s", got)
		}
		if !bytes.Contains(got, []byte("\r\n")) || !json.Valid(got) {
			t.Fatalf("CRLF JSON rewrite: %s", got)
		}
	})

	t.Run("duplicate-keys", func(t *testing.T) {
		t.Parallel()
		content := []byte(`{"a":1,"a":2}`)
		_, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
			Target:  adapter.SharedTarget{Path: "mcp", Observed: ptrObserved("mcp", content)},
			Format:  adapter.ConfigJSON,
			Desired: []adapter.ConfigEntry{testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigField, "managed", `true`)},
		})
		if err == nil || !strings.Contains(err.Error(), CodeDuplicateConfigEntry) {
			t.Fatalf("literal duplicate JSON error = %v", err)
		}
	})

	t.Run("edited-acr-entry", func(t *testing.T) {
		t.Parallel()
		entry := testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigField, "managed", `1`)
		initial := compileMissingConfig(t, adapter.ConfigJSON, entry)
		edited := []byte(`{"managed":9}` + "\n")
		_, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
			Target: adapter.SharedTarget{
				Path: "mcp", Observed: ptrObserved("mcp", edited),
				Previous: ptrTarget(configTarget("mcp", initial.Candidate.Content, realize.OwnershipShared, initial.Managed)),
				Force:    true,
			},
			Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
		})
		if err == nil || !strings.Contains(err.Error(), "was edited or removed") {
			t.Fatalf("edited JSON entry error = %v", err)
		}
	})

	t.Run("array-element-by-hash-after-reorder", func(t *testing.T) {
		t.Parallel()
		entry := testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigElement, "owned", `{"id":"acr"}`)
		initial := compileMissingConfig(t, adapter.ConfigJSON, entry)
		reordered := []byte(`[{"id":"user-b"},{"id":"acr"},{"id":"user-a"}]` + "\n")
		previous := configTarget("mcp", reordered, realize.OwnershipShared, initial.Managed)
		observed := observedFile("mcp", reordered)
		entry.EncodedValue = []byte(`{"id":"acr","v":2}`)
		updated, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
			Target: adapter.SharedTarget{Path: "mcp", Observed: &observed, Previous: &previous},
			Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
		})
		if err != nil {
			t.Fatal(err)
		}
		got := updated.Candidate.Content
		if bytes.Index(got, []byte("user-b")) > bytes.Index(got, []byte(`"acr"`)) || bytes.Index(got, []byte(`"acr"`)) > bytes.Index(got, []byte("user-a")) {
			t.Fatalf("hash lookup used position: %s", got)
		}
		if !bytes.Contains(got, []byte(`{"id":"acr","v":2}`)) {
			t.Fatalf("managed element not updated: %s", got)
		}
	})

	t.Run("duplicate-identical-array-elements-conflict", func(t *testing.T) {
		t.Parallel()
		entry := testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigElement, "owned", `{"id":"acr"}`)
		initial := compileMissingConfig(t, adapter.ConfigJSON, entry)
		copied := []byte(`[{"id":"acr"},{"id":"acr"}]` + "\n")
		_, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
			Target: adapter.SharedTarget{
				Path: "mcp", Observed: ptrObserved("mcp", copied),
				Previous: ptrTarget(configTarget("mcp", copied, realize.OwnershipShared, initial.Managed)),
			},
			Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
		})
		if err == nil || !strings.Contains(err.Error(), CodeDuplicateConfigEntry) {
			t.Fatalf("copied identical array elements error = %v, want duplicate_config_entry", err)
		}
	})

	t.Run("force-cannot-absorb-unowned-key", func(t *testing.T) {
		t.Parallel()
		content := []byte(`{"user":1}` + "\n")
		entry := testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigField, "user", `2`)
		_, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
			Target: adapter.SharedTarget{Path: "mcp", Observed: ptrObserved("mcp", content), Force: true},
			Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
		})
		if err == nil || !strings.Contains(err.Error(), "already exists without matching ownership") {
			t.Fatalf("force-absorb JSON error = %v", err)
		}
	})

	t.Run("extensionless-removal-leaves-unowned", func(t *testing.T) {
		t.Parallel()
		entry := testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigField, "managed", `1`)
		initial := compileMissingConfig(t, adapter.ConfigJSON, entry)
		content := []byte(`{"user":{"keep":true},"managed":1}` + "\n")
		removed, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
			Target: adapter.SharedTarget{
				Path: "mcp", Observed: ptrObserved("mcp", content),
				Previous: ptrTarget(configTarget("mcp", content, realize.OwnershipShared, initial.Managed)),
			},
			Format: adapter.ConfigJSON,
		})
		if err != nil {
			t.Fatal(err)
		}
		if removed.Action != realize.ActionRemove || bytes.Contains(removed.Candidate.Content, []byte(`"managed"`)) || !bytes.Contains(removed.Candidate.Content, []byte(`{"keep":true}`)) {
			t.Fatalf("extensionless JSON removal = %#v %s", removed, removed.Candidate.Content)
		}
	})
}

func TestHostileTOMLMatrix(t *testing.T) {
	t.Parallel()

	t.Run("order-unowned-comments", func(t *testing.T) {
		t.Parallel()
		entry := testConfigEntry(adapter.ConfigTOML, nil, adapter.ConfigField, "managed", `1`)
		initial := compileMissingConfig(t, adapter.ConfigTOML, entry)
		content := []byte("# keep\r\nzed = { nested = true }\r\nalpha = 2\r\nmanaged = 1\r\n")
		previous := configTarget("config", content, realize.OwnershipShared, initial.Managed)
		observed := observedFile("config", content)
		entry.EncodedValue = []byte(`2`)
		updated, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
			Target: adapter.SharedTarget{Path: "config", Observed: &observed, Previous: &previous},
			Format: adapter.ConfigTOML, Desired: []adapter.ConfigEntry{entry},
		})
		if err != nil {
			t.Fatal(err)
		}
		got := updated.Candidate.Content
		if bytes.Index(got, []byte("zed")) > bytes.Index(got, []byte("alpha")) || !bytes.Contains(got, []byte("# keep")) || !bytes.Contains(got, []byte("nested = true")) {
			t.Fatalf("TOML unowned rewrite: %s", got)
		}
		if !bytes.Contains(got, []byte("\r\n")) {
			t.Fatalf("TOML CRLF normalized: %s", got)
		}
	})

	t.Run("duplicate-keys", func(t *testing.T) {
		t.Parallel()
		content := []byte("a = 1\na = 2\n")
		_, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
			Target:  adapter.SharedTarget{Path: "config", Observed: ptrObserved("config", content)},
			Format:  adapter.ConfigTOML,
			Desired: []adapter.ConfigEntry{testConfigEntry(adapter.ConfigTOML, nil, adapter.ConfigField, "managed", `true`)},
		})
		if err == nil || !strings.Contains(err.Error(), CodeDuplicateConfigEntry) {
			t.Fatalf("duplicate TOML error = %v", err)
		}
	})

	t.Run("edited-acr-entry", func(t *testing.T) {
		t.Parallel()
		entry := testConfigEntry(adapter.ConfigTOML, nil, adapter.ConfigField, "managed", `1`)
		initial := compileMissingConfig(t, adapter.ConfigTOML, entry)
		edited := []byte("managed = 9\n")
		_, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
			Target: adapter.SharedTarget{
				Path: "config", Observed: ptrObserved("config", edited),
				Previous: ptrTarget(configTarget("config", initial.Candidate.Content, realize.OwnershipShared, initial.Managed)),
				Force:    true,
			},
			Format: adapter.ConfigTOML, Desired: []adapter.ConfigEntry{entry},
		})
		if err == nil || !strings.Contains(err.Error(), "was edited or removed") {
			t.Fatalf("edited TOML entry error = %v", err)
		}
	})

	t.Run("array-element-by-hash-after-reorder", func(t *testing.T) {
		t.Parallel()
		entry := testConfigEntry(adapter.ConfigTOML, []string{"plugins"}, adapter.ConfigElement, "owned", `"acr"`)
		initial := compileMissingConfig(t, adapter.ConfigTOML, entry)
		reordered := []byte("plugins = [\"user-b\", \"acr\", \"user-a\"]\n")
		previous := configTarget("config", reordered, realize.OwnershipShared, initial.Managed)
		observed := observedFile("config", reordered)
		entry.EncodedValue = []byte(`"acr-v2"`)
		updated, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
			Target: adapter.SharedTarget{Path: "config", Observed: &observed, Previous: &previous},
			Format: adapter.ConfigTOML, Desired: []adapter.ConfigEntry{entry},
		})
		if err != nil {
			t.Fatal(err)
		}
		got := updated.Candidate.Content
		if bytes.Index(got, []byte("user-b")) > bytes.Index(got, []byte("acr-v2")) || bytes.Index(got, []byte("acr-v2")) > bytes.Index(got, []byte("user-a")) {
			t.Fatalf("TOML hash lookup used position: %s", got)
		}
	})

	t.Run("extensionless-removal-leaves-unowned", func(t *testing.T) {
		t.Parallel()
		entry := testConfigEntry(adapter.ConfigTOML, nil, adapter.ConfigField, "managed", `1`)
		initial := compileMissingConfig(t, adapter.ConfigTOML, entry)
		content := []byte("# keep\nuser = 2\nmanaged = 1\n")
		removed, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
			Target: adapter.SharedTarget{
				Path: "config", Observed: ptrObserved("config", content),
				Previous: ptrTarget(configTarget("config", content, realize.OwnershipShared, initial.Managed)),
			},
			Format: adapter.ConfigTOML,
		})
		if err != nil {
			t.Fatal(err)
		}
		if removed.Action != realize.ActionRemove || bytes.Contains(removed.Candidate.Content, []byte("managed =")) || !bytes.Contains(removed.Candidate.Content, []byte("# keep")) {
			t.Fatalf("extensionless TOML removal = %s", removed.Candidate.Content)
		}
	})
}

func TestHostileOwnershipTransitions(t *testing.T) {
	t.Parallel()

	t.Run("filename-never-classifies", func(t *testing.T) {
		t.Parallel()
		generated, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
			Target:  adapter.SharedTarget{Path: "notes.md"},
			Desired: []adapter.MarkdownInsertion{testMarkdownInsertion("rule-a", "managed\n")},
		})
		if err != nil || generated.Candidate.Ownership != realize.OwnershipGenerated {
			t.Fatalf("missing notes.md = %#v, %v", generated, err)
		}
		existing := compileObservedMarkdown(t, "CLAUDE.md", []byte("user bytes\n"), testMarkdownInsertion("rule-a", "managed\n"))
		if existing.Candidate.Ownership != realize.OwnershipShared {
			t.Fatalf("existing CLAUDE.md ownership = %q", existing.Candidate.Ownership)
		}
		yamlObserved := observedFile("settings.yaml", []byte("user: true\n"))
		ownership, _, err := classifyTarget(adapter.SharedTarget{Path: "settings.yaml", Observed: &yamlObserved}, [][]byte{yamlObserved.Content})
		if err != nil || ownership != realize.OwnershipShared {
			t.Fatalf("yaml classification = %q, %v", ownership, err)
		}
		_, err = NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
			Target:  adapter.SharedTarget{Path: "settings.yaml", Observed: &yamlObserved},
			Format:  adapter.ConfigFormat("yaml"),
			Desired: []adapter.ConfigEntry{testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigField, "managed", `true`)},
		})
		if err == nil || !strings.Contains(err.Error(), "unsupported config format") {
			t.Fatalf("YAML merge error = %v", err)
		}
	})

	t.Run("promotion-is-sticky-and-reports-commit", func(t *testing.T) {
		t.Parallel()
		insertion := testMarkdownInsertion("rule-a", "managed v1\n")
		initial := compileMissingMarkdown(t, insertion)
		changed := append(append([]byte(nil), initial.Candidate.Content...), []byte("user appendix\n")...)
		promoted := updateMarkdown(t, "AGENTS.md", changed, initial, testMarkdownInsertion("rule-a", "managed v2\n"))
		if promoted.Candidate.Ownership != realize.OwnershipShared || len(promoted.Notices) != 1 || promoted.Notices[0].Code != "shared_file_requires_commit" {
			t.Fatalf("promotion = %#v", promoted)
		}
		if !bytes.HasSuffix(promoted.Candidate.Content, []byte("user appendix\n")) {
			t.Fatalf("promotion dropped unmanaged: %q", promoted.Candidate.Content)
		}
		cleanShared := bytes.TrimSuffix(promoted.Candidate.Content, []byte("user appendix\n"))
		sticky := updateMarkdown(t, "AGENTS.md", cleanShared, promoted, testMarkdownInsertion("rule-a", "managed v2\n"))
		if sticky.Candidate.Ownership != realize.OwnershipShared {
			t.Fatalf("auto-demoted after unmanaged deleted: %q", sticky.Candidate.Ownership)
		}
	})

	t.Run("explicit-demotion", func(t *testing.T) {
		t.Parallel()
		insertion := testMarkdownInsertion("rule-a", "managed\n")
		initial := compileMissingMarkdown(t, insertion)
		clean := markdownTarget("AGENTS.md", initial.Candidate.Content, realize.OwnershipShared, initial.Managed)
		demoted, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
			Target: adapter.SharedTarget{
				Path: "AGENTS.md", Observed: ptrObserved("AGENTS.md", initial.Candidate.Content),
				Previous: &clean, ExplicitDemotion: true,
			},
			Desired: []adapter.MarkdownInsertion{insertion},
		})
		if err != nil || demoted.Candidate.Ownership != realize.OwnershipGenerated {
			t.Fatalf("clean demotion = %#v, %v", demoted, err)
		}
		leftover := append(append([]byte(nil), initial.Candidate.Content...), []byte("user\n")...)
		_, err = NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
			Target: adapter.SharedTarget{
				Path: "AGENTS.md", Observed: ptrObserved("AGENTS.md", leftover),
				Previous:         ptrTarget(markdownTarget("AGENTS.md", leftover, realize.OwnershipShared, initial.Managed)),
				ExplicitDemotion: true, Force: true,
			},
			Desired: []adapter.MarkdownInsertion{insertion},
		})
		if err == nil || !strings.Contains(err.Error(), CodeOwnershipConflict) {
			t.Fatalf("forced leftover demotion error = %v", err)
		}
	})

	t.Run("force-cannot-replace-unmanaged", func(t *testing.T) {
		t.Parallel()
		original := []byte("user-owned 🔒\n")
		compiled, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
			Target:  adapter.SharedTarget{Path: "AGENTS.md", Observed: ptrObserved("AGENTS.md", original), Force: true},
			Desired: []adapter.MarkdownInsertion{testMarkdownInsertion("rule-a", "managed\n")},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(compiled.Candidate.Content, original) || compiled.Candidate.Ownership != realize.OwnershipShared {
			t.Fatalf("force replaced unmanaged: %q", compiled.Candidate.Content)
		}
		removed, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
			Target: adapter.SharedTarget{
				Path: "AGENTS.md", Observed: ptrObserved("AGENTS.md", compiled.Candidate.Content),
				Previous: ptrTarget(markdownTarget("AGENTS.md", compiled.Candidate.Content, realize.OwnershipShared, compiled.Managed)),
				Force:    true,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if removed.Action != realize.ActionRemove || !bytes.Equal(removed.Candidate.Content, original) {
			t.Fatalf("force final removal = %#v %q", removed, removed.Candidate.Content)
		}
	})

	t.Run("mode-and-tracked-retention-candidate", func(t *testing.T) {
		t.Parallel()
		original := []byte("keep\n")
		observed := adapter.ObservedFile{Path: "AGENTS.md", Content: append([]byte(nil), original...), Mode: fs.FileMode(0o600), Hash: hashBytes(original)}
		compiled, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
			Target:  adapter.SharedTarget{Path: "AGENTS.md", Observed: &observed},
			Desired: []adapter.MarkdownInsertion{testMarkdownInsertion("rule-a", "managed\n")},
		})
		if err != nil {
			t.Fatal(err)
		}
		if compiled.Candidate.Mode.Perm() != 0o600 {
			t.Fatalf("shared host mode = %o", compiled.Candidate.Mode)
		}
		removed := removeMarkdown(t, "AGENTS.md", compiled)
		if removed.Candidate == nil || !bytes.Equal(removed.Candidate.Content, original) || removed.Action != realize.ActionRemove {
			t.Fatalf("tracked-style retained candidate = %#v", removed)
		}
	})
}

func compileObservedMarkdown(t *testing.T, path string, content []byte, insertions ...adapter.MarkdownInsertion) adapter.SharedCompilation {
	t.Helper()
	compiled, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target: adapter.SharedTarget{Path: path, Observed: ptrObserved(path, content)}, Desired: insertions,
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Candidate == nil {
		t.Fatal("missing candidate")
	}
	return compiled
}

func updateMarkdown(t *testing.T, path string, content []byte, previous adapter.SharedCompilation, insertions ...adapter.MarkdownInsertion) adapter.SharedCompilation {
	t.Helper()
	target := markdownTarget(path, previous.Candidate.Content, previous.Candidate.Ownership, previous.Managed)
	if previous.Candidate.Ownership == realize.OwnershipGenerated && !bytes.Equal(content, previous.Candidate.Content) {
		target.Ownership = realize.OwnershipGenerated
	}
	compiled, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target:  adapter.SharedTarget{Path: path, Observed: ptrObserved(path, content), Previous: &target},
		Desired: insertions,
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func removeMarkdown(t *testing.T, path string, previous adapter.SharedCompilation) adapter.SharedCompilation {
	t.Helper()
	target := markdownTarget(path, previous.Candidate.Content, previous.Candidate.Ownership, previous.Managed)
	compiled, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target: adapter.SharedTarget{Path: path, Observed: ptrObserved(path, previous.Candidate.Content), Previous: &target},
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func ptrObserved(path string, content []byte) *adapter.ObservedFile {
	observed := observedFile(path, content)
	return &observed
}

func ptrTarget(target realize.Target) *realize.Target {
	return &target
}
