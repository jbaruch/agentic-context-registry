package tesslplugin

import (
	"errors"
	"testing"
)

func TestReadsPluginJSONShape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name":        "example/alpha",
		"version":     "1.0.0",
		"description": "alpha plugin",
		"private":     false,
		"repository":  "https://github.com/example/alpha",
		"homepage":    "https://example.test",
		"license":     "Apache-2.0",
		"author":      map[string]string{"name": "Ada", "email": "ada@example.test", "url": "https://ada.example.test"},
		"rules":       []string{"rules/always.md", "rules/paths.md"},
		"skills":      []string{"skills/review-change"},
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/check.sh"},
			}}}},
		},
		"nativeHooks": map[string]any{
			"claude-code": map[string]any{
				"Stop": []any{map[string]any{"hooks": []any{map[string]any{
					"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"},
				}}}},
			},
		},
	})

	sources, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if sources.Plugin == nil || sources.Tile != nil {
		t.Fatalf("sources plugin=%v tile=%v", sources.Plugin != nil, sources.Tile != nil)
	}
	plugin := sources.Plugin
	if plugin.Name != "example/alpha" || plugin.Version != "1.0.0" || plugin.Description != "alpha plugin" {
		t.Fatalf("identity = %#v", plugin)
	}
	if plugin.Repository != "https://github.com/example/alpha" || plugin.Homepage != "https://example.test" || plugin.License != "Apache-2.0" {
		t.Fatalf("provenance = %#v", plugin)
	}
	if plugin.Author == nil || plugin.Author.Name != "Ada" {
		t.Fatalf("author = %#v", plugin.Author)
	}
	if plugin.Private == nil || *plugin.Private {
		t.Fatalf("private = %v", plugin.Private)
	}
	if plugin.Rules.Kind != PathSpecList || len(plugin.Rules.List) != 2 {
		t.Fatalf("rules = %#v", plugin.Rules)
	}
	if plugin.Skills.Kind != PathSpecList || plugin.Skills.List[0] != "skills/review-change" {
		t.Fatalf("skills = %#v", plugin.Skills)
	}
	if _, ok := plugin.Hooks["SessionStart"]; !ok {
		t.Fatalf("hooks = %#v", plugin.Hooks)
	}
	if _, ok := plugin.NativeHooks["claude-code"]["Stop"]; !ok {
		t.Fatalf("nativeHooks = %#v", plugin.NativeHooks)
	}
}

func TestReadsTileJSONShape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTileJSON(t, root, map[string]any{
		"name":    "example/beta",
		"version": "2.0.0",
		"summary": "beta plugin",
		"rules": map[string]any{
			"named-rule": map[string]string{"rules": "rules/other.md"},
		},
		"skills": map[string]any{
			"named-skill": map[string]string{"path": "skills/review-change/SKILL.md"},
		},
	})

	sources, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if sources.Tile == nil || sources.Plugin != nil {
		t.Fatalf("sources plugin=%v tile=%v", sources.Plugin != nil, sources.Tile != nil)
	}
	tile := sources.Tile
	if tile.Name != "example/beta" || tile.Summary != "beta plugin" {
		t.Fatalf("tile = %#v", tile)
	}
	if tile.Rules.Kind != PathSpecNamed || tile.Rules.Named[0].ID != "named-rule" || tile.Rules.Named[0].Path != "rules/other.md" {
		t.Fatalf("rules = %#v", tile.Rules)
	}
	if tile.Skills.Kind != PathSpecNamed || tile.Skills.Named[0].Path != "skills/review-change/SKILL.md" {
		t.Fatalf("skills = %#v", tile.Skills)
	}
}

func TestReadsDirectoryForm(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name":    "example/gamma",
		"version": "1.0.0",
		"rules":   "rules/",
		"skills":  "skills/",
	})

	sources, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if sources.Plugin.Rules.Kind != PathSpecDirectory || sources.Plugin.Rules.Directory != "rules/" {
		t.Fatalf("rules = %#v", sources.Plugin.Rules)
	}
	if sources.Plugin.Skills.Kind != PathSpecDirectory || sources.Plugin.Skills.Directory != "skills/" {
		t.Fatalf("skills = %#v", sources.Plugin.Skills)
	}
}

func TestTrailingTopLevelJSONIsRejected(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	payload := []byte(`{"name":"example/alpha","version":"1.0.0"}{"extra":true}` + "\n")
	writeFile(t, root, pluginManifestRel, payload, 0o644)

	_, err := Read(root)
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeUnknownField {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownKeyIsRejected(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name":        "example/alpha",
		"version":     "1.0.0",
		"description": "alpha",
		"mystery":     true,
	})

	_, err := Read(root)
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeUnknownField || conv.Field != "mystery" {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownNestedHookFieldIsRejected(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name":    "example/alpha",
		"version": "1.0.0",
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": "bash", "cwd": "/tmp"}},
			}},
		},
	})

	_, err := Read(root)
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeUnknownField || conv.Field != "cwd" {
		t.Fatalf("err = %v", err)
	}
}

func TestPrivateFalseIsDecoded(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name":    "example/alpha",
		"version": "1.0.0",
		"private": false,
	})

	sources, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if sources.Plugin.Private == nil || *sources.Plugin.Private {
		t.Fatalf("private = %v", sources.Plugin.Private)
	}
}

func TestMissingManifestFails(t *testing.T) {
	t.Parallel()

	_, err := Read(t.TempDir())
	if err == nil {
		t.Fatal("expected missing-manifest error")
	}
}

func TestReadsBothManifestsWithoutMerging(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, pluginShape("example/alpha", "1.0.0"))
	writeTileJSON(t, root, tileShape("example/alpha", "1.0.0"))

	sources, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if sources.Plugin == nil || sources.Tile == nil {
		t.Fatalf("expected both manifests, plugin=%v tile=%v", sources.Plugin != nil, sources.Tile != nil)
	}
}
