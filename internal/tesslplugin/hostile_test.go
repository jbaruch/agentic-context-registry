package tesslplugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// hostileHashTree fingerprints every path under root by content and permission
// bits, so a chmod is a change even when the bytes are identical.
func hostileHashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(name)
			if err != nil {
				return err
			}
			result[relative] = "link->" + filepath.ToSlash(target)
		case entry.IsDir():
			result[relative] = "dir " + info.Mode().Perm().String()
		default:
			content, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(content)
			result[relative] = hex.EncodeToString(sum[:]) + " " + info.Mode().Perm().String()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func hostileAssertUnchanged(t *testing.T, before, after map[string]string, allowedNew ...string) {
	t.Helper()
	allowed := make(map[string]struct{}, len(allowedNew))
	for _, name := range allowedNew {
		allowed[name] = struct{}{}
	}
	for name, digest := range before {
		got, present := after[name]
		if !present {
			t.Fatalf("path %q disappeared", name)
		}
		if got != digest {
			t.Fatalf("path %q changed: before %q after %q", name, digest, got)
		}
	}
	for name := range after {
		if _, present := before[name]; present {
			continue
		}
		if _, ok := allowed[name]; !ok {
			t.Fatalf("conversion created unexpected path %q", name)
		}
	}
}

func hostileConversionError(t *testing.T, err error) *Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected a conversion error, got nil")
	}
	var conv *Error
	if !errors.As(err, &conv) {
		t.Fatalf("error %v is not a *tesslplugin.Error", err)
	}
	return conv
}

func hostileManifestAbsent(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, manifest.Filename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists after a refused conversion: %v", manifest.Filename, err)
	}
}

// hostileAlphaTree seeds the plugin.json shape the design note calls
// representative: array rules and skills, a consensus hook with extra argv, and
// a nativeHooks entry declared for every ACR adapter in both command forms.
func hostileAlphaTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name":        "example/alpha",
		"version":     "1.0.0",
		"description": "alpha plugin",
		"private":     false,
		"repository":  "https://github.com/example/alpha",
		"rules":       []string{"rules/always.md", "rules/paths.md"},
		"skills":      []string{"skills/review-change"},
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash",
				"args": []string{"${TESSL_PLUGIN_DIR}/hooks/check-freshness.sh", "--fast", "--limit=3"},
			}}}},
		},
		"nativeHooks": map[string]any{
			"claude-code": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash",
				"args": []string{"${TESSL_PLUGIN_DIR}/hooks/handoff.sh"},
			}}}}},
			"codex": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": `bash "${TESSL_PLUGIN_DIR}/hooks/handoff.sh"`,
			}}}}},
			"cursor": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash",
				"args": []string{"${TESSL_PLUGIN_DIR}/hooks/handoff.sh"},
			}}}}},
		},
	})
	writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# Always\n")
	writeRuleFile(t, root, "rules/paths.md",
		"alwaysApply: false\napplyTo: \"skills/**/SKILL.md, rules/*.md — when authoring skills\"\n", "# Paths\n")
	writeSkillDir(t, root, "skills/review-change")
	writeHookScript(t, root, "hooks/check-freshness.sh")
	writeHookScript(t, root, "hooks/handoff.sh")
	return root
}

// hostileBetaTree seeds a legacy tile.json-only package: named maps, summary,
// skill paths pointing at SKILL.md, and no hooks.
func hostileBetaTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTileJSON(t, root, map[string]any{
		"name":       "example/beta",
		"version":    "2.0.0",
		"summary":    "beta plugin",
		"private":    false,
		"repository": "https://github.com/example/beta",
		"rules":      map[string]any{"always": map[string]string{"rules": "rules/always.md"}},
		"skills":     map[string]any{"review-change": map[string]string{"path": "skills/review-change/SKILL.md"}},
	})
	writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# Always\n")
	writeSkillDir(t, root, "skills/review-change")
	return root
}

