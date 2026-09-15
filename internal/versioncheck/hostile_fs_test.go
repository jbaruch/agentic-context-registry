//go:build darwin || linux

package versioncheck

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

// withWatchdog runs operation off the test goroutine and fails the test when
// it has not returned within the bound. The bound guards against a regression
// to a blocking open on a special file; no assertion measures it. A blocked
// operation leaks its goroutine, which the test binary's exit collects.
func withWatchdog(t *testing.T, what string, operation func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		operation()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return; a special file blocked it", what)
	}
}

func plantFIFO(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func lstatMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func directoryNames(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestSpecialRecordsAreRejectedWithoutBlocking is the FIFO control: a named
// pipe with no writer at either record path used to block the first open
// forever — before the command's first output byte for latest.json, after it
// and outside the refresh timeout for attempt.json. Now the entry is
// classified from its metadata and refused without a blocking open or read,
// the notice stays silent, and the next successful write replaces the pipe
// inside the verified directory.
func TestSpecialRecordsAreRejectedWithoutBlocking(t *testing.T) {
	t.Parallel()

	t.Run("latest.json", func(t *testing.T) {
		t.Parallel()
		store := Store{BaseDirectory: t.TempDir()}
		plantFIFO(t, store.CachePath())
		source := &fakeSource{t: t, release: dependency.Release{ID: 9, Tag: "v0.2.0"}}
		checker := newTestChecker(t, store, &stepClock{now: fixedNow}, source)

		var usable bool
		var readErr error
		withWatchdog(t, "ReadCache on a FIFO", func() { _, usable, readErr = store.ReadCache() })
		if usable || !errors.Is(readErr, ErrUntrustedEntry) {
			t.Fatalf("ReadCache() on a FIFO = usable %t, error %v; want an untrusted-entry refusal", usable, readErr)
		}
		var notice string
		var reason Reason
		withWatchdog(t, "Notice on a FIFO", func() { notice, reason = checker.Notice("0.1.0") })
		if notice != "" || reason != ReasonUnreadableCache {
			t.Fatalf("Notice() on a FIFO = %q, %q", notice, reason)
		}
		if source.count() != 0 {
			t.Fatal("Notice() reached the release source")
		}

		var outcome Outcome
		withWatchdog(t, "Refresh with a FIFO cache", func() { outcome = checker.Refresh(context.Background()) })
		if outcome.Kind != KindRefreshed {
			t.Fatalf("Refresh() with a FIFO at the cache = %+v; the pipe sits inside the verified directory and is replaced, not followed", outcome)
		}
		if mode := lstatMode(t, store.CachePath()); !mode.IsRegular() {
			t.Fatalf("cache after refresh is %v, want a regular record", mode)
		}
		if notice, reason := checker.Notice("0.1.0"); reason != ReasonNewer || notice == "" {
			t.Fatalf("Notice() after the replacement = %q, %q", notice, reason)
		}
	})

	t.Run("attempt.json", func(t *testing.T) {
		t.Parallel()
		store := Store{BaseDirectory: t.TempDir()}
		if err := store.WriteCache(fixedNow.Add(-2*Window), "v0.1.5"); err != nil {
			t.Fatal(err)
		}
		published := readBytes(t, store.CachePath())
		plantFIFO(t, store.AttemptPath())
		source := &fakeSource{t: t, release: dependency.Release{ID: 9, Tag: "v0.2.0"}}
		checker := newTestChecker(t, store, &stepClock{now: fixedNow}, source)

		var usable bool
		var readErr error
		withWatchdog(t, "ReadAttempt on a FIFO", func() { _, usable, readErr = store.ReadAttempt() })
		if usable || !errors.Is(readErr, ErrUntrustedEntry) {
			t.Fatalf("ReadAttempt() on a FIFO = usable %t, error %v", usable, readErr)
		}
		var outcome Outcome
		withWatchdog(t, "Refresh with a FIFO attempt record", func() { outcome = checker.Refresh(context.Background()) })
		if outcome.Kind != KindRefreshed {
			t.Fatalf("Refresh() with a FIFO at the attempt record = %+v", outcome)
		}
		if mode := lstatMode(t, store.AttemptPath()); !mode.IsRegular() {
			t.Fatalf("attempt record after refresh is %v, want a regular record", mode)
		}
		if got := readBytes(t, store.CachePath()); bytes.Equal(got, published) {
			t.Fatal("the refresh that replaced the attempt pipe did not publish the new release")
		}
		if source.count() != 1 {
			t.Fatalf("source called %d times, want 1", source.count())
		}
	})

	t.Run("directory at the cache", func(t *testing.T) {
		t.Parallel()
		store := Store{BaseDirectory: t.TempDir()}
		if err := os.MkdirAll(store.CachePath(), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, usable, err := store.ReadCache(); usable || !errors.Is(err, ErrUntrustedEntry) {
			t.Fatalf("ReadCache() on a directory = %t, %v", usable, err)
		}
		if err := store.WriteCache(fixedNow, "v0.2.0"); err == nil {
			t.Fatal("WriteCache() replaced a directory")
		}
		if mode := lstatMode(t, store.CachePath()); !mode.IsDir() {
			t.Fatalf("the directory at the cache path became %v", mode)
		}
		if names := directoryNames(t, filepath.Dir(store.CachePath())); len(names) != 1 {
			t.Fatalf("a refused write left entries behind: %v", names)
		}
	})
}

// TestVersionDirectorySymlinkIsRefused is the directory control: with the
// version directory replaced by a symlink, every read and write used to follow
// it. Now the directory's identity is verified on the descriptor the store
// holds, so a symlink — escaping the store or pointing back inside it — is
// refused before any record is read, created or renamed, and the bytes at the
// other end are exactly what they were.
func TestVersionDirectorySymlinkIsRefused(t *testing.T) {
	t.Parallel()

	t.Run("escaping the store", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		store := Store{BaseDirectory: filepath.Join(parent, "state")}
		outside := filepath.Join(parent, "outside")
		for _, directory := range []string{store.BaseDirectory, outside} {
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		victim := filepath.Join(outside, "latest.json")
		valid := []byte(`{"schemaVersion":1,"checkedAt":"2026-09-01T12:00:00Z","latestVersion":"v9.9.9"}` + "\n")
		if err := os.WriteFile(victim, valid, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(store.BaseDirectory, "version")); err != nil {
			t.Fatal(err)
		}
		source := &fakeSource{t: t, release: dependency.Release{ID: 9, Tag: "v0.2.0"}}
		checker := newTestChecker(t, store, &stepClock{now: fixedNow}, source)

		if notice, reason := checker.Notice("0.1.0"); notice != "" || reason != ReasonUnreadableCache {
			t.Fatalf("Notice() through a directory symlink = %q, %q; a valid record at the other end must not be trusted", notice, reason)
		}
		outcome := checker.Refresh(context.Background())
		if outcome.Kind != KindFailed || !errors.Is(outcome.Err, ErrUntrustedEntry) {
			t.Fatalf("Refresh() through a directory symlink = %+v", outcome)
		}
		if got := readBytes(t, victim); !bytes.Equal(got, valid) {
			t.Fatalf("the refresh wrote through the directory symlink: %q", got)
		}
		if names := directoryNames(t, outside); len(names) != 1 || names[0] != "latest.json" {
			t.Fatalf("the refresh created entries outside the store: %v", names)
		}
		if names := directoryNames(t, store.BaseDirectory); len(names) != 1 || names[0] != "version" {
			t.Fatalf("the refresh changed the store root: %v", names)
		}
		if mode := lstatMode(t, filepath.Join(store.BaseDirectory, "version")); mode&os.ModeSymlink == 0 {
			t.Fatalf("the planted directory symlink was replaced: %v", mode)
		}
		if source.count() != 0 {
			t.Fatalf("a refused refresh reached the release source %d times", source.count())
		}
	})

	t.Run("pointing back inside the store", func(t *testing.T) {
		t.Parallel()
		store := Store{BaseDirectory: t.TempDir()}
		sibling := filepath.Join(store.BaseDirectory, "freshness")
		if err := os.Mkdir(sibling, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("freshness", filepath.Join(store.BaseDirectory, "version")); err != nil {
			t.Fatal(err)
		}
		source := &fakeSource{t: t, release: dependency.Release{ID: 9, Tag: "v0.2.0"}}
		checker := newTestChecker(t, store, &stepClock{now: fixedNow}, source)

		outcome := checker.Refresh(context.Background())
		if outcome.Kind != KindFailed || !errors.Is(outcome.Err, ErrUntrustedEntry) {
			t.Fatalf("Refresh() through an in-store directory symlink = %+v", outcome)
		}
		if names := directoryNames(t, sibling); len(names) != 0 {
			t.Fatalf("the refresh wrote into the sibling directory: %v", names)
		}
		if _, usable, err := store.ReadCache(); usable || !errors.Is(err, ErrUntrustedEntry) {
			t.Fatalf("ReadCache() through an in-store directory symlink = %t, %v", usable, err)
		}
		if source.count() != 0 {
			t.Fatalf("a refused refresh reached the release source %d times", source.count())
		}
	})
}

// TestLockSymlinkNeverCreatesOrFollowsItsTarget is the lock control: creating
// the advisory lock used to follow a symlink at its path, creating a dangling
// link's target wherever it pointed. Now a lock entry that is not a regular
// file is refused before anything is created or locked, whether it points
// outside the store, at a sibling that does not exist, or at a record that
// does.
func TestLockSymlinkNeverCreatesOrFollowsItsTarget(t *testing.T) {
	t.Parallel()

	t.Run("dangling outside the store", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		store := Store{BaseDirectory: filepath.Join(parent, "state")}
		target := filepath.Join(parent, "outside-new-file")
		if err := os.MkdirAll(filepath.Dir(store.LockPath()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, store.LockPath()); err != nil {
			t.Fatal(err)
		}
		source := &fakeSource{t: t, release: dependency.Release{ID: 9, Tag: "v0.2.0"}}
		outcome := newTestChecker(t, store, &stepClock{now: fixedNow}, source).Refresh(context.Background())
		if outcome.Kind != KindFailed || !errors.Is(outcome.Err, ErrUntrustedEntry) {
			t.Fatalf("Refresh() with a lock symlink = %+v", outcome)
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatalf("the lock symlink's target was created outside the store: %v", err)
		}
		if names := directoryNames(t, filepath.Dir(store.LockPath())); len(names) != 1 {
			t.Fatalf("a refused refresh left entries behind: %v", names)
		}
		if source.count() != 0 {
			t.Fatal("a refused refresh reached the release source")
		}
	})

	t.Run("dangling inside the store", func(t *testing.T) {
		t.Parallel()
		store := Store{BaseDirectory: t.TempDir()}
		if err := os.MkdirAll(filepath.Dir(store.LockPath()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("planted.lock", store.LockPath()); err != nil {
			t.Fatal(err)
		}
		source := &fakeSource{t: t, release: dependency.Release{ID: 9, Tag: "v0.2.0"}}
		outcome := newTestChecker(t, store, &stepClock{now: fixedNow}, source).Refresh(context.Background())
		if outcome.Kind != KindFailed || !errors.Is(outcome.Err, ErrUntrustedEntry) {
			t.Fatalf("Refresh() with an in-store lock symlink = %+v", outcome)
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(store.LockPath()), "planted.lock")); !os.IsNotExist(err) {
			t.Fatalf("the in-store link target was created: %v", err)
		}
	})

	t.Run("pointing at the published record", func(t *testing.T) {
		t.Parallel()
		store := Store{BaseDirectory: t.TempDir()}
		if err := store.WriteCache(fixedNow.Add(-2*Window), "v0.1.5"); err != nil {
			t.Fatal(err)
		}
		published := readBytes(t, store.CachePath())
		if err := os.Symlink("latest.json", store.LockPath()); err != nil {
			t.Fatal(err)
		}
		source := &fakeSource{t: t, release: dependency.Release{ID: 9, Tag: "v0.2.0"}}
		outcome := newTestChecker(t, store, &stepClock{now: fixedNow}, source).Refresh(context.Background())
		if outcome.Kind != KindFailed || !errors.Is(outcome.Err, ErrUntrustedEntry) {
			t.Fatalf("Refresh() with a lock symlink to the record = %+v", outcome)
		}
		if got := readBytes(t, store.CachePath()); !bytes.Equal(got, published) {
			t.Fatalf("the refused refresh changed the record the lock pointed at: %q", got)
		}
		if _, err := os.Lstat(store.AttemptPath()); !os.IsNotExist(err) {
			t.Fatalf("a refused refresh recorded an attempt: %v", err)
		}
	})

	t.Run("named pipe at the lock", func(t *testing.T) {
		t.Parallel()
		store := Store{BaseDirectory: t.TempDir()}
		plantFIFO(t, store.LockPath())
		var outcome Outcome
		source := &fakeSource{t: t, release: dependency.Release{ID: 9, Tag: "v0.2.0"}}
		withWatchdog(t, "Refresh with a FIFO lock", func() {
			outcome = newTestChecker(t, store, &stepClock{now: fixedNow}, source).Refresh(context.Background())
		})
		if outcome.Kind != KindFailed || !errors.Is(outcome.Err, ErrUntrustedEntry) {
			t.Fatalf("Refresh() with a FIFO at the lock = %+v", outcome)
		}
		if mode := lstatMode(t, store.LockPath()); mode&os.ModeNamedPipe == 0 {
			t.Fatalf("the planted pipe was replaced: %v", mode)
		}
	})

	t.Run("regular lock from an earlier run", func(t *testing.T) {
		t.Parallel()
		store := Store{BaseDirectory: t.TempDir()}
		if err := os.MkdirAll(filepath.Dir(store.LockPath()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(store.LockPath(), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		source := &fakeSource{t: t, release: dependency.Release{ID: 9, Tag: "v0.2.0"}}
		if outcome := newTestChecker(t, store, &stepClock{now: fixedNow}, source).Refresh(context.Background()); outcome.Kind != KindRefreshed {
			t.Fatalf("Refresh() with an existing regular lock = %+v", outcome)
		}
	})
}
