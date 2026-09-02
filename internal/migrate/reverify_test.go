package migrate

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestReverifyUserNativeEntriesStayPreserved(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeCursorMDC(t, root, "example/alpha", "always-rule", ruleSource(t, root, "example/alpha", "always-rule"))
	writeNativeSkills(t, root, ".claude/skills", "example/alpha", "review-change", false)

	userRule := ".cursor/rules/operator-rule.mdc"
	operatorSkill := ".claude/skills/operator-skill"
	writeFile(t, root, userRule, []byte("---\nalwaysApply: true\n---\n\n# Operator rule\n"), 0o644)
	writeFile(t, root, operatorSkill+"/SKILL.md", []byte("# Operator skill\n"), 0o644)

	report := inventoryProject(t, root)
	if !hasPath(report.Preserved, userRule) {
		t.Errorf("user .mdc beside Tessl natives is absent from preserved: %#v", report.Preserved)
	}
	if !hasPathAtOrBelow(report.Preserved, operatorSkill) {
		t.Errorf("operator skill beside Tessl natives is absent from preserved: %#v", report.Preserved)
	}
	for _, record := range report.Unmapped {
		if record.Path == userRule || record.Path == operatorSkill || strings.HasPrefix(record.Path, operatorSkill+"/") {
			t.Errorf("operator-owned native path was classified unmapped: %+v", record)
		}
	}
}

func TestReverifyOpenHandsAndGeminiOrphansAreUnmapped(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	orphans := []string{
		".openhands/skills/tessl-ghost/SKILL.md",
		".gemini/skills/tessl__ghost/SKILL.md",
	}
	for _, orphan := range orphans {
		writeFile(t, root, orphan, []byte("# Orphan\n"), 0o644)
	}

	report := inventoryProject(t, root)
	for _, orphan := range orphans {
		if !hasRecord(report.Unmapped, orphan, reasonOrphanNative) {
			t.Errorf("orphan %s is not unmapped/%s: %#v", orphan, reasonOrphanNative, report.Unmapped)
		}
	}
}

func TestReverifyClaimedCopiedSkillExtraIsNotOrphan(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeNativeSkills(t, root, ".claude/skills", "example/alpha", "review-change", false)
	extra := ".claude/skills/tessl__review-change/NOTES.md"
	writeFile(t, root, extra, []byte("operator notes\n"), 0o644)

	report := inventoryProject(t, root)
	if !hasRecord(report.Preserved, extra, reasonUnmanagedSkill) {
		t.Fatalf("claimed copied-skill extra is not preserved: %#v", report.Preserved)
	}
	if hasRecord(report.Unmapped, extra, reasonOrphanNative) {
		t.Fatalf("claimed copied-skill extra was classified as an orphan: %#v", report.Unmapped)
	}
}

func TestReverifyOrphanReportIsDeterministicAndPOSIX(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	fixtures := []struct {
		relative string
		content  string
	}{
		{relative: ".cursor/rules/tessl__rule__example__alpha__zeta.mdc", content: "# Zeta\n"},
		{relative: ".gemini/skills/tessl__alpha/references/guide.md", content: "guide\n"},
		{relative: ".openhands/skills/tessl-middle/SKILL.md", content: "# Middle\n"},
	}
	for _, fixture := range fixtures {
		writeFile(t, root, fixture.relative, []byte(fixture.content), 0o644)
	}

	first, err := json.Marshal(inventoryProject(t, root))
	if err != nil {
		t.Fatal(err)
	}
	secondReport := inventoryProject(t, root)
	second, err := json.Marshal(secondReport)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("orphan-bearing report is not deterministic\nfirst:  %s\nsecond: %s", first, second)
	}
	if bytes.Contains(first, []byte(root)) {
		t.Fatalf("orphan-bearing report leaked host root %q: %s", root, first)
	}
	for _, filename := range reportPaths(secondReport) {
		if filename == "" || strings.HasPrefix(filename, "/") || strings.Contains(filename, `\`) {
			t.Errorf("report path %q is not relative POSIX", filename)
		}
	}
	for index := 1; index < len(secondReport.Unmapped); index++ {
		previous := secondReport.Unmapped[index-1].Path + "\x00" + secondReport.Unmapped[index-1].Reason
		current := secondReport.Unmapped[index].Path + "\x00" + secondReport.Unmapped[index].Reason
		if current < previous {
			t.Errorf("unmapped paths are not sorted: %q precedes %q", previous, current)
		}
	}
}

func TestReverifyUnreadableDeclaredSkillHasNoPhantomChild(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change", "skills/broken"}, ""))
	writeFile(t, root, pluginPath("example/alpha", "skills/broken/SKILL.md"), []byte("# Broken\n"), 0)
	writeFile(t, root, pluginPath("example/alpha", "skills/broken/references/table.md"), []byte("table\n"), 0o644)

	report := inventoryProject(t, root)
	if class, ok := findArtifact(report, "example/alpha", kindSkill, "broken"); !ok || class == classMigratable {
		t.Fatalf("unreadable declared skill = class %q present %t, want reported non-migratable: %#v", class, ok, report.Packages)
	}
	for _, pkg := range report.Packages {
		if pkg.TesslIdentity != "example/alpha" {
			continue
		}
		for _, artifact := range pkg.Artifacts {
			if artifact.Kind == kindSkill && artifact.ID != "review-change" && artifact.ID != "broken" {
				t.Errorf("unreadable declared skill produced phantom child artifact: %+v", artifact)
			}
		}
	}
}

func TestReverifyDispatcherLiteralInsideUserHookIsPreserved(t *testing.T) {
	t.Parallel()

	commands := []struct {
		name    string
		command string
	}{
		{name: "argument", command: `./scripts/notify.sh --message "tessl hook run"`},
		{name: "comment", command: `./scripts/notify.sh # document tessl hook run for operators`},
	}
	for _, test := range commands {
		test := test
		t.Run(test.name, func(t *testing.T) {
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
						"command": test.command,
					}}},
				}},
			})

			report := inventoryProject(t, root)
			if !hasRecord(report.Preserved, ".claude/settings.json", reasonUnmanagedHook) {
				t.Fatalf("user hook with dispatcher words in %s was treated as Tessl-owned: %#v", test.name, report.Preserved)
			}
		})
	}
}

func hasPath(records []PathRecord, filename string) bool {
	for _, record := range records {
		if record.Path == filename {
			return true
		}
	}
	return false
}

func hasPathAtOrBelow(records []PathRecord, root string) bool {
	for _, record := range records {
		if record.Path == root || strings.HasPrefix(record.Path, root+"/") {
			return true
		}
	}
	return false
}
