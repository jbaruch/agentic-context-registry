package adapter

import (
	"reflect"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestVendorTreeIsNotAnAdapterTarget(t *testing.T) {
	t.Parallel()
	if err := realize.ValidateTargetPath(".agents/vendor/example/orphan/rule.md"); err == nil {
		t.Fatal("adapter accepted a target inside the vendor tree")
	}
}

func TestNativeNameCollidesAcrossSchemes(t *testing.T) {
	t.Parallel()

	github, err := NativeArtifactName("github:example/orphan", "review-change")
	if err != nil {
		t.Fatal(err)
	}
	vendor, err := NativeArtifactName("vendor:example/orphan", "review-change")
	if err != nil {
		t.Fatal(err)
	}
	if github != vendor || github != "acr__example__orphan__review-change" {
		t.Fatalf("native names = %q and %q, want one cross-scheme identity", github, vendor)
	}
}

const (
	rebaseSourceRoot = "skills/review-change"
	rebaseNativeRoot = ".codex/skills/acr__example__all-agents__review-change"
	rebaseIdentity   = "legacy-workspace/review-plugin"
)

func reviewChangeReferences() SkillReferences {
	return SkillReferences{
		Rebases:    []SkillRebase{{SourceRoot: rebaseSourceRoot, NativeRoot: rebaseNativeRoot}},
		Identities: []string{rebaseIdentity},
	}
}

func TestRebaseSkillReferences(t *testing.T) {
	t.Parallel()

	content := []byte("Run `skills/review-change/scripts/check.sh`. Keep `docs/check.sh`.\n")
	got := RebasePackageReferences(content, reviewChangeReferences())
	want := "Run `.codex/skills/acr__example__all-agents__review-change/scripts/check.sh`. Keep `docs/check.sh`.\n"
	if string(got) != want {
		t.Fatalf("rebased skill = %q, want %q", got, want)
	}
}

// TestRebaseSkillReferencesKeepsTheTwoRootEntryPoint holds the exported
// signature an adapter built against boundary version 1 compiles against.
// Changing it is a package compatibility break the boundary constant would
// have to carry, so the original call keeps working and the whole-package
// form is a separate name.
func TestRebaseSkillReferencesKeepsTheTwoRootEntryPoint(t *testing.T) {
	t.Parallel()

	content := []byte("Run `skills/review-change/scripts/check.sh`. Keep `docs/check.sh`.\n")
	got := RebaseSkillReferences(content, rebaseSourceRoot, rebaseNativeRoot)
	want := "Run `.codex/skills/acr__example__all-agents__review-change/scripts/check.sh`. Keep `docs/check.sh`.\n"
	if string(got) != want {
		t.Fatalf("rebased skill = %q, want %q", got, want)
	}
	if trailing := RebaseSkillReferences(content, rebaseSourceRoot+"/", rebaseNativeRoot+"/"); string(trailing) != want {
		t.Fatalf("rebased with trailing separators = %q, want %q", trailing, want)
	}
	// The two-root form carries no identity, so it never rewrites a legacy
	// Tessl path — the caller could not have supplied the evidence.
	legacy := []byte(".tessl/plugins/" + rebaseIdentity + "/skills/review-change/scripts/check.sh")
	if got := RebaseSkillReferences(legacy, rebaseSourceRoot, rebaseNativeRoot); string(got) != string(legacy) {
		t.Fatalf("two-root form rewrote a legacy path = %q, want %q", got, legacy)
	}
}

// TestRebaseSkillReferenceBoundaries states, case by case, which references
// address this package's installed tree and which do not.
//
// Issue #92 round 1: the first fix decided by the single byte in front of a
// match, which rewrote a URL query value, a filename with a punctuation or
// non-ASCII prefix, and — through an identity reader that accepted any two
// slash-separated runs — a quote and a newline between two program literals.
// A reference is a whole token now, and a legacy Tessl path is this package's
// only when it names an identity the package is evidenced to own.
//
// Issue #92 round 5: the first interior position of a quoted argument may
// begin a reference, and the round-4 scanner read that eligibility as being
// outside the argument, so a quote written there opened a second argument and
// the first one's interior rebased. The two questions are separate now — the
// argument is active from its first interior byte, and only a supported
// reference at that byte is rewritten.
func TestRebaseSkillReferenceBoundaries(t *testing.T) {
	t.Parallel()

	rebased := rebaseNativeRoot + "/scripts/check.sh"
	legacy := ".tessl/plugins/" + rebaseIdentity + "/skills/review-change/scripts/check.sh"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "package root at content start", in: "skills/review-change/scripts/check.sh", want: rebased},
		{name: "package root after a delimiter", in: "Run `skills/review-change/scripts/check.sh` now", want: "Run `" + rebased + "` now"},
		{name: "package root twice on one line", in: "skills/review-change/a and skills/review-change/b", want: rebaseNativeRoot + "/a and " + rebaseNativeRoot + "/b"},
		{name: "markdown destination", in: "[check](skills/review-change/scripts/check.sh)", want: "[check](" + rebased + ")"},
		{name: "bracket that opens a token", in: "See (skills/review-change/scripts/check.sh)", want: "See (" + rebased + ")"},
		{name: "quoted argument that is the reference", in: "\"skills/review-change/scripts/check.sh\"", want: "\"" + rebased + "\""},
		{name: "single-quoted argument that is the reference", in: "'skills/review-change/scripts/check.sh'", want: "'" + rebased + "'"},
		{name: "argument of a backquoted command", in: "`python3 skills/review-change/scripts/check.sh --info`", want: "`python3 " + rebased + " --info`"},
		{name: "unmatched quote opens no argument", in: "\"archive and skills/review-change/scripts/check.sh", want: "\"archive and " + rebased},
		{name: "nested opening brackets", in: "[(skills/review-change/scripts/check.sh)]", want: "[(" + rebased + ")]"},
		{name: "escaped leading punctuation", in: "\\" + legacy, want: "\\" + rebased},
		{name: "option assignment", in: "--helper=skills/review-change/scripts/check.sh", want: "--helper=" + rebased},
		{name: "short option assignment", in: "-h=skills/review-change/scripts/check.sh", want: "-h=" + rebased},
		{name: "environment assignment", in: "HELPER=skills/review-change/scripts/check.sh", want: "HELPER=" + rebased},
		{name: "quoted environment assignment", in: "HELPER=\"skills/review-change/scripts/check.sh\"", want: "HELPER=\"" + rebased + "\""},
		{name: "single-quoted environment assignment", in: "HELPER='skills/review-change/scripts/check.sh'", want: "HELPER='" + rebased + "'"},
		{name: "quoted option assignment", in: "cat --file=\"skills/review-change/scripts/check.sh\"", want: "cat --file=\"" + rebased + "\""},
		{name: "markdown label wrapped onto a second line", in: "[helper\nlabel](skills/review-change/scripts/check.sh)", want: "[helper\nlabel](" + rebased + ")"},
		{name: "reference at the first interior position", in: "\"skills/review-change/scripts/check.sh --leading\"", want: "\"" + rebased + " --leading\""},
		{name: "reference at the first interior position of an assignment value", in: "HELPER=\"skills/review-change/scripts/check.sh --leading\"", want: "HELPER=\"" + rebased + " --leading\""},
		{name: "own legacy identity", in: legacy, want: rebased},

		{name: "another identity with the same skill path", in: ".tessl/plugins/other-workspace/other-plugin/skills/review-change/scripts/check.sh", want: ".tessl/plugins/other-workspace/other-plugin/skills/review-change/scripts/check.sh"},
		{name: "another identity with another skill path", in: ".tessl/plugins/other-workspace/other-plugin/skills/other/check.sh", want: ".tessl/plugins/other-workspace/other-plugin/skills/other/check.sh"},
		{name: "url path segment", in: "https://example.com/skills/review-change/scripts/check.sh", want: "https://example.com/skills/review-change/scripts/check.sh"},
		{name: "parenthesis inside a url path", in: "https://example.com/(skills/review-change/scripts/check.sh)", want: "https://example.com/(skills/review-change/scripts/check.sh)"},
		{name: "bracket inside a url query", in: "https://example.com/?next=[skills/review-change/scripts/check.sh]", want: "https://example.com/?next=[skills/review-change/scripts/check.sh]"},
		{name: "parenthesis inside a filename", in: "archive(skills/review-change/scripts/check.sh)", want: "archive(skills/review-change/scripts/check.sh)"},
		{name: "label close inside a filename", in: "archive](skills/review-change/scripts/check.sh)", want: "archive](skills/review-change/scripts/check.sh)"},
		{name: "label close inside a url", in: "https://example.com/a](skills/review-change/scripts/check.sh)", want: "https://example.com/a](skills/review-change/scripts/check.sh)"},
		{name: "apostrophe inside a url", in: "https://example.com/a'skills/review-change/scripts/check.sh", want: "https://example.com/a'skills/review-change/scripts/check.sh"},
		{name: "apostrophe inside a filename", in: "archive'skills/review-change/scripts/check.sh", want: "archive'skills/review-change/scripts/check.sh"},
		{name: "double quote inside a filename", in: "archive\"skills/review-change/scripts/check.sh", want: "archive\"skills/review-change/scripts/check.sh"},
		{name: "whitespace inside a quoted argument", in: "\"archive\n skills/review-change/scripts/check.sh\"", want: "\"archive\n skills/review-change/scripts/check.sh\""},
		{name: "whitespace inside a single-quoted argument", in: "'archive skills/review-change/scripts/check.sh'", want: "'archive skills/review-change/scripts/check.sh'"},
		{name: "single quotes inside a double-quoted argument", in: "\"archive 'nested' skills/review-change/scripts/check.sh\"", want: "\"archive 'nested' skills/review-change/scripts/check.sh\""},
		{name: "double quotes inside a single-quoted argument", in: "'archive \"nested\" skills/review-change/scripts/check.sh'", want: "'archive \"nested\" skills/review-change/scripts/check.sh'"},
		{name: "escaped quote inside a quoted argument", in: "\"archive \\\" skills/review-change/scripts/check.sh\"", want: "\"archive \\\" skills/review-change/scripts/check.sh\""},
		{name: "quoted assignment value that is not the reference", in: "HELPER=\"archive skills/review-change/scripts/check.sh\"", want: "HELPER=\"archive skills/review-change/scripts/check.sh\""},
		{name: "single quote at the first interior position", in: "\"'archive' skills/review-change/scripts/check.sh\"", want: "\"'archive' skills/review-change/scripts/check.sh\""},
		{name: "double quote at the first interior position", in: "'\"archive\" skills/review-change/scripts/check.sh'", want: "'\"archive\" skills/review-change/scripts/check.sh'"},
		{name: "quote at the first interior position of an assignment value", in: "LABEL=\"'archive' skills/review-change/scripts/check.sh\"", want: "LABEL=\"'archive' skills/review-change/scripts/check.sh\""},
		{name: "quoted path at the first interior position", in: "\"'skills/review-change/scripts/check.sh' archive\"", want: "\"'skills/review-change/scripts/check.sh' archive\""},
		{name: "markdown label opened inside a word", in: "note[label](skills/review-change/scripts/check.sh)", want: "note[label](skills/review-change/scripts/check.sh)"},
		{name: "markdown label from a previous line", in: "[label]\narchive](skills/review-change/scripts/check.sh)", want: "[label]\narchive](skills/review-change/scripts/check.sh)"},
		{name: "markdown label across a blank line", in: "[label\n\narchive](skills/review-change/scripts/check.sh)", want: "[label\n\narchive](skills/review-change/scripts/check.sh)"},
		{name: "brace inside a filename", in: "archive{skills/review-change/scripts/check.sh}", want: "archive{skills/review-change/scripts/check.sh}"},
		{name: "angle bracket inside a filename", in: "archive<skills/review-change/scripts/check.sh>", want: "archive<skills/review-change/scripts/check.sh>"},
		{name: "url query value", in: "https://example.com/?next=skills/review-change/scripts/check.sh", want: "https://example.com/?next=skills/review-change/scripts/check.sh"},
		{name: "url fragment", in: "https://example.com/archive#skills/review-change/scripts/check.sh", want: "https://example.com/archive#skills/review-change/scripts/check.sh"},
		{name: "punctuation filename prefix", in: "archive#skills/review-change/scripts/check.sh", want: "archive#skills/review-change/scripts/check.sh"},
		{name: "non-ASCII filename prefix", in: "caf\u00e9skills/review-change/scripts/check.sh", want: "caf\u00e9skills/review-change/scripts/check.sh"},
		{name: "interior directory segment", in: "vendor/skills/review-change/scripts/check.sh", want: "vendor/skills/review-change/scripts/check.sh"},
		{name: "longer leading directory name", in: "myskills/review-change/scripts/check.sh", want: "myskills/review-change/scripts/check.sh"},
		{name: "sibling directory name", in: "skills/review-change-archive/scripts/check.sh", want: "skills/review-change-archive/scripts/check.sh"},
		{name: "absolute path", in: "/home/user/skills/review-change/scripts/check.sh", want: "/home/user/skills/review-change/scripts/check.sh"},
		{name: "explicit relative path", in: "./skills/review-change/scripts/check.sh", want: "./skills/review-change/scripts/check.sh"},
		{name: "identity missing the package segment", in: ".tessl/plugins/legacy-workspace/skills/review-change/scripts/check.sh", want: ".tessl/plugins/legacy-workspace/skills/review-change/scripts/check.sh"},
		{name: "whitespace inside the identity", in: ".tessl/plugins/legacy-workspace bad/review-plugin/skills/review-change/scripts/check.sh", want: ".tessl/plugins/legacy-workspace bad/review-plugin/skills/review-change/scripts/check.sh"},
		{
			name: "adjacent program literals",
			in:   "mount = (\".tessl/plugins/legacy-workspace/review-plugin\"\n         \"/skills/review-change/scripts/check.sh\")",
			want: "mount = (\".tessl/plugins/legacy-workspace/review-plugin\"\n         \"/skills/review-change/scripts/check.sh\")",
		},
		{
			name: "prose between identity slashes",
			in:   ".tessl/plugins/\nKEEP THIS PROSE\n/review-plugin/skills/review-change/scripts/check.sh",
			want: ".tessl/plugins/\nKEEP THIS PROSE\n/review-plugin/skills/review-change/scripts/check.sh",
		},
		{name: "skill root without a trailing separator", in: "skills/review-change", want: "skills/review-change"},
		{name: "unrelated content", in: "docs/check.sh and skills/other/check.sh", want: "docs/check.sh and skills/other/check.sh"},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got := RebasePackageReferences([]byte(testCase.in), reviewChangeReferences())
			if string(got) != testCase.want {
				t.Fatalf("rebased %q = %q, want %q", testCase.in, got, testCase.want)
			}
		})
	}
}

