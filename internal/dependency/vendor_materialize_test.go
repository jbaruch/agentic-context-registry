package dependency

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestVendorLockRebuildsOfflineWhenMissing(t *testing.T) {
	t.Parallel()
	root, locked := writeVendorFixture(t)
	state := State{
		Project: Project{SchemaVersion: VendorSchemaVersion, Dependencies: []Declaration{{Source: locked.Source, Requested: "vendored"}}},
		Lock:    Lockfile{SchemaVersion: BaselineSchemaVersion},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	result, err := NewService(NewResolver(vendorPanicGitHub{})).Reconcile(context.Background(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || len(result.Dependencies) != 1 || result.Dependencies[0].ContentHash != locked.ContentHash {
		t.Fatalf("rebuilt result = %#v", result)
	}
}

func TestVendorSourceRejectedByInstallAndUpdate(t *testing.T) {
	t.Parallel()
	root, locked := writeVendorFixture(t)
	state := State{Project: Project{SchemaVersion: VendorSchemaVersion, Dependencies: []Declaration{{Source: locked.Source, Requested: "vendored"}}}, Lock: Lockfile{SchemaVersion: VendorSchemaVersion, Dependencies: []LockedDependency{locked}}}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	service := NewService(NewResolver(vendorPanicGitHub{}))
	for name, operation := range map[string]func() error{
		"install": func() error {
			_, err := service.Install(context.Background(), root, locked.Source, "vendored", DowngradeUnset, true)
			return err
		},
		"update": func() error { _, err := service.Update(context.Background(), root, locked.Source, true); return err },
		"resume": func() error { _, err := service.Resume(context.Background(), root, locked.Source, true); return err },
	} {
		t.Run(name, func(t *testing.T) {
			err := operation()
			if err == nil || !strings.Contains(err.Error(), "acr migrate tessl --vendor-unmapped") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestOutdatedReportsVendoredAsNonActionable(t *testing.T) {
	t.Parallel()
	root, locked := writeVendorFixture(t)
	writeVendorState(t, root, locked)
	rows, err := NewService(NewResolver(vendorPanicGitHub{})).Outdated(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != OutdatedVendored || rows[0].Actionable() {
		t.Fatalf("outdated = %#v", rows)
	}
	if message := outdatedMessage(rows); !strings.Contains(message, "Vendored dependencies (non-actionable)") {
		t.Fatalf("message = %q", message)
	}
}

func TestListRendersVendorRow(t *testing.T) {
	t.Parallel()
	root, locked := writeVendorFixture(t)
	writeVendorState(t, root, locked)
	rows, err := NewService(NewResolver(vendorPanicGitHub{})).List(root)
	if err != nil {
		t.Fatal(err)
	}
	message := listMessage(rows)
	if !strings.Contains(message, "vendored legacy sha256:") || bytes.Contains([]byte(message), []byte(" -> \n")) {
		t.Fatalf("list = %q", message)
	}
}

func writeVendorState(t *testing.T, root string, locked LockedDependency) {
	t.Helper()
	state := State{Project: Project{SchemaVersion: VendorSchemaVersion, Dependencies: []Declaration{{Source: locked.Source, Requested: "vendored"}}}, Lock: Lockfile{SchemaVersion: VendorSchemaVersion, Dependencies: []LockedDependency{locked}}}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
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
