package tesslplugin

import (
	"encoding/json"
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
