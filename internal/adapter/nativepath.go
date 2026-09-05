package adapter

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
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

// tesslInstalledRoot is the directory a Tessl consumer installs a package
// tree under, followed by that package's `<workspace>/<package>` identity.
const tesslInstalledRoot = ".tessl/plugins/"

// SkillRebase maps one skill's package-root tree to the native directory that
// replaces it.
type SkillRebase struct {
	SourceRoot string
	NativeRoot string
}

// SkillReferences is every reference one package's files may make to its own
// bundled content, resolved against one adapter's installed skills root.
//
// Identities holds the Tessl identities this package is evidenced to have
// been installed under — its recorded `source.tesslIdentity` and its own
// package name. A `.tessl/plugins/<identity>/...` reference naming anything
// else addresses a different package and is never rewritten: the identity is
// the only evidence separating "my own tree, before ACR owned it" from
// "somebody else's tree that happens to expose the same skill path".
type SkillReferences struct {
	Rebases    []SkillRebase
	Identities []string
}

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

// RebaseSkillReferences maps one skill tree's supported references to its
// installed native directory.
//
// This is the original two-root entry point, kept so an adapter compiled
// against boundary version 1 keeps compiling and behaving as documented. A
// caller that needs a whole package's references — cross-skill paths, and the
// legacy Tessl form, which needs an evidenced identity this signature cannot
// carry — uses RebasePackageReferences instead.
func RebaseSkillReferences(content []byte, sourceRoot, nativeRoot string) []byte {
	return RebasePackageReferences(content, SkillReferences{
		Rebases: []SkillRebase{{SourceRoot: strings.TrimSuffix(sourceRoot, "/"), NativeRoot: strings.TrimSuffix(nativeRoot, "/")}},
	})
}

// RebasePackageReferences maps a package's own bundled-content references to
// their installed native directories while preserving every other byte.
//
// A reference is rewritten only where it begins a token. A token begins at
// the start of the content, after whitespace, after one of the three quote
// characters, after a Markdown destination's `](`, and after an opening
// bracket that itself begins a token. That last clause is what separates
// `[helper](skills/…)` and `(skills/…)` from `archive(skills/…)` and
// `https://host/(skills/…)`: a bracket embedded in a word continues that
// word, so the URL and the filename stay whole. A leading escape and one
// shell assignment prefix (`NAME=` or `--flag=`) are carried through ahead of
// the reference.
//
// Two forms are supported: the package-root path `<sourceRoot>/...`, and
// `.tessl/plugins/<identity>/` followed by that same package-root path for an
// identity the package is evidenced to own. A reference outside those forms
// is preserved unchanged rather than rewritten into a path that resolves
// nowhere; see docs/adapters.md for the boundary.
func RebasePackageReferences(content []byte, references SkillReferences) []byte {
	if len(references.Rebases) == 0 {
		return append([]byte(nil), content...)
	}
	result := make([]byte, 0, len(content))
	for index := 0; index < len(content); {
		if !isReferenceStart(content, index) {
			result = append(result, content[index])
			index++
			continue
		}
		carried, width, native, matched := references.match(content[index:])
		if !matched {
			result = append(result, content[index])
			index++
			continue
		}
		result = append(result, content[index:index+carried]...)
		result = append(result, native...)
		index += carried + width
	}
	return result
}

// isReferenceStart reports whether index begins a token a reference may open.
//
// An opening bracket qualifies only when it begins a token itself, so a
// bracket that continues a word — a URL path, a query value, a filename —
// never starts one. `](` is recognized on its own because it is how a
// Markdown destination opens.
func isReferenceStart(content []byte, index int) bool {
	for {
		if index == 0 {
			return true
		}
		previous := content[index-1]
		switch {
		case isTokenOpener(previous):
			return true
		case previous == '(' && index >= 2 && content[index-2] == ']':
			return true
		case isOpeningBracket(previous):
			index--
		default:
			return false
		}
	}
}

// isTokenOpener reports whether b unconditionally ends a token. These are the
// bytes a supported reference cannot contain and that prose, Markdown code
// spans and shell or program string literals use to bound one.
func isTokenOpener(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	case '"', '\'', '`':
		return true
	}
	return false
}

func isOpeningBracket(b byte) bool {
	switch b {
	case '(', '[', '<', '{':
		return true
	}
	return false
}

// match reports the supported reference the content at a token start opens
// with: how many leading bytes are carried through unchanged (an escape and a
// shell assignment prefix), how many further bytes the matched form spans,
// and the native prefix that replaces them.
func (references SkillReferences) match(rest []byte) (carried, width int, native string, matched bool) {
	if len(rest) != 0 && rest[0] == '\\' {
		carried = 1
	}
	carried += assignmentWidth(rest[carried:])
	rest = rest[carried:]
	if width, native, matched = references.matchPackageRoot(rest); matched {
		return carried, width, native, true
	}
	for _, identity := range references.Identities {
		installed := tesslInstalledRoot + identity + "/"
		if !bytes.HasPrefix(rest, []byte(installed)) {
			continue
		}
		if width, native, matched = references.matchPackageRoot(rest[len(installed):]); matched {
			return carried, len(installed) + width, native, true
		}
	}
	return 0, 0, "", false
}

func (references SkillReferences) matchPackageRoot(rest []byte) (int, string, bool) {
	for _, rebase := range references.Rebases {
		prefix := rebase.SourceRoot + "/"
		if bytes.HasPrefix(rest, []byte(prefix)) {
			return len(prefix), rebase.NativeRoot + "/", true
		}
	}
	return 0, "", false
}

// assignmentWidth reports the width of a leading shell assignment prefix —
// `NAME=` or `-f=` / `--flag=` — or zero when the content does not open with
// one. The name is read as a bounded run of assignment-name bytes, so a URL
// query such as `https://host/?next=` never qualifies: the run stops at the
// colon, long before its `=`.
func assignmentWidth(rest []byte) int {
	end := 0
	for end < len(rest) && isAssignmentNameByte(rest[end]) {
		end++
	}
	if end == 0 || end == len(rest) || rest[end] != '=' {
		return 0
	}
	if !isEnvironmentName(rest[:end]) && !isOptionName(rest[:end]) {
		return 0
	}
	return end + 1
}

func isAssignmentNameByte(b byte) bool {
	return b == '_' || b == '-' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func isEnvironmentName(head []byte) bool {
	for index, b := range head {
		alphabetic := b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
		if alphabetic || index != 0 && b >= '0' && b <= '9' {
			continue
		}
		return false
	}
	return true
}

func isOptionName(head []byte) bool {
	name := bytes.TrimPrefix(head, []byte("--"))
	if len(name) == len(head) {
		name = bytes.TrimPrefix(head, []byte("-"))
	}
	if len(name) == len(head) || len(name) == 0 {
		return false
	}
	for index, b := range name {
		alphanumeric := b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
		if alphanumeric || index != 0 && b == '-' {
			continue
		}
		return false
	}
	return true
}
