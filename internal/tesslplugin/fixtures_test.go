package tesslplugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

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
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, relative, append(payload, '\n'), 0o644)
}

func writePluginJSON(t *testing.T, root string, value any) {
	t.Helper()
	writeJSON(t, root, pluginManifestRel, value)
}

func writeTileJSON(t *testing.T, root string, value any) {
	t.Helper()
	writeJSON(t, root, tileManifestName, value)
}

func pluginShape(name, version string) map[string]any {
	return map[string]any{
		"name":        name,
		"version":     version,
		"description": "example plugin",
		"private":     false,
		"repository":  "https://github.com/" + name,
		"rules":       []string{"rules/always.md"},
		"skills":      []string{"skills/review-change"},
	}
}

func tileShape(name, version string) map[string]any {
	return map[string]any{
		"name":    name,
		"version": version,
		"summary": "example plugin",
		"private": false,
		"rules": map[string]any{
			"always": map[string]string{"rules": "rules/always.md"},
		},
		"skills": map[string]any{
			"review-change": map[string]string{"path": "skills/review-change/SKILL.md"},
		},
	}
}

func writeRuleFile(t *testing.T, root, relative, frontmatter, body string) {
	t.Helper()
	content := "---\n" + frontmatter + "---\n" + body
	writeFile(t, root, relative, []byte(content), 0o644)
}

func writeSkillDir(t *testing.T, root, dir string) {
	t.Helper()
	writeFile(t, root, dir+"/SKILL.md", []byte("# Review\n"), 0o644)
	writeFile(t, root, dir+"/scripts/check.sh", []byte("#!/bin/sh\necho check\n"), 0o755)
	writeFile(t, root, dir+"/references/guide.md", []byte("# Guide\n"), 0o644)
}

func writeHookScript(t *testing.T, root, relative string) {
	t.Helper()
	writeFile(t, root, relative, []byte("#!/bin/sh\necho hook\n"), 0o755)
}

func writeAlwaysAndPathsRules(t *testing.T, root string) {
	t.Helper()
	writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# Always\n")
	writeRuleFile(t, root, "rules/paths.md", "alwaysApply: false\napplyTo: \"skills/**/*.md — when authoring skills\"\n", "# Paths\n")
}

func writeAlphaSources(t *testing.T, root string) {
	t.Helper()
	writeAlwaysAndPathsRules(t, root)
	writeSkillDir(t, root, "skills/review-change")
	writeHookScript(t, root, "hooks/check.sh")
	writeHookScript(t, root, "hooks/stop.sh")
}

func alphaPlugin(hooks bool) map[string]any {
	value := map[string]any{
		"name":        "example/alpha",
		"version":     "1.0.0",
		"description": "alpha plugin",
		"private":     false,
		"repository":  "https://github.com/example/alpha",
		"rules":       []string{"rules/always.md", "rules/paths.md"},
		"skills":      []string{"skills/review-change"},
	}
	if hooks {
		value["hooks"] = map[string]any{
			"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/check.sh", "--fast"},
			}}}},
		}
		value["nativeHooks"] = map[string]any{
			"claude-code": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"},
			}}}}},
			"codex": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": `bash "${TESSL_PLUGIN_DIR}/hooks/stop.sh"`,
			}}}}},
			"cursor": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"},
			}}}}},
		}
	}
	return value
}

func hashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(filename)
			if err != nil {
				return err
			}
			result[rel] = "link→" + filepath.ToSlash(target)
			return nil
		}
		if entry.IsDir() {
			result[rel] = "dir"
			return nil
		}
		content, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		result[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func chmodTree(t *testing.T, root string, dirMode, fileMode os.FileMode) {
	t.Helper()
	var dirs, files []string
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			dirs = append(dirs, filename)
			return nil
		}
		files = append(files, filename)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, filename := range files {
		if err := os.Chmod(filename, fileMode); err != nil {
			t.Fatal(err)
		}
	}
	for index := len(dirs) - 1; index >= 0; index-- {
		if err := os.Chmod(dirs[index], dirMode); err != nil {
			t.Fatal(err)
		}
	}
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
