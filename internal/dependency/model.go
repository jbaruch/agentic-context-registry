// Package dependency resolves project package declarations into immutable locks.
package dependency

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"go.yaml.in/yaml/v3"
)

const (
	// ProjectFilename stores requested dependency policies.
	ProjectFilename = "agents.yaml"
	// LockFilename stores immutable dependency resolutions.
	LockFilename = ".agents/registry.lock"
	// CurrentSchemaVersion is the supported project and lock schema version.
	CurrentSchemaVersion = 1
)

// Project describes user-requested dependency policy. Extra top-level fields
// are preserved so other project configuration owners can extend agents.yaml.
type Project struct {
	SchemaVersion int            `yaml:"schemaVersion" json:"schemaVersion"`
	Dependencies  []Declaration  `yaml:"dependencies,omitempty" json:"dependencies,omitempty"`
	Extra         map[string]any `yaml:",inline" json:"-"`
}

// Declaration records one requested GitHub dependency policy.
type Declaration struct {
	Source    string         `yaml:"source" json:"source"`
	Requested string         `yaml:"requested" json:"requested"`
	Extra     map[string]any `yaml:",inline" json:"-"`
}

// Lockfile records immutable resolutions separately from requested policy.
// Extra top-level fields are preserved for realization ownership metadata.
type Lockfile struct {
	SchemaVersion int                `yaml:"schemaVersion" json:"schemaVersion"`
	Dependencies  []LockedDependency `yaml:"dependencies,omitempty" json:"dependencies,omitempty"`
	Realization   map[string]any     `yaml:"realization,omitempty" json:"realization,omitempty"`
	Extra         map[string]any     `yaml:",inline" json:"-"`
}

// LockedDependency is one reproducible package resolution.
type LockedDependency struct {
	Source         string         `yaml:"source" json:"source"`
	Requested      string         `yaml:"requested" json:"requested"`
	Kind           ResolutionKind `yaml:"kind" json:"kind"`
	ReleaseID      int64          `yaml:"releaseId,omitempty" json:"releaseId,omitempty"`
	Tag            string         `yaml:"tag,omitempty" json:"tag,omitempty"`
	Commit         string         `yaml:"commit" json:"commit"`
	PackageVersion string         `yaml:"packageVersion" json:"packageVersion"`
	ContentHash    string         `yaml:"contentHash" json:"contentHash"`
	Extra          map[string]any `yaml:",inline" json:"-"`
}

// ResolutionKind distinguishes stable releases from direct commit pins.
type ResolutionKind string

const (
	ResolutionRelease ResolutionKind = "release"
	ResolutionCommit  ResolutionKind = "commit"
)

// State is the complete dependency state for one project.
type State struct {
	Project Project
	Lock    Lockfile
}

type stateFileWriter func(*os.Root, string, []byte, os.FileMode) (bool, error)

type fileSnapshot struct {
	exists   bool
	contents []byte
	mode     os.FileMode
}

// LoadState reads agents.yaml and .agents/registry.lock. Missing files produce
// empty versioned state; malformed or unsafe files fail with recovery guidance.
func LoadState(root string) (State, error) {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return State{}, fmt.Errorf("open project directory %q: %w; verify --project names an accessible directory", root, err)
	}
	defer projectRoot.Close()

	project := Project{SchemaVersion: CurrentSchemaVersion}
	if err := loadYAML(projectRoot, ProjectFilename, &project); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return State{}, fmt.Errorf("load %s: %w; fix or remove the invalid project file and retry", ProjectFilename, err)
		}
	}
	if _, err := validateStateDirectory(projectRoot); err != nil {
		return State{}, fmt.Errorf("load %s: %w", LockFilename, err)
	}
	lock := Lockfile{SchemaVersion: CurrentSchemaVersion}
	if err := loadYAML(projectRoot, LockFilename, &lock); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return State{}, fmt.Errorf("load %s: %w; fix or remove the invalid lockfile and retry", LockFilename, err)
		}
	}
	if err := validateState(project, lock); err != nil {
		return State{}, err
	}
	if err := normalizeRealization(&lock); err != nil {
		return State{}, fmt.Errorf("normalize %s realization ledger: %w", LockFilename, err)
	}
	sortState(&project, &lock)
	return State{Project: project, Lock: lock}, nil
}

