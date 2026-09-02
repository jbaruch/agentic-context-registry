package tesslplugin

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestConvertsPluginJSONShape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, alphaPlugin(true))
	writeAlphaSources(t, root)

	report, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Wrote || report.SourceManifest != pluginManifest || report.Package != "example/alpha" {
		t.Fatalf("report = %#v", report)
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Name != "example/alpha" || loaded.Source.Repository != "https://github.com/example/alpha" {
		t.Fatalf("loaded = %#v", loaded)
	}
	if len(loaded.Artifacts.Scripts) != 0 {
		t.Fatalf("scripts = %#v", loaded.Artifacts.Scripts)
	}
	if len(loaded.Artifacts.Rules) != 2 || loaded.Artifacts.Rules[0].Activation.Mode != manifest.ActivationAlways {
		t.Fatalf("rules = %#v", loaded.Artifacts.Rules)
	}
	if len(loaded.Artifacts.Skills) != 1 || loaded.Artifacts.Skills[0].Path != "skills/review-change" {
		t.Fatalf("skills = %#v", loaded.Artifacts.Skills)
	}
	if len(loaded.Artifacts.Hooks) != 2 {
		t.Fatalf("hooks = %#v", loaded.Artifacts.Hooks)
	}
}

func TestConvertsTileJSONShape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTileJSON(t, root, map[string]any{
		"name":       "example/beta",
		"version":    "2.0.0",
		"summary":    "beta plugin",
		"private":    false,
		"repository": "https://github.com/example/beta",
		"rules": map[string]any{
			"always": map[string]string{"rules": "rules/always.md"},
		},
		"skills": map[string]any{
			"review-change": map[string]string{"path": "skills/review-change/SKILL.md"},
		},
	})
	writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# Always\n")
	writeSkillDir(t, root, "skills/review-change")

	report, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if report.SourceManifest != tileManifestName {
		t.Fatalf("source = %q", report.SourceManifest)
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Description != "beta plugin" || len(loaded.Artifacts.Hooks) != 0 {
		t.Fatalf("loaded = %#v", loaded)
	}
	if loaded.Artifacts.Skills[0].Path != "skills/review-change" {
		t.Fatalf("skill path = %q", loaded.Artifacts.Skills[0].Path)
	}
}

func TestDirectoryFormRulesAndSkillsExpand(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name":       "example/gamma",
		"version":    "1.0.0",
		"repository": "https://github.com/example/gamma",
		"rules":      "rules/",
		"skills":     "skills/",
	})
	writeAlwaysAndPathsRules(t, root)
	writeSkillDir(t, root, "skills/review-change")
	writeSkillDir(t, root, "skills/other-skill")

	_, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Artifacts.Rules) != 2 || len(loaded.Artifacts.Skills) != 2 {
		t.Fatalf("rules=%d skills=%d", len(loaded.Artifacts.Rules), len(loaded.Artifacts.Skills))
	}
}

func TestTileKeyIsTheArtifactID(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTileJSON(t, root, map[string]any{
		"name":       "example/beta",
		"version":    "2.0.0",
		"summary":    "beta",
		"repository": "https://github.com/example/beta",
		"rules": map[string]any{
			"custom-id": map[string]string{"rules": "rules/always.md"},
		},
	})
	writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# Always\n")

	_, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Artifacts.Rules[0].ID != "custom-id" {
		t.Fatalf("id = %q", loaded.Artifacts.Rules[0].ID)
	}
}

func TestHookBasenameCollisionSuffixesBothWithEvent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name":       "example/alpha",
		"version":    "1.0.0",
		"repository": "https://github.com/example/alpha",
		"rules":      []string{"rules/always.md"},
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/a/run.sh"},
			}}}},
			"Stop": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/b/run.sh"},
			}}}},
		},
	})
	writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# Always\n")
	writeHookScript(t, root, "hooks/a/run.sh")
	writeHookScript(t, root, "hooks/b/run.sh")

	_, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, hook := range loaded.Artifacts.Hooks {
		ids[hook.ID] = true
	}
	if !ids["run-session-start"] || !ids["run-stop"] {
		t.Fatalf("hook ids = %#v", loaded.Artifacts.Hooks)
	}
}

