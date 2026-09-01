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
	lock := Lockfile{SchemaVersion: CurrentSchemaVersion}
	if err := loadYAML(projectRoot, LockFilename, &lock); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return State{}, fmt.Errorf("load %s: %w; fix or remove the invalid lockfile and retry", LockFilename, err)
		}
	}
	if err := validateState(project, lock); err != nil {
		return State{}, err
	}
	sortState(&project, &lock)
	return State{Project: project, Lock: lock}, nil
}

// WriteState writes stable YAML for both requested and locked dependency state.
func WriteState(root string, state State) error {
	if err := validateState(state.Project, state.Lock); err != nil {
		return err
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
	if err := writeFileAtomic(projectRoot, ProjectFilename, projectData); err != nil {
		return fmt.Errorf("write %s: %w; verify the project directory is writable and retry", ProjectFilename, err)
	}
	if err := writeFileAtomic(projectRoot, LockFilename, lockData); err != nil {
		return fmt.Errorf("write %s: %w; verify the project directory is writable and retry", LockFilename, err)
	}
	return nil
}

func loadYAML(root *os.Root, filename string, target any) error {
	info, err := root.Lstat(filename)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file, not a symlink or special file", filename)
	}
	file, err := root.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened file: %w", err)
	}
	currentInfo, err := root.Lstat(filename)
	if err != nil {
		return fmt.Errorf("inspect file after opening: %w", err)
	}
	if currentInfo.Mode()&os.ModeSymlink != 0 || !currentInfo.Mode().IsRegular() || !os.SameFile(openedInfo, currentInfo) {
		return fmt.Errorf("%s changed while being opened; retry with a stable regular file", filename)
	}
	contents, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}
	if len(contents) > 8<<20 {
		return fmt.Errorf("state file exceeds 8 MiB; remove unexpected content and retry")
	}
	if err := yaml.Unmarshal(contents, target); err != nil {
		return fmt.Errorf("decode YAML: %w", err)
	}
	return nil
}

func writeFileAtomic(root *os.Root, filename string, contents []byte) error {
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
			return fmt.Errorf("destination must be a regular file, not a symlink or special file")
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
	if err := temporary.Chmod(0o644); err != nil {
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