// WriteState writes stable YAML for both requested and locked dependency state.
func WriteState(root string, state State) error {
	return writeStateWith(root, state, func(root *os.Root, filename string, contents []byte, mode os.FileMode) (bool, error) {
		err := writeFileAtomic(root, filename, contents, mode)
		return err == nil, err
	})
}

func writeStateWith(root string, state State, writer stateFileWriter) error {
	if err := validateState(state.Project, state.Lock); err != nil {
		return err
	}
	if err := normalizeRealization(&state.Lock); err != nil {
		return fmt.Errorf("normalize %s realization ledger: %w", LockFilename, err)
	}
	sortState(&state.Project, &state.Lock)
	projectData, err := yaml.Marshal(state.Project)
	if err != nil {
		return fmt.Errorf("encode %s: %w; report the invalid project state", ProjectFilename, err)
	}
	lockData, err := yaml.Marshal(state.Lock)
	if err != nil {
		return fmt.Errorf("encode %s: %w; report the invalid lock state", LockFilename, err)
	}
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("open project directory %q: %w; verify --project names a writable directory", root, err)
	}
	defer projectRoot.Close()
	agentsDirectoryExisted, err := validateStateDirectory(projectRoot)
	if err != nil {
		return err
	}
	projectBefore, err := snapshotFile(projectRoot, ProjectFilename)
	if err != nil {
		return fmt.Errorf("snapshot %s: %w; keep the project state stable and retry", ProjectFilename, err)
	}
	lockBefore, err := snapshotFile(projectRoot, LockFilename)
	if err != nil {
		return fmt.Errorf("snapshot %s: %w; keep the project state stable and retry", LockFilename, err)
	}
	projectReplaced, err := writer(projectRoot, ProjectFilename, projectData, 0o644)
	if err != nil {
		return rollbackWriteError(projectRoot, ProjectFilename, err, projectBefore, lockBefore, projectReplaced, false, agentsDirectoryExisted)
	}
	lockReplaced, err := writer(projectRoot, LockFilename, lockData, 0o644)
	if err != nil {
		return rollbackWriteError(projectRoot, LockFilename, err, projectBefore, lockBefore, projectReplaced, lockReplaced, agentsDirectoryExisted)
	}
	return nil
}

func loadYAML(root *os.Root, filename string, target any) error {
	contents, _, err := readRegularFile(root, filename)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(contents, target); err != nil {
		return fmt.Errorf("decode YAML: %w", err)
	}
	return nil
}

func readRegularFile(root *os.Root, filename string) ([]byte, os.FileMode, error) {
	info, err := root.Lstat(filename)
	if err != nil {
		return nil, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s must be a regular file, not a symlink or special file", filename)
	}
	file, err := root.Open(filename)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("inspect opened file: %w", err)
	}
	currentInfo, err := root.Lstat(filename)
	if err != nil {
		return nil, 0, fmt.Errorf("inspect file after opening: %w", err)
	}
	if currentInfo.Mode()&os.ModeSymlink != 0 || !currentInfo.Mode().IsRegular() || !os.SameFile(openedInfo, currentInfo) {
		return nil, 0, fmt.Errorf("%s changed while being opened; retry with a stable regular file", filename)
	}
	contents, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
	if err != nil {
		return nil, 0, fmt.Errorf("read file: %w", err)
	}
	if len(contents) > 8<<20 {
		return nil, 0, fmt.Errorf("state file exceeds 8 MiB; remove unexpected content and retry")
	}
	return contents, info.Mode().Perm(), nil
}