func TestSelfValidationRunsBeforeWrite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name":       "example/alpha",
		"version":    "1.0.0",
		"repository": "https://github.com/example/alpha",
		"rules":      []string{"rules/always.md", "extra/always.md"},
	})
	writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# Always\n")
	writeRuleFile(t, root, "extra/always.md", "alwaysApply: true\n", "# Extra\n")

	_, err := Convert(Options{PackageRoot: root})
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != string(manifest.CodeDuplicateArtifactID) {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, manifest.Filename)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("wrote manifest after validation failure: %v", statErr)
	}
}

func TestSourceTreeHashesUnchanged(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, alphaPlugin(true))
	writeAlphaSources(t, root)
	writeJSON(t, root, "tessl-package.json", map[string]string{"name": "example/alpha"})
	before := hashTree(t, root)

	if _, err := Convert(Options{PackageRoot: root}); err != nil {
		t.Fatal(err)
	}
	after := hashTree(t, root)
	for path, digest := range before {
		if after[path] != digest {
			t.Fatalf("source path %s changed", path)
		}
	}
	if after[manifest.Filename] == "" {
		t.Fatal("missing converted manifest")
	}
}

func TestDryRunUnderReadOnlyTree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, alphaPlugin(true))
	writeAlphaSources(t, root)
	chmodTree(t, root, 0o555, 0o555)
	t.Cleanup(func() { chmodTree(t, root, 0o755, 0o644) })
	before := hashTree(t, root)

	report, err := Convert(Options{PackageRoot: root, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Wrote {
		t.Fatal("dry-run wrote the manifest")
	}
	after := hashTree(t, root)
	if !mapsEqual(before, after) {
		t.Fatalf("dry-run mutated the tree")
	}
}

func TestEmittedPathsDifferOnlyByTheTwoNormalizations(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTileJSON(t, root, map[string]any{
		"name":       "example/beta",
		"version":    "2.0.0",
		"summary":    "beta",
		"repository": "https://github.com/example/beta",
		"skills": map[string]any{
			"review-change": map[string]string{"path": "skills/review-change/SKILL.md"},
		},
	})
	writeSkillDir(t, root, "skills/review-change")

	_, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Artifacts.Skills[0].Path != "skills/review-change" {
		t.Fatalf("unexpected skill path %q", loaded.Artifacts.Skills[0].Path)
	}
}

func TestSecondRunReportsNotWritten(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, alphaPlugin(true))
	writeAlphaSources(t, root)

	first, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	firstBytes, err := os.ReadFile(filepath.Join(root, manifest.Filename))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if first.Wrote == false || second.Wrote {
		t.Fatalf("wrote first=%v second=%v", first.Wrote, second.Wrote)
	}
	secondBytes, err := os.ReadFile(filepath.Join(root, manifest.Filename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatal("second run changed YAML bytes")
	}
}

func TestRenderIsByteStableAcrossShuffledInput(t *testing.T) {
	t.Parallel()

	first := t.TempDir()
	writePluginJSON(t, first, map[string]any{
		"name":       "example/alpha",
		"version":    "1.0.0",
		"repository": "https://github.com/example/alpha",
		"rules":      []string{"rules/paths.md", "rules/always.md"},
		"skills":     []string{"skills/review-change"},
	})
	writeAlwaysAndPathsRules(t, first)
	writeSkillDir(t, first, "skills/review-change")
	if _, err := Convert(Options{PackageRoot: first}); err != nil {
		t.Fatal(err)
	}

	second := t.TempDir()
	writePluginJSON(t, second, map[string]any{
		"name":       "example/alpha",
		"version":    "1.0.0",
		"repository": "https://github.com/example/alpha",
		"skills":     []string{"skills/review-change"},
		"rules":      []string{"rules/always.md", "rules/paths.md"},
	})
	writeAlwaysAndPathsRules(t, second)
	writeSkillDir(t, second, "skills/review-change")
	if _, err := Convert(Options{PackageRoot: second}); err != nil {
		t.Fatal(err)
	}

	left, err := os.ReadFile(filepath.Join(first, manifest.Filename))
	if err != nil {
		t.Fatal(err)
	}
	right, err := os.ReadFile(filepath.Join(second, manifest.Filename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(left, right) {
		t.Fatalf("YAML differed:\n%s\n----\n%s", left, right)
	}
}

func TestEditedManifestIsNeverOverwritten(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, alphaPlugin(false))
	writeAlphaSources(t, root)
	if _, err := Convert(Options{PackageRoot: root}); err != nil {
		t.Fatal(err)
	}
	edited := []byte("schemaVersion: 1\nname: example/hand-edit\n")
	if err := os.WriteFile(filepath.Join(root, manifest.Filename), edited, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Convert(Options{PackageRoot: root})
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeManifestConflict {
		t.Fatalf("err = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, manifest.Filename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, edited) {
		t.Fatal("converter overwrote a hand-edited manifest")
	}
}

func TestPackageFilesExcludesTesslManifests(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, alphaPlugin(false))
	writeTileJSON(t, root, map[string]any{
		"name":       "example/alpha",
		"version":    "1.0.0",
		"summary":    "alpha plugin",
		"private":    false,
		"repository": "https://github.com/example/alpha",
		"rules": map[string]any{
			"always": map[string]string{"rules": "rules/always.md"},
			"paths":  map[string]string{"rules": "rules/paths.md"},
		},
		"skills": map[string]any{
			"review-change": map[string]string{"path": "skills/review-change/SKILL.md"},
		},
	})
	writeAlphaSources(t, root)
	writeFile(t, root, "README.md", []byte("# readme\n"), 0o644)
	writeJSON(t, root, "tessl-package.json", map[string]string{"name": "example/alpha"})

	_, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := manifest.PackageFiles(root, loaded)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(files, "\n")
	for _, forbidden := range []string{pluginManifestRel, tileManifestName, "README.md", "tessl-package.json"} {
		for _, file := range files {
			if file == forbidden {
				t.Fatalf("PackageFiles includes %s:\n%s", forbidden, joined)
			}
		}
	}
}

func TestPackageFilesListsEachSkillSiblingOnce(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, alphaPlugin(false))
	writeAlphaSources(t, root)
	if _, err := Convert(Options{PackageRoot: root}); err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := manifest.PackageFiles(root, loaded)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, file := range files {
		seen[file]++
		if seen[file] > 1 {
			t.Fatalf("duplicate published path %s", file)
		}
	}
	for _, want := range []string{
		"skills/review-change/SKILL.md",
		"skills/review-change/scripts/check.sh",
		"skills/review-change/references/guide.md",
	} {
		if seen[want] != 1 {
			t.Fatalf("missing %s in %v", want, files)
		}
	}
}

func TestLoadIgnoresTesslManifests(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, alphaPlugin(false))
	writeTileJSON(t, root, map[string]any{
		"name":       "example/alpha",
		"version":    "1.0.0",
		"summary":    "alpha plugin",
		"private":    false,
		"repository": "https://github.com/example/alpha",
		"rules": map[string]any{
			"always": map[string]string{"rules": "rules/always.md"},
			"paths":  map[string]string{"rules": "rules/paths.md"},
		},
		"skills": map[string]any{
			"review-change": map[string]string{"path": "skills/review-change/SKILL.md"},
		},
	})
	writeAlphaSources(t, root)
	if _, err := Convert(Options{PackageRoot: root}); err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Load(root); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryNeverGuessedFromName(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name":    "tessl-labs/good-oss-citizen",
		"version": "1.0.0",
		"rules":   []string{"rules/always.md"},
	})
	writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# Always\n")

	_, err := Convert(Options{PackageRoot: root})
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != string(manifest.CodeRequired) {
		t.Fatalf("err = %v", err)
	}

	_, err = Convert(Options{PackageRoot: root, Repository: "https://github.com/tesslio/good-oss-citizen"})
	if !errors.As(err, &conv) || conv.Code != string(manifest.CodeInvalidSource) {
		t.Fatalf("err = %v", err)
	}
}

