package dependency

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// LocalSourceError refuses local access before materialization or state changes.
type LocalSourceError struct {
	Code string
	Err  error
}

func (err *LocalSourceError) Error() string { return err.Code + ": " + err.Err.Error() }
func (err *LocalSourceError) Unwrap() error { return err.Err }

// LocalCLIError preserves local refusal codes across application boundaries.
func LocalCLIError(err error) *cli.Error {
	var local *LocalSourceError
	if !errors.As(err, &local) {
		return nil
	}
	return &cli.Error{ExitCode: cli.ExitOperational, Code: local.Code, Message: err.Error(), Cause: err}
}

func localError(code string, declaration Declaration, err error) error {
	return &LocalSourceError{Code: code, Err: fmt.Errorf("%s at %q: %w; run 'acr install %s' to authorize and refresh, 'acr install %s' to use a release, or 'acr uninstall %s'", declaration.Source, declaration.Path, err, localInstallArgument(declaration.Path), declaration.Source, declaration.Source)}
}

func localInstallArgument(value string) string { return "file:" + value }

func validateLocalPath(requested, value string) error {
	if requested != RequestedLocal {
		if value != "" {
			return errors.New("path is only valid with requested: local")
		}
		return nil
	}
	if value == "" || path.Clean(value) != value || strings.HasPrefix(value, "//") || strings.ContainsAny(value, "\\\x00\r\n") {
		return errors.New("local path must be a nonempty normalized directory path; run explicit path install to refresh")
	}
	return nil
}

func localRoot(project, value string) (string, error) {
	if !filepath.IsAbs(value) {
		value = filepath.Join(project, filepath.FromSlash(value))
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("local source is not a directory")
	}
	return canonical, nil
}

// snapshotLocal copies only the release inventory. Adapters never see the live
// source tree, and executable bits have the same normalization as an archive.
func snapshotLocal(root string) (MaterializedPackage, LockedDependency, func() error, error) {
	return snapshotLocalBounded(root, maxArchiveEntries, maxExtractedBytes)
}

func snapshotLocalBounded(root string, entries int, byteLimit int64) (pkg MaterializedPackage, locked LockedDependency, cleanup func() error, err error) {
	source, err := os.OpenRoot(root)
	if err != nil {
		return pkg, locked, nil, err
	}
	var remove func() error
	defer func() {
		err = errors.Join(err, source.Close())
		if err != nil && remove != nil {
			err = errors.Join(err, remove())
		}
	}()
	value, files, err := manifest.LoadPackageBounded(source, entries, byteLimit)
	if err != nil {
		return pkg, locked, nil, err
	}
	temporary, err := os.MkdirTemp("", "acr-package-*")
	if err != nil {
		return pkg, locked, nil, err
	}
	remove = func() error { return os.RemoveAll(temporary) }
	for _, relative := range files {
		var size int64
		size, err = copyLocalFile(source, relative, temporary, byteLimit)
		if err != nil {
			return pkg, locked, nil, fmt.Errorf("snapshot %s: %w", relative, err)
		}
		byteLimit -= size
	}
	copied, err := manifest.Load(temporary)
	if err != nil {
		return pkg, locked, nil, err
	}
	if !reflect.DeepEqual(value, copied) {
		return pkg, locked, nil, errors.New("manifest changed while snapshotting; retry with a stable source")
	}
	digest, err := HashPackageFiles(temporary, copied)
	if err != nil {
		return pkg, locked, nil, err
	}
	locked = LockedDependency{Source: "github:" + copied.Name, Requested: RequestedLocal, Kind: ResolutionLocal, PackageVersion: copied.Version, ContentHash: digest}
	return MaterializedPackage{Root: temporary, Manifest: copied}, locked, remove, nil
}

func copyLocalFile(root *os.Root, relative, destination string, limit int64) (size int64, err error) {
	if err := realize.ValidateParentDirectories(root, relative); err != nil {
		return 0, err
	}
	before, err := root.Lstat(relative)
	if err != nil {
		return 0, err
	}
	if !before.Mode().IsRegular() {
		return 0, errors.New("source must be a regular file")
	}
	if before.Size() > limit {
		return 0, errors.New("local package exceeds byte limit; reduce package size")
	}
	file, err := root.Open(relative)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	opened, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if err := realize.ValidateParentDirectories(root, relative); err != nil {
		return 0, err
	}
	current, err := root.Lstat(relative)
	if err != nil {
		return 0, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(opened, current) {
		return 0, errors.New("source changed while opening; retry with a stable source")
	}
	target := filepath.Join(destination, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return 0, err
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, output.Close()) }()
	size, err = io.Copy(output, io.LimitReader(file, limit+1))
	if err != nil {
		return 0, err
	}
	if size > limit {
		return 0, errors.New("local package exceeds byte limit; reduce package size")
	}
	if err := output.Chmod(normalizedPackageMode(opened.Mode())); err != nil {
		return 0, err
	}
	return size, nil
}