// Row: legacy tile.json and current plugin.json both convert to a manifest
// internal/manifest accepts.
func TestHostileBothManifestShapesLoadAfterConversion(t *testing.T) {
	t.Parallel()

	t.Run("pluginOnly", func(t *testing.T) {
		t.Parallel()
		root := hostileAlphaTree(t)
		report, err := Convert(Options{PackageRoot: root})
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		if !report.Wrote || report.SourceManifest != pluginManifest {
			t.Fatalf("report wrote=%v source=%q", report.Wrote, report.SourceManifest)
		}
		loaded, err := manifest.Load(root)
		if err != nil {
			t.Fatalf("manifest.Load rejected the conversion: %v", err)
		}
		if loaded.Name != "example/alpha" || loaded.Version != "1.0.0" || loaded.Description != "alpha plugin" {
			t.Fatalf("identity = %+v", loaded)
		}
		if loaded.Source.Repository != "https://github.com/example/alpha" {
			t.Fatalf("repository = %q", loaded.Source.Repository)
		}
		if len(loaded.Artifacts.Scripts) != 0 {
			t.Fatalf("scripts must stay empty, got %+v", loaded.Artifacts.Scripts)
		}
		wantRules := map[string]manifest.RuleActivation{
			"always": {Mode: manifest.ActivationAlways},
			"paths":  {Mode: manifest.ActivationPaths, Paths: []string{"skills/**/SKILL.md", "rules/*.md"}},
		}
		if len(loaded.Artifacts.Rules) != len(wantRules) {
			t.Fatalf("rules = %+v", loaded.Artifacts.Rules)
		}
		for _, rule := range loaded.Artifacts.Rules {
			want, ok := wantRules[rule.ID]
			if !ok {
				t.Fatalf("unexpected rule id %q", rule.ID)
			}
			if rule.Activation.Mode != want.Mode || strings.Join(rule.Activation.Paths, "|") != strings.Join(want.Paths, "|") {
				t.Fatalf("rule %q activation = %+v want %+v", rule.ID, rule.Activation, want)
			}
		}
		if len(loaded.Artifacts.Skills) != 1 || loaded.Artifacts.Skills[0].Path != "skills/review-change" {
			t.Fatalf("skills = %+v", loaded.Artifacts.Skills)
		}
		wantHooks := map[string]manifest.HookArtifact{
			"check-freshness": {ID: "check-freshness", Event: manifest.HookSessionStart, Path: "hooks/check-freshness.sh", Args: []string{"--fast", "--limit=3"}},
			"handoff":         {ID: "handoff", Event: manifest.HookStop, Path: "hooks/handoff.sh"},
		}
		if len(loaded.Artifacts.Hooks) != len(wantHooks) {
			t.Fatalf("hooks = %+v", loaded.Artifacts.Hooks)
		}
		for _, hook := range loaded.Artifacts.Hooks {
			want, ok := wantHooks[hook.ID]
			if !ok {
				t.Fatalf("unexpected hook id %q", hook.ID)
			}
			if hook.Event != want.Event || hook.Path != want.Path || strings.Join(hook.Args, "|") != strings.Join(want.Args, "|") {
				t.Fatalf("hook %q = %+v want %+v", hook.ID, hook, want)
			}
		}
	})

	t.Run("tileOnly", func(t *testing.T) {
		t.Parallel()
		root := hostileBetaTree(t)
		report, err := Convert(Options{PackageRoot: root})
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		if report.SourceManifest != tileManifestName {
			t.Fatalf("source manifest = %q", report.SourceManifest)
		}
		loaded, err := manifest.Load(root)
		if err != nil {
			t.Fatalf("manifest.Load rejected the conversion: %v", err)
		}
		if loaded.Description != "beta plugin" {
			t.Fatalf("tile summary did not become description: %q", loaded.Description)
		}
		if len(loaded.Artifacts.Skills) != 1 || loaded.Artifacts.Skills[0].Path != "skills/review-change" {
			t.Fatalf("tile SKILL.md path was not reduced to its directory: %+v", loaded.Artifacts.Skills)
		}
		if len(loaded.Artifacts.Hooks) != 0 {
			t.Fatalf("tile.json cannot declare hooks, got %+v", loaded.Artifacts.Hooks)
		}
	})
}

