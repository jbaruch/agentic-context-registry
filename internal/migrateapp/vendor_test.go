package migrateapp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
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

func TestVendoredManifestIsStableAcrossMigrationRealizeAndCheck(t *testing.T) {
	root := writeUnmappedConsumer(t)
	packageRoot := filepath.Join(root, ".tessl/plugins/example/orphan")
	if err := os.MkdirAll(filepath.Join(packageRoot, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "rules/Error_Handling.md"), []byte("---\nalwaysApply: false\napplyTo: internal/**/*.go — Go source\n---\n\nHandle errors.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "hooks/check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pluginJSON := []byte(`{"name":"example/orphan","version":"legacy","rules":["rules"],"skills":["skills"],"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/check.sh","--fast"]}]}]}}`)
	if err := os.WriteFile(filepath.Join(packageRoot, ".tessl-plugin/plugin.json"), pluginJSON, 0o644); err != nil {
		t.Fatal(err)
	}

	snapshot, err := adapter.NewRootSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := snapshot.Close(); err != nil {
			t.Errorf("close snapshot: %v", err)
		}
	})
	installs, err := migrate.LoadInstalls(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(installs) != 1 {
		t.Fatalf("installs = %#v", installs)
	}
	planned, err := migrate.PlanVendor(snapshot, installs[0])
	if err != nil {
		t.Fatal(err)
	}

	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	state, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Lock.Dependencies) != 1 {
		t.Fatalf("locks = %#v", state.Lock.Dependencies)
	}
	materialized, cleanup, err := dependency.NewResolver(vendorPanicRemote{}).MaterializeLockedAt(context.Background(), root, state.Lock.Dependencies[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(planned.Manifest, materialized.Manifest) {
		t.Fatalf("migration manifest = %#v, materialized manifest = %#v", planned.Manifest, materialized.Manifest)
	}
	if len(materialized.Manifest.Artifacts.Hooks) != 1 || materialized.Manifest.Artifacts.Hooks[0].ID != "check" || !reflect.DeepEqual(materialized.Manifest.Artifacts.Hooks[0].Args, []string{"--fast"}) {
		t.Fatalf("hooks = %#v", materialized.Manifest.Artifacts.Hooks)
	}
	foundScopedRule := false
	for _, rule := range materialized.Manifest.Artifacts.Rules {
		if rule.ID == "error-handling" && rule.Activation.Mode == "paths" && reflect.DeepEqual(rule.Activation.Paths, []string{"internal/**/*.go"}) {
			foundScopedRule = true
		}
	}
	if !foundScopedRule {
		t.Fatalf("rules = %#v", materialized.Manifest.Artifacts.Rules)
	}
	result, err := service.realizer.Run(context.Background(), root, nil, realize.ModeApply)
	if err != nil {
		t.Fatal(err)
	}
	if result.Plan.HasChanges() {
		t.Fatalf("second realization changed output: %#v", result.Plan)
	}
	if _, err := service.realizer.Run(context.Background(), root, nil, realize.ModeCheck); err != nil {
		t.Fatalf("check after vendoring: %v", err)
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

func TestMapSupersedesVendorMismatchKeepsLocalSource(t *testing.T) {
	t.Parallel()
	root := writeUnmappedConsumer(t)
	if _, err := newService(vendorPanicRemote{}).Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	before := hashTree(t, root)
	remote := &integrationGitHub{release: dependency.Release{ID: 9, Tag: "v1.0.0"}, commit: strings.Repeat("9", 40), archive: orphanPackageArchiveWithRule(t, "Different.\n")}
	mappings, err := migrate.ParseInlineMappings([]string{"example/orphan=github:example/orphan@latest"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = newService(remote).Migrate(context.Background(), root, Options{CLIMappings: mappings})
	var migrationErr *Error
	if !errors.As(err, &migrationErr) || migrationErr.Code != "effective_mismatch" {
		t.Fatalf("mismatch error = %v", err)
	}
	if after := hashTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("mismatch changed vendor project: before=%v after=%v", before, after)
	}
}

func TestSupersedeRejectsMalformedOldVendorSourceBeforeRemoval(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(root, ".agents/vendor/sentinel")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const oldSource = "vendor:../escape"
	const newSource = "github:example/orphan"
	existing := dependency.State{Lock: dependency.Lockfile{Dependencies: []dependency.LockedDependency{{Source: oldSource, Kind: dependency.ResolutionVendor}}}}
	desired := dependency.State{Lock: dependency.Lockfile{Dependencies: []dependency.LockedDependency{{Source: newSource, Kind: dependency.ResolutionRelease}}}}
	_, err := newService(vendorPanicRemote{}).validateSupersedes(context.Background(), root, existing, desired, []migrate.Mapping{{From: "../escape", Source: newSource}})
	if err == nil || !strings.Contains(err.Error(), "parse superseded vendor source") {
		t.Fatalf("malformed vendor error = %v", err)
	}
	content, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(content) != "keep\n" {
		t.Fatalf("vendor sentinel = %q, %v", content, readErr)
	}
}

func TestSupersedePropagatesEffectiveDiffEncodingFailure(t *testing.T) {
	root := writeUnmappedConsumer(t)
	if _, err := newService(vendorPanicRemote{}).Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	before := hashTree(t, root)
	original := marshalEffectiveDiffs
	marshalEffectiveDiffs = func(any) ([]byte, error) { return nil, errors.New("injected marshal failure") }
	t.Cleanup(func() { marshalEffectiveDiffs = original })
	remote := &integrationGitHub{release: dependency.Release{ID: 9, Tag: "v1.0.0"}, commit: strings.Repeat("9", 40), archive: orphanPackageArchiveWithRule(t, "Different.\n")}
	mappings, err := migrate.ParseInlineMappings([]string{"example/orphan=github:example/orphan@latest"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = newService(remote).Migrate(context.Background(), root, Options{CLIMappings: mappings})
	if err == nil || !strings.Contains(err.Error(), "encode effective artifact differences") {
		t.Fatalf("marshal error = %v", err)
	}
	if after := hashTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("marshal failure changed project: before=%v after=%v", before, after)
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

func TestFinalizeRemovesOnlyTesslOwned(t *testing.T) {
	root := writeUnmappedConsumer(t)
	unmanaged := filepath.Join(root, "notes.md")
	if err := os.WriteFile(unmanaged, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	gitCommitFixture(t, root)
	beforeDryRun := hashTree(t, root)
	dryRun, err := service.Migrate(context.Background(), root, Options{Finalize: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dryRun.Wrote || len(dryRun.Removed) == 0 {
		t.Fatalf("finalize dry-run = %#v", dryRun)
	}
	if afterDryRun := hashTree(t, root); !mapsEqual(beforeDryRun, afterDryRun) {
		t.Fatalf("finalize dry-run wrote files: before=%v after=%v", beforeDryRun, afterDryRun)
	}
	report, err := service.Migrate(context.Background(), root, Options{Finalize: true})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Wrote || len(report.Removed) == 0 {
		t.Fatalf("finalize report = %#v", report)
	}
	if _, err := os.Stat(filepath.Join(root, "tessl.json")); !os.IsNotExist(err) {
		t.Fatalf("tessl.json remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".tessl")); !os.IsNotExist(err) {
		t.Fatalf(".tessl remains: %v", err)
	}
	if content, err := os.ReadFile(unmanaged); err != nil || string(content) != "keep me\n" {
		t.Fatalf("unmanaged file changed: %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents/vendor/example/orphan/rules/always.md")); err != nil {
		t.Fatalf("vendor tree removed: %v", err)
	}
	if _, err := service.realizer.Run(context.Background(), root, nil, realize.ModeCheck); err != nil {
		t.Fatalf("check after finalize: %v", err)
	}
	second, err := service.Migrate(context.Background(), root, Options{Finalize: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.Wrote || len(second.Removed) != 0 {
		t.Fatalf("second finalize = %#v", second)
	}
}

func TestFinalizeRefusesWhenGitCannotExecute(t *testing.T) {
	root := writeUnmappedConsumer(t)
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	before := hashTree(t, root)
	t.Setenv("PATH", "")
	_, err := service.Migrate(context.Background(), root, Options{Finalize: true})
	if err == nil || !strings.Contains(err.Error(), "inspect Git work tree") {
		t.Fatalf("git execution error = %v", err)
	}
	if after := hashTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("failed Git check changed project: before=%v after=%v", before, after)
	}
}

func TestSurvivingAgentsIgnorePropagatesReadFailure(t *testing.T) {
	root := t.TempDir()
	original := readFinalizationFile
	readFinalizationFile = func(filename string) ([]byte, error) {
		return nil, &os.PathError{Op: "read", Path: filename, Err: fs.ErrPermission}
	}
	t.Cleanup(func() { readFinalizationFile = original })
	_, err := survivingAgentsIgnore(root)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("gitignore read error = %v", err)
	}
}

func TestStaleReferenceScanPropagatesTrackedReadFailure(t *testing.T) {
	root := t.TempDir()
	tracked := filepath.Join(root, "notes.md")
	if err := os.WriteFile(tracked, []byte("references .tessl/state\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommitFixture(t, root)
	original := readFinalizationFile
	readFinalizationFile = func(filename string) ([]byte, error) {
		if filename == tracked {
			return nil, &os.PathError{Op: "read", Path: filename, Err: fs.ErrPermission}
		}
		return os.ReadFile(filename)
	}
	t.Cleanup(func() { readFinalizationFile = original })
	_, err := findStaleReferences(root, nil)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("tracked read error = %v", err)
	}
}

func TestFinalizeRollsBackOnFailure(t *testing.T) {
	root := writeUnmappedConsumer(t)
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	before := hashTree(t, root)
	setFinalizationHooks(t, realize.FileTransactionHooks{AfterEdit: func(index int, _ realize.FileTransactionEdit) error {
		if index == 0 {
			return errors.New("injected finalization failure after operation 1")
		}
		return nil
	}})
	if _, err := service.Migrate(context.Background(), root, Options{Finalize: true}); err == nil {
		t.Fatal("injected finalization failure succeeded")
	}
	if after := hashTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("failed finalization changed project: before=%v after=%v", before, after)
	}
}

func TestFinalizeDryRunWritesNothing(t *testing.T) {
	root := writeUnmappedConsumer(t)
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	before := hashTree(t, root)
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0o755)
	report, err := service.Migrate(context.Background(), root, Options{Finalize: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Wrote || len(report.Removed) == 0 {
		t.Fatalf("dry-run report = %#v", report)
	}
	if after := hashTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("finalize dry-run changed project: before=%v after=%v", before, after)
	}
}

func TestFinalizeIdempotentSecondRun(t *testing.T) {
	root := writeUnmappedConsumer(t)
	writeRetainedMCP(t, root)
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Migrate(context.Background(), root, Options{Finalize: true}); err != nil {
		t.Fatal(err)
	}
	before := hashTree(t, root)
	report, err := service.Migrate(context.Background(), root, Options{Finalize: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Wrote || len(report.Removed) != 0 {
		t.Fatalf("second finalize = %#v", report)
	}
	if after := hashTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("second finalize changed residue: before=%v after=%v", before, after)
	}
}

func TestMigrateAfterFinalizeReportsNoTesslInstall(t *testing.T) {
	root := writeUnmappedConsumer(t)
	writeRetainedMCP(t, root)
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Migrate(context.Background(), root, Options{Finalize: true}); err != nil {
		t.Fatal(err)
	}
	report, err := service.Migrate(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Wrote || len(report.Removed) != 0 {
		t.Fatalf("post-finalize migration = %#v", report)
	}
	if _, err := os.Stat(filepath.Join(root, ".cursor", "mcp.json")); err != nil {
		t.Fatalf("MCP residue removed: %v", err)
	}
}

func TestFinalizeRollbackReportListsRemovals(t *testing.T) {
	root := writeUnmappedConsumer(t)
	spliceEntries := []string{
		`{"hooks":[{"type":"command","command":"tessl hook run --plugin-path=\".tessl/plugins/example/orphan\" --event=\"SessionStart\" --slot=one"}]}`,
		`{"hooks":[{"type":"command","command":"tessl hook run --plugin-path=\".tessl/plugins/example/orphan\" --event=\"SessionStart\" --slot=two"}]}`,
		`{"hooks":[{"type":"command","command":"tessl hook run --plugin-path=\".tessl/plugins/example/orphan\" --event=\"SessionStart\" --slot=three"}]}`,
	}
	settings := []byte(`{"hooks":{"SessionStart":[` + strings.Join(spliceEntries, ",") + `]}}`)
	if err := os.WriteFile(filepath.Join(root, ".claude/settings.json"), settings, 0o644); err != nil {
		t.Fatal(err)
	}
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	report, err := service.Migrate(context.Background(), root, Options{Finalize: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[migrate.RemovalRecord]bool{}
	for _, item := range spliceEntries {
		want[migrate.RemovalRecord{Path: ".claude/settings.json", Kind: "structured-entry", ID: "tessl.hooks.example/orphan", Hash: migrate.HashFinalizationContent([]byte(item)), Operation: "splice"}] = true
	}
	for _, item := range []struct {
		path string
		kind string
		id   string
	}{
		{path: ".tessl/plugins/example/orphan/.tessl-plugin/plugin.json", kind: "tessl-state"},
		{path: ".tessl/plugins/example/orphan/rules/always.md", kind: "tessl-state"},
		{path: ".tessl/plugins/example/orphan/skills/review/SKILL.md", kind: "tessl-state"},
		{path: "tessl.json", kind: "manifest"},
	} {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(item.path)))
		if err != nil {
			t.Fatal(err)
		}
		want[migrate.RemovalRecord{Path: item.path, Kind: item.kind, ID: item.id, Hash: migrate.HashFinalizationContent(content), Operation: "delete"}] = true
	}
	want[migrate.RemovalRecord{
		Path: ".claude/skills/tessl__review", Kind: "skill", ID: "review", Operation: "delete",
		Hash: migrate.HashFinalizationContent([]byte("../../.tessl/plugins/example/orphan/skills/review")), Replacement: ".claude/skills/acr__example__orphan__review",
	}] = true
	got := make(map[migrate.RemovalRecord]bool, len(report.Removed))
	for _, removal := range report.Removed {
		got[removal] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("removal set = %#v, want %#v", got, want)
	}
}

func TestFinalizeRetainsAmbiguousAndModified(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		retainPath string
		edit       func(*testing.T, string)
	}{
		{
			name: "marked span with extra prose",
			path: "AGENTS.md",
			edit: func(t *testing.T, root string) {
				writeFile(t, root, "AGENTS.md", []byte("# User\n\n## Agent Rules <!-- tessl-managed -->\n\n@.tessl/RULES.md\n\n### Operator notes\n\nKeep these bytes.\n"), 0o644)
			},
		},
		{
			name:       "native copy diverging from plugin tree",
			path:       ".claude/skills/tessl__review-change/SKILL.md",
			retainPath: ".claude/skills/tessl__review-change",
			edit: func(t *testing.T, root string) {
				native := filepath.Join(root, ".claude/skills/tessl__review-change")
				if err := os.Remove(native); err != nil {
					t.Fatal(err)
				}
				writeFile(t, root, ".claude/skills/tessl__review-change/SKILL.md", []byte("# Hand edited\n"), 0o644)
			},
		},
		{
			name: "cursor mdc remainder mismatch",
			path: ".cursor/rules/tessl__rule__example__alpha__always-rule.mdc",
			edit: func(t *testing.T, root string) {
				source, err := os.ReadFile(filepath.Join(root, ".tessl/plugins/example/alpha/rules/always-rule.md"))
				if err != nil {
					t.Fatal(err)
				}
				native := append([]byte("---\nalwaysApply: true\n---\n\n"), source...)
				native = append(native, []byte("hand-edited\n")...)
				writeFile(t, root, ".cursor/rules/tessl__rule__example__alpha__always-rule.mdc", native, 0o644)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := seedConsumer(t)
			test.edit(t, root)
			filename := filepath.Join(root, filepath.FromSlash(test.path))
			before, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}
			remote := &integrationGitHub{release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("4", 40), archive: migrationPackageArchive(t)}
			mappings, err := migrate.ParseInlineMappings([]string{"example/alpha=github:example/alpha@latest"})
			if err != nil {
				t.Fatal(err)
			}
			report, err := newService(remote).Migrate(context.Background(), root, Options{Finalize: true, CLIMappings: mappings})
			var migrationErr *Error
			if !errors.As(err, &migrationErr) || migrationErr.Code != "finalization_blocked" {
				t.Fatalf("finalize error = %v", err)
			}
			found := false
			wantRetained := test.retainPath
			if wantRetained == "" {
				wantRetained = test.path
			}
			for _, retained := range report.Retained {
				if retained.Path == wantRetained {
					found = true
				}
			}
			if !found {
				t.Fatalf("retained = %#v, want %s", report.Retained, wantRetained)
			}
			after, readErr := os.ReadFile(filename)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("retained file changed: before=%q after=%q", before, after)
			}
		})
	}
}

func TestFinalizeAbsorbsTesslUpgradeInventory(t *testing.T) {
	t.Run("matching upgrade files enter the live removal plan", func(t *testing.T) {
		root := seedConsumer(t)
		remote := &integrationGitHub{release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("4", 40), archive: migrationPackageArchive(t)}
		service := newService(remote)
		mappings, err := migrate.ParseInlineMappings([]string{"example/alpha=github:example/alpha@latest"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Migrate(context.Background(), root, Options{CLIMappings: mappings}); err != nil {
			t.Fatal(err)
		}
		const added = ".tessl/plugins/example/alpha/upgrade-metadata.json"
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(added)), []byte("{\"installed\":true}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		report, err := service.Migrate(context.Background(), root, Options{Finalize: true, CLIMappings: mappings})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, removal := range report.Removed {
			if removal.Path == added && removal.Kind == "tessl-state" && removal.Operation == "delete" {
				found = true
			}
		}
		if !found {
			t.Fatalf("live upgrade file missing from removals: %#v", report.Removed)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(added))); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("upgrade file remains after finalization: %v", err)
		}
	})

	t.Run("digest moving upgrade blocks finalization", func(t *testing.T) {
		root := seedConsumer(t)
		remote := &integrationGitHub{release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("4", 40), archive: migrationPackageArchive(t)}
		service := newService(remote)
		mappings, err := migrate.ParseInlineMappings([]string{"example/alpha=github:example/alpha@latest"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Migrate(context.Background(), root, Options{CLIMappings: mappings}); err != nil {
			t.Fatal(err)
		}
		rule := filepath.Join(root, ".tessl/plugins/example/alpha/rules/always-rule.md")
		moved := []byte("---\nalwaysApply: true\n---\n# Upgraded body\n")
		if err := os.WriteFile(rule, moved, 0o644); err != nil {
			t.Fatal(err)
		}
		report, err := service.Migrate(context.Background(), root, Options{Finalize: true, CLIMappings: mappings})
		var migrationErr *Error
		if !errors.As(err, &migrationErr) || migrationErr.Code != "finalization_blocked" {
			t.Fatalf("digest-moving error = %v", err)
		}
		if len(report.EffectiveDiffs) == 0 {
			t.Fatalf("digest-moving report = %#v", report)
		}
		content, readErr := os.ReadFile(rule)
		if readErr != nil || !bytes.Equal(content, moved) {
			t.Fatalf("upgraded rule = %q, %v", content, readErr)
		}
	})
}

func TestFinalizeRejectsConcurrentTesslDrift(t *testing.T) {
	tests := []struct {
		name         string
		selectTarget func(int, realize.FileTransactionEdit) bool
	}{
		{name: "before first rename", selectTarget: func(index int, _ realize.FileTransactionEdit) bool { return index == 0 }},
		{name: "before tessl manifest rename", selectTarget: func(_ int, edit realize.FileTransactionEdit) bool { return edit.Path == "tessl.json" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeUnmappedConsumer(t)
			service := newService(vendorPanicRemote{})
			if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
				t.Fatal(err)
			}
			before := hashTree(t, root)
			injected := false
			setFinalizationHooks(t, realize.FileTransactionHooks{BeforeEdit: func(index int, edit realize.FileTransactionEdit) error {
				if injected || !test.selectTarget(index, edit) {
					return nil
				}
				injected = true
				return os.Remove(filepath.Join(root, filepath.FromSlash(edit.Path)))
			}})
			_, err := service.Migrate(context.Background(), root, Options{Finalize: true})
			var migrationErr *Error
			if !errors.As(err, &migrationErr) || migrationErr.Code != "finalization_conflict" {
				t.Fatalf("concurrent drift error = %v", err)
			}
			if !injected {
				t.Fatal("drift hook did not run")
			}
			if after := hashTree(t, root); !mapsEqual(before, after) {
				t.Fatalf("concurrent drift was not fully rolled back: before=%v after=%v", before, after)
			}
		})
	}
}

func TestFinalizeSurvivesProcessKillMidSplice(t *testing.T) {
	if os.Getenv("ACR_TEST_FINALIZE_HELPER") == "1" {
		applyFinalizationFileTransaction = func(project string, edits []realize.FileTransactionEdit, finalize func() error) error {
			return realize.ApplyFileTransactionWithHooks(project, edits, finalize, realize.FileTransactionHooks{
				TransactionID: func() (string, error) { return os.Getenv("ACR_TEST_TRANSACTION_ID"), nil },
				AfterEdit: func(_ int, edit realize.FileTransactionEdit) error {
					if edit.Operation == os.Getenv("ACR_TEST_KILL_OPERATION") {
						os.Exit(86)
					}
					return nil
				}})
		}
		_, _ = newService(vendorPanicRemote{}).Migrate(context.Background(), os.Getenv("ACR_TEST_PROJECT"), Options{Finalize: true})
		os.Exit(0)
	}
	for _, test := range []struct {
		name      string
		operation string
	}{
		{name: "splice", operation: "splice"},
		{name: "removal", operation: "remove"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := writeUnmappedConsumer(t)
			if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# User\n\n## Agent Rules <!-- tessl-managed -->\n\n@.tessl/RULES.md\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			settings := []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"tessl hook run --plugin-path=\".tessl/plugins/example/orphan\" --event=\"SessionStart\""}]},{"hooks":[{"type":"command","command":"user-hook.sh"}]}]}}`)
			if err := os.WriteFile(filepath.Join(root, ".claude/settings.json"), settings, 0o644); err != nil {
				t.Fatal(err)
			}
			service := newService(vendorPanicRemote{})
			if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(os.Args[0], "-test.run=^TestFinalizeSurvivesProcessKillMidSplice$")
			command.Env = append(os.Environ(), "ACR_TEST_FINALIZE_HELPER=1", "ACR_TEST_PROJECT="+root, "ACR_TEST_KILL_OPERATION="+test.operation, "ACR_TEST_TRANSACTION_ID=finalize-kill-"+test.operation)
			err := command.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
				t.Fatalf("helper exit = %v", err)
			}
			result, err := service.Migrate(context.Background(), root, Options{Finalize: true})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Wrote {
				t.Fatalf("recovered finalize = %#v", result)
			}
			if _, err := service.realizer.Run(context.Background(), root, nil, realize.ModeCheck); err != nil {
				content, readErr := os.ReadFile(filepath.Join(root, ".claude/settings.json"))
				t.Fatalf("check after recovery: %v; settings=%q read=%v", err, content, readErr)
			}
		})
	}
}

func TestFinalizeRecoveryConflictPreservesEdit(t *testing.T) {
	root := writeUnmappedConsumer(t)
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	dryRun, err := service.Migrate(context.Background(), root, Options{Finalize: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestFinalizeSurvivesProcessKillMidSplice$")
	command.Env = append(os.Environ(), "ACR_TEST_FINALIZE_HELPER=1", "ACR_TEST_PROJECT="+root, "ACR_TEST_KILL_OPERATION=remove", "ACR_TEST_TRANSACTION_ID=finalize-recovery-conflict")
	if err := command.Run(); err == nil {
		t.Fatal("kill helper completed successfully")
	}
	editPath := filepath.Join(root, filepath.FromSlash(dryRun.Removed[0].Path))
	if err := os.MkdirAll(filepath.Dir(editPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(editPath, []byte("concurrent edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = service.Migrate(context.Background(), root, Options{Finalize: true})
	var recovery *realize.RecoveryConflictError
	if !errors.As(err, &recovery) {
		t.Fatalf("error = %v, want recovery conflict", err)
	}
	content, readErr := os.ReadFile(editPath)
	if readErr != nil || string(content) != "concurrent edit\n" {
		t.Fatalf("concurrent edit = %q, %v", content, readErr)
	}
}

func TestFinalizeConflictsWhenManifestDisappearsMidRun(t *testing.T) {
	root := writeUnmappedConsumer(t)
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	before := hashTree(t, root)
	setFinalizationHooks(t, realize.FileTransactionHooks{BeforeEdit: func(_ int, edit realize.FileTransactionEdit) error {
		if edit.Path == "tessl.json" {
			return os.Remove(filepath.Join(root, "tessl.json"))
		}
		return nil
	}})
	_, err := service.Migrate(context.Background(), root, Options{Finalize: true})
	var migrationErr *Error
	if !errors.As(err, &migrationErr) || migrationErr.Code != "finalization_conflict" {
		t.Fatalf("manifest drift error = %v", err)
	}
	if after := hashTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("manifest drift was not rolled back: before=%v after=%v", before, after)
	}
}

func setFinalizationHooks(t *testing.T, hooks realize.FileTransactionHooks) {
	t.Helper()
	original := applyFinalizationFileTransaction
	applyFinalizationFileTransaction = func(project string, edits []realize.FileTransactionEdit, finalize func() error) error {
		return realize.ApplyFileTransactionWithHooks(project, edits, finalize, hooks)
	}
	t.Cleanup(func() { applyFinalizationFileTransaction = original })
}

func TestFinalizeReanchorsLedgerOutputHash(t *testing.T) {
	root := writeUnmappedConsumer(t)
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(root, "CLAUDE.md")
	acrContent, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	mixed := append([]byte("# Tessl rules <!-- tessl-managed -->\n\n# ACR rules\n"), acrContent...)
	if err := os.WriteFile(claudePath, mixed, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	report, err := service.Migrate(context.Background(), root, Options{Finalize: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Reanchored) != 1 || report.Reanchored[0].Path != "CLAUDE.md" || report.Reanchored[0].BeforeHash == report.Reanchored[0].AfterHash {
		t.Fatalf("reanchored = %#v", report.Reanchored)
	}
	if _, err := service.realizer.Run(context.Background(), root, nil, realize.ModeCheck); err != nil {
		t.Fatalf("check after shared splice: %v", err)
	}
}

func TestFinalizeReportsStaleTesslReferences(t *testing.T) {
	root := writeUnmappedConsumer(t)
	trackedReference := filepath.Join(root, "notes.md")
	if err := os.WriteFile(trackedReference, []byte("see .tessl/plugins/example/orphan and tessl__review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mcp-note.md"), []byte("tessl mcp start reads .tessl/ state\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	gitCommitFixture(t, root)
	if err := os.WriteFile(filepath.Join(root, "untracked.md"), []byte("tessl__review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := service.Migrate(context.Background(), root, Options{Finalize: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.StaleReferences) != 1 {
		t.Fatalf("stale references = %#v", report.StaleReferences)
	}
	stale := report.StaleReferences[0]
	if stale.Path != "notes.md" || stale.Line != 1 || !strings.Contains(stale.Text, "tessl__review") || !strings.Contains(stale.Replacement, "acr__") {
		t.Fatalf("stale reference = %#v, removed = %#v, ledger = %#v", stale, report.Removed, report.Lock.Realization)
	}
}

func TestFinalizeReportsEmptyTesslHookContainerAsRetained(t *testing.T) {
	root := writeUnmappedConsumer(t)
	settings := filepath.Join(root, ".claude/settings.json")
	if err := os.WriteFile(settings, []byte(`{"tessl":{"hooks":{"example/orphan":[]}}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	report, err := service.Migrate(context.Background(), root, Options{Finalize: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, retained := range report.Retained {
		if retained.Path == ".claude/settings.json" && retained.Kind == "structured-container" && retained.ID == "tessl.hooks.example/orphan" && retained.Reason == "empty Tessl hook container" {
			found = true
		}
	}
	if !found {
		t.Fatalf("retained = %#v", report.Retained)
	}
	content, readErr := os.ReadFile(settings)
	if readErr != nil || !bytes.Contains(content, []byte(`"example/orphan":[]`)) {
		t.Fatalf("empty hook container = %q, %v", content, readErr)
	}
}

func TestFinalizeSecondRunWithPartialTesslStateBlocks(t *testing.T) {
	root := writeUnmappedConsumer(t)
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Migrate(context.Background(), root, Options{Finalize: true}); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(root, ".tessl", "leftover")
	if err := os.MkdirAll(filepath.Dir(leftover), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leftover, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := service.Migrate(context.Background(), root, Options{Finalize: true})
	var migrationErr *Error
	if !errors.As(err, &migrationErr) || migrationErr.Code != "finalization_blocked" || !strings.Contains(migrationErr.Error(), "tessl.json") {
		t.Fatalf("partial-state error = %v", err)
	}
	if content, readErr := os.ReadFile(leftover); readErr != nil || string(content) != "keep\n" {
		t.Fatalf("partial state changed = %q, %v", content, readErr)
	}
}

func TestVendorCollisionWithGithubName(t *testing.T) {
	t.Parallel()
	declarations := []dependency.Declaration{
		{Source: "github:example/orphan", Requested: "latest"},
		{Source: "vendor:example/orphan", Requested: "vendored"},
	}
	err := validateSourceCollisions(declarations)
	var migrationErr *Error
	if !errors.As(err, &migrationErr) || migrationErr.Code != "vendor_collision" {
		t.Fatalf("collision error = %v", err)
	}
}

func TestVendorErrorClassificationUsesTypedCauses(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "escape", err: &migrate.VendorEscapeError{Reason: "unsafe path"}, code: "vendor_escape"},
		{name: "collision", err: &vendorCollisionError{Destination: ".agents/vendor/example/orphan"}, code: "vendor_collision"},
		{name: "misleading escape text", err: errors.New("package vendor_escape-tools failed")},
		{name: "misleading collision text", err: errors.New("package already exists with different content metadata")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyVendorError(test.err)
			var migrationErr *Error
			if test.code == "" {
				if errors.As(got, &migrationErr) || !errors.Is(got, test.err) {
					t.Fatalf("classification = %#v", got)
				}
				return
			}
			if !errors.As(got, &migrationErr) || migrationErr.Code != test.code {
				t.Fatalf("classification = %#v, want %s", got, test.code)
			}
		})
	}
}

func TestVendorDestinationHashIOErrorIsNotACollision(t *testing.T) {
	root := t.TempDir()
	plan := migrate.VendorPlan{
		Destination: ".agents/vendor/example/orphan",
		Files:       []migrate.VendorFile{{Path: "README.md", Content: []byte("vendored\n"), Mode: 0o644}},
		ContentHash: "sha256:expected",
	}
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(plan.Destination)), 0o755); err != nil {
		t.Fatal(err)
	}
	original := hashVendorTree
	hashVendorTree = func(string) (string, error) { return "", fs.ErrPermission }
	t.Cleanup(func() { hashVendorTree = original })
	_, _, err := applyVendorPlan(root, plan)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("hash error = %v", err)
	}
	var collision *vendorCollisionError
	if errors.As(err, &collision) {
		t.Fatalf("I/O error classified as collision: %v", err)
	}
}

func TestVendorJSONEnvelope(t *testing.T) {
	root := writeUnmappedConsumer(t)
	application := &Application{service: newService(vendorPanicRemote{}), fallback: cli.UnavailableApplication{}}
	stdout, stderr, exitCode := runCLI(t, application, "migrate", "tessl", "--vendor-unmapped", "--dry-run", "--json", "--project", root)
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"source":"vendor:example/orphan"`) || !strings.Contains(stdout, `"wrote":false`) {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
}

func TestUntrackedTesslManifestRefusalNamesGitAdd(t *testing.T) {
	root := writeUnmappedConsumer(t)
	command := exec.Command("git", "init", "-q")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	application := &Application{service: service, fallback: cli.UnavailableApplication{}}
	_, stderr, exitCode := runCLI(t, application, "migrate", "tessl", "--finalize", "--json", "--project", root)
	if exitCode != cli.ExitConflict || !strings.Contains(stderr, `"code":"finalization_blocked"`) || !strings.Contains(stderr, `"remedy":"git add tessl.json && git commit"`) || !strings.Contains(stderr, "git add tessl.json && git commit") {
		t.Fatalf("exit=%d stderr=%q", exitCode, stderr)
	}
}

func TestFinalizeRequiresEquivalenceUntrackedVendorNamesGitignoreRemedy(t *testing.T) {
	root := writeUnmappedConsumer(t)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("# user policy\n/.agents/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	gitCommitFixture(t, root)
	_, err := service.Migrate(context.Background(), root, Options{Finalize: true})
	var migrationErr *Error
	if !errors.As(err, &migrationErr) || migrationErr.Code != "finalization_blocked" {
		t.Fatalf("untracked vendor error = %v", err)
	}
	if !strings.Contains(migrationErr.Message, `.gitignore:2 "/.agents/"`) || !strings.Contains(migrationErr.Remedy, "/.agents/*") || !strings.Contains(migrationErr.Remedy, "!/.agents/vendor/") {
		t.Fatalf("untracked vendor refusal = %#v", migrationErr)
	}
}

func writeUnmappedConsumer(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	packageRoot := filepath.Join(root, ".tessl/plugins/example/orphan")
	for _, directory := range []string{filepath.Join(packageRoot, ".tessl-plugin"), filepath.Join(packageRoot, "rules"), filepath.Join(packageRoot, "skills", "review"), filepath.Join(root, ".claude/skills")} {
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
		filepath.Join(packageRoot, ".tessl-plugin/plugin.json"): []byte(`{"name":"example/orphan","version":"legacy","rules":["rules"],"skills":["skills"]}`),
		filepath.Join(packageRoot, "rules/always.md"):           []byte("Always.\n"),
		filepath.Join(packageRoot, "skills/review/SKILL.md"):    []byte("# Review\n"),
	}
	for filename, content := range files {
		if err := os.WriteFile(filename, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../../.tessl/plugins/example/orphan/skills/review", filepath.Join(root, ".claude/skills/tessl__review")); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeRetainedMCP(t *testing.T, root string) {
	t.Helper()
	directory := filepath.Join(root, ".cursor")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte(`{"mcpServers":{"tessl":{"command":"tessl","args":["mcp","start"]}}}` + "\n")
	if err := os.WriteFile(filepath.Join(directory, "mcp.json"), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitCommitFixture(t *testing.T, root string) {
	t.Helper()
	commands := [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-qm", "fixture"}}
	for _, arguments := range commands {
		command := exec.Command("git", arguments...)
		command.Dir = root
		command.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=ACR Test", "GIT_AUTHOR_EMAIL=acr@example.invalid",
			"GIT_COMMITTER_NAME=ACR Test", "GIT_COMMITTER_EMAIL=acr@example.invalid",
			"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
		}
	}
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
	return orphanPackageArchiveWithRule(t, "Always.\n")
}

func orphanPackageArchiveWithRule(t *testing.T, ruleBody string) []byte {
	t.Helper()
	manifest := "schemaVersion: 1\nname: example/orphan\nversion: 1.0.0\nsource:\n  repository: https://github.com/example/orphan\nartifacts:\n  rules:\n    - id: always\n      path: rules/always.md\n      activation:\n        mode: always\n  skills:\n    - id: review\n      path: skills/review\n"
	files := map[string]struct {
		content string
		mode    int64
	}{
		"agent-plugin.yaml":      {manifest, 0o644},
		"rules/always.md":        {ruleBody, 0o644},
		"skills/review/SKILL.md": {"# Review\n", 0o644},
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
