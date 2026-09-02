package migrate

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestHostileDeclaredSkillIsNeverSilentlyDropped pokes the skill expansion in
// expandPluginSkills. A manifest-declared skill directory whose SKILL.md is
// absent must still reach the report as an artifact — the whole point of the
// inventory is that #2 and #8 see every declared artifact. Dropping it leaves
// the consumer with no handle at all. An unreadable SKILL.md is a read
// failure and is covered by TestHostileUnreadableSkillMarkdownDoesNotSwallowTheError.
func TestHostileDeclaredSkillIsNeverSilentlyDropped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		seed func(t *testing.T, root string)
	}{
		{
			name: "skillMarkdownAbsentBesideSiblingFile",
			seed: func(t *testing.T, root string) {
				writeFile(t, root, pluginPath("example/alpha", "skills/broken/reference.md"), []byte("reference\n"), 0o644)
			},
		},
		{
			name: "skillMarkdownAbsentBesideSubdirectory",
			seed: func(t *testing.T, root string) {
				writeFile(t, root, pluginPath("example/alpha", "skills/broken/references/table.md"), []byte("table\n"), 0o644)
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
			seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change", "skills/broken"}, ""))
			test.seed(t, root)

			report := inventoryProject(t, root)
			class, ok := findArtifact(report, "example/alpha", kindSkill, "broken")
			if !ok {
				t.Fatalf("declared skill \"broken\" vanished from the inventory; packages = %#v", report.Packages)
			}
			if class == classMigratable {
				t.Fatalf("skill without a readable SKILL.md must not be migratable, got %s", class)
			}
		})
	}
}

// TestHostileUnreadableSkillMarkdownDoesNotSwallowTheError is the error-handling
// half of the case above: a permission error on a declared SKILL.md is a read
// failure, not evidence that the file is absent. missing-skill is reserved for
// fs.ErrNotExist; any other read failure fails the inventory.
func TestHostileUnreadableSkillMarkdownDoesNotSwallowTheError(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change", "skills/locked"}, ""))
	locked := pluginPath("example/alpha", "skills/locked/SKILL.md")
	writeFile(t, root, locked, []byte("# Locked\n"), 0o644)

	report, err := Inventory(failingReadFileSnapshot{DirectorySnapshot: openSnapshot(t, root), failPath: locked})
	if err == nil {
		t.Fatalf("unreadable SKILL.md succeeded with report %#v, want a read error", report)
	}
}

func TestHostileRulesIndexParentTraversalFailsInventory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeFile(t, root, "README.md", []byte("# outside\n"), 0o644)
	writeFile(t, root, rulesIndexPath, []byte("# Agent Rules\n\n@plugins/example/alpha/rules/../../../../README.md\n"), 0o644)

	report, err := Inventory(openSnapshot(t, root))
	if err == nil {
		t.Fatalf("escaping RULES.md include succeeded with report %#v, want a path error", report)
	}
}

func TestHostileDuplicatePluginRuleIDIsAmbiguous(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	plugin := alphaPlugin(false, []string{"skills/review-change"}, "")
	plugin["rules"] = []string{"rules/always-rule.md", "other/always-rule.md"}
	seedAlpha(t, root, plugin)
	writeFile(t, root, pluginPath("example/alpha", "other/always-rule.md"), []byte("---\nalwaysApply: true\n---\n# Other always\n"), 0o644)

	report := inventoryProject(t, root)
	if class := artifactClass(t, report, "example/alpha", kindRule, "always-rule"); class != classAmbiguous {
		t.Fatalf("duplicate plugin rule id = %s, want ambiguous", class)
	}
}

func TestHostileEscapingDeclaredRulePathFailsInventory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	plugin := alphaPlugin(false, []string{"skills/review-change"}, "")
	plugin["rules"] = []string{"../../other/rules/x.md"}
	seedAlpha(t, root, plugin)

	report, err := Inventory(openSnapshot(t, root))
	if err == nil {
		t.Fatalf("escaping declared rule path succeeded with report %#v, want a path error", report)
	}
}

func TestHostileEmptyDeclaredSkillPathFailsInventory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	plugin := alphaPlugin(false, []string{"."}, "")
	seedAlpha(t, root, plugin)

	report, err := Inventory(openSnapshot(t, root))
	if err == nil {
		t.Fatalf("package-root skill path succeeded with report %#v, want a path error", report)
	}
}

func TestHostileMalformedNativeJSONFailsInventory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeFile(t, root, ".claude/settings.json", []byte("{not json"), 0o644)

	report, err := Inventory(openSnapshot(t, root))
	if err == nil {
		t.Fatalf("malformed native JSON succeeded with report %#v, want a decode error", report)
	}
}

