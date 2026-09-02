package migrate

import (
	"reflect"
	"testing"
)

func TestLegacyTileAndCurrentPluginManifests(t *testing.T) {
	t.Parallel()

	t.Run("tileOnly", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeTesslJSON(t, root, map[string]string{"example/beta": "2.0.0"})
		seedBeta(t, root, betaTile("skills/legacy-skill/SKILL.md"))

		install := installByIdentity(t, loadTestInstalls(t, root), "example/beta")
		if install.ManifestKind != tileManifest || install.Version != "2.0.0" {
			t.Fatalf("tile-only install = %+v", install)
		}
		if got := declaredIDs(install.Rules); !reflect.DeepEqual(got, []string{"legacy-rule"}) {
			t.Fatalf("tile-only rules = %v", got)
		}
		if got := declaredIDs(install.Skills); !reflect.DeepEqual(got, []string{"legacy-skill"}) {
			t.Fatalf("tile-only skills = %v", got)
		}
		if len(install.Hooks) != 0 {
			t.Fatalf("tile.json cannot declare hooks, got %#v", install.Hooks)
		}
		for _, item := range append(append([]DeclaredPath{}, install.Rules...), install.Skills...) {
			if item.Ambiguous {
				t.Fatalf("tile-only %s unexpectedly ambiguous", item.ID)
			}
		}
	})

	t.Run("pluginOnly", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
		seedAlpha(t, root, alphaPlugin(true, []string{"skills/review-change"}, ""))

		install := installByIdentity(t, loadTestInstalls(t, root), "example/alpha")
		if install.ManifestKind != pluginManifest || install.Version != "1.0.0" {
			t.Fatalf("plugin-only install = %+v", install)
		}
		if got := declaredIDs(install.Rules); !reflect.DeepEqual(got, []string{"always-rule", "paths-rule"}) {
			t.Fatalf("plugin-only rules = %v", got)
		}
		if got := declaredIDs(install.Skills); !reflect.DeepEqual(got, []string{"review-change"}) {
			t.Fatalf("plugin-only skills = %v", got)
		}
		if len(install.Hooks) != 2 {
			t.Fatalf("plugin-only hooks = %#v, want session-start and stop", install.Hooks)
		}
		gotEvents := map[string]string{}
		for _, hook := range install.Hooks {
			gotEvents[hook.ID] = hook.NativeEvent
		}
		if gotEvents["session-start"] != "SessionStart" || gotEvents["stop"] != "Stop" {
			t.Fatalf("plugin-only hook events = %#v", gotEvents)
		}
	})

	t.Run("bothAgreeing", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
		seedAlpha(t, root, alphaPlugin(true, []string{"skills/review-change"}, ""))
		writeTileJSON(t, root, "example/alpha", map[string]any{
			"name":    "example/alpha",
			"version": "1.0.0",
			"rules": map[string]any{
				"always-rule": map[string]string{"rules": "rules/always-rule.md"},
				"paths-rule":  map[string]string{"rules": "rules/paths-rule.md"},
			},
			"skills": map[string]any{
				"review-change": map[string]string{"path": "skills/review-change/SKILL.md"},
			},
		})

		install := installByIdentity(t, loadTestInstalls(t, root), "example/alpha")
		if install.ManifestKind != pluginManifest {
			t.Fatalf("authoritative manifest = %s, want plugin.json", install.ManifestKind)
		}
		for _, item := range append(append([]DeclaredPath{}, install.Rules...), install.Skills...) {
			if item.Ambiguous {
				t.Fatalf("agreeing %s is ambiguous at path %s", item.ID, item.Path)
			}
		}
		if len(install.Hooks) != 2 {
			t.Fatalf("tile silence must not drop plugin hooks, got %#v", install.Hooks)
		}
	})

	t.Run("bothDisagreeing", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
		seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
		writeSkillTree(t, root, "example/alpha", "other-skill", map[string]string{"SKILL.md": "# Other\n"})
		writeTileJSON(t, root, "example/alpha", map[string]any{
			"name":    "example/alpha",
			"version": "1.0.0",
			"rules": map[string]any{
				"always-rule": map[string]string{"rules": "rules/always-rule.md"},
				"paths-rule":  map[string]string{"rules": "rules/paths-rule.md"},
			},
			"skills": map[string]any{
				"review-change": map[string]string{"path": "skills/other-skill/SKILL.md"},
			},
		})

		install := installByIdentity(t, loadTestInstalls(t, root), "example/alpha")
		skill, ok := declaredByID(install.Skills, "review-change")
		if !ok || !skill.Ambiguous || skill.Path != "skills/review-change" {
			t.Fatalf("disagreement = %+v, want ambiguous plugin path", skill)
		}
	})

	t.Run("staleTileMissingRule", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
		seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
		writeTileJSON(t, root, "example/alpha", map[string]any{
			"name":    "example/alpha",
			"version": "1.0.0",
			"rules": map[string]any{
				"always-rule": map[string]string{"rules": "rules/always-rule.md"},
			},
			"skills": map[string]any{
				"review-change": map[string]string{"path": "skills/review-change/SKILL.md"},
			},
		})

		install := installByIdentity(t, loadTestInstalls(t, root), "example/alpha")
		if install.ManifestKind != pluginManifest {
			t.Fatalf("authoritative manifest = %s, want plugin.json", install.ManifestKind)
		}
		pathsRule, ok := declaredByID(install.Rules, "paths-rule")
		if !ok || pathsRule.Ambiguous || pathsRule.Path != "rules/paths-rule.md" {
			t.Fatalf("plugin-only rule on a stale tile = %+v, want migratable plugin path", pathsRule)
		}
		always, ok := declaredByID(install.Rules, "always-rule")
		if !ok || always.Ambiguous {
			t.Fatalf("shared rule = %+v", always)
		}
	})

	t.Run("tileSilentOnHooks", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
		seedAlpha(t, root, alphaPlugin(true, []string{"skills/review-change"}, ""))
		writeTileJSON(t, root, "example/alpha", map[string]any{
			"name":    "example/alpha",
			"version": "1.0.0",
			"rules": map[string]any{
				"always-rule": map[string]string{"rules": "rules/always-rule.md"},
				"paths-rule":  map[string]string{"rules": "rules/paths-rule.md"},
			},
			"skills": map[string]any{
				"review-change": map[string]string{"path": "skills/review-change/SKILL.md"},
			},
		})

		install := installByIdentity(t, loadTestInstalls(t, root), "example/alpha")
		if len(install.Hooks) != 2 {
			t.Fatalf("tile.json silence on hooks is not disagreement, got %#v", install.Hooks)
		}
		for _, hook := range install.Hooks {
			if hook.ID == "" {
				t.Fatalf("hook missing id: %#v", hook)
			}
		}
	})
}