func TestOneSidedScalarsArePreserved(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"private": false,
		"rules":   []string{"rules/always.md"},
		"skills":  []string{"skills/review-change"},
	})
	writeTileJSON(t, root, map[string]any{
		"name":       "example/alpha",
		"version":    "1.0.0",
		"summary":    "tile description",
		"repository": "https://github.com/example/alpha",
		"homepage":   "https://example.com/alpha",
		"license":    "Apache-2.0",
		"author": map[string]string{
			"name": "Alpha Maintainer", "email": "maintainer@example.com", "url": "https://example.com/maintainer",
		},
		"private": false,
		"rules":   map[string]any{"always": map[string]string{"rules": "rules/always.md"}},
		"skills":  map[string]any{"review-change": map[string]string{"path": "skills/review-change/SKILL.md"}},
	})
	writeAlphaSources(t, root)

	report, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Name != "example/alpha" || loaded.Version != "1.0.0" || loaded.Description != "tile description" {
		t.Fatalf("identity = %+v", loaded)
	}
	if loaded.Source.Repository != "https://github.com/example/alpha" {
		t.Fatalf("repository = %q", loaded.Source.Repository)
	}
	lossy := map[string]string{}
	for _, item := range report.Lossy {
		lossy[item.Field] = item.Value
	}
	for field, want := range map[string]string{
		"homepage":     "https://example.com/alpha",
		"license":      "Apache-2.0",
		"author.name":  "Alpha Maintainer",
		"author.email": "maintainer@example.com",
		"author.url":   "https://example.com/maintainer",
	} {
		if lossy[field] != want {
			t.Errorf("lossy[%q] = %q want %q", field, lossy[field], want)
		}
	}
}

