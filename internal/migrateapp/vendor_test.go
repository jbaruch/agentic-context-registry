package migrateapp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestMapSupersedesVendor(t *testing.T) {
	t.Parallel()
	root := writeUnmappedConsumer(t)
	if _, err := newService(vendorPanicRemote{}).Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	remote := &integrationGitHub{release: dependency.Release{ID: 7, Tag: "v1.0.0"}, commit: strings.Repeat("7", 40), archive: orphanPackageArchive(t)}
	mappings, err := migrate.ParseInlineMappings([]string{"example/orphan=github:example/orphan@latest"})
	if err != nil {
		t.Fatal(err)
	}
	report, err := newService(remote).Migrate(context.Background(), root, Options{CLIMappings: mappings})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Wrote || len(report.Lock.Dependencies) != 1 || report.Lock.Dependencies[0].Source != "github:example/orphan" {
		t.Fatalf("supersede report = %#v", report)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents/vendor/example/orphan")); !os.IsNotExist(err) {
		t.Fatalf("vendor tree remains after supersede: %v", err)
	}
}

func TestMapWinsOverVendorUnmapped(t *testing.T) {
	t.Parallel()
	root := writeUnmappedConsumer(t)
	remote := &integrationGitHub{release: dependency.Release{ID: 8, Tag: "v1.0.0"}, commit: strings.Repeat("8", 40), archive: orphanPackageArchive(t)}
	mappings, err := migrate.ParseInlineMappings([]string{"example/orphan=github:example/orphan@latest"})
	if err != nil {
		t.Fatal(err)
	}
	report, err := newService(remote).Migrate(context.Background(), root, Options{CLIMappings: mappings, VendorUnmapped: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Vendored) != 0 {
		t.Fatalf("explicit mapping also planned a vendor: %#v", report.Vendored)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents/vendor/example/orphan")); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote vendor tree: %v", err)
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

func orphanPackageArchive(t *testing.T) []byte {
	t.Helper()
	manifest := "schemaVersion: 1\nname: example/orphan\nversion: 1.0.0\nsource:\n  repository: https://github.com/example/orphan\nartifacts:\n  rules:\n    - id: always\n      path: rules/always.md\n      activation:\n        mode: always\n"
	files := map[string]struct {
		content string
		mode    int64
	}{
		"agent-plugin.yaml": {manifest, 0o644},
		"rules/always.md":   {"Always.\n", 0o644},
	}
	var encoded bytes.Buffer
	gzipWriter := gzip.NewWriter(&encoded)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, file := range files {
		data := []byte(file.content)
		header := &tar.Header{Name: "example-orphan-commit/" + name, Mode: file.mode, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}
