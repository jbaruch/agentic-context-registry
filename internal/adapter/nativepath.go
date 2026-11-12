package adapter

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/packageref"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// claudeInstructionAlias is the only instruction path whose symlink is an
// alias of the regular file it resolves to. Other instruction roots share
// readInstructionCandidate and keep refusing when they are symlinks.
// GEMINI.md is not an instruction root of detection or realization, and a
// nested AGENTS.md is discovered by the include-graph walk, which skips
// symlinks instead of inspecting them here, so neither gets this alias.
const claudeInstructionAlias = "CLAUDE.md"

// maxInstructionSymlinkHops matches the usual kernel symlink limit. A longer
// chain is refused rather than followed forever.
const maxInstructionSymlinkHops = 40

const (
	CodeInvalidNativeEvent    = "invalid_native_event"
	CodeMalformedFrontmatter  = "malformed_frontmatter"
	CodeInvalidSkillTree      = "invalid_skill_tree"
	CodeInvalidExecutableMode = "invalid_executable_mode"
)

// NativeValidationError is an adapter-owned validation failure with a stable
// machine-readable code.
type NativeValidationError struct {
	Code    string
	Message string
}

func (err *NativeValidationError) Error() string {
	return err.Code + ": " + err.Message
}

// NativeError constructs a stable native validation error.
func NativeError(code, format string, args ...any) error {
	return &NativeValidationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// NativeArtifactName returns the collision-resistant native directory or file
// stem for one package artifact.
func NativeArtifactName(source, artifactID string) (string, error) {
	scheme, identity, hasScheme := strings.Cut(source, ":")
	owner, repository, found := strings.Cut(identity, "/")
	if !hasScheme || scheme == "" || !found || owner == "" || repository == "" || strings.Contains(repository, "/") {
		return "", fmt.Errorf("source %q must use scheme:owner/repository syntax", source)
	}
	return "acr__" + owner + "__" + repository + "__" + artifactID, nil
}

// PackageFile is one regular source file beneath an artifact tree.
type PackageFile struct {
	Path    string
	Content []byte
	Mode    fs.FileMode
}

// ReadPackageFile reads one regular package file without following a leaf
// symlink or accepting a special file.
func ReadPackageFile(pkg Package, filename string) (PackageFile, error) {
	info, err := fs.Stat(pkg.Root, filename)
	if err != nil {
		return PackageFile{}, err
	}
	if !info.Mode().IsRegular() {
		return PackageFile{}, fmt.Errorf("package path %q must be a regular file", filename)
	}
	content, err := fs.ReadFile(pkg.Root, filename)
	if err != nil {
		return PackageFile{}, err
	}
	return PackageFile{Path: filename, Content: content, Mode: info.Mode()}, nil
}

// ReadPackageTree returns every regular file beneath root in POSIX lexical
// order. Symlinks and special entries fail closed.
func ReadPackageTree(pkg Package, root string) ([]PackageFile, error) {
	var result []PackageFile
	err := fs.WalkDir(pkg.Root, root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("package tree %q contains symlink %q", root, filename)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("package tree %q contains special entry %q", root, filename)
		}
		content, err := fs.ReadFile(pkg.Root, filename)
		if err != nil {
			return err
		}
		result = append(result, PackageFile{Path: filename, Content: content, Mode: info.Mode()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Path < result[right].Path })
	return result, nil
}

// ExistingEvidence returns sorted regular-file and directory evidence. Missing
// paths are ignored; every other snapshot error is returned. A project-root
// CLAUDE.md symlink that resolves to an in-project regular file is evidence
// for the file's own path; the symlink is not followed by ReadFile.
func ExistingEvidence(snapshot Snapshot, files, directories []string) ([]string, error) {
	var evidence []string
	for _, filename := range files {
		observed, alias, err := readInstructionCandidate(snapshot, filename)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("inspect detection evidence %q: %w", filename, err)
		}
		if alias != "" || observed.Mode.IsRegular() {
			evidence = append(evidence, filename)
		}
	}
	directorySnapshot, supportsDirectories := snapshot.(DirectorySnapshot)
	if supportsDirectories {
		for _, directory := range directories {
			_, err := directorySnapshot.ReadDir(directory)
			if err == nil {
				evidence = append(evidence, directory+"/")
				continue
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("inspect detection evidence %q: %w", directory, err)
			}
		}
	}
	sort.Strings(evidence)
	return evidence, nil
}

