package tesslplugin

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestPrivateTrueBlocks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	plugin := alphaPlugin(false)
	plugin["private"] = true
	writePluginJSON(t, root, plugin)
	writeAlphaSources(t, root)

	report, err := Convert(Options{PackageRoot: root})
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeUnmappedField || conv.Field != "private" {
		t.Fatalf("err = %v", err)
	}
	if len(report.Unmapped) != 1 || report.Unmapped[0].Field != "private" {
		t.Fatalf("unmapped = %#v", report.Unmapped)
	}
	if _, statErr := os.Stat(filepath.Join(root, manifest.Filename)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("wrote YAML for private:true")
	}
}

func TestDivergentPrivateIsAmbiguous(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	plugin := pluginShape("example/alpha", "1.0.0")
	plugin["private"] = false
	tile := tileShape("example/alpha", "1.0.0")
	tile["private"] = true
	writePluginJSON(t, root, plugin)
	writeTileJSON(t, root, tile)
	writeAlphaSources(t, root)

	_, err := Convert(Options{PackageRoot: root})
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeAmbiguousManifest || conv.Field != "private" {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, manifest.Filename)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("wrote YAML for divergent private")
	}
}

func TestPrivateFalseIsNotReported(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, alphaPlugin(false))
	writeAlphaSources(t, root)

	report, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range report.Lossy {
		if item.Field == "private" {
			t.Fatalf("private:false reported as lossy: %#v", report.Lossy)
		}
	}
	for _, item := range report.Unmapped {
		if item.Field == "private" {
			t.Fatalf("private:false reported as unmapped: %#v", report.Unmapped)
		}
	}
}

func TestProvenanceIsLossyNotBlocking(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	plugin := alphaPlugin(false)
	plugin["homepage"] = "https://example.test"
	plugin["license"] = "Apache-2.0"
	plugin["author"] = map[string]string{"name": "Ada", "email": "ada@example.test"}
	writePluginJSON(t, root, plugin)
	writeAlphaSources(t, root)

	report, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Wrote {
		t.Fatal("provenance blocked the write")
	}
	fields := map[string]string{}
	for _, item := range report.Lossy {
		fields[item.Field] = item.Value
	}
	if fields["homepage"] != "https://example.test" || fields["license"] != "Apache-2.0" || fields["author.name"] != "Ada" {
		t.Fatalf("lossy = %#v", report.Lossy)
	}
}

func TestIgnoreLinesEchoedVerbatim(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, alphaPlugin(false))
	writeAlphaSources(t, root)
	writeFile(t, root, ".tesslignore", []byte("# comment\n\nREADME.md\nhooks/tests/\n"), 0o644)
	writeFile(t, root, "README.md", []byte("# readme\n"), 0o644)

	report, err := Convert(Options{PackageRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Ignored) != 2 {
		t.Fatalf("ignored = %#v", report.Ignored)
	}
	if report.Ignored[0].Path != "README.md" || report.Ignored[0].Reason != "tesslignore" {
		t.Fatalf("ignored = %#v", report.Ignored)
	}
	if report.Ignored[1].Path != "hooks/tests/" {
		t.Fatalf("ignored = %#v", report.Ignored)
	}
}

func TestPycacheInSkillTreeBlocks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePluginJSON(t, root, alphaPlugin(false))
	writeAlphaSources(t, root)
	writeFile(t, root, "skills/review-change/pkg/__pycache__/mod.pyc", []byte("not-binary-just-named"), 0o644)

	_, err := Convert(Options{PackageRoot: root})
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeUnpublishableContent {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, manifest.Filename)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("wrote YAML for unpublishable skill tree")
	}
}

func TestConvertUnknownKeyIsRejected(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	plugin := alphaPlugin(false)
	plugin["mystery"] = true
	writePluginJSON(t, root, plugin)
	writeAlphaSources(t, root)

	_, err := Convert(Options{PackageRoot: root})
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != CodeUnknownField {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, manifest.Filename)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("wrote YAML for unknown field")
	}
}

func TestFormatTextNamesSource(t *testing.T) {
	t.Parallel()

	text := FormatText(Report{
		SourceManifest: pluginManifest,
		Manifest:       manifest.Filename,
		Package:        "example/alpha",
		Version:        "1.0.0",
		Wrote:          true,
		Artifacts:      []ArtifactRecord{{ID: "always", Kind: "rule", Path: "rules/always.md"}},
	})
	if text == "" {
		t.Fatal("empty text report")
	}
}

func TestFormatFailureTextRendersUnmapped(t *testing.T) {
	t.Parallel()

	text := FormatFailureText(Report{Unmapped: []UnmappedItem{{Field: "private", Reason: "private cannot be published"}}})
	if text != "unmapped:\n  - private: private cannot be published\n" {
		t.Fatalf("text = %q", text)
	}
	if text := FormatFailureText(Report{}); text != "" {
		t.Fatalf("empty failure report = %q", text)
	}
}
