package adaptertest

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
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
		caseDir := filepath.Join("testdata", name)
		if _, err := os.Stat(filepath.Join(caseDir, "want", "plan.json")); errors.Is(err, fs.ErrNotExist) {
			if _, errorErr := os.Stat(filepath.Join(caseDir, "want", "error.json")); errors.Is(errorErr, fs.ErrNotExist) {
				continue
			}
		}
		t.Run(name, func(t *testing.T) {
			runCase(t, caseDir, filepath.Join(caseDir, "want"), adapterUnderTest, compilerUnderTest)
		})
	}
}

// RunGoldenFixture realizes one shared package/project fixture and compares it
// with the adapter-specific golden directory.
func RunGoldenFixture(t *testing.T, caseDir, wantDir string, adapterUnderTest adapter.Adapter, compilerUnderTest adapter.SharedCompiler) {
	t.Helper()
	runCase(t, caseDir, wantDir, adapterUnderTest, compilerUnderTest)
}

func runCase(t *testing.T, caseDir, wantDir string, adapterUnderTest adapter.Adapter, compilerUnderTest adapter.SharedCompiler) {
	t.Helper()
	packageDir := filepath.Join(caseDir, "package")
	loaded, err := manifest.Load(packageDir)
	if err != nil {
		t.Fatalf("load fixture package: %v", err)
	}

	projectDir := t.TempDir()
	projectSource := filepath.Join(caseDir, "project")
	hasProjectSource, err := dirExists(projectSource)
	if err != nil {
		t.Fatalf("stat %s: %v", projectSource, err)
	}
	if hasProjectSource {
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

	errorPath := filepath.Join(wantDir, "error.json")
	if *update {
		updateCase(t, wantDir, errorPath, realizeErr, projectDir, previous, intents)
		return
	}

	hasErrorFixture, err := wantsError(errorPath)
	if err != nil {
		t.Fatalf("stat %s: %v", errorPath, err)
	}
	if hasErrorFixture {
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
	assertGoldenPlan(t, wantDir, plan)
	assertGoldenFiles(t, wantDir, projectDir)
}

// wantsError reports whether errorPath exists. Only a missing-file error
// reads as "no error fixture"; every other stat failure (permissions, I/O)
// is returned so the caller fails loudly instead of silently choosing the
// success path.
// statFunc matches os.Stat's signature; wantsError and dirExists take it as
// a parameter so tests can inject a deterministic non-NotExist failure
// instead of manipulating real filesystem permissions (which requires
// skipping under root and leaves cleanup errors to chase).
type statFunc func(string) (os.FileInfo, error)

func wantsError(errorPath string) (bool, error) {
	return wantsErrorWith(errorPath, os.Stat)
}

func wantsErrorWith(errorPath string, stat statFunc) (bool, error) {
	_, err := stat(errorPath)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
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

func assertGoldenPlan(t *testing.T, wantDir string, plan realize.Plan) {
	t.Helper()
	planPath := filepath.Join(wantDir, "plan.json")
	want, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read %s: %v", planPath, err)
	}
	got := marshalPlan(t, plan)
	if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
		t.Fatalf("plan mismatch.\n got: %s\nwant: %s", got, want)
	}
}

func assertGoldenFiles(t *testing.T, wantDir, projectDir string) {
	t.Helper()
	want := readTree(t, filepath.Join(wantDir, "files"))
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

func updateCase(t *testing.T, wantDir, errorPath string, realizeErr error, projectDir string, previous realize.Ledger, intents []realize.Intent) {
	t.Helper()
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

// dirExists reports whether path exists and is a directory. Only a
// missing-file error reads as "does not exist"; every other stat failure is
// returned so the caller fails loudly instead of silently treating the path
// as absent.
func dirExists(path string) (bool, error) {
	return dirExistsWith(path, os.Stat)
}

func dirExistsWith(path string, stat statFunc) (bool, error) {
	info, err := stat(path)
	if err == nil {
		return info.IsDir(), nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := make(map[string]string)
	exists, err := dirExists(root)
	if err != nil {
		t.Fatalf("stat %s: %v", root, err)
	}
	if !exists {
		return tree
	}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
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
