package dependency

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
)

type localAuthorization struct {
	SchemaVersion int    `json:"schemaVersion"`
	Project       string `json:"project"`
	Source        string `json:"source"`
	Path          string `json:"path"`
	SourceRoot    string `json:"sourceRoot"`
	Pending       bool   `json:"pending,omitempty"`
}

func localDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// localAuthorizationPath resolves through the nearest existing ancestor before
// enforcing the outside-project boundary, including a not-yet-created store.
func localAuthorizationPath(project, source string) (filename, identity string, err error) {
	store, err := freshness.DefaultStore()
	if err != nil {
		return "", "", err
	}
	key, identity, err := freshness.ProjectIdentity(project)
	if err != nil {
		return "", "", err
	}
	base, err := canonicalFuturePath(store.BaseDirectory)
	if err != nil {
		return "", "", err
	}
	projectRoot, err := localRoot(project, ".")
	if err != nil {
		return "", "", err
	}
	if withinDirectory(projectRoot, base) {
		return "", "", errors.New("ACR_STATE_HOME must be outside the project")
	}
	for _, directory := range []string{filepath.Join(base, "local"), filepath.Join(base, "local", key)} {
		info, inspectErr := os.Lstat(directory)
		if errors.Is(inspectErr, os.ErrNotExist) {
			break
		}
		if inspectErr != nil {
			return "", "", inspectErr
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			return "", "", fmt.Errorf("authorization directory %s must be a real directory with permissions 0700", directory)
		}
	}
	return filepath.Join(base, "local", key, strings.TrimPrefix(localDigest(source), "sha256:")+".json"), identity, nil
}

