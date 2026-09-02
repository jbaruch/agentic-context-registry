package publish

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestReleaseAssetSet(t *testing.T) {
	t.Parallel()

	assets := buildFixtureAssets(t)
	if assets.Archive.Name != "all-agents-1.0.0.tar.gz" || assets.Metadata.Name != MetadataAssetName || assets.Checksums.Name != ChecksumsAssetName {
		t.Fatalf("asset names = %q, %q, %q", assets.Archive.Name, assets.Metadata.Name, assets.Checksums.Name)
	}
	if assets.Evidence.MetadataVersion != 1 || assets.Evidence.ContentHash == "" || len(assets.Evidence.Files) != 2 {
		t.Fatalf("metadata = %#v", assets.Evidence)
	}
	if len(assets.Evidence.Adapters) != 2 || assets.Evidence.Adapters[0].ID != "claude-code" || assets.Evidence.Adapters[1].ID != "cursor" {
		t.Fatalf("adapter evidence = %#v", assets.Evidence.Adapters)
	}
}

func TestChecksumsFileMatchesUploadedBytes(t *testing.T) {
	t.Parallel()

	assets := buildFixtureAssets(t)
	for _, asset := range []Asset{assets.Archive, assets.Metadata} {
		digest := sha256.Sum256(asset.Bytes)
		line := hex.EncodeToString(digest[:]) + "  " + asset.Name + "\n"
		if !strings.Contains(string(assets.Checksums.Bytes), line) {
			t.Fatalf("checksums missing %q: %s", line, assets.Checksums.Bytes)
		}
	}
}

func TestMetadataValidatesAgainstSchema(t *testing.T) {
	t.Parallel()

	assets := buildFixtureAssets(t)
	var decoded Metadata
	if err := json.Unmarshal(assets.Metadata.Bytes, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, assets.Evidence) {
		t.Fatalf("decoded metadata differs from encoded evidence")
	}
	schemaPath := filepath.Join("..", "..", "schemas", "acr-package.schema.json")
	rawSchema, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Schema               string                     `json:"$schema"`
		Title                string                     `json:"title"`
		Type                 string                     `json:"type"`
		AdditionalProperties bool                       `json:"additionalProperties"`
		Required             []string                   `json:"required"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Schema == "" || schema.Title != "ACR Package Release Metadata" || schema.Type != "object" || schema.AdditionalProperties || len(schema.Required) != 12 {
		t.Fatalf("schema contract = %#v", schema)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(assets.Metadata.Bytes, &document); err != nil {
		t.Fatal(err)
	}
	if len(document) != len(schema.Required) {
		t.Fatalf("metadata keys = %d, schema requires %d", len(document), len(schema.Required))
	}
	for _, name := range schema.Required {
		if _, exists := document[name]; !exists {
			t.Errorf("metadata omits schema-required property %q", name)
		}
		if _, documented := schema.Properties[name]; !documented {
			t.Errorf("schema requires undocumented property %q", name)
		}
	}
}

func TestContentHashIsPinned(t *testing.T) {
	t.Parallel()

	assets := buildFixtureAssets(t)
	const want = "sha256:29f2d07581f0fec20485f90f1db1091e7c52cc68b659f7b9e40c66cc346bc953"
	if assets.Evidence.ContentHash != want {
		t.Fatalf("content hash = %s, want %s", assets.Evidence.ContentHash, want)
	}
}

func buildFixtureAssets(t *testing.T) ReleaseAssets {
	t.Helper()
	value := manifest.Manifest{
		SchemaVersion: 1,
		Name:          "example/all-agents",
		Version:       "1.0.0",
		Source:        manifest.Source{Repository: "https://github.com/example/all-agents"},
		Artifacts:     manifest.Artifacts{Scripts: []manifest.ScriptArtifact{{ID: "check", Path: "scripts/check.sh"}}},
	}
	files := []File{
		{Path: manifest.Filename, Mode: 0o644, Content: []byte("schemaVersion: 1\n")},
		{Path: "scripts/check.sh", Mode: 0o755, Content: []byte("#!/bin/sh\nexit 0\n")},
	}
	assets, err := BuildReleaseAssets(value, Identity{Tag: "v1.0.0", Commit: strings.Repeat("a", 40)}, files, []adapter.Descriptor{
		{ID: "cursor", Version: "1.0.0", Boundary: 1},
		{ID: "claude-code", Version: "1.0.0", Boundary: 1},
	}, "acr test")
	if err != nil {
		t.Fatal(err)
	}
	return assets
}
