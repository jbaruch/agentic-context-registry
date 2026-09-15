package versioncheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
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

	directoryName = "version"
	cacheName     = "latest.json"
	attemptName   = "attempt.json"
	lockName      = "version.lock"

	// temporaryAttempts bounds the exclusive-create loop for a temporary
	// record name. A name already present belongs to a crashed earlier
	// process that shared this PID; the next sequence number is tried.
	temporaryAttempts = 8
)

// ErrUntrustedEntry reports a filesystem entry at a version-state path that is
// not the regular file or directory acr wrote there: a symlink, a named pipe,
// a device, or a directory where a record belongs. Every operation refuses
// such an entry before opening, reading, creating or locking through it, so a
// planted entry can neither block a command nor redirect a write.
var ErrUntrustedEntry = errors.New("version state entry is not the regular file or directory acr wrote")

// temporarySequence numbers this process's temporary record names.
var temporarySequence atomic.Uint64

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
//
// Every operation runs through a descriptor to the version directory whose
// identity is verified after it is opened, and every entry beneath it is
// classified from its own metadata before it is opened, so nothing below the
// store root is ever followed: a symlink, a pipe or a device where a record,
// the lock or the directory belongs is refused, and a write that replaces an
// entry replaces it inside that verified directory alone.
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
	return filepath.Join(store.BaseDirectory, directoryName)
}

// CachePath is the published record's path.
func (store Store) CachePath() string {
	return filepath.Join(store.directory(), cacheName)
}

// AttemptPath is the throttle record's path.
func (store Store) AttemptPath() string {
	return filepath.Join(store.directory(), attemptName)
}

// LockPath is the advisory lock every Refresh holds while it reads and writes.
func (store Store) LockPath() string {
	return filepath.Join(store.directory(), lockName)
}

// openDirectory opens the version directory beneath the store root and hands
// back a descriptor-rooted handle to it. The entry named version is
// classified before it is opened and its identity is verified on the opened
// descriptor afterwards, so a symlink planted there — escaping the store or
// pointing back inside it — is refused rather than followed, and an entry
// swapped between the check and the open is refused rather than used. With
// create set, a missing root and a missing directory are created; without
// it, a missing one is reported through fs.ErrNotExist and nothing is made.
func (store Store) openDirectory(create bool) (*os.Root, error) {
	if create {
		if err := os.MkdirAll(store.BaseDirectory, 0o700); err != nil {
			return nil, fmt.Errorf("create version state root %q: %w", store.BaseDirectory, err)
		}
	}
	base, err := os.OpenRoot(store.BaseDirectory)
	if err != nil {
		return nil, fmt.Errorf("open version state root %q: %w", store.BaseDirectory, err)
	}
	defer base.Close()
	if create {
		if err := base.Mkdir(directoryName, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("create version state directory %q: %w", store.directory(), err)
		}
	}
	entry, err := base.Lstat(directoryName)
	if err != nil {
		return nil, fmt.Errorf("inspect version state directory %q: %w", store.directory(), err)
	}
	if !entry.IsDir() {
		return nil, fmt.Errorf("version state directory %q: %w", store.directory(), ErrUntrustedEntry)
	}
	directory, err := base.OpenRoot(directoryName)
	if err != nil {
		return nil, fmt.Errorf("open version state directory %q: %w", store.directory(), err)
	}
	self, err := directory.Stat(".")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("inspect open version state directory %q: %w", store.directory(), err), directory.Close())
	}
	if !os.SameFile(entry, self) {
		return nil, errors.Join(fmt.Errorf("version state directory %q changed while opening it: %w", store.directory(), ErrUntrustedEntry), directory.Close())
	}
	return directory, nil
}