// symlinkClassifier is implemented by the production root snapshot. Test
// snapshots that cannot see a symlink as a symlink keep ReadFile's result.
type symlinkClassifier interface {
	classifySymlink(filename string) (isSymlink bool, resolved string, err error)
}

// readInstructionCandidate reads one detection or instruction-root path.
// A project-root CLAUDE.md symlink whose chain stays inside the project and
// ends at a regular file is reported as alias of that file. Every other
// symlink is refused with the path and the resolved target.
func readInstructionCandidate(project Snapshot, filename string) (ObservedFile, string, error) {
	if classifier, ok := project.(symlinkClassifier); ok {
		isLink, resolved, linkErr := classifier.classifySymlink(filename)
		if linkErr != nil && !isLink {
			return ObservedFile{}, "", linkErr
		}
		if isLink {
			if filename == claudeInstructionAlias && linkErr == nil && acceptedInstructionAlias(resolved) {
				return ObservedFile{}, resolved, nil
			}
			target := resolved
			var refusal *NonRegularFileError
			if errors.As(linkErr, &refusal) && refusal.Target != "" {
				target = refusal.Target
			}
			if linkErr != nil && target == "" && !errors.As(linkErr, &refusal) {
				return ObservedFile{}, "", linkErr
			}
			return ObservedFile{}, "", &NonRegularFileError{Path: filename, Target: target}
		}
	}
	observed, err := project.ReadFile(filename)
	return observed, "", err
}

// acceptedInstructionAlias rejects a resolved path that is not a project-relative
// regular-file path. The chase already enforces this; the check is the backstop
// before that path is used as an instruction root.
func acceptedInstructionAlias(resolved string) bool {
	if resolved == "" || resolved == "." || resolved == ".." || path.IsAbs(resolved) {
		return false
	}
	if strings.HasPrefix(resolved, "../") || strings.Contains(resolved, "/../") || strings.HasSuffix(resolved, "/..") {
		return false
	}
	return resolved == path.Clean(resolved)
}

// classifySymlink reports whether filename's leaf is a symlink. Parents that
// are symlinks are rejected before the leaf is inspected. A chain that stays
// inside the project and ends at a regular file returns that file's
// project-relative path. Every other chain returns the path it resolved to,
// including a missing or escaping target, so the refusal can name it.
func (snapshot *RootSnapshot) classifySymlink(filename string) (bool, string, error) {
	if err := realize.ValidateParentDirectories(snapshot.root, filename); err != nil {
		return false, "", err
	}
	info, err := snapshot.root.Lstat(filename)
	if errors.Is(err, fs.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return false, "", nil
	}
	resolved, target, err := followInstructionSymlink(snapshot.root, snapshot.root.Name(), filename)
	if err != nil {
		if target == "" {
			return true, "", err
		}
		return true, "", &NonRegularFileError{Path: filename, Target: target}
	}
	return true, resolved, nil
}

// instructionChase walks one symlink chain one component at a time. os.Root
// follows intermediate symlinks, and path.Clean of "shortcut/../file" drops a
// symlink that was never inspected, so neither is used to decide containment.
type instructionChase struct {
	root        *os.Root
	projectName string
	hops        int
	seen        map[string]bool
}

func followInstructionSymlink(root *os.Root, projectName, filename string) (resolved, display string, err error) {
	parent := path.Dir(filename)
	if parent == "." {
		parent = ""
	}
	chase := &instructionChase{root: root, projectName: projectName, seen: map[string]bool{}}
	return chase.walk(parent, []string{path.Base(filename)})
}

