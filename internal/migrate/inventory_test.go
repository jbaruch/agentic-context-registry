package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

func TestInventoryClassifiesByPackage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0", "example/beta": "2.0.0"})
	seedAlpha(t, root, alphaPlugin(true, []string{"skills/review-change"}, ""))
	seedBeta(t, root, betaTile("skills/legacy-skill/SKILL.md"))
	writeNativeSkills(t, root, ".claude/skills", "example/alpha", "review-change", true)
	writeCursorMDC(t, root, "example/alpha", "always-rule", ruleSource(t, root, "example/alpha", "always-rule"))

	report := inventoryProject(t, root)
	if len(report.Packages) != 2 {
		t.Fatalf("packages = %#v", report.Packages)
	}
	if report.Packages[0].Name != "example/alpha" || report.Packages[1].Name != "example/beta" {
		t.Fatalf("packages not sorted by name: %s %s", report.Packages[0].Name, report.Packages[1].Name)
	}
	seen := map[string]string{}
	for _, pkg := range report.Packages {
		for _, artifact := range pkg.Artifacts {
			key := pkg.Name + ":" + artifact.Kind + ":" + artifact.ID
			if previous, ok := seen[key]; ok {
				t.Fatalf("artifact %s classified twice (%s and %s)", key, previous, artifact.Classification)
			}
			seen[key] = artifact.Classification
			switch artifact.Classification {
			case classMigratable, classUnmapped, classAmbiguous, classUnsupported:
			default:
				t.Fatalf("unknown classification %s for %s", artifact.Classification, key)
			}
		}
	}
	if seen["example/alpha:rule:always-rule"] != classMigratable {
		t.Fatalf("alpha always-rule = %s", seen["example/alpha:rule:always-rule"])
	}
	if seen["example/alpha:skill:review-change"] != classMigratable {
		t.Fatalf("alpha skill = %s", seen["example/alpha:skill:review-change"])
	}
	if seen["example/alpha:hook:session-start"] != classMigratable {
		t.Fatalf("alpha hook = %s", seen["example/alpha:hook:session-start"])
	}
	if seen["example/beta:rule:legacy-rule"] != classMigratable {
		t.Fatalf("beta rule = %s", seen["example/beta:rule:legacy-rule"])
	}
}

func TestClassificationCodes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0", "example/beta": "2.0.0"})
	seedAlpha(t, root, alphaPlugin(true, []string{"skills/review-change"}, ""))
	seedBeta(t, root, betaTile("skills/legacy-skill/SKILL.md"))
	writeSkillTree(t, root, "example/beta", "review-change", map[string]string{"SKILL.md": "# Beta review\n"})
	writeTileJSON(t, root, "example/beta", map[string]any{
		"name":    "example/beta",
		"version": "2.0.0",
		"rules":   map[string]any{"legacy-rule": map[string]string{"rules": "rules/legacy-rule.md"}},
		"skills": map[string]any{
			"legacy-skill":  map[string]string{"path": "skills/legacy-skill/SKILL.md"},
			"review-change": map[string]string{"path": "skills/review-change/SKILL.md"},
		},
	})
	writeRulesMD(t, root, []string{"example/alpha/rules/always-rule.md", "example/alpha/rules/gone.md"})
	writeFile(t, root, pluginPath("example/alpha", "tessl-package.json"), []byte(`{"name":"example/alpha","version":"1.0.0"}`+"\n"), 0o644)
	writeFile(t, root, pluginPath("example/alpha", "README.md"), []byte("plugin readme\n"), 0o644)
	writeGitignoreTesslBlock(t, root)
	writeJSON(t, root, ".cursor/mcp.json", map[string]any{"mcpServers": map[string]any{"tessl": map[string]any{}}})
	writeCodexTOML(t, root, false, true)

	report := inventoryProject(t, root)
	if !hasRecord(report.Unmapped, ".tessl/RULES.md", reasonTesslIndex) {
		t.Fatalf("unmapped = %#v", report.Unmapped)
	}
	if !hasRecord(report.Unmapped, ".gitignore", reasonTesslGitignore) {
		t.Fatalf("gitignore not unmapped: %#v", report.Unmapped)
	}
	if !hasRecord(report.Unmapped, pluginPath("example/alpha", "tessl-package.json"), reasonTesslPackage) {
		t.Fatalf("tessl-package.json not unmapped: %#v", report.Unmapped)
	}
	if !hasRecord(report.Unmapped, pluginPath("example/alpha", "README.md"), reasonUndeclaredPlugin) {
		t.Fatalf("undeclared plugin file not unmapped: %#v", report.Unmapped)
	}
	if !hasRecord(report.Unsupported, ".cursor/mcp.json", reasonMCPServer) {
		t.Fatalf("mcp not unsupported: %#v", report.Unsupported)
	}
	if !hasRecord(report.Unsupported, ".codex/config.toml", reasonMCPServer) {
		t.Fatalf("toml mcp not unsupported: %#v", report.Unsupported)
	}
	if artifactClass(t, report, "example/alpha", kindRule, "gone") != classAmbiguous {
		t.Fatalf("missing RULES.md target not ambiguous")
	}
	if artifactClass(t, report, "example/alpha", kindSkill, "review-change") != classAmbiguous {
		t.Fatalf("duplicate tessl__review-change must be ambiguous on alpha")
	}
	if artifactClass(t, report, "example/beta", kindSkill, "review-change") != classAmbiguous {
		t.Fatalf("duplicate tessl__review-change must be ambiguous on beta")
	}
}