func canonicalFuturePath(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return canonical, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(absolute)
	if parent == absolute {
		return "", err
	}
	parent, err = canonicalFuturePath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func withinDirectory(root, value string) bool {
	relative, err := filepath.Rel(root, value)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func readAuthorization(filename string) (data []byte, err error) {
	directory, err := openAuthorizationDirectory(filename, false)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	return directory.read(filepath.Base(filename))
}

func authorizeLocal(project string, declaration Declaration) (string, error) {
	filename, identity, err := localAuthorizationPath(project, declaration.Source)
	if err != nil {
		return "", localError(cli.CodeLocalSourceUnauthorized, declaration, err)
	}
	data, err := readAuthorization(filename)
	if err != nil {
		return "", localError(cli.CodeLocalSourceUnauthorized, declaration, err)
	}
	var record localAuthorization
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return "", localError(cli.CodeLocalSourceUnauthorized, declaration, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", localError(cli.CodeLocalSourceUnauthorized, declaration, errors.New("authorization contains trailing data"))
	}
	if record.SchemaVersion != 1 || record.Pending || record.Project != identity || record.Source != declaration.Source || record.Path != declaration.Path {
		return "", localError(cli.CodeLocalSourceUnauthorized, declaration, errors.New("no matching completed authorization on this machine"))
	}
	root, err := localRoot(project, declaration.Path)
	if err != nil {
		return "", localError(cli.CodeLocalSourceUnavailable, declaration, err)
	}
	if record.SourceRoot != localDigest("acr-local-source-v1\x00"+root) {
		return "", localError(cli.CodeLocalSourceUnauthorized, declaration, errors.New("authorization named a different canonical directory"))
	}
	return root, nil
}

// LocalNotice reports authorization without adopting content or accessing a network.
func LocalNotice(project string, declaration Declaration) string {
	status := "authorized on this machine"
	if _, err := authorizeLocal(project, declaration); err != nil {
		status = "not authorized on this machine; " + err.Error()
	}
	return fmt.Sprintf("%s is installed from local path %q (%s). Local dependency state is user/machine-specific; do not commit this local row.", declaration.Source, declaration.Path, status)
}

// ChangeLocalRemoval revokes authorization transactionally around a local
// uninstall. Missing authorization needs no store creation or source access.
func ChangeLocalRemoval(project, source string, operation func() error) error {
	return changeLocalAuthorization(project, source, nil, operation)
}

func changeLocalAuthorization(project, source string, next *localAuthorization, operation func() error) error {
	return changeLocalAuthorizationWith(project, source, next, operation, writeAuthorization)
}

func changeLocalAuthorizationWith(project, source string, next *localAuthorization, operation func() error, write func(*localAuthorizationDirectory, string, []byte) error) (err error) {
	authFailure, completed := true, false
	defer func() {
		if err == nil {
			return
		}
		if completed {
			err = fmt.Errorf("project operation and authorization completed, but coordination cleanup failed: %w; inspect current state before retrying", err)
		}
		var local *LocalSourceError
		if authFailure && !errors.As(err, &local) {
			err = &LocalSourceError{Code: cli.CodeLocalAuthorizationUnwritable, Err: err}
		}
	}()

	filename, identity, err := localAuthorizationPath(project, source)
	if err != nil {
		return &LocalSourceError{Code: cli.CodeLocalAuthorizationUnwritable, Err: err}
	}
	if next == nil {
		if _, readErr := os.Lstat(filename); errors.Is(readErr, os.ErrNotExist) {
			authFailure = false
			return operation()
		} else if readErr != nil {
			return &LocalSourceError{Code: cli.CodeLocalAuthorizationUnwritable, Err: readErr}
		}
	} else if withinDirectory(next.SourceRoot, filename) {
		return &LocalSourceError{Code: cli.CodeLocalAuthorizationUnwritable, Err: errors.New("ACR_STATE_HOME must be outside the local source tree")}
	}
	directory, err := openAuthorizationDirectory(filename, next != nil)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	name := filepath.Base(filename)
	if err := directory.verify(); err != nil {
		return err
	}
	lock, err := directory.root().OpenFile(name+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	opened, err := lock.Stat()
	if err != nil {
		return err
	}
	current, err := directory.root().Lstat(name + ".lock")
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(opened, current) || current.Mode().Perm() != 0o600 {
		return errors.New("local authorization lock is unsafe; repair ACR_STATE_HOME")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("local authorization is busy; retry: %w", err)
	}
	defer func() { err = errors.Join(err, syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)) }()
	before, err := directory.read(name)
	absent := errors.Is(err, os.ErrNotExist)
	if err != nil && !absent {
		return &LocalSourceError{Code: cli.CodeLocalAuthorizationUnwritable, Err: err}
	}
	if !absent {
		info, err := directory.root().Lstat(name)
		if err != nil {
			return err
		}
		directory.record = &authorizationRecord{info: info, data: before}
	}
	pending := localAuthorization{SchemaVersion: 1, Project: identity, Source: source, Pending: true}
	if next != nil {
		pending = *next
		pending.Project = identity
		pending.Source = source
		pending.SourceRoot = localDigest("acr-local-source-v1\x00" + next.SourceRoot)
		pending.Pending = true
	}
	staged, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	if err := write(directory, name, staged); err != nil {
		return &LocalSourceError{Code: cli.CodeLocalAuthorizationUnwritable, Err: err}
	}
	pendingInfo, err := directory.root().Lstat(name)
	if err != nil {
		return err
	}
	pendingOwnership := func() error {
		info, err := directory.root().Lstat(name)
		if err != nil {
			return err
		}
		if !os.SameFile(pendingInfo, info) {
			return errors.New("pending authorization file was replaced concurrently")
		}
		return nil
	}
	// A pending record grants no access, even if the process exits during the
	// project write. Failure restores only this operation's own pending bytes.
	restore := func(cause error) error {
		live, readErr := directory.read(name)
		ownershipErr := pendingOwnership()
		if readErr != nil || !bytes.Equal(live, staged) || ownershipErr != nil {
			return errors.Join(cause, readErr, ownershipErr, errors.New("authorization changed concurrently; retained current record; inspect ACR_STATE_HOME and rerun explicit install"))
		}
		var restoreErr error
		if absent {
			restoreErr = directory.remove(name)
		} else {
			restoreErr = write(directory, name, before)
		}
		if restoreErr != nil {
			return errors.Join(cause, fmt.Errorf("restore authorization failed: %w; pending record grants no new access; rerun explicit install", restoreErr))
		}
		return cause
	}
	if err := directory.checkRecord(name); err != nil {
		return restore(err)
	}
	if err := operation(); err != nil {
		authFailure = false
		return restore(err)
	}
	live, err := directory.read(name)
	ownershipErr := pendingOwnership()
	if err != nil || !bytes.Equal(live, staged) || ownershipErr != nil {
		return errors.Join(err, ownershipErr, errors.New("project operation completed but authorization changed concurrently; kept concurrent record; inspect state before retrying"))
	}
	if next == nil {
		err = directory.remove(name)
	} else {
		pending.Pending = false
		var data []byte
		data, err = json.Marshal(pending)
		if err == nil {
			err = write(directory, name, data)
		}
	}
	if err != nil {
		return restore(&LocalSourceError{Code: cli.CodeLocalAuthorizationUnwritable, Err: fmt.Errorf("project operation completed but authorization could not be finalized: %w; project state may have changed; inspect it and rerun explicit install", err)})
	}
	completed = true
	return nil
}
