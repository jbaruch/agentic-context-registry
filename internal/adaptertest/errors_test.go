package adaptertest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

// These tests cover finding F5 (reviewer): Detect, wantsError, and
// dirExists converted every ReadFile/Stat failure into "absent," including
// permission and I/O errors that are not fs.ErrNotExist. Only a genuine
// missing-file error may read as absence; every other error must propagate.

func blockedDirectory(t *testing.T, name string) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission checks are ineffective when running as root")
	}
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, name), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(blocked, 0o755) })
	return blocked
}

func TestWantsErrorPropagatesNonNotExistStatFailure(t *testing.T) {
	t.Parallel()

	blocked := blockedDirectory(t, "error.json")
	_, err := wantsError(filepath.Join(blocked, "error.json"))
	if err == nil {
		t.Fatal("wantsError() error = nil on a permission-denied stat error; it must propagate, not read as absent")
	}
}

func TestDirExistsPropagatesNonNotExistStatFailure(t *testing.T) {
	t.Parallel()

	blocked := blockedDirectory(t, "marker")
	_, err := dirExists(filepath.Join(blocked, "marker"))
	if err == nil {
		t.Fatal("dirExists() error = nil on a permission-denied stat error; it must propagate, not read as absent")
	}
}

// blockedSnapshot is a Snapshot whose ReadFile always returns a non-NotExist
// error, simulating a permission or I/O failure underneath the project
// tree.
type blockedSnapshot struct{ err error }

func (snapshot blockedSnapshot) ReadFile(path string) (adapter.ObservedFile, error) {
	return adapter.ObservedFile{}, snapshot.err
}

func TestReferenceAdapterDetectPropagatesNonNotExistReadFailure(t *testing.T) {
	t.Parallel()

	injected := os.ErrPermission
	reference := NewReferenceAdapter("1.0.0")
	_, err := reference.Detect(context.Background(), adapter.DetectRequest{Project: blockedSnapshot{err: injected}})
	if err == nil {
		t.Fatal("Detect() = nil error, want the injected non-NotExist ReadFile error propagated")
	}
}
