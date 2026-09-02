package dependency_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/publish"
)

func TestPublishedArchiveExtractsThroughConsumerPath(t *testing.T) {
	t.Parallel()

	manifestBytes := []byte("schemaVersion: 1\nname: owner/plugin\nversion: 1.2.3\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  rules:\n    - id: guidance\n      path: rules/guidance.md\n      activation:\n        mode: always\n")
	files := []publish.File{
		{Path: manifest.Filename, Mode: 0o644, Content: manifestBytes},
		{Path: "rules/guidance.md", Mode: 0o644, Content: []byte("Use deterministic tests.\n")},
	}
	value := manifest.Manifest{
		SchemaVersion: 1, Name: "owner/plugin", Version: "1.2.3",
		Source:    manifest.Source{Repository: "https://github.com/owner/plugin"},
		Artifacts: manifest.Artifacts{Rules: []manifest.RuleArtifact{{ID: "guidance", Path: "rules/guidance.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}}},
	}
	assets, err := publish.BuildReleaseAssets(value, publish.Identity{Tag: "v1.2.3", Commit: strings.Repeat("a", 40)}, files, []adapter.Descriptor{{ID: "fixture", Version: "1.0.0", Boundary: 1}}, "acr test")
	if err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := dependency.ExtractPackageArchive(assets.Archive.Bytes, destination); err != nil {
		t.Fatal(err)
	}
	extracted, err := manifest.Load(destination)
	if err != nil {
		t.Fatal(err)
	}
	contentHash, err := dependency.HashPackageFiles(destination, extracted)
	if err != nil {
		t.Fatal(err)
	}
	if contentHash != assets.Evidence.ContentHash {
		t.Fatalf("consumer hash = %s, published %s", contentHash, assets.Evidence.ContentHash)
	}
	if _, err := os.Stat(filepath.Join(destination, "rules", "guidance.md")); err != nil {
		t.Fatal(err)
	}
}
