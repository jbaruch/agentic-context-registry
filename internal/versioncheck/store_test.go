package versioncheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)

func TestStoreRoundTripsBothRecordsWithExactBytes(t *testing.T) {
	t.Parallel()

	store := Store{BaseDirectory: t.TempDir()}
	if err := store.WriteCache(fixedNow, "v0.2.1"); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteAttempt(fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	cache, usable, err := store.ReadCache()
	if err != nil || !usable {
		t.Fatalf("ReadCache() = %#v, %t, %v", cache, usable, err)
	}
	if cache.SchemaVersion != 1 || cache.CheckedAt != fixedNow || cache.LatestVersion != "v0.2.1" {
		t.Fatalf("cache = %#v", cache)
	}
	attempt, usable, err := store.ReadAttempt()
	if err != nil || !usable {
		t.Fatalf("ReadAttempt() = %#v, %t, %v", attempt, usable, err)
	}
	if attempt.SchemaVersion != 1 || attempt.AttemptedAt != fixedNow.Add(time.Minute) {
		t.Fatalf("attempt = %#v", attempt)
	}

	content, err := os.ReadFile(store.CachePath())
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"schemaVersion":1,"checkedAt":"2026-09-01T12:00:00Z","latestVersion":"v0.2.1"}` + "\n"; string(content) != want {
		t.Fatalf("cache bytes = %q, want %q", content, want)
	}
	content, err = os.ReadFile(store.AttemptPath())
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"schemaVersion":1,"attemptedAt":"2026-09-01T12:01:00Z"}` + "\n"; string(content) != want {
		t.Fatalf("attempt bytes = %q, want %q", content, want)
	}
	for _, path := range []string{store.CachePath(), store.AttemptPath()} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s mode = %o, want 600", path, mode)
		}
	}
	info, err := os.Stat(filepath.Dir(store.CachePath()))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("record directory mode = %o, want 700", mode)
	}
	if entries, err := os.ReadDir(filepath.Dir(store.CachePath())); err != nil || len(entries) != 2 {
		t.Fatalf("record directory holds %d entries (%v), want the two records and no temporary file", len(entries), err)
	}
}

func TestStoreRefusesToPublishAnythingButAStableRelease(t *testing.T) {
	t.Parallel()

	store := Store{BaseDirectory: t.TempDir()}
	for _, version := range []string{"", "latest", "v0.2.0-rc1", "0.2.0.1", "v1.2.3.4"} {
		if err := store.WriteCache(fixedNow, version); err == nil {
			t.Errorf("WriteCache(%q) accepted a version that is not a stable release", version)
		}
	}
	if _, err := os.Stat(store.CachePath()); !os.IsNotExist(err) {
		t.Fatalf("a refused write left a cache file: %v", err)
	}
}

// TestHostileRecordsAreNoUsablePriorState feeds the reader everything a file
// at the record path might hold that is not a record it wrote: each is
// unusable and silent, never an error and never a notice.
func TestHostileRecordsAreNoUsablePriorState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{name: "empty", content: ""},
		{name: "truncated", content: `{"schemaVersion":`},
		{name: "invalid json", content: `{not json}`},
		{name: "array", content: `[]`},
		{name: "missing schema version", content: `{"checkedAt":"2026-09-01T12:00:00Z","latestVersion":"v0.2.1"}` + "\n"},
		{name: "newer schema", content: `{"schemaVersion":99,"checkedAt":"2026-09-01T12:00:00Z","latestVersion":"v0.2.1"}` + "\n"},
		{name: "zero time", content: `{"schemaVersion":1,"checkedAt":"0001-01-01T00:00:00Z","latestVersion":"v0.2.1"}` + "\n"},
		{name: "unparsable time", content: `{"schemaVersion":1,"checkedAt":"yesterday","latestVersion":"v0.2.1"}` + "\n"},
		{name: "garbage version", content: `{"schemaVersion":1,"checkedAt":"2026-09-01T12:00:00Z","latestVersion":"newest"}` + "\n"},
		{name: "prerelease version", content: `{"schemaVersion":1,"checkedAt":"2026-09-01T12:00:00Z","latestVersion":"v0.3.0-rc1"}` + "\n"},
		{name: "empty version", content: `{"schemaVersion":1,"checkedAt":"2026-09-01T12:00:00Z","latestVersion":""}` + "\n"},
		{name: "oversized", content: `{"schemaVersion":1,"checkedAt":"2026-09-01T12:00:00Z","latestVersion":"v0.2.1","padding":"` + strings.Repeat("x", maxRecordBytes) + `"}` + "\n"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := Store{BaseDirectory: t.TempDir()}
			if err := os.MkdirAll(filepath.Dir(store.CachePath()), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.CachePath(), []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, usable, err := store.ReadCache(); err != nil || usable {
				t.Fatalf("ReadCache() usable = %t, error = %v; a hostile record must be no prior state", usable, err)
			}
			if err := os.WriteFile(store.AttemptPath(), []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, usable, err := store.ReadAttempt(); err != nil || usable {
				t.Fatalf("ReadAttempt() usable = %t, error = %v; a hostile record must be no prior state", usable, err)
			}
		})
	}
}

