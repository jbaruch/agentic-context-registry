// Package packageref scans owned package references without interpreting code.
package packageref

import (
	"bytes"
	"strings"
)

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
//     start and has a closing partner. The whole interior belongs to that
//     argument, so whitespace, a further quote and an escaped quote inside it
//     all separate nothing; only its first interior position may begin a
//     reference, and being eligible there does not make that position outside
//     the argument. The partner is that argument's terminator and is consumed
//     as one, so an argument ending in whitespace closes rather than reopening
//     through the next quote in the document. A quote with no partner opens
//     nothing, so an odd quote cannot swallow the rest of the file.
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
	argumentFrom  int
	argumentTo    int
	argumentAt    int
	terminatorAt  int
	argumentEmpty bool
	labels        []bool
	destinationAt int
}

func newReferenceScanner(content []byte) *referenceScanner {
	return &referenceScanner{content: content, fresh: true, blankLine: true, argumentAt: -1, terminatorAt: -1, destinationAt: -1}
}

// atReferenceStart reports whether a reference may begin at the current
// offset. Inside an argument only its first interior position qualifies; the
// rest of the interior is one opaque unit.
func (scanner *referenceScanner) atReferenceStart() bool {
	if scanner.inArgument(scanner.index) && scanner.index != scanner.argumentFrom {
		return false
	}
	return scanner.fresh || scanner.index == scanner.destinationAt
}

// inArgument reports whether position lies in the interior of the argument
// the scanner currently has open, first interior position included. That
// position may begin a reference, which is a separate question this predicate
// does not answer: reading it as "outside the argument" is what let a quote
// written there open a second argument and expose the first one's interior.
func (scanner *referenceScanner) inArgument(position int) bool {
	return scanner.argumentTo != 0 && position >= scanner.argumentFrom && position < scanner.argumentTo
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
	inArgument := scanner.inArgument(position)
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
	case fresh && !inArgument && opensEmphasizedCodeSpan(scanner.content[position:]):
		scanner.fresh = true
	case current == '`' && fresh:
		scanner.fresh = true
	case (current == '"' || current == '\'') && position == scanner.terminatorAt:
		scanner.closeArgument(fresh)
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
	if scanner.argumentTo != 0 && scanner.index >= scanner.argumentTo {
		scanner.argumentFrom, scanner.argumentTo = 0, 0
	}
}

// opensEmphasizedCodeSpan recognizes ordinary emphasis directly before a
// Markdown code span. It is consulted only at a token start outside an
// argument, so markers embedded in filenames, URLs or quoted values stay
// opaque. Requiring the backtick keeps bare shell glob/filename prefixes
// from becoming reference boundaries.
func opensEmphasizedCodeSpan(rest []byte) bool {
	if rest[0] != '*' && rest[0] != '_' {
		return false
	}
	end := 1
	for end < len(rest) && end < 4 && rest[end] == rest[0] {
		end++
	}
	return end <= 3 && end < len(rest) && rest[end] == '`'
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

// openArgument records the interior of a quoted argument, the first interior
// position included, and only that position may begin a reference. A quote
// with no closing partner opens no argument at all.
//
// Inside a double-quoted argument a backslash escapes the byte after it, so
// `"archive \" skills/…"` is one argument rather than two: taking the escaped
// quote for the terminator would end the argument early and rebase the
// interior of an unrelated one. A single-quoted argument has no escape, which
// is the shell's own rule.
func (scanner *referenceScanner) openArgument(quote byte) {
	scanner.terminatorAt = -1
	interior := scanner.content[scanner.index:]
	for offset := 0; offset < len(interior); offset++ {
		if quote == '"' && interior[offset] == '\\' {
			offset++
			continue
		}
		if interior[offset] == quote {
			scanner.argumentFrom, scanner.argumentTo = scanner.index, scanner.index+offset
			scanner.terminatorAt, scanner.argumentEmpty = scanner.index+offset, offset == 0
			return
		}
	}
}

// closeArgument consumes the quote an open argument was matched against. That
// quote is the argument's known terminator, so it opens nothing — whatever the
// last interior byte was. Reading it as a fresh opener because the argument
// ended in whitespace paired it with the next quote in the document, and every
// supported reference between the two went unrebased: `sh skills/…/check.sh
// "label "` followed by a second `sh skills/…/check.sh "last"` realized one
// installed path and one package-root path, and running the realized pair
// exited 127 on the second command.
//
// The word carrying the argument continues past the terminator, because the
// shell removes the quotes and joins what is left: `"archive "skills/…` is one
// word whose interior space is quoted, not an argument followed by a path. So
// the position after a terminator begins a reference only when the argument
// was empty and contributed nothing to that word, which is what `""skills/…`
// writes and what a shell resolves to the path alone.
func (scanner *referenceScanner) closeArgument(fresh bool) {
	scanner.fresh = fresh && scanner.argumentEmpty
	scanner.terminatorAt = -1
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