func writeFileAtomic(root *os.Root, filename string, contents []byte, mode os.FileMode) error {
	directory := path.Dir(filename)
	if directory != "." {
		if info, err := root.Lstat(directory); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("parent %q must be a directory, not a symlink or special file", directory)
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if err := root.MkdirAll(directory, 0o755); err != nil {
				return fmt.Errorf("create parent directory: %w", err)
			}
		} else {
			return fmt.Errorf("inspect parent directory: %w", err)
		}
	}
	if info, err := root.Lstat(filename); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("destination %q must be a regular file, not a symlink or special file", filename)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect destination: %w", err)
	}
	temporaryName, err := temporaryStateName(directory)
	if err != nil {
		return err
	}
	temporary, err := root.OpenFile(temporaryName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	defer root.Remove(temporaryName)
	if err := temporary.Chmod(mode.Perm()); err != nil {
		temporary.Close()
		return fmt.Errorf("set temporary file permissions: %w", err)
	}
	written, err := temporary.Write(contents)
	if err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if written != len(contents) {
		temporary.Close()
		return fmt.Errorf("write temporary file: %w", io.ErrShortWrite)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := root.Rename(temporaryName, filename); err != nil {
		return fmt.Errorf("replace destination: %w", err)
	}
	return nil
}

func validateStateDirectory(root *os.Root) (bool, error) {
	info, err := root.Lstat(".agents")
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect .agents: %w; verify the project state directory and retry", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf(".agents must be a directory, not a symlink or special file; replace it with a directory and retry")
	}
	return true, nil
}

func snapshotFile(root *os.Root, filename string) (fileSnapshot, error) {
	contents, mode, err := readRegularFile(root, filename)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{}, nil
	}
	if err != nil {
		return fileSnapshot{}, err
	}
	return fileSnapshot{exists: true, contents: contents, mode: mode}, nil
}

func rollbackWriteError(root *os.Root, failedFile string, writeErr error, projectBefore, lockBefore fileSnapshot, projectReplaced, lockReplaced, agentsDirectoryExisted bool) error {
	var rollbackErrors []error
	if lockReplaced {
		if err := restoreFile(root, LockFilename, lockBefore); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restore %s: %w", LockFilename, err))
		}
	}
	if projectReplaced {
		if err := restoreFile(root, ProjectFilename, projectBefore); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restore %s: %w", ProjectFilename, err))
		}
	}
	if !agentsDirectoryExisted {
		if err := root.Remove(".agents"); err != nil && !errors.Is(err, os.ErrNotExist) {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("remove newly created .agents directory: %w", err))
		}
	}
	if len(rollbackErrors) != 0 {
		return fmt.Errorf("write %s: %w; rollback also failed: %v; restore %s and %s from version control before retrying", failedFile, writeErr, errors.Join(rollbackErrors...), ProjectFilename, LockFilename)
	}
	return fmt.Errorf("write %s: %w; both state files were restored, so verify the project directory is writable and retry", failedFile, writeErr)
}

func restoreFile(root *os.Root, filename string, snapshot fileSnapshot) error {
	if snapshot.exists {
		return writeFileAtomic(root, filename, snapshot.contents, snapshot.mode)
	}
	if err := root.Remove(filename); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func temporaryStateName(directory string) (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate temporary file name: %w", err)
	}
	return path.Join(directory, ".acr-state-"+hex.EncodeToString(random[:])), nil
}

