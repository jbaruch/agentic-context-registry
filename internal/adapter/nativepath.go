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
// A reference is rewritten only where one may begin, which referenceScanner
// decides from the enclosing structure rather than from the byte in front of
// it. A leading escape and one shell assignment prefix (`NAME=` or `--flag=`)
// are carried through ahead of the reference.
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
	scanner := newReferenceScanner(content)
	for scanner.index < len(content) {
		if scanner.atReferenceStart() {
			if carried, width, native, matched := references.match(content[scanner.index:]); matched {
				result = append(result, content[scanner.index:scanner.index+carried]...)
				result = append(result, native...)
				scanner.consume(carried + width)
				continue
			}
		}
		result = append(result, content[scanner.index])
		scanner.consume(1)
	}
	return result
}

// referenceScanner walks content once and reports the offsets at which a
// reference may begin.
//
// One byte of lookbehind is not enough to answer that. `archive](skills/…)`
// is a filename whose `](` opens no link because nothing opened a label;
// `https://host/a'skills/…` is a URL whose apostrophe opens no argument
// because it sits inside a word; and `"archive\n skills/…"` is one quoted
// argument whose interior whitespace separates nothing. Each of those needs
// the structure the scanner is already inside, so the scanner carries it:
//
//   - inWord — the run of non-whitespace bytes currently being read. A quote
//     or a bracket inside one is part of that word, not an opener.
//   - an argument container — a `'` or `"` that opened where an argument may
//     start and has a closing partner. Only the position just after the
//     opening quote begins a reference; the rest of the argument is one
//     opaque unit, so whitespace, a further quote and an escaped quote inside
//     it all separate nothing. A quote with no partner opens nothing, so an
//     odd quote cannot swallow the rest of the file.
//   - a label stack — `[` openings, so `]` followed by `(` opens a Markdown
//     destination only where a label actually opened. Labels reset at a blank
//     line, which is the only thing a CommonMark link text cannot contain, so
//     a label whose text wraps still reaches its destination.
//
// An argument may start at a token start, and also directly after a shell
// assignment prefix — the `HELPER=` of `HELPER="skills/…"` and the `--file=`
// of `--file="skills/…"`, both of which are supported unquoted and so must
// stay supported around a quoted value.
//
// A backtick is a Markdown code span, not an argument: it opens a token, and
// whitespace inside it still separates tokens, so a backquoted command's
// arguments each begin a reference.
type referenceScanner struct {
	content       []byte
	index         int
	fresh         bool
	blankLine     bool
	opaqueFrom    int
	opaqueTo      int
	argumentAt    int
	labels        []bool
	destinationAt int
}

func newReferenceScanner(content []byte) *referenceScanner {
	return &referenceScanner{content: content, fresh: true, blankLine: true, argumentAt: -1, destinationAt: -1}
}

// atReferenceStart reports whether a reference may begin at the current
// offset.
func (scanner *referenceScanner) atReferenceStart() bool {
	if scanner.index >= scanner.opaqueFrom && scanner.index < scanner.opaqueTo {
		return false
	}
	return scanner.fresh || scanner.index == scanner.destinationAt
}

// consume advances past width bytes, updating the enclosing structure for
// each one. A matched reference is consumed the same way as ordinary bytes so
// the scanner's state stays exact.
func (scanner *referenceScanner) consume(width int) {
	for step := 0; step < width && scanner.index < len(scanner.content); step++ {
		scanner.step()
	}
}

func (scanner *referenceScanner) step() {
	position := scanner.index
	current := scanner.content[position]
	fresh := scanner.fresh
	inArgument := scanner.opaqueTo != 0 && position >= scanner.opaqueFrom && position < scanner.opaqueTo
	if fresh && !inArgument {
		scanner.markArgumentAfterAssignment(position)
	}
	if current != '\n' && !isReferenceSpace(current) {
		scanner.blankLine = false
	}
	scanner.index++
	switch {
	case current == '\n':
		if scanner.blankLine {
			scanner.labels = scanner.labels[:0]
		}
		scanner.blankLine = true
		scanner.fresh = true
	case isReferenceSpace(current):
		scanner.fresh = true
	case current == '`' && fresh:
		scanner.fresh = true
	case (current == '"' || current == '\'') && !inArgument && (fresh || position == scanner.argumentAt):
		scanner.fresh = true
		scanner.openArgument(current)
	case current == '[':
		scanner.labels = append(scanner.labels, fresh)
		scanner.fresh = fresh
	case current == '(' || current == '<' || current == '{':
		scanner.fresh = fresh
	case current == ']':
		scanner.closeLabel()
		scanner.fresh = false
	default:
		scanner.fresh = false
	}
	if scanner.opaqueTo != 0 && scanner.index >= scanner.opaqueTo {
		scanner.opaqueFrom, scanner.opaqueTo = 0, 0
	}
}

// markArgumentAfterAssignment records where a quoted argument may open inside
// the token starting at position. `HELPER=skills/…` and `--file=skills/…`
// are supported unquoted, and quoting that value is the same reference, so
// the quote after the assignment prefix opens an argument exactly as one at a
// token start does.
func (scanner *referenceScanner) markArgumentAfterAssignment(position int) {
	scanner.argumentAt = -1
	if width := assignmentWidth(scanner.content[position:]); width != 0 {
		scanner.argumentAt = position + width
	}
}

// openArgument marks the interior of a quoted argument opaque, leaving only
// the position just inside the quote able to begin a reference. A quote with
// no closing partner opens no argument at all.
//
// Inside a double-quoted argument a backslash escapes the byte after it, so
// `"archive \" skills/…"` is one argument rather than two: taking the escaped
// quote for the terminator would end the argument early and rebase the
// interior of an unrelated one. A single-quoted argument has no escape, which
// is the shell's own rule.
func (scanner *referenceScanner) openArgument(quote byte) {
	interior := scanner.content[scanner.index:]
	for offset := 0; offset < len(interior); offset++ {
		if quote == '"' && interior[offset] == '\\' {
			offset++
			continue
		}
		if interior[offset] == quote {
			scanner.opaqueFrom, scanner.opaqueTo = scanner.index+1, scanner.index+offset
			return
		}
	}
}

// closeLabel pops the innermost `[` and, when that label opened at a token
// start and a `(` follows immediately, opens a Markdown destination.
func (scanner *referenceScanner) closeLabel() {
	opened := false
	if depth := len(scanner.labels); depth != 0 {
		opened = scanner.labels[depth-1]
		scanner.labels = scanner.labels[:depth-1]
	}
	if opened && scanner.index < len(scanner.content) && scanner.content[scanner.index] == '(' {
		scanner.destinationAt = scanner.index + 1
	}
}

func isReferenceSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
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