func TestDuplicateTesslSkillName(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0", "example/beta": "2.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	seedBeta(t, root, betaTile("skills/legacy-skill/SKILL.md"))
	writeSkillTree(t, root, "example/beta", "review-change", map[string]string{"SKILL.md": "# Beta\n"})
	writeTileJSON(t, root, "example/beta", map[string]any{
		"name":    "example/beta",
		"version": "2.0.0",
		"rules":   map[string]any{"legacy-rule": map[string]string{"rules": "rules/legacy-rule.md"}},
		"skills": map[string]any{
			"legacy-skill":  map[string]string{"path": "skills/legacy-skill/SKILL.md"},
			"review-change": map[string]string{"path": "skills/review-change/SKILL.md"},
		},
	})

	report := inventoryProject(t, root)
	if artifactClass(t, report, "example/alpha", kindSkill, "review-change") != classAmbiguous {
		t.Fatal("alpha duplicate skill must be ambiguous")
	}
	if artifactClass(t, report, "example/beta", kindSkill, "review-change") != classAmbiguous {
		t.Fatal("beta duplicate skill must be ambiguous")
	}
	if artifactClass(t, report, "example/beta", kindSkill, "legacy-skill") != classMigratable {
		t.Fatal("non-colliding Cursor-style unique skill must stay migratable")
	}
}

func TestDuplicateUnsupportedSkillStaysUnsupported(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0", "example/beta": "2.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	link := filepath.Join(root, filepath.FromSlash(pluginPath("example/alpha", "skills/review-change/link.md")))
	if err := os.Symlink("SKILL.md", link); err != nil {
		t.Fatal(err)
	}
	seedBeta(t, root, betaTile("skills/legacy-skill/SKILL.md"))
	writeSkillTree(t, root, "example/beta", "review-change", map[string]string{"SKILL.md": "# Beta review\n"})
	writeTileJSON(t, root, "example/beta", map[string]any{
		"name":    "example/beta",
		"version": "2.0.0",
		"rules":   map[string]any{"legacy-rule": map[string]string{"rules": "rules/legacy-rule.md"}},
		"skills": map[string]any{
			"legacy-skill":  map[string]string{"path": "skills/legacy-skill/SKILL.md"},
			"review-change": map[string]string{"path": "skills/review-change/SKILL.md"},
		},
	})

	report := inventoryProject(t, root)
	if artifactClass(t, report, "example/alpha", kindSkill, "review-change") != classUnsupported {
		t.Fatal("escaped skill must stay unsupported when duplicated")
	}
	if artifactClass(t, report, "example/beta", kindSkill, "review-change") != classAmbiguous {
		t.Fatal("duplicate migratable skill must still be ambiguous")
	}
}

func TestInventoryPreservesUnmanagedSpans(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeAgentsMD(t, root, "# User title\n\nUser prose lives here.\n\n", "")
	writeRulesMD(t, root, []string{"example/alpha/rules/always-rule.md", "example/alpha/rules/paths-rule.md"})
	writeFile(t, root, ".claude/settings.local.json", []byte(`{"permissions":{}}`+"\n"), 0o644)
	writeClaudeSettings(t, root, true)

	report := inventoryProject(t, root)
	if !hasRecord(report.Preserved, "AGENTS.md", reasonUnmanagedPrefix) {
		t.Fatalf("user prefix not preserved: %#v", report.Preserved)
	}
	if !hasRecord(report.Preserved, ".claude/settings.local.json", reasonUnmanagedConfig) {
		t.Fatalf("settings.local.json not preserved: %#v", report.Preserved)
	}
	if !hasRecord(report.Preserved, ".claude/settings.json", reasonUnmanagedHook) {
		t.Fatalf("user hook not preserved: %#v", report.Preserved)
	}
	assertPreservedHoldsUserFiles(t, report)
}

