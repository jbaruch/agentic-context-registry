package migrate

import (
	"testing"
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

func TestInventoryPreservesUnmanagedSpans(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeAgentsMD(t, root, "# User title\n\nUser prose lives here.\n\n", "")
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