func TestPackageSkillReferencesOrderNestedTreesFirst(t *testing.T) {
	t.Parallel()

	pkg := Package{
		Source: "github:example/nested",
		Manifest: manifest.Manifest{
			Name:   "example/nested",
			Source: manifest.Source{TesslIdentity: "legacy/nested-plugin"},
			Artifacts: manifest.Artifacts{Skills: []manifest.SkillArtifact{
				{ID: "outer", Path: "skills/outer"},
				{ID: "inner", Path: "skills/outer/inner"},
			}},
		},
	}
	references, err := PackageSkillReferences(pkg, ".claude/skills")
	if err != nil {
		t.Fatal(err)
	}
	want := SkillReferences{
		Rebases: []SkillRebase{
			{SourceRoot: "skills/outer/inner", NativeRoot: ".claude/skills/acr__example__nested__inner"},
			{SourceRoot: "skills/outer", NativeRoot: ".claude/skills/acr__example__nested__outer"},
		},
		Identities: []string{"legacy/nested-plugin", "example/nested"},
	}
	if !reflect.DeepEqual(references, want) {
		t.Fatalf("references = %#v, want %#v", references, want)
	}

	content := []byte("skills/outer/inner/run.sh and skills/outer/run.sh and .tessl/plugins/example/nested/skills/outer/run.sh")
	got := string(RebasePackageReferences(content, references))
	wantContent := ".claude/skills/acr__example__nested__inner/run.sh and .claude/skills/acr__example__nested__outer/run.sh" +
		" and .claude/skills/acr__example__nested__outer/run.sh"
	if got != wantContent {
		t.Fatalf("rebased nested trees = %q, want %q", got, wantContent)
	}
}