// Row: every artifact path is preserved byte-for-byte, hooks and nativeHooks
// with args included.
func TestHostileEmittedPathsAndArgsAreVerbatim(t *testing.T) {
	t.Parallel()

	root := hostileAlphaTree(t)
	report, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	got := map[string]string{}
	for _, record := range report.Artifacts {
		got[record.ID] = record.Kind + " " + record.Path + " " + record.Event
	}
	want := map[string]string{
		"always":          "rule rules/always.md ",
		"paths":           "rule rules/paths.md ",
		"review-change":   "skill skills/review-change ",
		"check-freshness": "hook hooks/check-freshness.sh session-start",
		"handoff":         "hook hooks/handoff.sh stop",
	}
	if len(got) != len(want) {
		t.Fatalf("artifacts = %+v", report.Artifacts)
	}
	for id, value := range want {
		if got[id] != value {
			t.Fatalf("artifact %q = %q want %q", id, got[id], value)
		}
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, hook := range loaded.Artifacts.Hooks {
		if hook.ID != "check-freshness" {
			continue
		}
		if len(hook.Args) != 2 || hook.Args[0] != "--fast" || hook.Args[1] != "--limit=3" {
			t.Fatalf("extra argv was not preserved: %+v", hook.Args)
		}
	}
	published, err := manifest.PackageFiles(root, loaded)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"hooks/check-freshness.sh", "hooks/handoff.sh", "rules/always.md", "rules/paths.md",
		"skills/review-change/SKILL.md", "skills/review-change/references/guide.md",
		"skills/review-change/scripts/check.sh",
	} {
		if !slicesContain(published, want) {
			t.Fatalf("published set %v is missing %q", published, want)
		}
	}
	for _, script := range []string{"hooks/check-freshness.sh", "skills/review-change/scripts/check.sh"} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(script)))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s permission = %v want 0755", script, info.Mode().Perm())
		}
	}
}

// Row: the source tree hash is unchanged; agent-plugin.yaml is the only new path.
func TestHostileSourceTreeIsFrozen(t *testing.T) {
	t.Parallel()

	root := hostileAlphaTree(t)
	writeTileJSON(t, root, map[string]any{
		"name":       "example/alpha",
		"version":    "1.0.0",
		"summary":    "alpha plugin",
		"private":    false,
		"repository": "https://github.com/example/alpha",
		"rules":      []string{"rules/always.md", "rules/paths.md"},
		"skills":     []string{"skills/review-change"},
	})
	writeJSON(t, root, "tessl-package.json", map[string]string{"name": "example/alpha"})
	writeFile(t, root, ".tesslignore", []byte("README.md\n"), 0o644)
	writeFile(t, root, "README.md", []byte("# Alpha\n"), 0o644)

	before := hostileHashTree(t, root)
	if _, err := Convert(Options{PackageRoot: root}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	after := hostileHashTree(t, root)
	hostileAssertUnchanged(t, before, after, manifest.Filename)
}

// Row: converting twice is byte-identical and reports wrote:false.
func TestHostileSecondConversionIsByteIdentical(t *testing.T) {
	t.Parallel()

	root := hostileAlphaTree(t)
	first, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatalf("first convert: %v", err)
	}
	if !first.Wrote {
		t.Fatal("first run reported wrote:false")
	}
	firstBytes, err := os.ReadFile(filepath.Join(root, manifest.Filename))
	if err != nil {
		t.Fatal(err)
	}
	afterFirst := hostileHashTree(t, root)

	second, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatalf("second convert: %v", err)
	}
	if second.Wrote {
		t.Fatal("second run rewrote the manifest")
	}
	secondBytes, err := os.ReadFile(filepath.Join(root, manifest.Filename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatalf("second run changed the bytes:\nfirst:\n%s\nsecond:\n%s", firstBytes, secondBytes)
	}
	hostileAssertUnchanged(t, afterFirst, hostileHashTree(t, root))

	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondCopy := second
	secondCopy.Wrote = true
	secondJSON, err := json.Marshal(secondCopy)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("report drifted between runs:\nfirst:  %s\nsecond: %s", firstJSON, secondJSON)
	}
}

