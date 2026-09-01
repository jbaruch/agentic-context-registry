package adaptertest

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

// These tests cover finding F5 (reviewer): Detect, wantsError, and
// dirExists converted every ReadFile/Stat failure into "absent," including
// permission and I/O errors that are not fs.ErrNotExist. Only a genuine
// missing-file error may read as absence; every other error must propagate.

func TestWantsErrorPropagatesNonNotExistStatFailure(t *testing.T) {
	t.Parallel()

	injected := errors.New("injected stat failure")
	_, err := wantsErrorWith("irrelevant/path", func(string) (os.FileInfo, error) { return nil, injected })
	if !errors.Is(err, injected) {
		t.Fatalf("wantsErrorWith() error = %v, want the injected error propagated, not read as absent", err)
	}
}

func TestDirExistsPropagatesNonNotExistStatFailure(t *testing.T) {
	t.Parallel()

	injected := errors.New("injected stat failure")
	_, err := dirExistsWith("irrelevant/path", func(string) (os.FileInfo, error) { return nil, injected })
	if !errors.Is(err, injected) {
		t.Fatalf("dirExistsWith() error = %v, want the injected error propagated, not read as absent", err)
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