// TestPackageSkillReferencesWithoutARecordedIdentity covers a package
// published before migration recorded the identity, or renamed away from it:
// its own package-root references still rebase, and a legacy path it can no
// longer prove it owns keeps its bytes.
func TestPackageSkillReferencesWithoutARecordedIdentity(t *testing.T) {
	t.Parallel()

	pkg := Package{
		Source: "github:example/renamed",
		Manifest: manifest.Manifest{
			Name:      "example/renamed",
			Artifacts: manifest.Artifacts{Skills: []manifest.SkillArtifact{{ID: "advocate", Path: "skills/advocate"}}},
		},
	}
	references, err := PackageSkillReferences(pkg, ".claude/skills")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(references.Identities, []string{"example/renamed"}) {
		t.Fatalf("identities = %#v, want the package name alone", references.Identities)
	}
	content := []byte("skills/advocate/run.sh and .tessl/plugins/legacy/advocate-plugin/skills/advocate/run.sh")
	got := string(RebasePackageReferences(content, references))
	want := ".claude/skills/acr__example__renamed__advocate/run.sh and .tessl/plugins/legacy/advocate-plugin/skills/advocate/run.sh"
	if got != want {
		t.Fatalf("rebased = %q, want %q", got, want)
	}
}

func TestPackageSkillReferencesRejectsAnUnparsableSource(t *testing.T) {
	t.Parallel()

	pkg := Package{
		Source:   "example/no-scheme",
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Skills: []manifest.SkillArtifact{{ID: "outer", Path: "skills/outer"}}}},
	}
	if _, err := PackageSkillReferences(pkg, ".claude/skills"); err == nil {
		t.Fatal("PackageSkillReferences accepted a source without a scheme")
	}
}
