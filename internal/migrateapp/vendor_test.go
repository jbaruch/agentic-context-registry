package migrateapp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/migrate"
)

func TestVendorUnmappedProducesHashedTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	files := []migrate.VendorFile{{Path: "README.md", Content: []byte("vendored\n"), Mode: 0o644}}
	plan := migrate.VendorPlan{Destination: ".agents/vendor/example/orphan", Files: files, ContentHash: migrate.HashVendorFiles(files)}
	rollback, err := applyVendorPlan(root, plan)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, ".agents/vendor/example/orphan/README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "vendored\n" || plan.ContentHash == "" {
		t.Fatalf("content = %q, hash = %q", content, plan.ContentHash)
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
}
