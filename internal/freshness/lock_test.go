package freshness

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestTryLockFileIsNonBlockingAndCreatesItsDirectory proves the file-level
// lock behaves exactly as the project lock does: a second holder is refused
// with ErrLockBusy instead of waiting, the lock file and its directory are
// created on first use, and closing the first holder frees the path.
func TestTryLockFileIsNonBlockingAndCreatesItsDirectory(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), "nested", "record.lock")
	first, err := TryLockFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("lock file was not created: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("lock file mode = %o, want 600", mode)
	}
	second, err := TryLockFile(lockPath)
	if second != nil {
		second.Close()
		t.Fatal("second TryLockFile() acquired an already-held lock")
	}
	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("second TryLockFile() error = %v, want ErrLockBusy", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := TryLockFile(lockPath)
	if err != nil {
		t.Fatalf("TryLockFile() after release: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestProjectLockStillSharesTheFileLock holds the project lock and the
// file-level lock on the same path against each other, so the refactor that
// introduced TryLockFile cannot have left the two on different descriptors.
func TestProjectLockStillSharesTheFileLock(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	store := Store{BaseDirectory: t.TempDir()}
	_, lockPath, err := store.Paths(project)
	if err != nil {
		t.Fatal(err)
	}
	held, err := store.TryLock(project)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	direct, err := TryLockFile(lockPath)
	if direct != nil {
		direct.Close()
		t.Fatal("TryLockFile() acquired the path the project lock holds")
	}
	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("TryLockFile() error = %v, want ErrLockBusy", err)
	}
}

// TestTryLockDescriptorOwnsTheFileItIsGiven proves the descriptor-level lock
// is the same lock the path-level one takes: a descriptor the caller opened
// contends with TryLockFile on the same path, a busy result closes the
// descriptor, and releasing the returned lock frees the path.
func TestTryLockDescriptorOwnsTheFileItIsGiven(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), "record.lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	held, err := TryLockDescriptor(file)
	if err != nil {
		t.Fatal(err)
	}
	byPath, err := TryLockFile(lockPath)
	if byPath != nil {
		byPath.Close()
		t.Fatal("TryLockFile() acquired the path a descriptor lock holds")
	}
	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("TryLockFile() error = %v, want ErrLockBusy", err)
	}
	second, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if lock, err := TryLockDescriptor(second); lock != nil || !errors.Is(err, ErrLockBusy) {
		if lock != nil {
			lock.Close()
		}
		t.Fatalf("second TryLockDescriptor() = %v, %v, want ErrLockBusy", lock, err)
	}
	if _, err := second.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("a busy TryLockDescriptor() left its file open: %v", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Close() on the lock left the descriptor open: %v", err)
	}
	released, err := TryLockFile(lockPath)
	if err != nil {
		t.Fatalf("TryLockFile() after release: %v", err)
	}
	if err := released.Close(); err != nil {
		t.Fatal(err)
	}
}