func TestHostileMalformedNativeTOMLFailsInventory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeFile(t, root, ".codex/config.toml", []byte("hooks = {"), 0o644)

	report, err := Inventory(openSnapshot(t, root))
	if err == nil {
		t.Fatalf("malformed native TOML succeeded with report %#v, want a decode error", report)
	}
}

// TestHostileOrphanTesslNativesAreClassified leaves Tessl-written native files
// behind that no installed package declares — the shape a partial uninstall or
// a stale native tree leaves. Every Tessl-owned path must land in one of the
// four path classes so #2 knows the file exists.
func TestHostileOrphanTesslNativesAreClassified(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	orphanRule := ".cursor/rules/tessl__rule__example__alpha__ghost-rule.mdc"
	orphanSkill := ".claude/skills/tessl__ghost-skill/SKILL.md"
	writeFile(t, root, orphanRule, []byte("---\nalwaysApply: true\n---\n\n# Ghost\n"), 0o644)
	writeFile(t, root, orphanSkill, []byte("# Ghost skill\n"), 0o644)

	report := inventoryProject(t, root)
	for _, orphan := range []string{orphanRule, orphanSkill} {
		if classes := pathClasses(report, orphan); len(classes) == 0 {
			t.Errorf("orphan Tessl native %s appears in no class; preserved=%v unmapped=%v ambiguous=%v unsupported=%v",
				orphan, report.Preserved, report.Unmapped, report.Ambiguous, report.Unsupported)
		}
	}
}

// TestHostileUserHookNamingATesslPathIsPreserved writes a user-authored hook
// whose command happens to mention a .tessl/ path. The design note's ownership
// marker is the dispatcher literal (tessl hook run … --schema-version=1); a
// command that merely names a Tessl path is the operator's, and the file has to
// be reported preserved or #2 may rewrite it.
func TestHostileUserHookNamingATesslPathIsPreserved(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeJSON(t, root, ".claude/settings.json", map[string]any{
		"hooks": map[string]any{"SessionStart": []any{
			map[string]any{"hooks": []any{map[string]any{
				"type":    "command",
				"command": `tessl hook run --plugin-path=".tessl/plugins/example/alpha" --event="SessionStart" --agent=claude-code --schema-version=1`,
			}}},
			map[string]any{"hooks": []any{map[string]any{
				"type":    "command",
				"command": "./scripts/notify.sh --state .tessl/state.json",
			}}},
		}},
	})

	report := inventoryProject(t, root)
	if !hasRecord(report.Preserved, ".claude/settings.json", reasonUnmanagedHook) {
		t.Fatalf("user hook naming a .tessl/ path was treated as Tessl-owned; preserved = %#v", report.Preserved)
	}
}

// TestHostileUserSectionInsideTesslSpanIsRetained puts operator content under a
// deeper heading inside the Tessl-managed span, where the span runs to EOF. The
// note requires that extra content inside the span make the file ambiguous and
// retained, and the user prefix stays preserved.
func TestHostileUserSectionInsideTesslSpanIsRetained(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeAgentsMD(t, root, "# User title\n\nUser prose lives here.\n\n", "### Operator notes\n\nKeep these bytes.\n")

	report := inventoryProject(t, root)
	if !hasRecord(report.Ambiguous, "AGENTS.md", reasonTesslSpanExtra) {
		t.Fatalf("user section inside the Tessl span must be ambiguous; ambiguous = %#v", report.Ambiguous)
	}
	if !hasRecord(report.Preserved, "AGENTS.md", reasonUnmanagedPrefix) {
		t.Fatalf("user prefix must stay preserved; preserved = %#v", report.Preserved)
	}
}

// TestHostileTwoPackagesClaimOneRuleID checks the negative half of the duplicate
// rule: Cursor rule natives carry the workspace and package, so two packages
// declaring the same rule id do not collide and neither goes ambiguous.
func TestHostileTwoPackagesClaimOneRuleID(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0", "example/beta": "2.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writePluginJSON(t, root, "example/beta", map[string]any{
		"name":    "example/beta",
		"version": "2.0.0",
		"rules":   []string{"rules/always-rule.md"},
		"skills":  []string{},
	})
	writeRuleFile(t, root, "example/beta", "always-rule", "---\nalwaysApply: true\n---\n", "# Beta always\n")
	writeCursorMDC(t, root, "example/alpha", "always-rule", ruleSource(t, root, "example/alpha", "always-rule"))
	writeCursorMDC(t, root, "example/beta", "always-rule", ruleSource(t, root, "example/beta", "always-rule"))

	report := inventoryProject(t, root)
	if class := artifactClass(t, report, "example/alpha", kindRule, "always-rule"); class != classMigratable {
		t.Fatalf("alpha rule = %s, want migratable (Cursor rule natives are package-qualified)", class)
	}
	if class := artifactClass(t, report, "example/beta", kindRule, "always-rule"); class != classMigratable {
		t.Fatalf("beta rule = %s, want migratable", class)
	}
	alpha := artifactNatives(t, report, "example/alpha", kindRule, "always-rule")
	beta := artifactNatives(t, report, "example/beta", kindRule, "always-rule")
	if len(alpha) != 1 || len(beta) != 1 || alpha[0] == beta[0] {
		t.Fatalf("package-qualified natives collided: alpha=%v beta=%v", alpha, beta)
	}
}