func (chase *instructionChase) walk(dir string, parts []string) (string, string, error) {
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if dir == "" {
				return "", strings.Join(append([]string{part}, parts...), "/"), errSymlinkEscapes
			}
			dir = path.Dir(dir)
			if dir == "." {
				dir = ""
			}
			continue
		}
		next := joinProject(dir, part)
		info, err := chase.root.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) {
			return "", joinDisplay(dir, append([]string{part}, parts...)), errSymlinkDangling
		}
		if err != nil {
			return "", "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if chase.seen[next] {
				return "", next, errSymlinkCycle
			}
			chase.seen[next] = true
			chase.hops++
			if chase.hops > maxInstructionSymlinkHops {
				return "", next, errSymlinkCycle
			}
			raw, err := chase.root.Readlink(next)
			if err != nil {
				return "", "", err
			}
			rest := append([]string(nil), parts...)
			if filepath.IsAbs(raw) {
				relative, inside, err := projectRelative(chase.projectName, raw)
				if err != nil {
					return "", "", err
				}
				if !inside {
					return "", joinAbsolute(raw, rest), errSymlinkEscapes
				}
				if relative == "." {
					if len(skipEmptyDots(rest)) == 0 {
						return "", projectDisplay(chase.projectName), errSymlinkNotRegular
					}
					return chase.walk("", rest)
				}
				return chase.walk("", append(splitComponents(relative), rest...))
			}
			return chase.walk(dir, append(splitComponents(raw), rest...))
		}
		if info.Mode().IsRegular() {
			if len(skipEmptyDots(parts)) == 0 {
				return next, "", nil
			}
			return "", joinDisplay(dir, append([]string{part}, parts...)), errSymlinkNotRegular
		}
		if info.IsDir() {
			dir = next
			continue
		}
		return "", joinDisplay(dir, append([]string{part}, parts...)), errSymlinkNotRegular
	}
	if dir == "" {
		return "", projectDisplay(chase.projectName), errSymlinkNotRegular
	}
	return "", dir, errSymlinkNotRegular
}

func joinProject(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func joinDisplay(dir string, parts []string) string {
	rest := strings.Join(parts, "/")
	if dir == "" {
		return rest
	}
	if rest == "" {
		return dir
	}
	return dir + "/" + rest
}

func joinAbsolute(raw string, rest []string) string {
	if len(rest) == 0 {
		return raw
	}
	suffix := strings.Join(rest, "/")
	if strings.HasSuffix(raw, "/") {
		return raw + suffix
	}
	return raw + "/" + suffix
}

func splitComponents(raw string) []string {
	return strings.Split(filepath.ToSlash(raw), "/")
}

func skipEmptyDots(parts []string) []string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		kept = append(kept, part)
	}
	return kept
}

func projectDisplay(projectName string) string {
	absolute, err := filepath.Abs(projectName)
	if err != nil {
		return "."
	}
	return filepath.Clean(absolute)
}

// projectRelative reports the project-relative form of an absolute symlink
// target. The final component is not evaluated: a symlink there is followed
// by the walk, inside the root. ".." is resolved by the kernel for the parent
// only, so a lexical clean cannot turn an outside symlink into an inside path.
func projectRelative(projectName, absolute string) (string, bool, error) {
	candidate, ok, err := absoluteCandidate(absolute)
	if err != nil || !ok {
		return "", false, err
	}
	return relativeToProject(projectName, candidate)
}

func absoluteCandidate(absolute string) (string, bool, error) {
	parent := filepath.Dir(absolute)
	base := filepath.Base(absolute)
	dotDot := false
	for _, part := range splitComponents(absolute) {
		if part == ".." {
			dotDot = true
			break
		}
	}
	parentEval, err := filepath.EvalSymlinks(parent)
	if err != nil {
		if dotDot {
			return "", false, nil
		}
		return filepath.Clean(absolute), true, nil
	}
	return filepath.Join(parentEval, base), true, nil
}