// ReadCache returns the published record when it is usable. A missing,
// truncated, corrupt, oversized, unsupported-version or invalid record is no
// usable prior state, reported as usable=false with a nil error. An entry
// that exists and is not a regular record, or a directory that is not the
// store's own, is reported through the error so the caller can classify it.
func (store Store) ReadCache() (cache Cache, usable bool, err error) {
	directory, err := store.openDirectory(false)
	if errors.Is(err, fs.ErrNotExist) {
		return Cache{}, false, nil
	}
	if err != nil {
		return Cache{}, false, err
	}
	defer func() {
		if closeErr := directory.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	return readCache(directory)
}

// ReadAttempt returns the throttle record under the same rules as ReadCache.
func (store Store) ReadAttempt() (attempt Attempt, usable bool, err error) {
	directory, err := store.openDirectory(false)
	if errors.Is(err, fs.ErrNotExist) {
		return Attempt{}, false, nil
	}
	if err != nil {
		return Attempt{}, false, err
	}
	defer func() {
		if closeErr := directory.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	return readAttempt(directory)
}

// WriteCache replaces the published record with the release observed at
// checkedAt. A version that is not a stable release is refused before any
// byte is written.
func (store Store) WriteCache(checkedAt time.Time, latest string) (err error) {
	if !stable(latest) {
		return fmt.Errorf("write version cache: %q is not a stable semantic version", latest)
	}
	directory, err := store.openDirectory(true)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := directory.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	return writeCache(directory, checkedAt, latest)
}

// WriteAttempt records that a refresh was attempted at attemptedAt.
func (store Store) WriteAttempt(attemptedAt time.Time) (err error) {
	directory, err := store.openDirectory(true)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := directory.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	return writeAttempt(directory, attemptedAt)
}

func readCache(directory *os.Root) (Cache, bool, error) {
	var cache Cache
	present, err := readRecord(directory, cacheName, &cache)
	if err != nil || !present {
		return Cache{}, false, err
	}
	if cache.SchemaVersion != CacheSchemaVersion || cache.CheckedAt.IsZero() || !stable(cache.LatestVersion) {
		return Cache{}, false, nil
	}
	cache.CheckedAt = cache.CheckedAt.UTC()
	return cache, true, nil
}

func readAttempt(directory *os.Root) (Attempt, bool, error) {
	var attempt Attempt
	present, err := readRecord(directory, attemptName, &attempt)
	if err != nil || !present {
		return Attempt{}, false, err
	}
	if attempt.SchemaVersion != CacheSchemaVersion || attempt.AttemptedAt.IsZero() {
		return Attempt{}, false, nil
	}
	attempt.AttemptedAt = attempt.AttemptedAt.UTC()
	return attempt, true, nil
}

func writeCache(directory *os.Root, checkedAt time.Time, latest string) error {
	if !stable(latest) {
		return fmt.Errorf("write version cache: %q is not a stable semantic version", latest)
	}
	return writeRecord(directory, cacheName, Cache{SchemaVersion: CacheSchemaVersion, CheckedAt: checkedAt.UTC(), LatestVersion: latest})
}

func writeAttempt(directory *os.Root, attemptedAt time.Time) error {
	return writeRecord(directory, attemptName, Attempt{SchemaVersion: CacheSchemaVersion, AttemptedAt: attemptedAt.UTC()})
}

// readRecord decodes one bounded JSON record. present is false for a missing
// file and for content that is not one record of the expected shape. The
// entry is classified from its metadata before it is opened, the open never
// blocks whatever the entry turns out to be, and the identity of what was
// opened is checked against what was classified, so a pipe, a device, a
// directory or a symlink at the path is refused without a read, and an entry
// swapped between the check and the open is refused rather than read.
func readRecord(directory *os.Root, name string, target any) (present bool, err error) {
	entry, err := directory.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect version record %q: %w", name, err)
	}
	if !entry.Mode().IsRegular() {
		return false, fmt.Errorf("version record %q: %w", name, ErrUntrustedEntry)
	}
	file, err := directory.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open version record %q: %w", name, err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("inspect open version record %q: %w", name, err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(entry, info) {
		return false, fmt.Errorf("version record %q changed while opening it: %w", name, ErrUntrustedEntry)
	}
	content, err := io.ReadAll(io.LimitReader(file, maxRecordBytes+1))
	if err != nil {
		return false, fmt.Errorf("read version record %q: %w", name, err)
	}
	if len(content) > maxRecordBytes {
		return false, nil
	}
	if err := json.Unmarshal(content, target); err != nil {
		return false, nil
	}
	return true, nil
}