func TestPackageMappingNeverGuessedFromName(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{
		"example/alpha": "1.0.0",
		"example/beta":  "2.0.0",
	})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	seedBeta(t, root, betaTile("skills/legacy-skill/SKILL.md"))
	writePluginJSON(t, root, "example/mapped", map[string]any{
		"name":       "tessl-labs/mapped",
		"version":    "3.0.0",
		"repository": "https://github.com/tesslio/mapped",
		"rules":      []string{"rules/always-rule.md"},
	})
	writeRuleFile(t, root, "example/mapped", "always-rule", "---\nalwaysApply: true\n---\n", "# Mapped\n")

	installs := loadTestInstalls(t, root)
	alpha := installByIdentity(t, installs, "example/alpha")
	if alpha.PackageMapping != mappingUnmapped {
		t.Fatalf("alpha mapping = %s, want unmapped (never guess github:example/alpha)", alpha.PackageMapping)
	}
	if alpha.MappingCandidate != "github:example/alpha" {
		t.Fatalf("alpha mappingCandidate = %s", alpha.MappingCandidate)
	}
	beta := installByIdentity(t, installs, "example/beta")
	if beta.PackageMapping != mappingUnmapped {
		t.Fatalf("beta mapping = %s, want unmapped", beta.PackageMapping)
	}
	mapped := installByIdentity(t, installs, "tessl-labs/mapped")
	if mapped.PackageMapping != mappingGitHub || mapped.Name != "tesslio/mapped" || mapped.MappingCandidate != "github:tesslio/mapped" {
		t.Fatalf("explicit repository mapping = %+v", mapped)
	}
}