func relativeToProject(projectName, candidate string) (string, bool, error) {
	projectAbs, err := filepath.Abs(projectName)
	if err != nil {
		return "", false, err
	}
	projects := []string{filepath.Clean(projectAbs)}
	if evaluated, evalErr := filepath.EvalSymlinks(projectAbs); evalErr == nil {
		evaluated = filepath.Clean(evaluated)
		if evaluated != projects[0] {
			projects = append(projects, evaluated)
		}
	}
	candidate = filepath.Clean(candidate)
	for _, project := range projects {
		relative, relErr := filepath.Rel(project, candidate)
		if relErr != nil {
			continue
		}
		if relative == "." {
			return ".", true, nil
		}
		if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			continue
		}
		return filepath.ToSlash(relative), true, nil
	}
	return "", false, nil
}

var (
	errSymlinkEscapes    = errors.New("symlink escapes the project")
	errSymlinkDangling   = errors.New("symlink target is missing")
	errSymlinkNotRegular = errors.New("symlink target is not a regular file")
	errSymlinkCycle      = errors.New("symlink cycle")
)

// WalkSnapshot returns the complete tree beneath root without following
// symlinked directories. A symlink is returned as an entry so validation can
// reject it with its adapter-owned error code.
func WalkSnapshot(snapshot Snapshot, root string) ([]ObservedEntry, error) {
	directorySnapshot, ok := snapshot.(DirectorySnapshot)
	if !ok {
		return nil, fmt.Errorf("snapshot does not support directory inspection")
	}
	var result []ObservedEntry
	var walk func(string) error
	walk = func(directory string) error {
		entries, err := directorySnapshot.ReadDir(directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			result = append(result, entry)
			if entry.Mode.IsDir() && entry.Mode&fs.ModeSymlink == 0 {
				if err := walk(entry.Path); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Path < result[right].Path })
	return result, nil
}

// SourceBasename returns a package source path's POSIX basename.
func SourceBasename(sourcePath string) string { return path.Base(sourcePath) }

// SkillRebase maps an authored skill root to its native root.
type SkillRebase = packageref.SkillRebase

// SkillReferences holds the package's owned skill roots and identities.
type SkillReferences = packageref.SkillReferences

// PackageSkillReferences resolves one package's skills against a native
// skills root. Rebases are ordered longest source root first so a nested
// skill tree is matched before the ancestor containing it, and a file may
// address any skill in its own package, so every caller applies the whole
// value to every file it renders.
func PackageSkillReferences(pkg Package, nativeSkillsRoot string) (SkillReferences, error) {
	references := SkillReferences{Rebases: make([]SkillRebase, 0, len(pkg.Manifest.Artifacts.Skills))}
	for _, skill := range pkg.Manifest.Artifacts.Skills {
		name, err := NativeArtifactName(pkg.Source, skill.ID)
		if err != nil {
			return SkillReferences{}, err
		}
		references.Rebases = append(references.Rebases, SkillRebase{SourceRoot: skill.Path, NativeRoot: path.Join(nativeSkillsRoot, name)})
	}
	sort.SliceStable(references.Rebases, func(left, right int) bool {
		return len(references.Rebases[left].SourceRoot) > len(references.Rebases[right].SourceRoot)
	})
	for _, identity := range []string{pkg.Manifest.Source.TesslIdentity, pkg.Manifest.Name} {
		if identity == "" || slices.Contains(references.Identities, identity) {
			continue
		}
		references.Identities = append(references.Identities, identity)
	}
	return references, nil
}

// RebaseSkillReferences preserves the original two-root adapter boundary.
func RebaseSkillReferences(content []byte, sourceRoot, nativeRoot string) []byte {
	return packageref.RebaseSkillReferences(content, sourceRoot, nativeRoot)
}

// RebasePackageReferences rewrites supported owned package references.
func RebasePackageReferences(content []byte, references SkillReferences) []byte {
	return packageref.RebasePackageReferences(content, references)
}
