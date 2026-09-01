package adaptertest

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

var update = flag.Bool("update", false, "rewrite golden want/ fixtures from actual adapter output")

// RunGolden runs every case directory under testdata/ against the given
// adapter and compiler: it renders through Coordinator.Realize, applies any
// resulting intents through realize.Engine, and compares the deterministic
// plan and the complete resulting project tree against that case's want/
// fixtures. A case with want/error.json expects Coordinator.Realize itself
// to fail with that exact message; every other case expects success. With
// -update, it rewrites want/ from the actual output instead of comparing.
func RunGolden(t *testing.T, adapterUnderTest adapter.Adapter, compilerUnderTest adapter.SharedCompiler) {
	t.Helper()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		t.Run(name, func(t *testing.T) {
			runCase(t, filepath.Join("testdata", name), adapterUnderTest, compilerUnderTest)
		})
	}
}

func runCase(t *testing.T, caseDir string, adapterUnderTest adapter.Adapter, compilerUnderTest adapter.SharedCompiler) {
	t.Helper()
	packageDir := filepath.Join(caseDir, "package")
	loaded, err := manifest.Load(packageDir)
	if err != nil {
		t.Fatalf("load fixture package: %v", err)
	}

	projectDir := t.TempDir()
	if projectSource := filepath.Join(caseDir, "project"); dirExists(projectSource) {
		if err := copyTree(projectSource, projectDir); err != nil {
			t.Fatalf("seed project tree: %v", err)
		}
	}

	pkg := adapter.Package{Source: "github:" + loaded.Name, Root: os.DirFS(packageDir), Manifest: loaded}
	coordinator, err := adapter.NewCoordinator(compilerUnderTest, adapterUnderTest)
	if err != nil {
		t.Fatalf("construct coordinator: %v", err)
	}

	snapshot := adapter.NewFSSnapshot(os.DirFS(projectDir))
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}
	intents, realizeErr := coordinator.Realize(context.Background(), snapshot, []adapter.Package{pkg}, previous)

	errorPath := filepath.Join(caseDir, "want", "error.json")
	if *update {
		updateCase(t, caseDir, errorPath, realizeErr, projectDir, previous, intents)
		return
	}

	if wantsError(errorPath) {
		if realizeErr == nil {
			t.Fatalf("Realize() succeeded, want the error recorded in %s", errorPath)
		}
		assertGoldenError(t, errorPath, realizeErr)
		return
	}
	if realizeErr != nil {
		t.Fatalf("Realize() = %v, want success (no %s fixture present)", realizeErr, errorPath)
	}

	plan, err := realize.NewEngine().Run(projectDir, previous, intents, realize.ModeApply, func(realize.Ledger) error { return nil })
	if err != nil {
		t.Fatalf("apply plan: %v", err)
	}
	assertGoldenPlan(t, caseDir, plan)
	assertGoldenFiles(t, caseDir, projectDir)
}

func wantsError(errorPath string) bool {
	_, err := os.Stat(errorPath)
	return err == nil
}

type goldenError struct {
	Error string `json:"error"`
}

func assertGoldenError(t *testing.T, errorPath string, gotErr error) {
	t.Helper()
	raw, err := os.ReadFile(errorPath)
	if err != nil {
		t.Fatalf("read %s: %v", errorPath, err)
	}
	var want goldenError
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode %s: %v", errorPath, err)
	}
	if gotErr.Error() != want.Error {
		t.Fatalf("Realize() error = %q, want %q", gotErr.Error(), want.Error)
	}
}

func assertGoldenPlan(t *testing.T, caseDir string, plan realize.Plan) {
	t.Helper()
	planPath := filepath.Join(caseDir, "want", "plan.json")
	want, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read %s: %v", planPath, err)
	}
	got := marshalPlan(t, plan)
	if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
		t.Fatalf("plan mismatch.\n got: %s\nwant: %s", got, want)
	}
}

func assertGoldenFiles(t *testing.T, caseDir, projectDir string) {
	t.Helper()
	wantDir := filepath.Join(caseDir, "want", "files")
	want := readTree(t, wantDir)
	got := readTree(t, projectDir)
	for path, wantContent := range want {
		gotContent, ok := got[path]
		if !ok {
			t.Errorf("missing realized file %q", path)
			continue
		}
		if gotContent != wantContent {
			t.Errorf("realized file %q content mismatch.\n got: %s\nwant: %s", path, gotContent, wantContent)
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			t.Errorf("unexpected realized file %q", path)
		}
	}
}

func updateCase(t *testing.T, caseDir, errorPath string, realizeErr error, projectDir string, previous realize.Ledger, intents []realize.Intent) {
	t.Helper()
	wantDir := filepath.Join(caseDir, "want")
	if err := os.RemoveAll(wantDir); err != nil {
		t.Fatalf("clear %s: %v", wantDir, err)
	}
	if err := os.MkdirAll(wantDir, 0o755); err != nil {
		t.Fatalf("create %s: %v", wantDir, err)
	}
	if realizeErr != nil {
		encoded, err := json.MarshalIndent(goldenError{Error: realizeErr.Error()}, "", "  ")
		if err != nil {
			t.Fatalf("encode error fixture: %v", err)
		}
		if err := os.WriteFile(errorPath, append(encoded, '\n'), 0o644); err != nil {
			t.Fatalf("write %s: %v", errorPath, err)
		}
		return
	}
	plan, err := realize.NewEngine().Run(projectDir, previous, intents, realize.ModeApply, func(realize.Ledger) error { return nil })
	if err != nil {
		t.Fatalf("apply plan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wantDir, "plan.json"), append(marshalPlan(t, plan), '\n'), 0o644); err != nil {
		t.Fatalf("write plan.json: %v", err)
	}
	if err := copyTree(projectDir, filepath.Join(wantDir, "files")); err != nil {
		t.Fatalf("write want/files: %v", err)
	}
}

func marshalPlan(t *testing.T, plan realize.Plan) []byte {
	t.Helper()
	encoded, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatalf("encode plan: %v", err)
	}
	return encoded
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := make(map[string]string)
	if !dirExists(root) {
		return tree
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[filepath.ToSlash(relative)] = string(content)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return tree
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, content, info.Mode().Perm())
	})
}
