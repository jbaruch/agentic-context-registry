package adapter

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

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
// paths are ignored; every other snapshot error is returned.
func ExistingEvidence(snapshot Snapshot, files, directories []string) ([]string, error) {
	var evidence []string
	for _, filename := range files {
		observed, err := snapshot.ReadFile(filename)
		if err == nil {
			if observed.Mode.IsRegular() {
				evidence = append(evidence, filename)
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("inspect detection evidence %q: %w", filename, err)
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

// tesslInstalledRoot is the directory a Tessl-installed package tree sits
// under, followed by the package's two-segment Tessl identity.
const tesslInstalledRoot = ".tessl/plugins/"

// SkillRebase maps one skill's package-root tree to the native directory that
// replaces it.
type SkillRebase struct {
	SourceRoot string
	NativeRoot string
}

// SkillRebases returns one rebase per skill the package declares, ordered
// longest source root first so a nested skill tree is matched before the
// ancestor that contains it. A skill file may address any skill in its own
// package, so every caller applies the whole set to every file it renders.
func SkillRebases(pkg Package, nativeSkillsRoot string) ([]SkillRebase, error) {
	rebases := make([]SkillRebase, 0, len(pkg.Manifest.Artifacts.Skills))
	for _, skill := range pkg.Manifest.Artifacts.Skills {
		name, err := NativeArtifactName(pkg.Source, skill.ID)
		if err != nil {
			return nil, err
		}
		rebases = append(rebases, SkillRebase{SourceRoot: skill.Path, NativeRoot: path.Join(nativeSkillsRoot, name)})
	}
	sort.SliceStable(rebases, func(left, right int) bool {
		return len(rebases[left].SourceRoot) > len(rebases[right].SourceRoot)
	})
	return rebases, nil
}

// RebaseSkillReferences maps one skill tree's supported references to their
// installed native directory while preserving every other byte.
//
// Two reference forms are supported: the package-root path `<sourceRoot>/...`
// and the legacy Tessl-installed path `.tessl/plugins/<workspace>/<package>/`
// followed by that same package-root path. Either is rewritten only where it
// starts a path — at the beginning of the content, or after a byte that
// cannot continue one. A match inside a longer path, a URL, or another
// package's reference is not a reference to this tree and is left alone.
//
// A reference outside those two forms is outside the migration contract and
// is preserved unchanged rather than rewritten into a path that resolves
// nowhere; see docs/adapters.md for the boundary.
func RebaseSkillReferences(content []byte, sourceRoot, nativeRoot string) []byte {
	sourcePrefix := []byte(strings.TrimSuffix(sourceRoot, "/") + "/")
	nativePrefix := []byte(strings.TrimSuffix(nativeRoot, "/") + "/")
	result := make([]byte, 0, len(content))
	for index := 0; index < len(content); {
		width, matched := skillReferenceWidth(content, index, sourcePrefix)
		if !matched {
			result = append(result, content[index])
			index++
			continue
		}
		result = append(result, nativePrefix...)
		index += width
	}
	return result
}

// skillReferenceWidth reports the byte width of the supported reference
// prefix that starts at index, or false when no supported form starts there.
func skillReferenceWidth(content []byte, index int, sourcePrefix []byte) (int, bool) {
	if index != 0 && continuesPath(content[index-1]) {
		return 0, false
	}
	rest := content[index:]
	if bytes.HasPrefix(rest, sourcePrefix) {
		return len(sourcePrefix), true
	}
	if !bytes.HasPrefix(rest, []byte(tesslInstalledRoot)) {
		return 0, false
	}
	identity, complete := tesslIdentityWidth(rest[len(tesslInstalledRoot):])
	if !complete {
		return 0, false
	}
	offset := len(tesslInstalledRoot) + identity
	if !bytes.HasPrefix(rest[offset:], sourcePrefix) {
		return 0, false
	}
	return offset + len(sourcePrefix), true
}

// tesslIdentityWidth reports the byte width of the `<workspace>/<package>/`
// identity following the Tessl install root, or false when two complete
// segments do not start there. The identity is matched structurally because a
// package's Tessl identity is not derivable from its ACR source: a package
// republished under a new name keeps the old identity in the references its
// own files carry.
func tesslIdentityWidth(rest []byte) (int, bool) {
	width := 0
	for segment := 0; segment < 2; segment++ {
		end := bytes.IndexByte(rest, '/')
		if end <= 0 {
			return 0, false
		}
		if name := string(rest[:end]); name == "." || name == ".." {
			return 0, false
		}
		width += end + 1
		rest = rest[end+1:]
	}
	return width, true
}

// continuesPath reports whether b can continue a path or URL. A reference
// preceded by one is an interior match, not the start of a reference.
func continuesPath(b byte) bool {
	if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' {
		return true
	}
	switch b {
	case '/', '.', '-', '_', '~', '@', '+', '%', ':':
		return true
	}
	return false
}
