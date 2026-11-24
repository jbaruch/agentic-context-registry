package dependency

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
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

func TestVendorPreviewRejectsContentHashDrift(t *testing.T) {
	t.Parallel()
	root, locked := writeVendorFixture(t)
	packageRoot := filepath.Join(root, ".agents/vendor/example/orphan")
	value := manifest.Manifest{
		SchemaVersion: manifest.CurrentSchemaVersion,
		Name:          "example/orphan",
		Version:       "legacy",
		Artifacts: manifest.Artifacts{Rules: []manifest.RuleArtifact{{
			ID: "always", Path: "rules/always.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways},
		}}},
	}
	resolver := NewResolver(vendorPanicGitHub{})
	if err := resolver.RegisterVendorPreview(locked.Source, packageRoot, value); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "rules/always.md"), []byte("Drifted.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := resolver.MaterializeLockedAt(context.Background(), root, locked)
	if err == nil || !strings.Contains(err.Error(), "content hash mismatch for vendor:example/orphan") {
		t.Fatalf("preview drift error = %v", err)
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
	persisted, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Lock.SchemaVersion != VendorSchemaVersion || !reflect.DeepEqual(persisted.Lock.Dependencies, []LockedDependency{locked}) {
		t.Fatalf("persisted vendor lock = %+v", persisted.Lock)
	}
}

func TestVendorLockRebuildPreservesLocalSchema(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refresh bool
		missing bool
	}{
		{name: "retained local row"},
		{name: "refreshed local row", refresh: true},
		{name: "missing local row", refresh: true, missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ACR_STATE_HOME", t.TempDir())
			root, vendor := writeVendorFixture(t)
			source := t.TempDir()
			if err := ExtractPackageArchive(packageArchive(t, "1.0.0", "original\n"), source); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			service := NewService(NewResolver(vendorPanicGitHub{}))
			if _, err := service.InstallLocal(ctx, root, source, false); err != nil {
				t.Fatal(err)
			}
			state, err := LoadState(root)
			if err != nil {
				t.Fatal(err)
			}
			original := state.Lock.Dependencies[0]
			state.Project.Dependencies = append(state.Project.Dependencies, Declaration{Source: vendor.Source, Requested: vendor.Requested})
			if tc.missing {
				state.Lock.Dependencies = nil
			}
			if err := WriteState(root, state); err != nil {
				t.Fatal(err)
			}
			writeLocalTestFile(t, source, "guidance.md", "edited\n", 0o644)

			var result ChangeResult
			if tc.refresh {
				result, err = service.Reconcile(ctx, root, false)
			} else {
				result, err = service.Update(ctx, root, "", false)
			}
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := LoadState(root)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Project.SchemaVersion != LocalSchemaVersion || persisted.Lock.SchemaVersion != LocalSchemaVersion {
				t.Fatalf("persisted project and lock grades = %d and %d, want %d", persisted.Project.SchemaVersion, persisted.Lock.SchemaVersion, LocalSchemaVersion)
			}
			if !result.Changed || len(persisted.Lock.Dependencies) != 2 || !reflect.DeepEqual(persisted.Lock.Dependencies, result.Dependencies) {
				t.Fatalf("persisted rows = %+v, operation result = %+v", persisted.Lock.Dependencies, result)
			}
			if got := lockFor(persisted, vendor.Source); got == nil || !reflect.DeepEqual(*got, vendor) {
				t.Fatalf("persisted vendor row = %+v, want %+v", got, vendor)
			}
			local := lockFor(persisted, original.Source)
			if local == nil {
				t.Fatalf("missing persisted local row for %s", original.Source)
			}
			if tc.refresh {
				if local.ContentHash == original.ContentHash {
					t.Fatalf("local row kept its old hash %s after the source changed", original.ContentHash)
				}
				original.ContentHash = local.ContentHash
			}
			if !reflect.DeepEqual(*local, original) {
				t.Fatalf("persisted local row = %+v, want %+v", *local, original)
			}
		})
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
	report, err := NewService(NewResolver(vendorPanicGitHub{})).OutdatedReport(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	rows := report.Dependencies
	if len(rows) != 1 || rows[0].Status != OutdatedVendored || rows[0].Actionable() || rows[0].CurrentContentHash != locked.ContentHash || rows[0].CurrentCommit != "" {
		t.Fatalf("outdated = %#v", rows)
	}
	message := outdatedMessage(report)
	if !strings.Contains(message, "Vendored (not tracked upstream):") || !strings.Contains(message, "acr migrate tessl --map <ws>/<pkg>=github:owner/repo") {
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
