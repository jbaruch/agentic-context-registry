package migrate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

func openSnapshot(t *testing.T, dir string) adapter.DirectorySnapshot {
	t.Helper()
	snapshot, err := adapter.NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := snapshot.Close(); err != nil {
			t.Errorf("close snapshot: %v", err)
		}
	})
	return snapshot
}

func writeFile(t *testing.T, root, relative string, content []byte, mode os.FileMode) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, mode); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, root, relative string, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, relative, append(payload, '\n'), 0o644)
}

func writeTesslJSON(t *testing.T, root string, dependencies map[string]string) {
	t.Helper()
	deps := make(map[string]any, len(dependencies))
	for name, version := range dependencies {
		deps[name] = map[string]string{"version": version}
	}
	writeJSON(t, root, "tessl.json", map[string]any{
		"name":         "consumer",
		"mode":         "vendored",
		"dependencies": deps,
	})
}

func writePluginJSON(t *testing.T, root, identity string, plugin map[string]any) {
	t.Helper()
	writeJSON(t, root, ".tessl/plugins/"+identity+"/.tessl-plugin/plugin.json", plugin)
}

func writeTileJSON(t *testing.T, root, identity string, tile map[string]any) {
	t.Helper()
	writeJSON(t, root, ".tessl/plugins/"+identity+"/tile.json", tile)
}

func writeRuleFile(t *testing.T, root, identity, id, frontmatter, body string) {
	t.Helper()
	content := body
	if frontmatter != "" {
		content = frontmatter + body
	}
	writeFile(t, root, ".tessl/plugins/"+identity+"/rules/"+id+".md", []byte(content), 0o644)
}

func writeSkillTree(t *testing.T, root, identity, id string, files map[string]string) {
	t.Helper()
	base := ".tessl/plugins/" + identity + "/skills/" + id
	if _, ok := files["SKILL.md"]; !ok {
		writeFile(t, root, base+"/SKILL.md", []byte("# "+id+"\n"), 0o644)
	}
	for relative, content := range files {
		mode := os.FileMode(0o644)
		if filepath.Ext(relative) == ".sh" {
			mode = 0o755
		}
		writeFile(t, root, base+"/"+relative, []byte(content), mode)
	}
}

func writeHookScript(t *testing.T, root, identity, name, body string) {
	t.Helper()
	writeFile(t, root, ".tessl/plugins/"+identity+"/hooks/"+name, []byte(body), 0o755)
}

func writeRulesMD(t *testing.T, root string, includes []string) {
	t.Helper()
	var builder strings.Builder
	builder.WriteString("# Agent Rules\n\n")
	for _, include := range includes {
		fmt.Fprintf(&builder, "## %s\n\n@plugins/%s\n\n", include, include)
	}
	writeFile(t, root, ".tessl/RULES.md", []byte(builder.String()), 0o644)
}

func writeCursorMDC(t *testing.T, root, identity, id string, source []byte) {
	t.Helper()
	writeCursorMDCMutated(t, root, identity, id, source, nil)
}

func writeCursorMDCMutated(t *testing.T, root, identity, id string, source []byte, mutate func([]byte) []byte) {
	t.Helper()
	workspace, pkg, _ := strings.Cut(identity, "/")
	relative := ".cursor/rules/tessl__rule__" + workspace + "__" + pkg + "__" + id + ".mdc"
	content := append([]byte("---\nalwaysApply: true\n---\n\n"), source...)
	if mutate != nil {
		content = mutate(content)
	}
	writeFile(t, root, relative, content, 0o644)
}