// writeRecord writes one record atomically inside the verified directory: a
// private temporary file created exclusively, so no symlink is ever followed
// while creating it, then synced and renamed over the record's name. The
// rename replaces whatever entry is at that name — a stale record, a symlink
// someone planted, a pipe — and never writes through it; a directory there is
// refused by the rename itself.
func writeRecord(directory *os.Root, name string, record any) error {
	content, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode version record: %w", err)
	}
	content = append(content, '\n')
	temporary, file, err := createTemporary(directory)
	if err != nil {
		return err
	}
	if err := fillRecord(file, content); err != nil {
		return errors.Join(err, discardTemporary(directory, temporary))
	}
	if err := directory.Rename(temporary, name); err != nil {
		return errors.Join(fmt.Errorf("replace version record %q: %w", name, err), discardTemporary(directory, temporary))
	}
	return nil
}

func createTemporary(directory *os.Root) (string, *os.File, error) {
	for attempt := 0; attempt < temporaryAttempts; attempt++ {
		name := fmt.Sprintf(".record-%d-%d", os.Getpid(), temporarySequence.Add(1))
		file, err := directory.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("create temporary version record: %w", err)
		}
		return name, file, nil
	}
	return "", nil, fmt.Errorf("create temporary version record: %d candidate names already exist", temporaryAttempts)
}

func fillRecord(file *os.File, content []byte) error {
	if err := file.Chmod(0o600); err != nil {
		return errors.Join(fmt.Errorf("set version record permissions: %w", err), file.Close())
	}
	written, err := file.Write(content)
	if err != nil {
		return errors.Join(fmt.Errorf("write temporary version record: %w", err), file.Close())
	}
	if written != len(content) {
		return errors.Join(fmt.Errorf("write temporary version record: wrote %d of %d bytes", written, len(content)), file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync temporary version record: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary version record: %w", err)
	}
	return nil
}

func discardTemporary(directory *os.Root, name string) error {
	if err := directory.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove temporary version record %q: %w", name, err)
	}
	return nil
}

// tryLock takes the store's advisory lock beside the records. The lock entry
// is opened only when it is a regular file, or created exclusively when it is
// absent, so a symlink at the lock's name — dangling or not, escaping the
// store or pointing back inside it — never has its target created or locked.
func tryLock(directory *os.Root) (*freshness.ProjectLock, error) {
	file, err := openLock(directory)
	if err != nil {
		return nil, err
	}
	return freshness.TryLockDescriptor(file)
}

// openLock returns the lock file open for locking. Two passes cover the two
// benign races: another process creating the lock between the check and the
// exclusive create, and the lock removed between the check and the open. A
// planted entry fails the check on whichever pass sees it, and an entry
// swapped in between the check and the open fails the identity comparison.
func openLock(directory *os.Root) (*os.File, error) {
	for pass := 0; pass < 2; pass++ {
		entry, err := directory.Lstat(lockName)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			file, err := directory.OpenFile(lockName, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NONBLOCK, 0o600)
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("create version lock: %w", err)
			}
			return file, nil
		case err != nil:
			return nil, fmt.Errorf("inspect version lock: %w", err)
		case !entry.Mode().IsRegular():
			return nil, fmt.Errorf("version lock: %w", ErrUntrustedEntry)
		}
		file, err := directory.OpenFile(lockName, os.O_RDWR|syscall.O_NONBLOCK, 0)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("open version lock: %w", err)
		}
		info, err := file.Stat()
		if err != nil {
			return nil, errors.Join(fmt.Errorf("inspect open version lock: %w", err), file.Close())
		}
		if !info.Mode().IsRegular() || !os.SameFile(entry, info) {
			return nil, errors.Join(fmt.Errorf("version lock changed while opening it: %w", ErrUntrustedEntry), file.Close())
		}
		return file, nil
	}
	return nil, fmt.Errorf("version lock changed shape on every pass: %w", ErrUntrustedEntry)
}