// TestHostileOneArtifactAcrossNativeFilenames is the effective-configuration
// row at report level rather than at the helper level: one skill reached under
// three native filenames and one rule reached through the plugin manifest, the
// RULES.md include, and a Cursor .mdc stay single artifacts with one digest.
func TestHostileOneArtifactAcrossNativeFilenames(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeRulesMD(t, root, []string{"example/alpha/rules/always-rule.md"})
	writeCursorMDC(t, root, "example/alpha", "always-rule", ruleSource(t, root, "example/alpha", "always-rule"))
	writeNativeSkills(t, root, ".claude/skills", "example/alpha", "review-change", true)
	writeNativeSkills(t, root, ".codex/skills", "example/alpha", "review-change", true)
	writeNativeSkills(t, root, ".cursor/skills", "example/alpha", "review-change", true)

	report := inventoryProject(t, root)
	skills := artifactsOfKind(report, "example/alpha", kindSkill)
	if len(skills) != 1 {
		t.Fatalf("three native filenames produced %d skill artifacts: %#v", len(skills), skills)
	}
	if len(skills[0].Natives) != 3 || skills[0].Digest == "" {
		t.Fatalf("skill artifact = %+v, want one digest and three native paths", skills[0])
	}
	rules := artifactsOfKind(report, "example/alpha", kindRule)
	count := 0
	for _, rule := range rules {
		if rule.ID == "always-rule" {
			count++
			if rule.Digest == "" || rule.Classification != classMigratable {
				t.Fatalf("rule artifact = %+v", rule)
			}
		}
	}
	if count != 1 {
		t.Fatalf("rule reached three ways produced %d artifacts: %#v", count, rules)
	}
}

