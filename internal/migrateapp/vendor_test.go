package migrateapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
)

func TestVendorUnmappedProducesHashedTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	files := []migrate.VendorFile{{Path: "README.md", Content: []byte("vendored\n"), Mode: 0o644}}
	plan := migrate.VendorPlan{Destination: ".agents/vendor/example/orphan", Files: files, ContentHash: migrate.HashVendorFiles(files)}
	changed, rollback, err := applyVendorPlan(root, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first vendor apply reported no change")
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

func TestVendorUnmappedProducesLockedLocalDep(t *testing.T) {
	t.Parallel()
	root := writeUnmappedConsumer(t)
	service := newService(vendorPanicRemote{})
	report, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Wrote || len(report.Vendored) != 1 {
		t.Fatalf("report = %#v", report)
	}
	state, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Lock.Dependencies) != 1 {
		t.Fatalf("locks = %#v", state.Lock.Dependencies)
	}
	locked := state.Lock.Dependencies[0]
	if locked.Source != "vendor:example/orphan" || locked.Requested != "vendored" || locked.Kind != dependency.ResolutionVendor || locked.Commit != "" {
		t.Fatalf("vendor lock = %#v", locked)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents/vendor/example/orphan/rules/always.md")); err != nil {
		t.Fatal(err)
	}
	second, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.Wrote {
		t.Fatalf("second migration wrote: %#v", second)
	}
}

func TestVendorDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	root := writeUnmappedConsumer(t)
	before := hashTree(t, root)
	report, err := newService(vendorPanicRemote{}).Migrate(context.Background(), root, Options{VendorUnmapped: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Wrote || len(report.Vendored) != 1 {
		t.Fatalf("report = %#v", report)
	}
	if after := hashTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("dry-run changed project: before=%v after=%v", before, after)
	}
}

func writeUnmappedConsumer(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	packageRoot := filepath.Join(root, ".tessl/plugins/example/orphan")
	for _, directory := range []string{filepath.Join(packageRoot, ".tessl-plugin"), filepath.Join(packageRoot, "rules"), filepath.Join(root, ".claude/skills")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	document := map[string]any{"name": "consumer", "dependencies": map[string]any{"example/orphan": map[string]string{"version": "legacy"}}}
	tesslJSON, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		filepath.Join(root, "tessl.json"):                       tesslJSON,
		filepath.Join(packageRoot, ".tessl-plugin/plugin.json"): []byte(`{"name":"example/orphan","version":"legacy","rules":["rules"]}`),
		filepath.Join(packageRoot, "rules/always.md"):           []byte("Always.\n"),
	}
	for filename, content := range files {
		if err := os.WriteFile(filename, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

type vendorPanicRemote struct{}

func (vendorPanicRemote) LatestRelease(context.Context, dependency.Repository) (dependency.Release, error) {
	panic("vendor migration contacted GitHub")
}
func (vendorPanicRemote) ReleaseByTag(context.Context, dependency.Repository, string) (dependency.Release, error) {
	panic("vendor migration contacted GitHub")
}
func (vendorPanicRemote) ResolveCommit(context.Context, dependency.Repository, string) (string, error) {
	panic("vendor migration contacted GitHub")
}
func (vendorPanicRemote) DownloadArchive(context.Context, dependency.Repository, string) ([]byte, error) {
	panic("vendor migration contacted GitHub")
}
func (vendorPanicRemote) DownloadReleaseAsset(context.Context, dependency.Repository, dependency.ReleaseAsset) ([]byte, error) {
	panic("vendor migration contacted GitHub")
}