// Row: an author edit is never overwritten.
func TestHostileEditedManifestIsRefusedNotOverwritten(t *testing.T) {
	t.Parallel()

	root := hostileAlphaTree(t)
	if _, err := Convert(Options{PackageRoot: root}); err != nil {
		t.Fatal(err)
	}
	edited := []byte("schemaVersion: 1\nname: example/alpha\nversion: 9.9.9\nsource:\n  repository: https://github.com/example/alpha\nartifacts:\n  rules:\n    - id: always\n      path: rules/always.md\n      activation:\n        mode: always\n")
	if err := os.WriteFile(filepath.Join(root, manifest.Filename), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	before := hostileHashTree(t, root)
	_, err := Convert(Options{PackageRoot: root})
	conv := hostileConversionError(t, err)
	if conv.Code != CodeManifestConflict {
		t.Fatalf("code = %q want %q", conv.Code, CodeManifestConflict)
	}
	if !strings.Contains(conv.Message, "version") {
		t.Fatalf("conflict message does not name the moved field: %q", conv.Message)
	}
	hostileAssertUnchanged(t, before, hostileHashTree(t, root))
}

// Row: both manifests on disk, no duplicate published content.
func TestHostileDualManifestPublishSetIsUniqueAndTesslFree(t *testing.T) {
	t.Parallel()

	root := hostileAlphaTree(t)
	writeTileJSON(t, root, map[string]any{
		"name":       "example/alpha",
		"version":    "1.0.0",
		"summary":    "alpha plugin",
		"private":    false,
		"repository": "https://github.com/example/alpha",
		"rules":      map[string]any{"always": map[string]string{"rules": "rules/always.md"}, "paths": map[string]string{"rules": "rules/paths.md"}},
		"skills":     map[string]any{"review-change": map[string]string{"path": "skills/review-change/SKILL.md"}},
	})
	writeJSON(t, root, "tessl-package.json", map[string]string{"name": "example/alpha"})
	writeFile(t, root, "README.md", []byte("# Alpha\n"), 0o644)
	writeFile(t, root, "hooks/tests/run.sh", []byte("#!/bin/sh\n"), 0o755)
	writeFile(t, root, ".tesslignore", []byte("README.md\nhooks/tests/\n"), 0o644)

	report, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	for _, keep := range []string{pluginManifestRel, tileManifestName, "tessl-package.json", ".tesslignore"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(keep))); err != nil {
			t.Fatalf("conversion removed %s: %v", keep, err)
		}
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	published, err := manifest.PackageFiles(root, loaded)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, name := range published {
		seen[name]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Fatalf("published %q %d times", name, count)
		}
	}
	for _, banned := range []string{pluginManifestRel, tileManifestName, "tessl-package.json", "README.md", ".tesslignore", "hooks/tests/run.sh"} {
		if slicesContain(published, banned) {
			t.Fatalf("published set leaks %q: %v", banned, published)
		}
	}
	if strings.Join(published, "|") != strings.Join(report.PublishedFiles, "|") {
		t.Fatalf("report.publishedFiles %v != manifest.PackageFiles %v", report.PublishedFiles, published)
	}
}

