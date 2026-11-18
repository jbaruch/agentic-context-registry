package migrate

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestDiagnosticToken(t *testing.T) {
	for _, test := range []struct {
		name, value, want string
	}{
		{"empty", "", `""`},
		{"newline", "review\nFAKE-TRUSTED-GUIDANCE", `"review\nFAKE-TRUSTED-GUIDANCE"`},
		{"escape", "review\x1b[2J", `"review\x1b[2J"`},
		{"controls", "\r\t\x00", `"\r\t\x00"`},
		{"quotes", "\"\\", `"\"\\"`},
		{"127 bytes", strings.Repeat("x", 127), `"` + strings.Repeat("x", 127) + `"`},
		{"128 bytes", strings.Repeat("x", 128), `"` + strings.Repeat("x", 128) + `"`},
		{"129 bytes", strings.Repeat("x", 129), `"` + strings.Repeat("x", 128) + `...[truncated]"`},
		{"UTF-8 boundary", strings.Repeat("x", 127) + "é", `"` + strings.Repeat("x", 127) + `\xc3...[truncated]"`},
		{"invalid UTF-8", "\xff", `"\xff"`},
		{"maximum escaping", strings.Repeat("\x00", 129), `"` + strings.Repeat(`\x00`, 128) + `...[truncated]"`},
		{"1 MiB", strings.Repeat("x", 1<<20), `"` + strings.Repeat("x", 128) + `...[truncated]"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := DiagnosticToken(test.value)
			if got != test.want {
				t.Fatalf("token=%q want=%q", got, test.want)
			}
			if len(got) > 528 || strings.ContainsAny(got, "\n\r\t\x00\x1b") {
				t.Fatal("token is unbounded or contains raw controls")
			}
			if _, err := strconv.Unquote(got); err != nil {
				t.Fatalf("invalid quoted token: %v", err)
			}
		})
	}
}

func TestReportJSONIsStructOrdered(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/beta": "2.0.0", "example/alpha": "1.0.0"})
	seedBeta(t, root, betaTile("skills/legacy-skill/SKILL.md"))
	seedAlpha(t, root, alphaPlugin(true, []string{"skills/review-change"}, ""))
	writeGitignoreTesslBlock(t, root)
	writeJSON(t, root, ".cursor/mcp.json", map[string]any{"mcpServers": map[string]any{}})
	writeAgentsMD(t, root, "# User\n\n", "")

	report := inventoryProject(t, root)
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, again) {
		t.Fatal("marshaled inventory must be byte-identical across calls")
	}
	var decoded Report
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	roundTrip, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, roundTrip) {
		t.Fatalf("round-trip JSON changed bytes:\n%s\n%s", encoded, roundTrip)
	}
	if bytes.Contains(encoded, []byte(`"packages":null`)) || bytes.Contains(encoded, []byte(`"preserved":null`)) {
		t.Fatalf("slices must encode as arrays, got %s", encoded)
	}
}

func TestFormatTextGroupsByPackage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(true, []string{"skills/review-change"}, ""))
	writeClaudeSettings(t, root, false)
	writeGeminiSettings(t, root)
	writeAgentsMD(t, root, "# User\n\n", "")
	writeJSON(t, root, ".cursor/mcp.json", map[string]any{"mcpServers": map[string]any{
		"tessl": map[string]any{"type": "stdio", "command": "tessl", "args": []any{"mcp", "start"}},
	}})

	text := FormatText(inventoryProject(t, root))
	for _, want := range []string{
		"Tessl inventory (dry-run; no files written)",
		"Agents",
		"claude-code",
		"covered",
		"gemini",
		"uncovered",
		"Package example/alpha",
		"migratable",
		"rule always-rule",
		"skill review-change",
		"hook session-start",
		"Preserved",
		"AGENTS.md",
		"MCP servers",
		".cursor/mcp.json",
		"mcpServers.tessl",
		"canonical",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\n  unmapped\n") {
		t.Fatalf("per-package unmapped group is always empty and must be omitted:\n%s", text)
	}
}

func TestFormatCoexistenceTextRendersNoteEvidence(t *testing.T) {
	t.Parallel()
	report := MigrationReport{DryRun: true, Notes: []CoexistenceNote{
		{Code: "gitignored_state", Path: ".agents/registry.lock", IgnoredBy: ".gitignore:36"},
		{Code: "uncovered-agent", Agent: "gemini", Artifacts: 4, Paths: []string{".gemini/settings.json"}},
		{Code: "ambiguous", Path: "AGENTS.md", Detail: "managed span changed"},
	}}
	text := FormatCoexistenceText(report)
	for _, want := range []string{
		"NOTE gitignored_state: .agents/registry.lock; ignored by .gitignore:36",
		"NOTE uncovered-agent: agent=gemini; artifacts=4; paths=.gemini/settings.json",
		"NOTE ambiguous: AGENTS.md; managed span changed",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text missing %q:\n%s", want, text)
		}
	}
}