func TestProvenanceDisagreementIsAmbiguous(t *testing.T) {
	t.Parallel()

	tests := []struct {
		field  string
		plugin map[string]any
		tile   map[string]any
	}{
		{field: "homepage", plugin: map[string]any{"homepage": "https://plugin.example"}, tile: map[string]any{"homepage": "https://tile.example"}},
		{field: "license", plugin: map[string]any{"license": "MIT"}, tile: map[string]any{"license": "Apache-2.0"}},
		{field: "author.name", plugin: map[string]any{"author": map[string]string{"name": "Plugin Author"}}, tile: map[string]any{"author": map[string]string{"name": "Tile Author"}}},
		{field: "author.email", plugin: map[string]any{"author": map[string]string{"email": "plugin@example.com"}}, tile: map[string]any{"author": map[string]string{"email": "tile@example.com"}}},
		{field: "author.url", plugin: map[string]any{"author": map[string]string{"url": "https://plugin.example/author"}}, tile: map[string]any{"author": map[string]string{"url": "https://tile.example/author"}}},
	}
	for _, test := range tests {
		t.Run(test.field, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			plugin := pluginShape("example/alpha", "1.0.0")
			tile := tileShape("example/alpha", "1.0.0")
			for field, value := range test.plugin {
				plugin[field] = value
			}
			for field, value := range test.tile {
				tile[field] = value
			}
			writePluginJSON(t, root, plugin)
			writeTileJSON(t, root, tile)
			writeAlphaSources(t, root)

			_, err := Convert(Options{PackageRoot: root})
			var conv *Error
			if !errors.As(err, &conv) || conv.Code != CodeAmbiguousManifest || conv.Field != test.field {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestAmbiguousManifestBlocks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, pluginShape("example/alpha", "1.0.0"))
	writeTileJSON(t, root, tileShape("example/other", "1.0.0"))
	writeAlphaSources(t, root)

	_, err := Convert(Options{PackageRoot: root})
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeAmbiguousManifest {
		t.Fatalf("err = %v", err)
	}
}

func TestPublishedFilesMatchPackageFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, alphaPlugin(true))
	writeAlphaSources(t, root)

	report, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := manifest.PackageFiles(root, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(report.PublishedFiles, files) {
		t.Fatalf("publishedFiles = %v PackageFiles = %v", report.PublishedFiles, files)
	}
}

func TestDotDotRulePathIsInvalidPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name":       "example/alpha",
		"version":    "1.0.0",
		"repository": "https://github.com/example/alpha",
		"rules":      []string{"rules/../secret.md"},
	})
	writeRuleFile(t, root, "secret.md", "alwaysApply: true\n", "# Secret\n")

	_, err := Convert(Options{PackageRoot: root})
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != string(manifest.CodeInvalidPath) {
		t.Fatalf("err = %v", err)
	}
}