// Row: a Tessl field with no mapping is reported and blocks the write.
func TestHostileUnmappedConceptsFailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(plugin map[string]any)
		wantCode string
	}{
		{
			name:     "privateTrue",
			mutate:   func(plugin map[string]any) { plugin["private"] = true },
			wantCode: CodeUnmappedField,
		},
		{
			name: "matcher",
			mutate: func(plugin map[string]any) {
				plugin["hooks"] = map[string]any{"SessionStart": []any{map[string]any{
					"matcher": "Bash",
					"hooks": []any{map[string]any{"type": "command", "command": "bash",
						"args": []string{"${TESSL_PLUGIN_DIR}/hooks/check-freshness.sh"}}},
				}}}
			},
			wantCode: CodeUnmappedField,
		},
		{
			name: "eventOutsideV1",
			mutate: func(plugin map[string]any) {
				plugin["hooks"] = map[string]any{"Notification": []any{map[string]any{
					"hooks": []any{map[string]any{"type": "command", "command": "bash",
						"args": []string{"${TESSL_PLUGIN_DIR}/hooks/check-freshness.sh"}}},
				}}}
			},
			wantCode: CodeUnmappedField,
		},
		{
			name: "typeNotCommand",
			mutate: func(plugin map[string]any) {
				plugin["hooks"] = map[string]any{"SessionStart": []any{map[string]any{
					"hooks": []any{map[string]any{"type": "prompt", "command": "bash",
						"args": []string{"${TESSL_PLUGIN_DIR}/hooks/check-freshness.sh"}}},
				}}}
			},
			wantCode: CodeUnmappedField,
		},
		{
			name: "commandOutsideGrammar",
			mutate: func(plugin map[string]any) {
				plugin["hooks"] = map[string]any{"SessionStart": []any{map[string]any{
					"hooks": []any{map[string]any{"type": "command", "command": "python3 hooks/check-freshness.py"}},
				}}}
			},
			wantCode: CodeUnmappedField,
		},
		{
			name: "nativeHookBodyDivergence",
			mutate: func(plugin map[string]any) {
				plugin["nativeHooks"] = map[string]any{
					"claude-code": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{
						"type": "command", "command": "bash",
						"args": []string{"${TESSL_PLUGIN_DIR}/hooks/handoff.sh", "--strict"}}}}}},
					"codex": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{
						"type": "command", "command": "bash",
						"args": []string{"${TESSL_PLUGIN_DIR}/hooks/handoff.sh"}}}}}},
					"cursor": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{
						"type": "command", "command": "bash",
						"args": []string{"${TESSL_PLUGIN_DIR}/hooks/handoff.sh"}}}}}},
				}
			},
			wantCode: CodeUnmappedField,
		},
		{
			name: "agentWidening",
			mutate: func(plugin map[string]any) {
				plugin["nativeHooks"] = map[string]any{
					"claude-code": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{
						"type": "command", "command": "bash",
						"args": []string{"${TESSL_PLUGIN_DIR}/hooks/handoff.sh"}}}}}},
					"codex": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{
						"type": "command", "command": "bash",
						"args": []string{"${TESSL_PLUGIN_DIR}/hooks/handoff.sh"}}}}}},
				}
			},
			wantCode: CodeAgentWidening,
		},
		{
			name:     "unknownKey",
			mutate:   func(plugin map[string]any) { plugin["publisher"] = "acme" },
			wantCode: CodeUnknownField,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := hostileAlphaTree(t)
			var plugin map[string]any
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(pluginManifestRel)))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &plugin); err != nil {
				t.Fatal(err)
			}
			test.mutate(plugin)
			writePluginJSON(t, root, plugin)
			before := hostileHashTree(t, root)

			report, err := Convert(Options{PackageRoot: root})
			conv := hostileConversionError(t, err)
			if conv.Code != test.wantCode {
				t.Fatalf("code = %q want %q (message %q)", conv.Code, test.wantCode, conv.Message)
			}
			if conv.Field == "" {
				t.Fatalf("code %q reported no field", conv.Code)
			}
			if len(report.Unmapped) == 0 {
				t.Fatalf("report.unmapped is empty for %s; the concept was dropped silently", test.name)
			}
			hostileManifestAbsent(t, root)
			hostileAssertUnchanged(t, before, hostileHashTree(t, root))
		})
	}
}

// Row: a declared rule or skill that is missing on disk.
func TestHostileMissingDeclaredPathFails(t *testing.T) {
	t.Parallel()

	t.Run("rule", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writePluginJSON(t, root, map[string]any{
			"name": "example/alpha", "version": "1.0.0", "description": "alpha",
			"repository": "https://github.com/example/alpha",
			"rules":      []string{"rules/gone.md"},
		})
		before := hostileHashTree(t, root)
		_, err := Convert(Options{PackageRoot: root})
		if err == nil {
			t.Fatal("a missing rule converted successfully")
		}
		if !strings.Contains(err.Error(), "rules/gone.md") {
			t.Fatalf("error does not name the missing path: %v", err)
		}
		hostileManifestAbsent(t, root)
		hostileAssertUnchanged(t, before, hostileHashTree(t, root))
	})

	t.Run("skill", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writePluginJSON(t, root, map[string]any{
			"name": "example/alpha", "version": "1.0.0", "description": "alpha",
			"repository": "https://github.com/example/alpha",
			"rules":      []string{"rules/always.md"},
			"skills":     []string{"skills/gone"},
		})
		writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# Always\n")
		before := hostileHashTree(t, root)
		_, err := Convert(Options{PackageRoot: root})
		conv := hostileConversionError(t, err)
		if conv.Code != string(manifest.CodePathNotFound) {
			t.Fatalf("code = %q want %q (%q)", conv.Code, manifest.CodePathNotFound, conv.Message)
		}
		hostileManifestAbsent(t, root)
		hostileAssertUnchanged(t, before, hostileHashTree(t, root))
	})
}

