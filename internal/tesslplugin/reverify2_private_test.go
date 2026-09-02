package tesslplugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// reverify2PrivateRoot writes a dual-manifest package whose only variable is
// each manifest's private declaration. A nil pointer means the manifest stays
// silent on the field, which is the distinction the round-4 fix turns on.
func reverify2PrivateRoot(t *testing.T, pluginPrivate, tilePrivate *bool) string {
	t.Helper()
	root := t.TempDir()
	plugin := map[string]any{
		"name":        "example/alpha",
		"version":     "1.0.0",
		"description": "alpha plugin",
		"repository":  "https://github.com/example/alpha",
		"rules":       []string{"rules/always.md"},
	}
	tile := map[string]any{
		"name":    "example/alpha",
		"version": "1.0.0",
		"rules":   []string{"rules/always.md"},
	}
	if pluginPrivate != nil {
		plugin["private"] = *pluginPrivate
	}
	if tilePrivate != nil {
		tile["private"] = *tilePrivate
	}
	reverify2WriteJSON(t, root, ".tessl-plugin/plugin.json", plugin)
	reverify2WriteJSON(t, root, "tile.json", tile)
	reverify2Write(t, root, "rules/always.md", "---\nalwaysApply: true\n---\n# Always\n")
	return root
}

func reverify2Write(t *testing.T, root, relative, content string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func reverify2WriteJSON(t *testing.T, root, relative string, value map[string]any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	reverify2Write(t, root, relative, string(payload)+"\n")
}

func reverify2Bool(value bool) *bool { return &value }

// Round-4 narrowed the private comparison to the case both manifests declare the
// field. This pins every cell of the resulting 3x3 predicate at once, so a later
// widening back to the pointer-collapsing form, or an over-narrowing that drops
// the disagreement check entirely, fails here rather than in one hand-picked row.
func TestReverify2PrivateDeclarationMatrixIsExhaustive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		plugin   *bool
		tile     *bool
		wantCode string
	}{
		{name: "bothSilent", plugin: nil, tile: nil, wantCode: ""},
		{name: "pluginFalseTileSilent", plugin: reverify2Bool(false), tile: nil, wantCode: ""},
		{name: "pluginSilentTileFalse", plugin: nil, tile: reverify2Bool(false), wantCode: ""},
		{name: "bothFalse", plugin: reverify2Bool(false), tile: reverify2Bool(false), wantCode: ""},
		{name: "pluginTrueTileSilent", plugin: reverify2Bool(true), tile: nil, wantCode: CodeUnmappedField},
		{name: "pluginSilentTileTrue", plugin: nil, tile: reverify2Bool(true), wantCode: CodeUnmappedField},
		{name: "bothTrue", plugin: reverify2Bool(true), tile: reverify2Bool(true), wantCode: CodeUnmappedField},
		{name: "pluginTrueTileFalse", plugin: reverify2Bool(true), tile: reverify2Bool(false), wantCode: CodeAmbiguousManifest},
		{name: "pluginFalseTileTrue", plugin: reverify2Bool(false), tile: reverify2Bool(true), wantCode: CodeAmbiguousManifest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := reverify2PrivateRoot(t, test.plugin, test.tile)
			report, err := Convert(Options{PackageRoot: root, DryRun: true})

			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("conversion refused: %v", err)
				}
				if !report.DryRun || report.Wrote {
					t.Fatalf("report = dryRun %t wrote %t, want a dry run that wrote nothing", report.DryRun, report.Wrote)
				}
				if _, statErr := os.Stat(filepath.Join(root, manifest.Filename)); !os.IsNotExist(statErr) {
					t.Fatalf("dry run wrote %s: %v", manifest.Filename, statErr)
				}
				return
			}

			if err == nil {
				t.Fatalf("private declaration converted: %+v", report)
			}
			conv := reverifyConversionError(t, err)
			if conv.Code != test.wantCode {
				t.Fatalf("code = %q, want %q (%v)", conv.Code, test.wantCode, err)
			}
			if conv.Field != "private" {
				t.Fatalf("field = %q, want private", conv.Field)
			}
			if _, statErr := os.Stat(filepath.Join(root, manifest.Filename)); !os.IsNotExist(statErr) {
				t.Fatalf("refusal wrote %s: %v", manifest.Filename, statErr)
			}
		})
	}
}

// Round-4 seeded DryRun at report construction so a refusal describes the
// invocation that produced it. The flag must track the option on every outcome,
// not only the success path it was originally set on.
func TestReverify2ReportMirrorsInvocationDryRunOnEveryOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		plugin  *bool
		tile    *bool
		refuses bool
	}{
		{name: "success", plugin: reverify2Bool(false), tile: reverify2Bool(false), refuses: false},
		{name: "unmappedRefusal", plugin: reverify2Bool(true), tile: nil, refuses: true},
		{name: "ambiguousRefusal", plugin: reverify2Bool(true), tile: reverify2Bool(false), refuses: true},
	}
	for _, test := range tests {
		for _, dryRun := range []bool{true, false} {
			name := test.name + "/dryRun=false"
			if dryRun {
				name = test.name + "/dryRun=true"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				root := reverify2PrivateRoot(t, test.plugin, test.tile)
				report, err := Convert(Options{PackageRoot: root, DryRun: dryRun})
				if test.refuses == (err == nil) {
					t.Fatalf("refuses = %t but err = %v", test.refuses, err)
				}
				if report.DryRun != dryRun {
					t.Fatalf("report.dryRun = %t, want %t; the report must describe its own invocation", report.DryRun, dryRun)
				}
			})
		}
	}
}

// Every collection in the report is iterable JSON on every outcome. Round 4
// fixed ignored on the success path; this holds the whole set, so a future field
// assignment cannot reintroduce a null for one shape and not another.
func TestReverify2ReportCollectionsAreNeverNullJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		plugin *bool
		tile   *bool
		dryRun bool
	}{
		{name: "successDryRun", plugin: reverify2Bool(false), tile: nil, dryRun: true},
		{name: "successWrote", plugin: reverify2Bool(false), tile: nil, dryRun: false},
		{name: "unmappedRefusal", plugin: reverify2Bool(true), tile: nil, dryRun: true},
		{name: "ambiguousRefusal", plugin: reverify2Bool(true), tile: reverify2Bool(false), dryRun: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := reverify2PrivateRoot(t, test.plugin, test.tile)
			report, _ := Convert(Options{PackageRoot: root, DryRun: test.dryRun})

			payload, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(payload, &decoded); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"artifacts", "lossy", "ignored", "unmapped", "publishedFiles"} {
				raw, ok := decoded[field]
				if !ok {
					t.Fatalf("%s is absent from %s", field, payload)
				}
				if string(raw) == "null" {
					t.Fatalf("%s marshalled as null; a consumer iterating it has to special-case this shape (%s)", field, payload)
				}
			}
		})
	}
}
