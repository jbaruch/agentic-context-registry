package tesslplugin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// reverifyWrite is an independent fixture writer for the re-verification suite:
// it shares no builder with the developer's fixtures, so a wrong assumption in
// those cannot be inherited here.
func reverifyWrite(t *testing.T, root, relative string, content []byte) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func reverifyWriteJSON(t *testing.T, root, relative string, value map[string]any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	reverifyWrite(t, root, relative, append(payload, '\n'))
}

// reverifyDualRoot writes a package whose rule set is identical on both
// manifests, so the only variable a caller introduces is the scalar under test.
func reverifyDualRoot(t *testing.T, plugin, tile map[string]any) string {
	t.Helper()
	root := t.TempDir()
	plugin["rules"] = []string{"rules/always.md"}
	tile["rules"] = []string{"rules/always.md"}
	reverifyWriteJSON(t, root, ".tessl-plugin/plugin.json", plugin)
	reverifyWriteJSON(t, root, "tile.json", tile)
	reverifyWrite(t, root, "rules/always.md", []byte("---\nalwaysApply: true\n---\n# Always\n"))
	return root
}

func reverifyConversionError(t *testing.T, err error) *Error {
	t.Helper()
	var conv *Error
	if !errors.As(err, &conv) {
		t.Fatalf("error %v is not a conversion Error", err)
	}
	return conv
}

func reverifyManifestAbsent(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, manifest.Filename)); !os.IsNotExist(err) {
		t.Fatalf("refused conversion left %s behind: %v", manifest.Filename, err)
	}
}

// A scalar both manifests declare and disagree on is ambiguous_manifest naming
// that field. B4 made one-sided scalars survive; this pins the other half of the
// same rule so the coalescing can never paper over a real disagreement.
func TestReverifyDualScalarDisagreementIsAmbiguous(t *testing.T) {
	t.Parallel()

	base := func() (map[string]any, map[string]any) {
		return map[string]any{
			"name":       "example/alpha",
			"version":    "1.0.0",
			"repository": "https://github.com/example/alpha",
		}, map[string]any{
			"name":       "example/alpha",
			"version":    "1.0.0",
			"repository": "https://github.com/example/alpha",
		}
	}
	tests := []struct {
		name  string
		field string
		apply func(plugin, tile map[string]any)
	}{
		{name: "description", field: "description", apply: func(plugin, tile map[string]any) {
			plugin["description"] = "plugin side"
			tile["summary"] = "tile side"
		}},
		{name: "repository", field: "repository", apply: func(plugin, tile map[string]any) {
			plugin["repository"] = "https://github.com/example/alpha"
			tile["repository"] = "https://github.com/example/other"
		}},
		{name: "homepage", field: "homepage", apply: func(plugin, tile map[string]any) {
			plugin["homepage"] = "https://plugin.example"
			tile["homepage"] = "https://tile.example"
		}},
		{name: "license", field: "license", apply: func(plugin, tile map[string]any) {
			plugin["license"] = "Apache-2.0"
			tile["license"] = "MIT"
		}},
		{name: "authorName", field: "author.name", apply: func(plugin, tile map[string]any) {
			plugin["author"] = map[string]any{"name": "Plugin Author"}
			tile["author"] = map[string]any{"name": "Tile Author"}
		}},
		{name: "authorEmail", field: "author.email", apply: func(plugin, tile map[string]any) {
			plugin["author"] = map[string]any{"email": "plugin@example.com"}
			tile["author"] = map[string]any{"email": "tile@example.com"}
		}},
		{name: "authorURL", field: "author.url", apply: func(plugin, tile map[string]any) {
			plugin["author"] = map[string]any{"url": "https://plugin.example/who"}
			tile["author"] = map[string]any{"url": "https://tile.example/who"}
		}},
		{name: "name", field: "name", apply: func(plugin, tile map[string]any) {
			plugin["name"] = "example/alpha"
			tile["name"] = "example/beta"
		}},
		{name: "version", field: "version", apply: func(plugin, tile map[string]any) {
			plugin["version"] = "1.0.0"
			tile["version"] = "2.0.0"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			plugin, tile := base()
			test.apply(plugin, tile)
			root := reverifyDualRoot(t, plugin, tile)

			report, err := Convert(Options{PackageRoot: root})
			if err == nil {
				t.Fatalf("disagreeing %s converted: %+v", test.field, report)
			}
			conv := reverifyConversionError(t, err)
			if conv.Code != CodeAmbiguousManifest {
				t.Fatalf("code = %q, want %q (%v)", conv.Code, CodeAmbiguousManifest, err)
			}
			if conv.Field != test.field {
				t.Fatalf("field = %q, want %q", conv.Field, test.field)
			}
			reverifyManifestAbsent(t, root)
		})
	}
}

