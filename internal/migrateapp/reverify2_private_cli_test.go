package migrateapp

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/tesslplugin"
)

// reverify2DualPrivateRoot writes a dual-manifest package that differs from the
// round-1 fixtures in exactly one axis: which manifest declares private, and
// with what value. An empty declaration string means the manifest stays silent.
func reverify2DualPrivateRoot(t *testing.T, pluginPrivate, tilePrivate string) string {
	t.Helper()
	root := t.TempDir()
	plugin := `{"name":"example/alpha","version":"1.0.0","description":"alpha plugin",` +
		`"repository":"https://github.com/example/alpha"` + reverify2PrivateField(pluginPrivate) +
		`,"rules":["rules/always.md"]}` + "\n"
	tile := `{"name":"example/alpha","version":"1.0.0"` + reverify2PrivateField(tilePrivate) +
		`,"rules":["rules/always.md"]}` + "\n"
	reverifyPut(t, root, ".tessl-plugin/plugin.json", plugin)
	reverifyPut(t, root, "tile.json", tile)
	reverifyPut(t, root, "rules/always.md", reverifyAlwaysRule)
	return root
}

func reverify2PrivateField(declaration string) string {
	if declaration == "" {
		return ""
	}
	return `,"private":` + declaration
}

// reverify2NoYAML fails if the run left any YAML behind, not only the manifest
// filename: a refusal must write nothing at all.
func reverify2NoYAML(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if extension := filepath.Ext(filename); extension == ".yaml" || extension == ".yml" {
			t.Fatalf("refusal wrote YAML at %s", filename)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type reverify2PartialReport struct {
	ReportVersion int  `json:"reportVersion"`
	DryRun        bool `json:"dryRun"`
	Wrote         bool `json:"wrote"`
	Unmapped      []struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	} `json:"unmapped"`
	Ignored json.RawMessage `json:"ignored"`
}

// A private: true only one manifest declares is unmapped input, not a
// disagreement — and the same holds when both declare it. The refusal reaches
// the operator through the JSON envelope with its evidence and the dry-run flag
// of the invocation that produced it.
func TestReverify2OneSidedPrivateTrueRefusalCarriesEvidenceAndDryRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		plugin string
		tile   string
	}{
		{name: "pluginDeclaresTileSilent", plugin: "true", tile: ""},
		{name: "tileDeclaresPluginSilent", plugin: "", tile: "true"},
		{name: "bothDeclareTrue", plugin: "true", tile: "true"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := reverify2DualPrivateRoot(t, test.plugin, test.tile)
			stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"),
				"migrate", "tessl-plugin", root, "--dry-run", "--json")

			if exitCode != cli.ExitOperational {
				t.Fatalf("exit = %d, want %d (stdout %q stderr %q)", exitCode, cli.ExitOperational, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
			envelope := reverifyDecodeRefusal(t, stderr)
			if envelope.OK || envelope.Command != "migrate" {
				t.Fatalf("envelope = %+v", envelope)
			}
			if envelope.Error.Code != tesslplugin.CodeUnmappedField {
				t.Fatalf("code = %q, want %q; one manifest's silence is not a disagreement (%s)",
					envelope.Error.Code, tesslplugin.CodeUnmappedField, envelope.Error.Message)
			}
			if envelope.Error.Field != "private" {
				t.Fatalf("field = %q, want private", envelope.Error.Field)
			}
			if envelope.Result == nil {
				t.Fatal("refusal with unmapped input carries no result")
			}

			var report reverify2PartialReport
			if err := json.Unmarshal(envelope.Result, &report); err != nil {
				t.Fatal(err)
			}
			if report.ReportVersion != 1 {
				t.Fatalf("reportVersion = %d, want 1", report.ReportVersion)
			}
			if !report.DryRun {
				t.Fatal("result.dryRun = false, but the invocation passed --dry-run")
			}
			if report.Wrote {
				t.Fatal("result.wrote = true on a refused conversion")
			}
			if len(report.Unmapped) != 1 || report.Unmapped[0].Field != "private" || report.Unmapped[0].Reason == "" {
				t.Fatalf("result.unmapped = %+v, want one reasoned entry naming private", report.Unmapped)
			}
			if string(report.Ignored) != "[]" {
				t.Fatalf("result.ignored = %s, want []", report.Ignored)
			}
			reverify2NoYAML(t, root)
		})
	}
}