// Row: .tesslignore / .tileignore carry-over — echoed, never interpreted, never
// copied into the manifest.
func TestHostileIgnoreFilesAreEchoedNotInterpreted(t *testing.T) {
	t.Parallel()

	root := hostileAlphaTree(t)
	writeFile(t, root, ".tesslignore", []byte("# comment\n\nREADME.md\nhooks/tests/\n"), 0o644)
	writeFile(t, root, ".tileignore", []byte("docs/**\n"), 0o644)
	writeFile(t, root, "README.md", []byte("# Alpha\n"), 0o644)

	report, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	got := map[string]string{}
	for _, item := range report.Ignored {
		got[item.Path] = item.Reason
	}
	want := map[string]string{"README.md": "tesslignore", "hooks/tests/": "tesslignore", "docs/**": "tileignore"}
	if len(got) != len(want) {
		t.Fatalf("ignored = %+v want %+v", report.Ignored, want)
	}
	for path, reason := range want {
		if got[path] != reason {
			t.Fatalf("ignored[%q] = %q want %q", path, got[path], reason)
		}
	}
	for _, item := range report.Ignored {
		if strings.HasPrefix(item.Path, "#") || item.Path == "" {
			t.Fatalf("comment or blank ignore line echoed: %q", item.Path)
		}
	}
	for _, name := range []string{".tesslignore", ".tileignore"} {
		if slicesContain(report.PublishedFiles, name) {
			t.Fatalf("ignore file %q entered the published set", name)
		}
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("ignore file %q was consumed: %v", name, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, manifest.Filename))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("tesslignore")) || bytes.Contains(raw, []byte("tileignore")) {
		t.Fatalf("ignore data leaked into the manifest:\n%s", raw)
	}
}

// Row: --dry-run writes nothing, even with a read-only tree.
func TestHostileDryRunUnderReadOnlyTreeWritesNothing(t *testing.T) {
	t.Parallel()

	root := hostileAlphaTree(t)
	chmodTree(t, root, 0o555, 0o555)
	t.Cleanup(func() { chmodTree(t, root, 0o755, 0o644) })
	before := hostileHashTree(t, root)

	report, err := Convert(Options{PackageRoot: root, DryRun: true})
	if err != nil {
		t.Fatalf("dry-run convert: %v", err)
	}
	if report.Wrote || !report.DryRun {
		t.Fatalf("dryRun=%v wrote=%v", report.DryRun, report.Wrote)
	}
	if len(report.Artifacts) == 0 || len(report.PublishedFiles) == 0 {
		t.Fatalf("dry-run produced no plan: %+v", report)
	}
	hostileManifestAbsent(t, root)
	hostileAssertUnchanged(t, before, hostileHashTree(t, root))
}