func TestUserHookBesideTesslHook(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		seed func(t *testing.T, root string)
	}{
		{name: "settings.json", path: ".claude/settings.json", seed: func(t *testing.T, root string) { writeClaudeSettings(t, root, true) }},
		{name: "config.toml", path: ".codex/config.toml", seed: func(t *testing.T, root string) { writeCodexTOML(t, root, true, false) }},
		{name: "hooks.json", path: ".cursor/hooks.json", seed: func(t *testing.T, root string) { writeCursorHooks(t, root, true) }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
			seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
			test.seed(t, root)
			report := inventoryProject(t, root)
			if !hasRecord(report.Preserved, test.path, reasonUnmanagedHook) {
				t.Fatalf("preserved = %#v", report.Preserved)
			}
		})
	}
}

func TestTesslManagedSpanWithExtraContent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeAgentsMD(t, root, "# User title\n\n", "operator stuffed extra prose into the Tessl span.\n")

	report := inventoryProject(t, root)
	if !hasRecord(report.Ambiguous, "AGENTS.md", reasonTesslSpanExtra) {
		t.Fatalf("extra tessl span content = %#v", report.Ambiguous)
	}
	if !hasRecord(report.Preserved, "AGENTS.md", reasonUnmanagedPrefix) {
		t.Fatalf("user prefix still preserved = %#v", report.Preserved)
	}
}

func TestUncoveredAgentReported(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeClaudeSettings(t, root, false)
	writeGeminiSettings(t, root)

	report := inventoryProject(t, root)
	var gemini, claude *AgentCoverage
	for index := range report.Agents {
		agent := &report.Agents[index]
		switch agent.ID {
		case "gemini":
			gemini = agent
		case "claude-code":
			claude = agent
		}
	}
	if claude == nil || !claude.Covered {
		t.Fatalf("claude-code coverage = %+v", claude)
	}
	if gemini == nil || gemini.Covered {
		t.Fatalf("gemini must be an uncovered target, got %+v", gemini)
	}
	if len(gemini.Evidence) == 0 {
		t.Fatal("uncovered agent still needs evidence paths")
	}
}

func TestInventoryClassifiesReferenceConsumer(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeRulesMD(t, root, []string{"example/alpha/rules/always-rule.md", "example/alpha/rules/paths-rule.md"})
	writeAgentsMD(t, root, "# User title\n\nUser prose lives here.\n\n", "")
	writeFile(t, root, "CLAUDE.md", []byte("# Claude user notes\n"), 0o644)
	writeFile(t, root, ".claude/settings.local.json", []byte(`{"permissions":{}}`+"\n"), 0o644)
	writeClaudeSettings(t, root, false)

	report := inventoryProject(t, root)
	if artifactClass(t, report, "example/alpha", kindRule, "always-rule") != classMigratable {
		t.Fatal("reference-consumer rule must stay a migratable artifact")
	}
	if artifactClass(t, report, "example/alpha", kindRule, "paths-rule") != classMigratable {
		t.Fatal("reference-consumer paths-rule must stay a migratable artifact")
	}
	if !hasRecord(report.Preserved, "AGENTS.md", reasonUnmanagedPrefix) {
		t.Fatalf("user AGENTS.md not preserved: %#v", report.Preserved)
	}
	if !hasRecord(report.Preserved, "CLAUDE.md", reasonUnmanagedPrefix) {
		t.Fatalf("user CLAUDE.md not preserved: %#v", report.Preserved)
	}
	if !hasRecord(report.Unmapped, rulesIndexPath, reasonTesslIndex) {
		t.Fatalf("RULES.md must stay unmapped, not preserved: %#v", report.Unmapped)
	}
	assertPreservedHoldsUserFiles(t, report)
	assertNoDoubleOwnership(t, report)
}

func TestMalformedHookDoesNotOwnPluginPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	plugin := alphaPlugin(false, []string{"skills/review-change"}, "")
	plugin["hooks"] = map[string]any{
		"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": "bash", "args": []string{"README.md"},
		}}}},
	}
	seedAlpha(t, root, plugin)
	writeFile(t, root, pluginPath("example/alpha", "README.md"), []byte("plugin readme\n"), 0o644)

	report := inventoryProject(t, root)
	if !hasRecord(report.Unmapped, pluginPath("example/alpha", "README.md"), reasonUndeclaredPlugin) {
		t.Fatalf("malformed hook must not hide undeclared file: %#v", report.Unmapped)
	}
	if artifactClass(t, report, "example/alpha", kindHook, "bash") != classUnsupported {
		t.Fatalf("malformed hook must be unsupported, got %#v", report.Packages)
	}
}

func TestInventoryIncludeGraphReadFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeAgentsMD(t, root, "# User title\n\n", "")

	snapshot := failingReadSnapshot{DirectorySnapshot: openSnapshot(t, root), failPath: "AGENTS.md"}
	report, err := Inventory(snapshot)
	if err == nil {
		t.Fatalf("inventory succeeded with a partial report: %#v", report)
	}
	if !strings.Contains(err.Error(), "AGENTS.md") {
		t.Fatalf("error = %v, want the failing snapshot path", err)
	}
	if report.SchemaVersion != 0 || len(report.Packages) != 0 || len(report.Preserved) != 0 {
		t.Fatalf("failing inventory must not return a partial report: %#v", report)
	}
}

type failingReadSnapshot struct {
	adapter.DirectorySnapshot
	failPath string
}

func (snapshot failingReadSnapshot) ReadFile(path string) (adapter.ObservedFile, error) {
	if path == snapshot.failPath {
		return adapter.ObservedFile{}, fmt.Errorf("injected read failure for %q", path)
	}
	return snapshot.DirectorySnapshot.ReadFile(path)
}

func TestInventoryKeepsUserFileWhenItIncludesTesslRule(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeFile(t, root, "AGENTS.md", []byte("# User title\n\n@.tessl/plugins/example/alpha/rules/always-rule.md\n"), 0o644)

	report := inventoryProject(t, root)
	if !hasRecord(report.Preserved, "AGENTS.md", reasonUnmanagedPrefix) {
		t.Fatalf("user file that includes a Tessl rule must stay preserved: %#v", report.Preserved)
	}
	rulePath := pluginPath("example/alpha", "rules/always-rule.md")
	if hasRecord(report.Preserved, rulePath, reasonUnmanagedPrefix) {
		t.Fatalf("included Tessl rule must not be preserved: %#v", report.Preserved)
	}
	if artifactClass(t, report, "example/alpha", kindRule, "always-rule") != classMigratable {
		t.Fatal("included Tessl rule must stay a migratable artifact")
	}
	assertPreservedHoldsUserFiles(t, report)
	assertNoDoubleOwnership(t, report)
}

func TestInventoryPreservesCopiedSkillExtraFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeNativeSkills(t, root, ".claude/skills", "example/alpha", "review-change", false)
	writeFile(t, root, ".claude/skills/tessl__review-change/NOTES.md", []byte("notes\n"), 0o644)

	report := inventoryProject(t, root)
	if artifactClass(t, report, "example/alpha", kindSkill, "review-change") != classMigratable {
		t.Fatal("skill with extra native file must stay migratable")
	}
	if !hasRecord(report.Preserved, ".claude/skills/tessl__review-change/NOTES.md", reasonUnmanagedSkill) {
		t.Fatalf("NOTES.md not preserved: %#v", report.Preserved)
	}
}

func inventoryProject(t *testing.T, root string) Report {
	t.Helper()
	report, err := Inventory(openSnapshot(t, root))
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func hasRecord(records []PathRecord, path, reason string) bool {
	for _, record := range records {
		if record.Path == path && record.Reason == reason {
			return true
		}
	}
	return false
}

func assertPreservedHoldsUserFiles(t *testing.T, report Report) {
	t.Helper()
	for _, record := range report.Preserved {
		if record.Path == rulesIndexPath || strings.HasPrefix(record.Path, ".tessl/") {
			t.Fatalf("preserved holds Tessl-owned path %s (%s); Tessl-owned content is an artifact or unmapped", record.Path, record.Reason)
		}
	}
}

func assertNoDoubleOwnership(t *testing.T, report Report) {
	t.Helper()
	claimed := map[string]string{}
	claim := func(path, class string) {
		t.Helper()
		if previous, ok := claimed[path]; ok && previous != class {
			t.Fatalf("path %s reported as both %s and %s", path, previous, class)
		}
		claimed[path] = class
	}
	for _, record := range report.Preserved {
		claim(record.Path, "preserved")
	}
	for _, record := range report.Unmapped {
		claim(record.Path, "unmapped")
	}
	for _, pkg := range report.Packages {
		for _, artifact := range pkg.Artifacts {
			for _, native := range artifact.Natives {
				if strings.HasPrefix(native, ".tessl/") {
					claim(native, "artifact:"+pkg.Name+":"+artifact.Kind+":"+artifact.ID)
				}
			}
		}
	}
}

func artifactClass(t *testing.T, report Report, pkg, kind, id string) string {
	t.Helper()
	for _, item := range report.Packages {
		if item.Name != pkg && item.TesslIdentity != pkg {
			continue
		}
		for _, artifact := range item.Artifacts {
			if artifact.Kind == kind && artifact.ID == id {
				return artifact.Classification
			}
		}
	}
	t.Fatalf("missing artifact %s %s %s in %#v", pkg, kind, id, report.Packages)
	return ""
}