// TestHostileReportIsDeterministicAndPOSIX runs the inventory twice over the
// same tree and compares marshaled bytes, then checks that no host path,
// separator, or absolute path leaked into the report.
func TestHostileReportIsDeterministicAndPOSIX(t *testing.T) {
	t.Parallel()

	root := seedMaximalConsumer(t)
	first, err := json.Marshal(inventoryProject(t, root))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(inventoryProject(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("inventory is not deterministic\nfirst  = %s\nsecond = %s", first, second)
	}
	if strings.Contains(string(first), root) {
		t.Fatalf("report leaked the host project path %q: %s", root, first)
	}
	for _, path := range reportPaths(inventoryProject(t, root)) {
		if path == "" {
			t.Fatal("report carries an empty path")
		}
		if strings.HasPrefix(path, "/") || strings.Contains(path, `\`) {
			t.Errorf("report path %q is not a relative POSIX path", path)
		}
	}
}

// TestHostilePreservedAndUnmappedStayDisjoint holds the invariant the report
// contract rests on: a path is either the operator's to keep or Tessl-owned
// with no #4 home, never both, over a fixture that exercises every classifier.
func TestHostilePreservedAndUnmappedStayDisjoint(t *testing.T) {
	t.Parallel()

	report := inventoryProject(t, seedMaximalConsumer(t))
	unmapped := map[string]string{}
	for _, record := range report.Unmapped {
		unmapped[record.Path] = record.Reason
	}
	for _, record := range report.Preserved {
		if reason, ok := unmapped[record.Path]; ok {
			t.Errorf("path %s is both preserved (%s) and unmapped (%s)", record.Path, record.Reason, reason)
		}
	}
	assertPreservedHoldsUserFiles(t, report)
	for _, native := range []string{".claude/settings.json", ".cursor/hooks.json", ".codex/config.toml", ".gemini/settings.json"} {
		if !hasRecord(report.Preserved, native, reasonUnmanagedHook) {
			t.Errorf("user hook beside a Tessl hook in %s is not preserved: %#v", native, report.Preserved)
		}
	}
}

// TestHostileDeclaredDependencyWithNoInstalledTree covers the shape a fresh
// clone has: tessl.json names dependencies while .tessl/plugins is gitignored
// and absent. The inventory must not fail, and it must not invent packages.
func TestHostileDeclaredDependencyWithNoInstalledTree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	writeFile(t, root, "AGENTS.md", []byte("# User title\n\nUser prose.\n"), 0o644)

	report := inventoryProject(t, root)
	if len(report.Packages) != 0 {
		t.Fatalf("uninstalled dependency must not become a package: %#v", report.Packages)
	}
	if !hasRecord(report.Preserved, "AGENTS.md", reasonUnmanagedPrefix) {
		t.Fatalf("user file still preserved with no packages installed: %#v", report.Preserved)
	}
}

func seedMaximalConsumer(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0", "example/beta": "2.0.0"})
	seedAlpha(t, root, alphaPlugin(true, []string{"skills/review-change"}, "https://github.com/example/alpha"))
	seedBeta(t, root, betaTile("skills/legacy-skill/SKILL.md"))
	writeRulesMD(t, root, []string{"example/alpha/rules/always-rule.md", "example/alpha/rules/gone.md"})
	writeAgentsMD(t, root, "# User title\n\nUser prose lives here.\n\n", "")
	writeFile(t, root, "CLAUDE.md", []byte("# Claude notes\n\n@AGENTS.md\n"), 0o644)
	writeFile(t, root, pluginPath("example/alpha", "tessl-package.json"), []byte(`{"name":"example/alpha"}`+"\n"), 0o644)
	writeFile(t, root, pluginPath("example/alpha", "README.md"), []byte("plugin readme\n"), 0o644)
	writeGitignoreTesslBlock(t, root)
	writeClaudeSettings(t, root, true)
	writeCursorHooks(t, root, true)
	writeCodexTOML(t, root, true, true)
	writeFile(t, root, ".claude/settings.local.json", []byte(`{"permissions":{}}`+"\n"), 0o644)
	writeJSON(t, root, ".cursor/mcp.json", map[string]any{"mcpServers": map[string]any{"tessl": map[string]any{}}})
	writeJSON(t, root, ".gemini/settings.json", map[string]any{
		"hooks": map[string]any{"SessionStart": []any{
			map[string]any{"hooks": []any{map[string]any{
				"type":    "command",
				"command": `tessl hook run --plugin-path=".tessl/plugins/example/alpha" --event="SessionStart" --agent=gemini --schema-version=1`,
			}}},
			map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "user-hook.sh"}}},
		}},
	})
	writeNativeSkills(t, root, ".claude/skills", "example/alpha", "review-change", true)
	writeNativeSkills(t, root, ".cursor/skills", "example/alpha", "review-change", true)
	writeCursorMDC(t, root, "example/alpha", "always-rule", ruleSource(t, root, "example/alpha", "always-rule"))
	return root
}

func findArtifact(report Report, pkg, kind, id string) (string, bool) {
	for _, item := range report.Packages {
		if item.Name != pkg && item.TesslIdentity != pkg {
			continue
		}
		for _, artifact := range item.Artifacts {
			if artifact.Kind == kind && artifact.ID == id {
				return artifact.Classification, true
			}
		}
	}
	return "", false
}

func artifactsOfKind(report Report, pkg, kind string) []ArtifactReport {
	var artifacts []ArtifactReport
	for _, item := range report.Packages {
		if item.Name != pkg && item.TesslIdentity != pkg {
			continue
		}
		for _, artifact := range item.Artifacts {
			if artifact.Kind == kind {
				artifacts = append(artifacts, artifact)
			}
		}
	}
	return artifacts
}

func artifactNatives(t *testing.T, report Report, pkg, kind, id string) []string {
	t.Helper()
	for _, item := range report.Packages {
		if item.Name != pkg && item.TesslIdentity != pkg {
			continue
		}
		for _, artifact := range item.Artifacts {
			if artifact.Kind == kind && artifact.ID == id {
				return artifact.Natives
			}
		}
	}
	t.Fatalf("missing artifact %s %s %s", pkg, kind, id)
	return nil
}

func pathClasses(report Report, path string) []string {
	var classes []string
	for _, group := range []struct {
		name    string
		records []PathRecord
	}{
		{classMigratable, nil},
		{"preserved", report.Preserved},
		{classUnmapped, report.Unmapped},
		{classAmbiguous, report.Ambiguous},
		{classUnsupported, report.Unsupported},
	} {
		for _, record := range group.records {
			if record.Path == path {
				classes = append(classes, group.name)
				break
			}
		}
	}
	return classes
}

func reportPaths(report Report) []string {
	var paths []string
	for _, agent := range report.Agents {
		paths = append(paths, agent.Evidence...)
	}
	for _, pkg := range report.Packages {
		for _, artifact := range pkg.Artifacts {
			paths = append(paths, artifact.Natives...)
		}
	}
	for _, records := range [][]PathRecord{report.Preserved, report.Unmapped, report.Ambiguous, report.Unsupported} {
		for _, record := range records {
			paths = append(paths, record.Path)
		}
	}
	return paths
}