// plugin.json silence on name and version is not a declaration, so tile.json
// supplies both and the conversion succeeds. The design note's authority rule
// covers disagreement, never one side's silence.
func TestReverifyTileOnlyNameAndVersionSurvive(t *testing.T) {
	t.Parallel()

	root := reverifyDualRoot(t,
		map[string]any{
			"description": "alpha plugin",
			"repository":  "https://github.com/example/alpha",
		},
		map[string]any{
			"name":    "example/alpha",
			"version": "1.0.0",
		})

	report, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatalf("tile-only name and version refused: %v", err)
	}
	if report.Package != "example/alpha" || report.Version != "1.0.0" {
		t.Fatalf("report identity = %q %q, want tile-declared example/alpha 1.0.0", report.Package, report.Version)
	}
	if report.SourceManifest != pluginManifest {
		t.Fatalf("sourceManifest = %q, want %q", report.SourceManifest, pluginManifest)
	}
	value, err := manifest.Load(root)
	if err != nil {
		t.Fatalf("converted manifest does not load: %v", err)
	}
	if value.Name != "example/alpha" || value.Version != "1.0.0" {
		t.Fatalf("manifest identity = %q %q, want example/alpha 1.0.0", value.Name, value.Version)
	}
	if value.Source.Repository != "https://github.com/example/alpha" {
		t.Fatalf("source.repository = %q", value.Source.Repository)
	}
}

// Provenance only tile.json declares is echoed into lossy, exactly as
// plugin-declared provenance is, and never dropped in silence.
func TestReverifyTileOnlyProvenanceIsEchoedAsLossy(t *testing.T) {
	t.Parallel()

	root := reverifyDualRoot(t,
		map[string]any{
			"name":        "example/alpha",
			"version":     "1.0.0",
			"description": "alpha plugin",
			"repository":  "https://github.com/example/alpha",
		},
		map[string]any{
			"name":     "example/alpha",
			"version":  "1.0.0",
			"homepage": "https://tile.example",
			"license":  "MIT",
			"author": map[string]any{
				"name":  "Tile Author",
				"email": "tile@example.com",
				"url":   "https://tile.example/author",
			},
		})

	report, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatalf("tile-only provenance refused: %v", err)
	}
	want := map[string]string{
		"homepage":     "https://tile.example",
		"license":      "MIT",
		"author.name":  "Tile Author",
		"author.email": "tile@example.com",
		"author.url":   "https://tile.example/author",
	}
	got := map[string]string{}
	for _, item := range report.Lossy {
		if item.Reason != "provenance" {
			continue
		}
		got[item.Field] = item.Value
	}
	if len(got) != len(want) {
		t.Fatalf("provenance lossy entries = %+v, want %d entries", report.Lossy, len(want))
	}
	for field, value := range want {
		if got[field] != value {
			t.Fatalf("lossy %s = %q, want %q", field, got[field], value)
		}
	}
	if len(report.Unmapped) != 0 {
		t.Fatalf("provenance must not block: unmapped = %+v", report.Unmapped)
	}
	if _, err := manifest.Load(root); err != nil {
		t.Fatalf("converted manifest does not load: %v", err)
	}
}

