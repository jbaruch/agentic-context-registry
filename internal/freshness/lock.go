package freshness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLockBusy reports that another process owns the project's advisory lock.
var ErrLockBusy = errors.New("freshness project lock is busy")

// ProjectLock owns one non-blocking advisory project lock.
type ProjectLock struct {
	file *os.File
}

// TryLock takes the canonical project's non-blocking advisory lock.
func (store Store) TryLock(root string) (*ProjectLock, error) {
	_, lockPath, err := store.Paths(root)
	if err != nil {
		return nil, err
	}
	return TryLockFile(lockPath)
}

// TryLockFile takes the non-blocking advisory lock at lockPath, creating the
// file and its directory when they are missing. It is the lock discipline
// every record in the store shares: a per-project record locks beside its
// state file through TryLock, and a machine-level record locks beside its
// own file through this function directly.
func TryLockFile(lockPath string) (*ProjectLock, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("create freshness lock directory: %w", err)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open freshness project lock %q: %w", lockPath, err)
	}
	return TryLockDescriptor(file)
}

// TryLockDescriptor takes the non-blocking advisory lock on a file the caller
// already opened and owns that file from then on: a busy or failed lock
// closes it, and Close on the returned lock releases and closes it. Deciding
// how the file is opened stays with the caller, which is what lets a store
// refuse to create or follow anything at its lock path before locking.
func TryLockDescriptor(file *os.File) (*ProjectLock, error) {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errors.Join(ErrLockBusy, closeErr)
		}
		return nil, errors.Join(fmt.Errorf("lock freshness project state: %w", err), closeErr)
	}
	return &ProjectLock{file: file}, nil
}

// Close releases the advisory lock and closes its file.
func (lock *ProjectLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(unlockErr, closeErr)
}