func writeNativeSkills(t *testing.T, root, nativeDir, identity, id string, symlink bool) {
	t.Helper()
	pluginSkill := filepath.Join(root, filepath.FromSlash(pluginPath(identity, "skills/"+id)))
	native := filepath.Join(root, filepath.FromSlash(nativeDir), "tessl__"+id)
	if err := os.MkdirAll(filepath.Dir(native), 0o755); err != nil {
		t.Fatal(err)
	}
	if symlink {
		rel, err := filepath.Rel(filepath.Dir(native), pluginSkill)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(rel, native); err != nil {
			t.Fatal(err)
		}
		return
	}
	copyTree(t, pluginSkill, native)
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			copyTree(t, from, to)
			continue
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Dir(to), entry.Name(), content, info.Mode().Perm())
	}
}

func ruleSource(t *testing.T, root, identity, id string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(pluginPath(identity, "rules/"+id+".md"))))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func pluginPath(identity, relative string) string {
	return ".tessl/plugins/" + identity + "/" + relative
}

func alphaPlugin(hooks bool, skills []string, repository string) map[string]any {
	plugin := map[string]any{
		"name":    "example/alpha",
		"version": "1.0.0",
		"rules":   []string{"rules/always-rule.md", "rules/paths-rule.md"},
		"skills":  skills,
	}
	if repository != "" {
		plugin["repository"] = repository
	}
	if hooks {
		plugin["hooks"] = map[string]any{
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": "bash",
							"args":    []string{"${TESSL_PLUGIN_DIR}/hooks/session-start.sh"},
						},
					},
				},
			},
		}
		plugin["nativeHooks"] = map[string]any{
			"claude-code": map[string]any{
				"Stop": []any{
					map[string]any{
						"hooks": []any{
							map[string]any{
								"type":    "command",
								"command": "bash",
								"args":    []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"},
							},
						},
					},
				},
			},
		}
	}
	return plugin
}

func betaTile(skillPath string) map[string]any {
	return map[string]any{
		"name":    "example/beta",
		"version": "2.0.0",
		"rules": map[string]any{
			"legacy-rule": map[string]string{"rules": "rules/legacy-rule.md"},
		},
		"skills": map[string]any{
			"legacy-skill": map[string]string{"path": skillPath},
		},
	}
}

func seedAlpha(t *testing.T, root string, plugin map[string]any) {
	t.Helper()
	writePluginJSON(t, root, "example/alpha", plugin)
	writeRuleFile(t, root, "example/alpha", "always-rule", "---\nalwaysApply: true\n---\n", "# Always\n")
	writeRuleFile(t, root, "example/alpha", "paths-rule", "---\nalwaysApply: false\napplyTo: \"*.go — Go files\"\ndescription: Paths rule\n---\n", "# Paths\n")
	writeSkillTree(t, root, "example/alpha", "review-change", map[string]string{"SKILL.md": "# Review\n"})
	if _, ok := plugin["hooks"]; ok {
		writeHookScript(t, root, "example/alpha", "session-start.sh", "#!/bin/sh\necho start\n")
		writeHookScript(t, root, "example/alpha", "stop.sh", "#!/bin/sh\necho stop\n")
	}
}

func seedBeta(t *testing.T, root string, tile map[string]any) {
	t.Helper()
	writeTileJSON(t, root, "example/beta", tile)
	writeRuleFile(t, root, "example/beta", "legacy-rule", "---\nalwaysApply: true\n---\n", "# Legacy\n")
	writeSkillTree(t, root, "example/beta", "legacy-skill", map[string]string{"SKILL.md": "# Legacy skill\n"})
}

func loadTestInstalls(t *testing.T, root string) []PackageInstall {
	t.Helper()
	installs, err := LoadInstalls(openSnapshot(t, root))
	if err != nil {
		t.Fatal(err)
	}
	return installs
}

func installByIdentity(t *testing.T, installs []PackageInstall, identity string) PackageInstall {
	t.Helper()
	for _, install := range installs {
		if install.TesslIdentity == identity {
			return install
		}
	}
	t.Fatalf("missing package %s in %#v", identity, installs)
	return PackageInstall{}
}

func declaredIDs(items []DeclaredPath) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func declaredByID(items []DeclaredPath, id string) (DeclaredPath, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return DeclaredPath{}, false
}