// Row: a manifest path that would escape the package root.
func TestHostilePathsEscapingPackageRootAreRefused(t *testing.T) {
	t.Parallel()

	base := map[string]any{
		"name": "example/alpha", "version": "1.0.0", "description": "alpha",
		"repository": "https://github.com/example/alpha",
	}
	tests := []struct {
		name     string
		plugin   map[string]any
		seed     func(t *testing.T, root string)
		wantCode string
	}{
		{
			name:     "parentSegmentRule",
			plugin:   map[string]any{"rules": []string{"../outside.md"}},
			wantCode: string(manifest.CodeInvalidPath),
		},
		{
			name:     "absoluteRule",
			plugin:   map[string]any{"rules": []string{"/etc/passwd"}},
			wantCode: string(manifest.CodeInvalidPath),
		},
		{
			name:     "backslashRule",
			plugin:   map[string]any{"rules": []string{`rules\always.md`}},
			wantCode: string(manifest.CodeInvalidPath),
		},
		{
			name:   "parentSegmentSkill",
			plugin: map[string]any{"rules": []string{"rules/always.md"}, "skills": []string{"../elsewhere"}},
			seed: func(t *testing.T, root string) {
				writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# A\n")
			},
			wantCode: string(manifest.CodeInvalidPath),
		},
		{
			name: "hookEscapesPluginDir",
			plugin: map[string]any{
				"rules": []string{"rules/always.md"},
				"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{
					"type": "command", "command": "bash",
					"args": []string{"${TESSL_PLUGIN_DIR}/../evil.sh"}}}}}},
			},
			seed: func(t *testing.T, root string) {
				writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# A\n")
			},
			wantCode: CodeUnmappedField,
		},
		{
			name:   "symlinkedRuleFile",
			plugin: map[string]any{"rules": []string{"rules/always.md"}},
			seed: func(t *testing.T, root string) {
				writeRuleFile(t, root, "rules/real.md", "alwaysApply: true\n", "# A\n")
				if err := os.Symlink("real.md", filepath.Join(root, "rules", "always.md")); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: string(manifest.CodeInvalidArtifactType),
		},
		{
			name:   "symlinkInsideSkill",
			plugin: map[string]any{"rules": []string{"rules/always.md"}, "skills": []string{"skills/review-change"}},
			seed: func(t *testing.T, root string) {
				writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# A\n")
				writeSkillDir(t, root, "skills/review-change")
				if err := os.Symlink("../../../etc/passwd", filepath.Join(root, "skills", "review-change", "leak.md")); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: string(manifest.CodeInvalidSkillTree),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			plugin := map[string]any{}
			for key, value := range base {
				plugin[key] = value
			}
			for key, value := range test.plugin {
				plugin[key] = value
			}
			writePluginJSON(t, root, plugin)
			if test.seed != nil {
				test.seed(t, root)
			}
			before := hostileHashTree(t, root)

			_, err := Convert(Options{PackageRoot: root})
			conv := hostileConversionError(t, err)
			if conv.Code != test.wantCode {
				t.Fatalf("code = %q want %q (message %q)", conv.Code, test.wantCode, conv.Message)
			}
			hostileManifestAbsent(t, root)
			hostileAssertUnchanged(t, before, hostileHashTree(t, root))
		})
	}
}

// Two hooks with the same basename at the same event cannot both take the
// event-suffixed ID; self-validation must refuse rather than write a manifest
// with a duplicate artifact ID.
func TestHostileHookBasenameCollisionAtOneEventFailsClosed(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name": "example/alpha", "version": "1.0.0", "description": "alpha",
		"repository": "https://github.com/example/alpha",
		"rules":      []string{"rules/always.md"},
		"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{
			map[string]any{"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/check.sh"}},
			map[string]any{"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/scripts/check.sh"}},
		}}}},
	})
	writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# Always\n")
	writeHookScript(t, root, "hooks/check.sh")
	writeHookScript(t, root, "scripts/check.sh")
	before := hostileHashTree(t, root)

	_, err := Convert(Options{PackageRoot: root})
	conv := hostileConversionError(t, err)
	if conv.Code != string(manifest.CodeDuplicateArtifactID) {
		t.Fatalf("code = %q want %q (%q)", conv.Code, manifest.CodeDuplicateArtifactID, conv.Message)
	}
	hostileManifestAbsent(t, root)
	hostileAssertUnchanged(t, before, hostileHashTree(t, root))
}

// Activation globs carrying YAML-significant characters must survive the encode
// and reload unchanged, and must not shift quoting between runs.
func TestHostileGlobQuotingSurvivesRoundTrip(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name": "example/alpha", "version": "1.0.0", "description": "alpha",
		"repository": "https://github.com/example/alpha",
		"rules":      []string{"rules/tricky.md"},
	})
	writeRuleFile(t, root, "rules/tricky.md",
		"alwaysApply: false\napplyTo: \"**/*.md, docs/a:b/**, '*.yaml' — prose half\"\n", "# Tricky\n")

	if _, err := Convert(Options{PackageRoot: root}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatalf("manifest.Load rejected quoted globs: %v", err)
	}
	want := []string{"**/*.md", "docs/a:b/**", "'*.yaml'"}
	if strings.Join(loaded.Artifacts.Rules[0].Activation.Paths, "|") != strings.Join(want, "|") {
		t.Fatalf("globs = %+v want %+v", loaded.Artifacts.Rules[0].Activation.Paths, want)
	}
	first, err := os.ReadFile(filepath.Join(root, manifest.Filename))
	if err != nil {
		t.Fatal(err)
	}
	report, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatalf("second convert: %v", err)
	}
	if report.Wrote {
		t.Fatal("second run rewrote a byte-identical manifest")
	}
	second, err := os.ReadFile(filepath.Join(root, manifest.Filename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("quoting drifted:\n%s\n---\n%s", first, second)
	}
}

