package migrate

import "testing"

// A declared skill whose own SKILL.md is unreadable, beside a child directory
// that does have a readable SKILL.md. Restoring the swallow drops "broken" and
// promotes "child" to a phantom artifact.
func TestUnreadableSkillMarkdownBesideRealChildSkill(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change", "skills/broken"}, ""))
	writeFile(t, root, pluginPath("example/alpha", "skills/broken/SKILL.md"), []byte("# Broken\n"), 0)
	writeFile(t, root, pluginPath("example/alpha", "skills/broken/child/SKILL.md"), []byte("# Child\n"), 0o644)

	report := inventoryProject(t, root)
	if _, ok := findArtifact(report, "example/alpha", kindSkill, "broken"); !ok {
		t.Errorf("declared skill %q vanished; artifacts = %#v", "broken", report.Packages[0].Artifacts)
	}
	if _, ok := findArtifact(report, "example/alpha", kindSkill, "child"); ok {
		t.Errorf("child of a declared skill became a phantom artifact; artifacts = %#v", report.Packages[0].Artifacts)
	}
}

// A declared container whose child skill has an unreadable SKILL.md. Restoring
// the swallow drops "locked" from the inventory and surfaces its bytes as
// undeclared-plugin-file — BLOCKING 1's exact failure signature.
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

	report := inventoryProject(t, root)
	if _, ok := findArtifact(report, "example/alpha", kindSkill, "good"); !ok {
		t.Errorf("readable container child missing; artifacts = %#v", report.Packages[0].Artifacts)
	}
	if _, ok := findArtifact(report, "example/alpha", kindSkill, "locked"); !ok {
		t.Errorf("container child with unreadable SKILL.md vanished; artifacts = %#v", report.Packages[0].Artifacts)
	}
}