func materializeLocal(project string, locked LockedDependency) (MaterializedPackage, func() error, error) {
	declaration := Declaration{Source: locked.Source, Requested: RequestedLocal, Path: locked.Path}
	root, err := authorizeLocal(project, declaration)
	if err != nil {
		return MaterializedPackage{}, nil, err
	}
	pkg, actual, cleanup, err := snapshotLocal(root)
	if err != nil {
		return MaterializedPackage{}, nil, localError(cli.CodeLocalSourceChanged, declaration, err)
	}
	if actual.Source != locked.Source || actual.PackageVersion != locked.PackageVersion || actual.ContentHash != locked.ContentHash {
		return MaterializedPackage{}, nil, errors.Join(localError(cli.CodeLocalSourceChanged, declaration, errors.New("source content no longer matches the lock")), cleanup())
	}
	return pkg, cleanup, nil
}

// InstallLocal authorizes the named canonical directory, then records the real
// package version and inventory hash without inventing remote provenance.
func (service *Service) InstallLocal(ctx context.Context, project, localPath string, dryRun bool, freshnessChoice ...string) (result ChangeResult, err error) {
	if err := validateLocalPath(RequestedLocal, localPath); err != nil {
		return result, err
	}
	root, err := localRoot(project, localPath)
	if err != nil {
		return result, localError(cli.CodeLocalSourceUnavailable, Declaration{Path: localPath}, err)
	}
	_, locked, cleanup, err := snapshotLocal(root)
	if err != nil {
		return result, err
	}
	// Remove the temporary inventory before any state is written: cleanup
	// failure cannot turn a completed install into an ambiguous failed install.
	if err := cleanup(); err != nil {
		return result, err
	}
	locked.Path = localPath
	apply := func() error {
		if !dryRun {
			// Explicit PATH is consent to repair this project. Recover before
			// reading before-images or considering an unchanged shortcut.
			if err := realize.RecoverTransactions(project); err != nil {
				return err
			}
		}
		state, err := LoadState(project)
		if err != nil {
			return err
		}
		before := cloneState(state)
		chosen := ""
		if len(freshnessChoice) > 0 {
			chosen = freshnessChoice[0]
		}
		if policy, persist := freshness.Resolve(state.Project.Freshness, chosen, len(freshnessChoice) > 0); persist {
			state.Project.Freshness = string(policy)
		}
		declaration := Declaration{Source: locked.Source, Requested: RequestedLocal, Path: localPath}
		if index, found := findDeclaration(state.Project.Dependencies, locked.Source); found {
			previous := state.Project.Dependencies[index]
			if previous.Hold != nil {
				return fmt.Errorf("%s has a rollback hold; run 'acr resume %s' or install its pin with --pin first", locked.Source, locked.Source)
			}
			declaration.Extra = previous.Extra
			state.Project.Dependencies[index] = declaration
		} else {
			state.Project.Dependencies = append(state.Project.Dependencies, declaration)
		}
		if index, found := findLock(state.Lock.Dependencies, locked.Source); found {
			state.Lock.Dependencies[index] = locked
		} else {
			state.Lock.Dependencies = append(state.Lock.Dependencies, locked)
		}
		state.Project.SchemaVersion, state.Lock.SchemaVersion = LocalSchemaVersion, LocalSchemaVersion
		// Only this explicit argument is authorized by this invocation. Other rows
		// are retained, never adopted or refreshed as a side effect.
		sortState(&state.Project, &state.Lock)
		result = ChangeResult{Changed: !reflect.DeepEqual(before, state), Dependencies: state.Lock.Dependencies}
		if dryRun {
			return nil
		}
		if !result.Changed {
			return checkExpectedState(project, before)
		}
		return writeExpectedState(project, before, state)
	}
	if dryRun {
		err = apply()
	} else {
		err = changeLocalAuthorization(project, locked.Source, &localAuthorization{SchemaVersion: 1, Path: localPath, SourceRoot: root}, apply)
	}
	return result, err
}

func hasLocalDeclarations(state State) bool {
	for _, declaration := range state.Project.Dependencies {
		if declaration.Requested == RequestedLocal {
			return true
		}
	}
	return false
}
