package migrate

import "testing"

// A declared skill whose own SKILL.md is unreadable, beside a child directory
// that does have a readable SKILL.md. Classifying the read failure as
// missing-skill would succeed with "broken" absent or "child" promoted to a
// phantom; the inventory must fail instead.
func TestUnreadableSkillMarkdownBesideRealChildSkill(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change", "skills/broken"}, ""))
	writeFile(t, root, pluginPath("example/alpha", "skills/broken/SKILL.md"), []byte("# Broken\n"), 0)
	writeFile(t, root, pluginPath("example/alpha", "skills/broken/child/SKILL.md"), []byte("# Child\n"), 0o644)

	report, err := Inventory(openSnapshot(t, root))
	if err == nil {
		t.Fatalf("unreadable declared SKILL.md succeeded with artifacts %#v, want a read error", report.Packages)
	}
}

// A declared container whose child skill has an unreadable SKILL.md. Treating
// that read failure as absence would drop "locked" and surface its bytes as
// undeclared-plugin-file; the inventory must fail instead of emitting a
// partial report.
func TestSkillContainerChildWithUnreadableMarkdown(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	writePluginJSON(t, root, "example/alpha", map[string]any{
		"name": "example/alpha", "version": "1.0.0",
		"rules": []string{}, "skills": []string{"skills"},
	})
	writeSkillTree(t, root, "example/alpha", "good", map[string]string{"SKILL.md": "# Good\n"})
	writeFile(t, root, pluginPath("example/alpha", "skills/locked/SKILL.md"), []byte("# Locked\n"), 0)

	report, err := Inventory(openSnapshot(t, root))
	if err == nil {
		t.Fatalf("unreadable container-child SKILL.md succeeded with artifacts %#v, want a read error", report.Packages)
	}
}
