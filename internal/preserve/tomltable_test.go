package preserve

import (
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

// TestRemoveForeignTOMLTablePreservesUnownedComments is R3. The canonical
// three-field object proves ownership of the integration, not of a comment
// somebody wrote inside its table, so every comment position survives the
// removal with its exact bytes.
func TestRemoveForeignTOMLTablePreservesUnownedComments(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "comments at every position",
			content: "# comment above the table\n" +
				"[mcp_servers.tessl] # header comment\n" +
				"type = \"stdio\"\n" +
				"# comment between fields\n" +
				"command = \"tessl\" # inline comment\n" +
				"args = [ \"mcp\", \"start\" ]\n" +
				"# comment after the table\n" +
				"[tools]\n" +
				"web_search = true\n",
			want: "# comment above the table\n" +
				"# header comment\n" +
				"# comment between fields\n" +
				"# inline comment\n" +
				"# comment after the table\n" +
				"[tools]\n" +
				"web_search = true\n",
		},
		{
			name: "no comments leaves no residue",
			content: "[mcp_servers.tessl]\n" +
				"type = \"stdio\"\n" +
				"command = \"tessl\"\n" +
				"args = [ \"mcp\", \"start\" ]\n" +
				"\n" +
				"[tools]\n" +
				"web_search = true\n",
			want: "[tools]\n" +
				"web_search = true\n",
		},
		{
			name: "trailing table at end of file",
			content: "[tools]\n" +
				"web_search = true\n" +
				"[mcp_servers.tessl]\n" +
				"type = \"stdio\"\n" +
				"command = \"tessl\"\n" +
				"args = [ \"mcp\", \"start\" ]\n",
			want: "[tools]\n" +
				"web_search = true\n",
		},
		{
			name: "indented table keeps its own comment column",
			content: "  [mcp_servers.tessl]   # spaced comment\n" +
				"  type = \"stdio\"\n" +
				"  command = \"tessl\"\n" +
				"  args = [ \"mcp\", \"start\" ]\n" +
				"[tools]\n",
			want: "# spaced comment\n" +
				"[tools]\n",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			selector := ForeignSelector{Container: []string{"mcp_servers", "tessl"}, Table: true}
			after, removed, err := RemoveForeignConfigEntries(adapter.ConfigTOML, ".codex/config.toml", []byte(testCase.content), []ForeignSelector{selector}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != testCase.want {
				t.Fatalf("after =\n%q\nwant\n%q", after, testCase.want)
			}
			if len(removed) != 1 {
				t.Fatalf("removed = %#v, want one splice record", removed)
			}
		})
	}
}

// TestRemoveForeignTOMLTableRefusesAnACRManagedField keeps the existing
// ownership refusal: a table whose fields ACR itself owns is never spliced out
// as foreign evidence.
func TestRemoveForeignTOMLTableRefusesAnACRManagedField(t *testing.T) {
	t.Parallel()

	content := []byte("[mcp_servers.tessl]\ntype = \"stdio\"\ncommand = \"tessl\"\nargs = [ \"mcp\", \"start\" ]\n")
	managed := structuredEntryHash(adapter.ConfigTOML, []string{"mcp_servers", "tessl"}, adapter.ConfigField, "command", []byte("\"tessl\""))
	selector := ForeignSelector{Container: []string{"mcp_servers", "tessl"}, Table: true}
	if _, _, err := RemoveForeignConfigEntries(adapter.ConfigTOML, ".codex/config.toml", content, []ForeignSelector{selector}, []string{managed}); err == nil {
		t.Fatal("removal accepted an ACR-managed field")
	}
}

// TestRemoveForeignTOMLTablePreservesCommentsInsideMultilineValues is R3. An
// assignment whose value spans several lines carries comments inside its own
// byte range; ownership of the canonical value proves nothing about them.
func TestRemoveForeignTOMLTablePreservesCommentsInsideMultilineValues(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "comments inside a multiline array",
			content: "# above\n" +
				"[mcp_servers.tessl] # header\n" +
				"type = \"stdio\"\n" +
				"# between\n" +
				"command = \"tessl\" # inline\n" +
				"args = [\n" +
				"  \"mcp\", # array-first\n" +
				"  # array-between\n" +
				"  \"start\"\n" +
				"]\n" +
				"# below\n" +
				"[tools]\n" +
				"web_search = true\n",
			want: "# above\n# header\n# between\n# inline\n# array-first\n# array-between\n# below\n[tools]\nweb_search = true\n",
		},
		{
			name: "a multiline array with no comments leaves no residue",
			content: "[mcp_servers.tessl]\n" +
				"type = \"stdio\"\n" +
				"command = \"tessl\"\n" +
				"args = [\n  \"mcp\",\n  \"start\"\n]\n" +
				"[tools]\n",
			want: "[tools]\n",
		},
		{
			name: "a hash inside a string value is not a comment",
			content: "[mcp_servers.tessl]\n" +
				"type = \"stdio\"\n" +
				"command = \"tessl\"\n" +
				"args = [\n  \"mcp\",\n  \"start#notacomment\"\n]\n" +
				"[tools]\n",
			want: "[tools]\n",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			selector := ForeignSelector{Container: []string{"mcp_servers", "tessl"}, Table: true}
			after, _, err := RemoveForeignConfigEntries(adapter.ConfigTOML, ".codex/config.toml", []byte(testCase.content), []ForeignSelector{selector}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != testCase.want {
				t.Fatalf("after =\n%q\nwant\n%q", after, testCase.want)
			}
		})
	}
}