func TestMissingRecordsAreSilentAndAnUnreadableOneIsReported(t *testing.T) {
	t.Parallel()

	store := Store{BaseDirectory: t.TempDir()}
	if _, usable, err := store.ReadCache(); err != nil || usable {
		t.Fatalf("ReadCache() on a missing file = %t, %v", usable, err)
	}
	if _, usable, err := store.ReadAttempt(); err != nil || usable {
		t.Fatalf("ReadAttempt() on a missing file = %t, %v", usable, err)
	}
	if entries, err := os.ReadDir(store.BaseDirectory); err != nil || len(entries) != 0 {
		t.Fatalf("a read created state beneath the base directory: %v, %v", entries, err)
	}

	// A directory where the record should be is a file that exists and cannot
	// be read as one: that is an error the caller classifies, not silence
	// that hides a broken store.
	if err := os.MkdirAll(store.CachePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, usable, err := store.ReadCache(); err == nil || usable {
		t.Fatalf("ReadCache() on a directory = %t, %v, want an error", usable, err)
	}
}

// TestWriteReplacesAPlantedSymlinkInsteadOfWritingThroughIt is the symlink
// half of the store's safety: a link at the record path pointing outside the
// directory is replaced by the rename, and the file it pointed at keeps its
// bytes.
func TestWriteReplacesAPlantedSymlinkInsteadOfWritingThroughIt(t *testing.T) {
	t.Parallel()

	store := Store{BaseDirectory: t.TempDir()}
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("do not touch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, store.CachePath()); err != nil {
		t.Fatal(err)
	}

	if err := store.WriteCache(fixedNow, "v0.2.1"); err != nil {
		t.Fatal(err)
	}
	victim, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(victim) != "do not touch\n" {
		t.Fatalf("the write followed the planted symlink: victim = %q", victim)
	}
	info, err := os.Lstat(store.CachePath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the record path is still a symlink after the write")
	}
	if cache, usable, err := store.ReadCache(); err != nil || !usable || cache.LatestVersion != "v0.2.1" {
		t.Fatalf("ReadCache() after replacing the symlink = %#v, %t, %v", cache, usable, err)
	}
}

func TestDefaultStoreFollowsTheFreshnessBaseDirectory(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ACR_STATE_HOME", base)
	store, err := DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	if store.BaseDirectory != base {
		t.Fatalf("DefaultStore().BaseDirectory = %q, want ACR_STATE_HOME %q", store.BaseDirectory, base)
	}
	if want := filepath.Join(base, "version", "latest.json"); store.CachePath() != want {
		t.Fatalf("CachePath() = %q, want %q", store.CachePath(), want)
	}
	if want := filepath.Join(base, "version", "attempt.json"); store.AttemptPath() != want {
		t.Fatalf("AttemptPath() = %q, want %q", store.AttemptPath(), want)
	}
	if want := filepath.Join(base, "version", "version.lock"); store.LockPath() != want {
		t.Fatalf("LockPath() = %q, want %q", store.LockPath(), want)
	}
}
