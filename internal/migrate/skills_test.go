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
	writeNativeSkills(t, root, ".claude/skills", "example/alpha", "review-change", true)
	writeNativeSkills(t, root, ".codex/skills", "example/alpha", "review-change", true)
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
