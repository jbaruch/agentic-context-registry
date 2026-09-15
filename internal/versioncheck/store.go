package versioncheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/freshness"
)

const (
	// CacheSchemaVersion is the version of both records this store writes.
	CacheSchemaVersion = 1
	// maxRecordBytes bounds one record read. The records are two short JSON
	// objects; anything larger is not one of them, and a startup read must
	// never grow with whatever a file at that path happens to hold.
	maxRecordBytes = 4096
)

// Cache is the published record: the latest stable release the last
// successful refresh observed. Notice reads it; Refresh rewrites it only on
// success, so a failed refresh leaves its bytes exactly as they were.
type Cache struct {
	SchemaVersion int       `json:"schemaVersion"`
	CheckedAt     time.Time `json:"checkedAt"`
	LatestVersion string    `json:"latestVersion"`
}

// Attempt is the throttle bookkeeping: when the last refresh was attempted,
// whatever it returned. It lives apart from Cache so a failed attempt can be
// counted against the daily window without touching the published bytes.
type Attempt struct {
	SchemaVersion int       `json:"schemaVersion"`
	AttemptedAt   time.Time `json:"attemptedAt"`
}

// Store owns the machine-level version records beneath BaseDirectory, the
// same directory the per-project freshness records live in. Nothing here is
// keyed by a project: one machine has one latest-release cache.
type Store struct {
	BaseDirectory string
}

// DefaultStore resolves the same base directory the freshness store uses:
// ACR_STATE_HOME, else the user cache directory.
func DefaultStore() (Store, error) {
	store, err := freshness.DefaultStore()
	return Store{BaseDirectory: store.BaseDirectory}, err
}

func (store Store) directory() string {
	return filepath.Join(store.BaseDirectory, "version")
}

// CachePath is the published record's path.
func (store Store) CachePath() string {
	return filepath.Join(store.directory(), "latest.json")
}

// AttemptPath is the throttle record's path.
func (store Store) AttemptPath() string {
	return filepath.Join(store.directory(), "attempt.json")
}

// LockPath is the advisory lock every Refresh holds while it reads and writes.
func (store Store) LockPath() string {
	return filepath.Join(store.directory(), "version.lock")
}

// ReadCache returns the published record when it is usable. A missing,
// truncated, corrupt, oversized, unsupported-version or invalid record is no
// usable prior state, reported as usable=false with a nil error; an error
// reading a file that exists is returned so the caller can classify it.
func (store Store) ReadCache() (Cache, bool, error) {
	var cache Cache
	present, err := readRecord(store.CachePath(), &cache)
	if err != nil || !present {
		return Cache{}, false, err
	}
	if cache.SchemaVersion != CacheSchemaVersion || cache.CheckedAt.IsZero() || !stable(cache.LatestVersion) {
		return Cache{}, false, nil
	}
	cache.CheckedAt = cache.CheckedAt.UTC()
	return cache, true, nil
}

// ReadAttempt returns the throttle record under the same rules as ReadCache.
func (store Store) ReadAttempt() (Attempt, bool, error) {
	var attempt Attempt
	present, err := readRecord(store.AttemptPath(), &attempt)
	if err != nil || !present {
		return Attempt{}, false, err
	}
	if attempt.SchemaVersion != CacheSchemaVersion || attempt.AttemptedAt.IsZero() {
		return Attempt{}, false, nil
	}
	attempt.AttemptedAt = attempt.AttemptedAt.UTC()
	return attempt, true, nil
}

// WriteCache replaces the published record with the release observed at
// checkedAt. A version that is not a stable release is refused before any
// byte is written.
func (store Store) WriteCache(checkedAt time.Time, latest string) error {
	if !stable(latest) {
		return fmt.Errorf("write version cache: %q is not a stable semantic version", latest)
	}
	return writeRecord(store.CachePath(), Cache{SchemaVersion: CacheSchemaVersion, CheckedAt: checkedAt.UTC(), LatestVersion: latest})
}

// WriteAttempt records that a refresh was attempted at attemptedAt.
func (store Store) WriteAttempt(attemptedAt time.Time) error {
	return writeRecord(store.AttemptPath(), Attempt{SchemaVersion: CacheSchemaVersion, AttemptedAt: attemptedAt.UTC()})
}

// readRecord decodes one bounded JSON record. present is false for a missing
// file and for content that is not one record of the expected shape.
func readRecord(path string, target any) (present bool, err error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open version record %q: %w", path, err)
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxRecordBytes+1))
	if err != nil {
		return false, fmt.Errorf("read version record %q: %w", path, err)
	}
	if len(content) > maxRecordBytes {
		return false, nil
	}
	if err := json.Unmarshal(content, target); err != nil {
		return false, nil
	}
	return true, nil
}

// writeRecord writes one record atomically: a private temporary file in the
// record's directory, synced, then renamed over the path. The rename replaces
// whatever was at the path — a stale record, or a symlink someone planted —
// and never writes through it.
func writeRecord(path string, record any) error {
	content, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode version record: %w", err)
	}
	content = append(content, '\n')
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create version record directory %q: %w", directory, err)
	}
	temporary, err := os.CreateTemp(directory, ".record-*")
	if err != nil {
		return fmt.Errorf("create temporary version record: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("set version record permissions: %w", err)
	}
	written, err := temporary.Write(content)
	if err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary version record: %w", err)
	}
	if written != len(content) {
		temporary.Close()
		return fmt.Errorf("write temporary version record: wrote %d of %d bytes", written, len(content))
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary version record: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary version record: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace version record %q: %w", path, err)
	}
	return nil
}