// N3: on alwaysApply: true an applyTo with no em dash is prose end to end, and
// one with an em dash reports both halves. Either way activation stays always.
func TestReverifyAlwaysApplyApplyToHalves(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		applyTo string
		want    map[string]string
	}{
		{
			name:    "proseOnly",
			applyTo: "when authoring or modifying skills",
			want:    map[string]string{"applyTo-prose": "when authoring or modifying skills"},
		},
		{
			name:    "emDashSplit",
			applyTo: "skills/**/SKILL.md, rules/*.md — when authoring or modifying skills",
			want: map[string]string{
				"applyTo-globs": "skills/**/SKILL.md, rules/*.md",
				"applyTo-prose": "when authoring or modifying skills",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			reverifyWriteJSON(t, root, ".tessl-plugin/plugin.json", map[string]any{
				"name":        "example/alpha",
				"version":     "1.0.0",
				"description": "alpha plugin",
				"repository":  "https://github.com/example/alpha",
				"rules":       []string{"rules/scoped.md"},
			})
			reverifyWrite(t, root, "rules/scoped.md",
				[]byte("---\nalwaysApply: true\napplyTo: \""+test.applyTo+"\"\n---\n# Scoped\n"))

			report, err := Convert(Options{PackageRoot: root})
			if err != nil {
				t.Fatalf("conversion refused: %v", err)
			}
			got := map[string]string{}
			for _, item := range report.Lossy {
				if item.Field != "rules/scoped.md#applyTo" {
					continue
				}
				if _, duplicate := got[item.Reason]; duplicate {
					t.Fatalf("reason %q reported twice: %+v", item.Reason, report.Lossy)
				}
				got[item.Reason] = item.Value
				if item.ID != "scoped" || item.Kind != "rule" {
					t.Fatalf("lossy entry loses its artifact identity: %+v", item)
				}
			}
			if len(got) != len(test.want) {
				t.Fatalf("applyTo lossy = %+v, want %+v", got, test.want)
			}
			for reason, value := range test.want {
				if got[reason] != value {
					t.Fatalf("lossy %s = %q, want %q", reason, got[reason], value)
				}
			}
			value, err := manifest.Load(root)
			if err != nil {
				t.Fatalf("converted manifest does not load: %v", err)
			}
			if len(value.Artifacts.Rules) != 1 {
				t.Fatalf("rules = %+v", value.Artifacts.Rules)
			}
			activation := value.Artifacts.Rules[0].Activation
			if activation.Mode != manifest.ActivationAlways || len(activation.Paths) != 0 {
				t.Fatalf("activation = %+v, want always with no paths", activation)
			}
		})
	}
}

// BLOCKING: `private` is a scalar both manifests can express, so a `true`
// declared on exactly one side while the other is silent is a one-sided
// declaration, not a disagreement. docs/migration-producer.md:15 says such a
// field "is used"; :30 says `private: true` is unmapped and blocking. The code
// collapses an absent `private` into `false` before comparing, so it reports
// ambiguous_manifest and tells the operator to make the manifests match — a
// remedy that only produces a second refusal.
func TestReverifyOneSidedPrivateTrueIsUnmappedNotAmbiguous(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		declare func(plugin, tile map[string]any)
	}{
		{name: "tileDeclaresPluginSilent", declare: func(_, tile map[string]any) {
			tile["private"] = true
		}},
		{name: "pluginDeclaresTileSilent", declare: func(plugin, _ map[string]any) {
			plugin["private"] = true
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			plugin := map[string]any{
				"name":        "example/alpha",
				"version":     "1.0.0",
				"description": "alpha plugin",
				"repository":  "https://github.com/example/alpha",
			}
			tile := map[string]any{
				"name":    "example/alpha",
				"version": "1.0.0",
			}
			test.declare(plugin, tile)
			root := reverifyDualRoot(t, plugin, tile)

			report, err := Convert(Options{PackageRoot: root})
			if err == nil {
				t.Fatalf("private: true converted: %+v", report)
			}
			conv := reverifyConversionError(t, err)
			if conv.Code != CodeUnmappedField {
				t.Fatalf("code = %q, want %q; one manifest's silence is not a disagreement (%v)",
					conv.Code, CodeUnmappedField, err)
			}
			if conv.Field != "private" {
				t.Fatalf("field = %q, want %q", conv.Field, "private")
			}
			if len(report.Unmapped) != 1 || report.Unmapped[0].Field != "private" {
				t.Fatalf("unmapped = %+v, want one entry naming private", report.Unmapped)
			}
			reverifyManifestAbsent(t, root)
		})
	}
}

// A `private` both manifests declare and disagree on stays ambiguous_manifest:
// the fix above must narrow the comparison, never remove it.
func TestReverifyBothDeclaredPrivateDisagreementStaysAmbiguous(t *testing.T) {
	t.Parallel()

	root := reverifyDualRoot(t,
		map[string]any{
			"name":        "example/alpha",
			"version":     "1.0.0",
			"description": "alpha plugin",
			"repository":  "https://github.com/example/alpha",
			"private":     false,
		},
		map[string]any{
			"name":    "example/alpha",
			"version": "1.0.0",
			"private": true,
		})

	report, err := Convert(Options{PackageRoot: root})
	if err == nil {
		t.Fatalf("divergent private converted: %+v", report)
	}
	conv := reverifyConversionError(t, err)
	if conv.Code != CodeAmbiguousManifest || conv.Field != "private" {
		t.Fatalf("error = %+v, want ambiguous_manifest on private", conv)
	}
	reverifyManifestAbsent(t, root)
}
