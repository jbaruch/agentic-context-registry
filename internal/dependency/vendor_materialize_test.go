package dependency

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestVendorMakesNoNetworkCall(t *testing.T) {
	t.Parallel()
	root, locked := writeVendorFixture(t)
	materialized, cleanup, err := NewResolver(vendorPanicGitHub{}).MaterializeLockedAt(context.Background(), root, locked)
	if err != nil {
		t.Fatal(err)
	}
	if materialized.Root != filepath.Join(root, ".agents/vendor/example/orphan") {
		t.Fatalf("materialized root = %q", materialized.Root)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(materialized.Root, "rules/always.md")); err != nil {
		t.Fatalf("no-op cleanup removed vendor tree: %v", err)
	}
}

func TestRealizeDoesNotDeleteVendorTree(t *testing.T) {
	t.Parallel()
	root, locked := writeVendorFixture(t)
	resolver := NewResolver(vendorPanicGitHub{})
	_, cleanup, err := resolver.MaterializeLockedAt(context.Background(), root, locked)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(root, ".agents/vendor/example/orphan/rules/always.md")); err != nil || string(content) != "Always.\n" {
		t.Fatalf("vendor bytes after cleanup = %q, %v", content, err)
	}
}

func writeVendorFixture(t *testing.T) (string, LockedDependency) {
	t.Helper()
	root := t.TempDir()
	packageRoot := filepath.Join(root, ".agents/vendor/example/orphan")
	if err := os.MkdirAll(filepath.Join(packageRoot, ".tessl-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(packageRoot, "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	plugin := []byte(`{"name":"example/orphan","version":"legacy","rules":["rules"]}`)
	if err := os.WriteFile(filepath.Join(packageRoot, ".tessl-plugin/plugin.json"), plugin, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "rules/always.md"), []byte("Always.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := HashVendorTree(packageRoot)
	if err != nil {
		t.Fatal(err)
	}
	return root, LockedDependency{Source: "vendor:example/orphan", Requested: "vendored", Kind: ResolutionVendor, PackageVersion: "legacy", ContentHash: hash}
}

type vendorPanicGitHub struct{}

func (vendorPanicGitHub) LatestRelease(context.Context, Repository) (Release, error) {
	panic("vendor materialization contacted GitHub")
}
func (vendorPanicGitHub) ReleaseByTag(context.Context, Repository, string) (Release, error) {
	panic("vendor materialization contacted GitHub")
}
func (vendorPanicGitHub) ResolveCommit(context.Context, Repository, string) (string, error) {
	panic("vendor materialization contacted GitHub")
}
func (vendorPanicGitHub) DownloadArchive(context.Context, Repository, string) ([]byte, error) {
	panic("vendor materialization contacted GitHub")
}
func (vendorPanicGitHub) DownloadReleaseAsset(context.Context, Repository, ReleaseAsset) ([]byte, error) {
	panic("vendor materialization contacted GitHub")
}
