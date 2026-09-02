package migrate

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSymlinkedSkillNotDoubleCounted(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeNativeSkills(t, root, ".claude/skills", "example/alpha", "review-change", true)
	writeNativeSkills(t, root, ".codex/skills", "example/alpha", "review-change", true)
	writeNativeSkills(t, root, ".cursor/skills", "example/alpha", "review-change", true)

	skills := normalizeTestSkills(t, root, "example/alpha")
	if len(skills) != 1 {
		t.Fatalf("skills = %#v, want one plugin-tree artifact", skills)
	}
	if skills[0].ID != "review-change" || skills[0].Digest == "" {
		t.Fatalf("skill = %+v", skills[0])
	}
	wantNatives := []string{
		".claude/skills/tessl__review-change",
		".codex/skills/tessl__review-change",
		".cursor/skills/tessl__review-change",
	}
	if !reflect.DeepEqual(skills[0].Natives, wantNatives) {
		t.Fatalf("natives = %v, want %v", skills[0].Natives, wantNatives)
	}
	for _, native := range skills[0].Natives {
		if native == pluginPath("example/alpha", "skills/review-change") {
			t.Fatalf("plugin path must not be a second artifact: %v", skills[0].Natives)
		}
	}
}

func TestEffectiveDigestIndependentOfNativePath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeCursorMDC(t, root, "example/alpha", "always-rule", ruleSource(t, root, "example/alpha", "always-rule"))
	writeRulesMD(t, root, []string{"example/alpha/rules/always-rule.md"})
	writeNativeSkills(t, root, ".claude/skills", "example/alpha", "review-change", false)
	writeNativeSkills(t, root, ".codex/skills", "example/alpha", "review-change", false)
	writeNativeSkills(t, root, ".agents/skills", "example/alpha", "review-change", true)

	install := installByIdentity(t, loadTestInstalls(t, root), "example/alpha")
	snapshot := openSnapshot(t, root)
	rules, err := NormalizeRules(snapshot, install)
	if err != nil {
		t.Fatal(err)
	}
	skills, err := NormalizeSkills(snapshot, install)
	if err != nil {
		t.Fatal(err)
	}
	rule := ruleByID(t, rules, "always-rule")
	if rule.Digest == "" || len(rule.Natives) != 1 {
		t.Fatalf("rule digest/natives = %+v", rule)
	}
	skill := skills[0]
	if skill.Digest == "" {
		t.Fatalf("skill digest missing: %+v", skill)
	}
	if len(skill.Natives) != 3 {
		t.Fatalf("skill natives = %v, want three agent paths", skill.Natives)
	}
	claudeFiles, escaped, err := readSkillTree(snapshot, ".claude/skills/tessl__review-change")
	if err != nil || escaped {
		t.Fatalf("claude native tree: escaped=%v err=%v", escaped, err)
	}
	codexFiles, escaped, err := readSkillTree(snapshot, ".codex/skills/tessl__review-change")
	if err != nil || escaped {
		t.Fatalf("codex native tree: escaped=%v err=%v", escaped, err)
	}
	claudeDigest := skillDigest(".claude/skills/tessl__review-change", claudeFiles)
	codexDigest := skillDigest(".codex/skills/tessl__review-change", codexFiles)
	if claudeDigest != codexDigest {
		t.Fatalf("two native spellings of one body yielded %s and %s", claudeDigest, codexDigest)
	}
	if skill.Digest != claudeDigest {
		t.Fatalf("plugin digest %s != native digest %s", skill.Digest, claudeDigest)
	}
}

func TestExtraUnmanagedSkillFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeNativeSkills(t, root, ".claude/skills", "example/alpha", "review-change", false)
	writeFile(t, root, ".claude/skills/tessl__review-change/NOTES.md", []byte("operator notes\n"), 0o644)

	skill := normalizeTestSkills(t, root, "example/alpha")[0]
	if skill.Ambiguous || skill.Digest == "" {
		t.Fatalf("copied skill with extra file should stay migratable: %+v", skill)
	}
	if !reflect.DeepEqual(skill.ExtraFiles, []string{".claude/skills/tessl__review-change/NOTES.md"}) {
		t.Fatalf("extra files = %v", skill.ExtraFiles)
	}
}

func TestDeclaredSkillWithoutReadableMarkdown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		seed func(t *testing.T, root string)
	}{
		{
			name: "unreadableSkillMarkdown",
			seed: func(t *testing.T, root string) {
				writeFile(t, root, pluginPath("example/alpha", "skills/broken/SKILL.md"), []byte("# Broken\n"), 0)
			},
		},
		{
			name: "absentBesideSiblingFile",
			seed: func(t *testing.T, root string) {
				writeFile(t, root, pluginPath("example/alpha", "skills/broken/reference.md"), []byte("reference\n"), 0o644)
			},
		},
		{
			name: "absentBesideSubdirectory",
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
			if class := artifactClass(t, report, "example/alpha", kindSkill, "broken"); class != classAmbiguous {
				t.Fatalf("declared skill without readable SKILL.md = %s, want ambiguous", class)
			}
			if _, ok := findArtifact(report, "example/alpha", kindSkill, "references"); ok {
				t.Fatal("subdirectory without SKILL.md must not become a phantom skill")
			}
			skill := skillByID(t, normalizeTestSkills(t, root, "example/alpha"), "broken")
			if skill.Reason != reasonMissingSkill {
				t.Fatalf("skill = %+v, want reason %s", skill, reasonMissingSkill)
			}
		})
	}
}

func TestSkillContainerExpandsChildSkills(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	writePluginJSON(t, root, "example/alpha", map[string]any{
		"name":    "example/alpha",
		"version": "1.0.0",
		"rules":   []string{},
		"skills":  []string{"skills"},
	})
	writeSkillTree(t, root, "example/alpha", "review-change", map[string]string{"SKILL.md": "# Review\n"})
	writeSkillTree(t, root, "example/alpha", "other-skill", map[string]string{"SKILL.md": "# Other\n"})

	report := inventoryProject(t, root)
	if class := artifactClass(t, report, "example/alpha", kindSkill, "review-change"); class != classMigratable {
		t.Fatalf("container child review-change = %s, want migratable", class)
	}
	if class := artifactClass(t, report, "example/alpha", kindSkill, "other-skill"); class != classMigratable {
		t.Fatalf("container child other-skill = %s, want migratable", class)
	}
	if _, ok := findArtifact(report, "example/alpha", kindSkill, "skills"); ok {
		t.Fatal("genuine container directory must not become a skill artifact")
	}
}

func skillByID(t *testing.T, skills []NormalizedSkill, id string) NormalizedSkill {
	t.Helper()
	for _, skill := range skills {
		if skill.ID == id {
			return skill
		}
	}
	t.Fatalf("missing skill %s in %#v", id, skills)
	return NormalizedSkill{}
}

func TestCopiedSkillDivergenceIsAmbiguous(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeNativeSkills(t, root, ".claude/skills", "example/alpha", "review-change", false)
	if err := os.WriteFile(filepath.Join(root, ".claude/skills/tessl__review-change/SKILL.md"), []byte("# edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	skill := normalizeTestSkills(t, root, "example/alpha")[0]
	if !skill.Ambiguous || skill.Reason != reasonNativeDivergence {
		t.Fatalf("diverged copy = %+v", skill)
	}
}

func normalizeTestSkills(t *testing.T, root, identity string) []NormalizedSkill {
	t.Helper()
	install := installByIdentity(t, loadTestInstalls(t, root), identity)
	skills, err := NormalizeSkills(openSnapshot(t, root), install)
	if err != nil {
		t.Fatal(err)
	}
	return skills
}