func validateState(project Project, lock Lockfile) error {
	if project.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("unsupported %s schemaVersion %d; use schemaVersion %d", ProjectFilename, project.SchemaVersion, CurrentSchemaVersion)
	}
	if lock.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("unsupported %s schemaVersion %d; use schemaVersion %d", LockFilename, lock.SchemaVersion, CurrentSchemaVersion)
	}
	if _, err := realize.DecodeLedger(lock.Realization); err != nil {
		return fmt.Errorf("invalid %s realization ledger: %w", LockFilename, err)
	}
	declarations := make(map[string]Declaration, len(project.Dependencies))
	for index, declaration := range project.Dependencies {
		if _, err := ParseSource(declaration.Source); err != nil {
			return fmt.Errorf("dependencies[%d].source: %w", index, err)
		}
		if err := validateRequested(declaration.Requested); err != nil {
			return fmt.Errorf("dependencies[%d].requested: %w", index, err)
		}
		if _, exists := declarations[declaration.Source]; exists {
			return fmt.Errorf("dependency %q is declared more than once; keep one requested policy", declaration.Source)
		}
		declarations[declaration.Source] = declaration
	}
	seenLocks := make(map[string]struct{}, len(lock.Dependencies))
	for index, dependency := range lock.Dependencies {
		if _, err := ParseSource(dependency.Source); err != nil {
			return fmt.Errorf("locked dependencies[%d].source: %w", index, err)
		}
		if _, exists := seenLocks[dependency.Source]; exists {
			return fmt.Errorf("dependency %q is locked more than once; regenerate %s", dependency.Source, LockFilename)
		}
		seenLocks[dependency.Source] = struct{}{}
		declaration, declared := declarations[dependency.Source]
		if !declared {
			return fmt.Errorf("locked dependency %q is not declared in %s; remove the orphaned lock entry or delete %s and run 'acr install'", dependency.Source, ProjectFilename, LockFilename)
		}
		if dependency.Requested != declaration.Requested {
			return fmt.Errorf("locked dependency %q requests %q but %s requests %q; delete %s and run 'acr install' to resolve the declaration", dependency.Source, dependency.Requested, ProjectFilename, declaration.Requested, LockFilename)
		}
		if validateRequested(dependency.Requested) != nil || !fullCommitPattern.MatchString(dependency.Commit) || dependency.PackageVersion == "" || !contentHashPattern.MatchString(dependency.ContentHash) {
			return fmt.Errorf("locked dependency %q is incomplete; run 'acr install' to regenerate %s", dependency.Source, LockFilename)
		}
		switch dependency.Kind {
		case ResolutionRelease:
			if dependency.ReleaseID <= 0 || dependency.Tag == "" || isCommitRequest(dependency.Requested) || dependency.Requested != "latest" && dependency.Tag != dependency.Requested {
				return fmt.Errorf("locked release %q has inconsistent release metadata or requested policy; run 'acr install' to regenerate %s", dependency.Source, LockFilename)
			}
		case ResolutionCommit:
			if dependency.ReleaseID != 0 || dependency.Tag != "" || !isCommitRequest(dependency.Requested) || !strings.HasPrefix(dependency.Commit, strings.ToLower(dependency.Requested)) {
				return fmt.Errorf("locked commit %q has inconsistent commit metadata or requested policy; run 'acr install' to regenerate %s", dependency.Source, LockFilename)
			}
		default:
			return fmt.Errorf("locked dependency %q has unsupported kind %q; run 'acr install' to regenerate %s", dependency.Source, dependency.Kind, LockFilename)
		}
	}
	return nil
}

func normalizeRealization(lock *Lockfile) error {
	if len(lock.Realization) == 0 {
		return nil
	}
	ledger, err := realize.DecodeLedger(lock.Realization)
	if err != nil {
		return err
	}
	encoded, err := realize.EncodeLedger(ledger)
	if err != nil {
		return err
	}
	lock.Realization = encoded
	return nil
}

func sortState(project *Project, lock *Lockfile) {
	sort.SliceStable(project.Dependencies, func(left, right int) bool {
		return project.Dependencies[left].Source < project.Dependencies[right].Source
	})
	sort.SliceStable(lock.Dependencies, func(left, right int) bool {
		return lock.Dependencies[left].Source < lock.Dependencies[right].Source
	})
}

func findDeclaration(declarations []Declaration, source string) (int, bool) {
	for index := range declarations {
		if declarations[index].Source == source {
			return index, true
		}
	}
	return 0, false
}

func findLock(dependencies []LockedDependency, source string) (int, bool) {
	for index := range dependencies {
		if dependencies[index].Source == source {
			return index, true
		}
	}
	return 0, false
}
