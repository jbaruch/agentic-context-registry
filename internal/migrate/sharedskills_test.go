package migrate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedSharedSurfaceProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	return root
}

func linkShared(t *testing.T, root, name, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".agents", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, ".agents", "skills", name)); err != nil {
		t.Fatal(err)
	}
}

// TestSharedSurfaceIsClassifiedPerEntry proves the surface is not an agent:
// an empty directory, a hand-written entry and a Tessl link each get their own
// disposition, and none of them is a declared coverage flag.
func TestSharedSurfaceIsClassifiedPerEntry(t *testing.T) {
	t.Parallel()

	t.Run("empty surface reports nothing and covers no agent", func(t *testing.T) {
		t.Parallel()
		root := seedSharedSurfaceProject(t)
		if err := os.MkdirAll(filepath.Join(root, ".agents", "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
		report := inventoryProject(t, root)
		if len(report.SharedSkills) != 0 {
			t.Fatalf("sharedSkills = %#v", report.SharedSkills)
		}
		for _, agent := range report.Agents {
			if agent.ID == "agents" {
				t.Fatal("the shared surface is still reported as an agent")
			}
		}
	})

	t.Run("a declared Tessl link is removable", func(t *testing.T) {
		t.Parallel()
		root := seedSharedSurfaceProject(t)
		linkShared(t, root, "tessl__review-change", "../../.tessl/plugins/example/alpha/skills/review-change")
		report := inventoryProject(t, root)
		if !hasSharedSkill(report.SharedSkills, ".agents/skills/tessl__review-change", SharedSkillRemovable, "") {
			t.Fatalf("sharedSkills = %#v", report.SharedSkills)
		}
	})

	t.Run("a hand-written entry is never inspected further", func(t *testing.T) {
		t.Parallel()
		root := seedSharedSurfaceProject(t)
		writeFile(t, root, ".agents/skills/my-skill/SKILL.md", []byte("# Mine\n"), 0o644)
		report := inventoryProject(t, root)
		if !hasSharedSkill(report.SharedSkills, ".agents/skills/my-skill", SharedSkillUser, reasonSharedUserEntry) {
			t.Fatalf("sharedSkills = %#v", report.SharedSkills)
		}
	})

	t.Run("a real directory is retained", func(t *testing.T) {
		t.Parallel()
		root := seedSharedSurfaceProject(t)
		writeFile(t, root, ".agents/skills/tessl__review-change/SKILL.md", []byte("# Review\n"), 0o644)
		report := inventoryProject(t, root)
		if !hasSharedSkill(report.SharedSkills, ".agents/skills/tessl__review-change", SharedSkillRetained, reasonSharedNonSymlink) {
			t.Fatalf("sharedSkills = %#v", report.SharedSkills)
		}
	})

	t.Run("an escaping link is retained without following it", func(t *testing.T) {
		t.Parallel()
		root := seedSharedSurfaceProject(t)
		linkShared(t, root, "tessl__review-change", "../../../elsewhere/review")
		report := inventoryProject(t, root)
		if !hasSharedSkill(report.SharedSkills, ".agents/skills/tessl__review-change", SharedSkillRetained, reasonSkillEscape) {
			t.Fatalf("sharedSkills = %#v", report.SharedSkills)
		}
	})

	t.Run("a link outside .tessl is retained", func(t *testing.T) {
		t.Parallel()
		root := seedSharedSurfaceProject(t)
		writeFile(t, root, "team/skills/review/SKILL.md", []byte("# Team\n"), 0o644)
		linkShared(t, root, "tessl__review-change", "../../team/skills/review")
		report := inventoryProject(t, root)
		if !hasSharedSkill(report.SharedSkills, ".agents/skills/tessl__review-change", SharedSkillRetained, reasonSharedForeign) {
			t.Fatalf("sharedSkills = %#v", report.SharedSkills)
		}
	})

	t.Run("a link naming undeclared Tessl state blocks", func(t *testing.T) {
		t.Parallel()
		root := seedSharedSurfaceProject(t)
		linkShared(t, root, "tessl__gone", "../../.tessl/plugins/example/alpha/skills/gone")
		report := inventoryProject(t, root)
		if !hasSharedSkill(report.SharedSkills, ".agents/skills/tessl__gone", SharedSkillBlocked, reasonOrphanNative) {
			t.Fatalf("sharedSkills = %#v", report.SharedSkills)
		}
	})

	t.Run("a symlinked surface directory is refused", func(t *testing.T) {
		t.Parallel()
		root := seedSharedSurfaceProject(t)
		writeFile(t, root, "elsewhere/keep.md", []byte("keep\n"), 0o644)
		if err := os.MkdirAll(filepath.Join(root, ".agents"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../elsewhere", filepath.Join(root, ".agents", "skills")); err != nil {
			t.Fatal(err)
		}
		_, err := Inventory(openSnapshot(t, root))
		var symlinkErr *SharedSurfaceSymlinkError
		if !errors.As(err, &symlinkErr) {
			t.Fatalf("Inventory() error = %v, want a shared-surface refusal", err)
		}
		if !strings.Contains(err.Error(), "replace it with a real directory") {
			t.Fatalf("refusal is not actionable: %v", err)
		}
	})
}

// TestMCPClassificationNeedsNameAndShape is D12, D21, D22, D23 and D26 at the
// inventory layer.
func TestMCPClassificationNeedsNameAndShape(t *testing.T) {
	t.Parallel()

	const token = "s3cr3t-token-do-not-print"
	for _, testCase := range []struct {
		name        string
		path        string
		document    string
		disposition string
		reason      string
		absent      bool
	}{
		{
			name:        "canonical entry",
			path:        ".mcp.json",
			document:    `{"mcpServers":{"tessl":{"type":"stdio","command":"tessl","args":["mcp","start"]}}}`,
			disposition: MCPCanonical, reason: reasonMCPRetiredShape,
		},
		{
			name:     "no tessl entry",
			path:     ".mcp.json",
			document: `{"mcpServers":{"notes":{"type":"stdio","command":"notes-server"}}}`,
			absent:   true,
		},
		{
			name:     "differently named server",
			path:     ".mcp.json",
			document: `{"mcpServers":{"tessl-proxy":{"type":"stdio","command":"tessl","args":["mcp","start"]}}}`,
			absent:   true,
		},
		{
			name:        "extra argument",
			path:        ".mcp.json",
			document:    `{"mcpServers":{"tessl":{"type":"stdio","command":"tessl","args":["mcp","start","--verbose"]}}}`,
			disposition: MCPAmbiguous, reason: reasonMCPArgs,
		},
		{
			name:        "environment block",
			path:        ".mcp.json",
			document:    `{"mcpServers":{"tessl":{"type":"stdio","command":"tessl","args":["mcp","start"],"env":{"TOKEN":"` + token + `"}}}}`,
			disposition: MCPAmbiguous, reason: reasonMCPExtraKeys,
		},
		{
			name:        "malformed document",
			path:        ".mcp.json",
			document:    `{"mcpServers":{"tessl":`,
			disposition: MCPAmbiguous, reason: reasonMCPMalformed,
		},
		{
			name:        "unsupported agent config",
			path:        ".vscode/mcp.json",
			document:    `{"mcpServers":{"tessl":{"type":"stdio","command":"tessl","args":["mcp","start"]}}}`,
			disposition: MCPForeign, reason: reasonMCPUnsupported,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			root := seedSharedSurfaceProject(t)
			writeFile(t, root, testCase.path, []byte(testCase.document+"\n"), 0o644)
			report := inventoryProject(t, root)
			if testCase.absent {
				for _, entry := range report.MCP {
					if entry.Path == testCase.path {
						t.Fatalf("unexpected MCP record: %#v", entry)
					}
				}
				return
			}
			if !hasMCPEntry(report.MCP, testCase.path, testCase.disposition, testCase.reason) {
				t.Fatalf("mcp = %#v", report.MCP)
			}
			assertNoMCPSecret(t, report, token)
		})
	}
}

// assertNoMCPSecret proves the inventory reports identifiers about an entry,
// never its content, in both rendered forms.
func assertNoMCPSecret(t *testing.T, report Report, token string) {
	t.Helper()
	text := FormatText(report)
	if strings.Contains(text, token) {
		t.Fatalf("inventory text leaked the credential:\n%s", text)
	}
	if strings.Contains(text, "/usr/local/bin/tessl") || strings.Contains(text, "--verbose") {
		t.Fatalf("inventory text rendered entry content:\n%s", text)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), token) {
		t.Fatalf("inventory JSON leaked the credential:\n%s", encoded)
	}
}

// TestCanonicalMCPDigestIsStable proves the digest identifies an entry
// independently of key order, so two operators comparing digests agree.
func TestCanonicalMCPDigestIsStable(t *testing.T) {
	t.Parallel()

	first, err := canonicalMCPDigest(map[string]any{
		"type": "stdio", "command": "tessl", "args": []any{"mcp", "start"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := canonicalMCPDigest(map[string]any{
		"args": []any{"mcp", "start"}, "command": "tessl", "type": "stdio",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("digest depends on key order: %s vs %s", first, second)
	}
	different, err := canonicalMCPDigest(map[string]any{
		"type": "stdio", "command": "tessl", "args": []any{"mcp", "start", "--verbose"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if different == first {
		t.Fatal("digest does not distinguish different entries")
	}
}