// The design note maps tile summary onto description and never licenses
// dropping a field the authoritative manifest is silent about.
func TestHostileTileOnlySummaryIsNotSilentlyDropped(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name": "example/alpha", "version": "1.0.0",
		"repository": "https://github.com/example/alpha",
		"rules":      []string{"rules/always.md"},
	})
	writeTileJSON(t, root, map[string]any{
		"name": "example/alpha", "version": "1.0.0",
		"summary": "the only human-readable description this package has",
		"rules":   map[string]any{"always": map[string]string{"rules": "rules/always.md"}},
	})
	writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# Always\n")

	report, err := Convert(Options{PackageRoot: root})
	if err != nil {
		conv := hostileConversionError(t, err)
		if conv.Code != CodeAmbiguousManifest {
			t.Fatalf("code = %q want %q", conv.Code, CodeAmbiguousManifest)
		}
		return
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Description == "" && !reportMentions(report, "summary") {
		t.Fatalf("tile.json summary vanished: description=%q lossy=%+v notes=%+v",
			loaded.Description, report.Lossy, report.Notes)
	}
}

// Provenance is lossy, not invisible: the design note requires each key and
// value echoed, whichever manifest declares it.
func TestHostileTileOnlyProvenanceIsReported(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name": "example/alpha", "version": "1.0.0", "description": "alpha plugin",
		"repository": "https://github.com/example/alpha",
		"rules":      []string{"rules/always.md"},
	})
	writeTileJSON(t, root, map[string]any{
		"name": "example/alpha", "version": "1.0.0", "summary": "alpha plugin",
		"license":  "Apache-2.0",
		"homepage": "https://example.com/alpha",
		"author":   map[string]string{"name": "Alpha Maintainer", "email": "maintainer@example.com"},
		"rules":    map[string]any{"always": map[string]string{"rules": "rules/always.md"}},
	})
	writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# Always\n")

	report, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	for _, field := range []string{"license", "homepage", "author.name", "author.email"} {
		if !reportMentions(report, field) {
			t.Fatalf("tile.json %s was dropped without a report entry: lossy=%+v unmapped=%+v notes=%+v",
				field, report.Lossy, report.Unmapped, report.Notes)
		}
	}
}

// tile.json declaring the repository is evidence; the converter must not demand
// --repository as though the package named none.
func TestHostileTileOnlyRepositoryIsEvidence(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, map[string]any{
		"name": "example/alpha", "version": "1.0.0", "description": "alpha plugin",
		"rules": []string{"rules/always.md"},
	})
	writeTileJSON(t, root, map[string]any{
		"name": "example/alpha", "version": "1.0.0", "summary": "alpha plugin",
		"repository": "https://github.com/example/alpha",
		"rules":      map[string]any{"always": map[string]string{"rules": "rules/always.md"}},
	})
	writeRuleFile(t, root, "rules/always.md", "alwaysApply: true\n", "# Always\n")

	if _, err := Convert(Options{PackageRoot: root}); err != nil {
		t.Fatalf("tile.json declares the repository, conversion still failed: %v", err)
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Source.Repository != "https://github.com/example/alpha" {
		t.Fatalf("repository = %q", loaded.Source.Repository)
	}
}

func reportMentions(report Report, needle string) bool {
	for _, item := range report.Lossy {
		if strings.Contains(item.Field, needle) || strings.Contains(item.Reason, needle) {
			return true
		}
	}
	for _, item := range report.Unmapped {
		if strings.Contains(item.Field, needle) || strings.Contains(item.Reason, needle) {
			return true
		}
	}
	for _, item := range report.Notes {
		if strings.Contains(item.Path, needle) || strings.Contains(item.Reason, needle) {
			return true
		}
	}
	return false
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