func TestOneSidedPathSetsAreAmbiguous(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		field      string
		dropPlugin string
		dropTile   string
	}{
		{name: "plugin rules only", field: "rules", dropTile: "rules"},
		{name: "tile rules only", field: "rules", dropPlugin: "rules"},
		{name: "plugin skills only", field: "skills", dropTile: "skills"},
		{name: "tile skills only", field: "skills", dropPlugin: "skills"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			plugin := pluginShape("example/alpha", "1.0.0")
			tile := tileShape("example/alpha", "1.0.0")
			if test.dropPlugin != "" {
				delete(plugin, test.dropPlugin)
			}
			if test.dropTile != "" {
				delete(tile, test.dropTile)
			}
			writePluginJSON(t, root, plugin)
			writeTileJSON(t, root, tile)
			writeAlphaSources(t, root)

			_, err := Convert(Options{PackageRoot: root})
			var conv *Error
			if !errors.As(err, &conv) || conv.Code != CodeAmbiguousManifest || conv.Field != test.field {
				t.Fatalf("%s: err = %v", test.name, err)
			}
		})
	}
}

func TestDirectoryFormAgreesWithNamedPaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name":        "example/alpha",
		"version":     "1.0.0",
		"description": "example plugin",
		"private":     false,
		"repository":  "https://github.com/example/alpha",
		"rules":       "rules/",
		"skills":      "skills/",
	})
	tile := tileShape("example/alpha", "1.0.0")
	tile["rules"] = map[string]any{
		"always": map[string]string{"rules": "rules/always.md"},
		"paths":  map[string]string{"rules": "rules/paths.md"},
	}
	writeTileJSON(t, root, tile)
	writeAlphaSources(t, root)

	if _, err := Convert(Options{PackageRoot: root}); err != nil {
		t.Fatal(err)
	}
}

func TestTileSilenceOnHooksIsNotAmbiguous(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, alphaPlugin(true))
	writeTileJSON(t, root, map[string]any{
		"name":       "example/alpha",
		"version":    "1.0.0",
		"summary":    "alpha plugin",
		"private":    false,
		"repository": "https://github.com/example/alpha",
		"rules": map[string]any{
			"always": map[string]string{"rules": "rules/always.md"},
			"paths":  map[string]string{"rules": "rules/paths.md"},
		},
		"skills": map[string]any{
			"review-change": map[string]string{"path": "skills/review-change/SKILL.md"},
		},
	})
	writeAlphaSources(t, root)

	if _, err := Convert(Options{PackageRoot: root}); err != nil {
		t.Fatal(err)
	}
}