// The narrowing must not remove the disagreement rule: both manifests declaring
// private with different values is still ambiguous_manifest, and that refusal
// carries no partial report because no conversion evidence exists.
func TestReverify2BothDeclaredPrivateDisagreementIsAmbiguousWithoutResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		plugin string
		tile   string
	}{
		{name: "pluginTrueTileFalse", plugin: "true", tile: "false"},
		{name: "pluginFalseTileTrue", plugin: "false", tile: "true"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := reverify2DualPrivateRoot(t, test.plugin, test.tile)
			stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"),
				"migrate", "tessl-plugin", root, "--dry-run", "--json")

			if exitCode != cli.ExitOperational {
				t.Fatalf("exit = %d, want %d (stdout %q stderr %q)", exitCode, cli.ExitOperational, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
			envelope := reverifyDecodeRefusal(t, stderr)
			if envelope.Error.Code != tesslplugin.CodeAmbiguousManifest || envelope.Error.Field != "private" {
				t.Fatalf("error = %+v, want ambiguous_manifest on private", envelope.Error)
			}
			if envelope.Result != nil {
				t.Fatalf("result = %s, want absent; the refusal identified no unmapped input", envelope.Result)
			}
			reverify2NoYAML(t, root)
		})
	}
}

// ignored is an array in every JSON envelope the command emits. A consumer
// iterating it must not have to special-case the packages that ignore nothing,
// nor the difference between a success and a refusal.
func TestReverify2IgnoredIsAnArrayInEveryJSONEnvelope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		root        func(t *testing.T) string
		wantSuccess bool
		wantEntries int
	}{
		{name: "successWithoutIgnoreFile", root: reverify2ConvertibleRoot, wantSuccess: true, wantEntries: 0},
		{name: "successWithIgnoreFile", root: reverify2IgnoringRoot, wantSuccess: true, wantEntries: 1},
		{name: "refusal", root: reverifyUnmappedRoot, wantSuccess: false, wantEntries: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := test.root(t)
			stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"),
				"migrate", "tessl-plugin", root, "--dry-run", "--json")

			stream := stdout
			if !test.wantSuccess {
				stream = stderr
			}
			if test.wantSuccess != (exitCode == cli.ExitSuccess) {
				t.Fatalf("exit = %d, wantSuccess = %t (stdout %q stderr %q)", exitCode, test.wantSuccess, stdout, stderr)
			}
			if strings.Count(stream, "\n") != 1 || !strings.HasSuffix(stream, "\n") {
				t.Fatalf("envelope stream must be one line, got %q", stream)
			}
			var envelope struct {
				Result reverify2PartialReport `json:"result"`
			}
			if err := json.Unmarshal([]byte(stream), &envelope); err != nil {
				t.Fatal(err)
			}
			if string(envelope.Result.Ignored) == "null" {
				t.Fatalf("ignored marshalled as null: %s", stream)
			}
			var entries []struct {
				Path   string `json:"path"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(envelope.Result.Ignored, &entries); err != nil {
				t.Fatalf("ignored is not an array: %v (%s)", err, envelope.Result.Ignored)
			}
			if len(entries) != test.wantEntries {
				t.Fatalf("ignored = %+v, want %d entries", entries, test.wantEntries)
			}
		})
	}
}

func reverify2ConvertibleRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	reverifyPut(t, root, ".tessl-plugin/plugin.json",
		`{"name":"example/alpha","version":"1.0.0","description":"alpha plugin",`+
			`"repository":"https://github.com/example/alpha","rules":["rules/always.md"]}`+"\n")
	reverifyPut(t, root, "rules/always.md", reverifyAlwaysRule)
	return root
}

func reverify2IgnoringRoot(t *testing.T) string {
	t.Helper()
	root := reverify2ConvertibleRoot(t)
	reverifyPut(t, root, ".tesslignore", "docs/\n")
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}
