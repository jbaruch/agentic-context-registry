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
)

func TestRebaseSkillReferences(t *testing.T) {
	t.Parallel()

	content := []byte("Run `skills/review-change/scripts/check.sh`. Keep `docs/check.sh`.\n")
	got := RebaseSkillReferences(content, rebaseSourceRoot, rebaseNativeRoot)
	want := "Run `.codex/skills/acr__example__all-agents__review-change/scripts/check.sh`. Keep `docs/check.sh`.\n"
	if string(got) != want {
		t.Fatalf("rebased skill = %q, want %q", got, want)
	}
}

// TestRebaseSkillReferenceBoundaries states, case by case, which references
// address the installed skill tree and which do not. Issue #92: an interior
// match inside a Tessl-installed path was rewritten as if it were a reference
// of its own, producing a path that resolves nowhere.
func TestRebaseSkillReferenceBoundaries(t *testing.T) {
	t.Parallel()

	rebased := rebaseNativeRoot + "/scripts/check.sh"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "package root at content start", in: "skills/review-change/scripts/check.sh", want: rebased},
		{name: "package root after a delimiter", in: "Run `skills/review-change/scripts/check.sh` now", want: "Run `" + rebased + "` now"},
		{name: "package root twice on one line", in: "skills/review-change/a and skills/review-change/b", want: rebaseNativeRoot + "/a and " + rebaseNativeRoot + "/b"},
		{
			name: "legacy Tessl-installed path is replaced whole",
			in:   ".tessl/plugins/example/legacy-name/skills/review-change/scripts/check.sh",
			want: rebased,
		},
		{
			// The Tessl identity is matched structurally: a package
			// republished under another name still carries its old identity
			// in the references its own files ship with.
			name: "legacy Tessl identity need not match the package source",
			in:   ".tessl/plugins/other-workspace/other-name/skills/review-change/scripts/check.sh",
			want: rebased,
		},
		{name: "url segment", in: "https://example.com/skills/review-change/scripts/check.sh", want: "https://example.com/skills/review-change/scripts/check.sh"},
		{name: "interior directory segment", in: "vendor/skills/review-change/scripts/check.sh", want: "vendor/skills/review-change/scripts/check.sh"},
		{name: "longer leading directory name", in: "myskills/review-change/scripts/check.sh", want: "myskills/review-change/scripts/check.sh"},
		{name: "sibling directory name", in: "skills/review-change-archive/scripts/check.sh", want: "skills/review-change-archive/scripts/check.sh"},
		{name: "absolute path", in: "/home/user/skills/review-change/scripts/check.sh", want: "/home/user/skills/review-change/scripts/check.sh"},
		{name: "another package tree under the Tessl root", in: ".tessl/plugins/example/legacy-name/skills/other/check.sh", want: ".tessl/plugins/example/legacy-name/skills/other/check.sh"},
		{name: "Tessl path missing the package segment", in: ".tessl/plugins/example/skills/review-change/scripts/check.sh", want: ".tessl/plugins/example/skills/review-change/scripts/check.sh"},
		{name: "Tessl path with a dot segment", in: ".tessl/plugins/./legacy-name/skills/review-change/scripts/check.sh", want: ".tessl/plugins/./legacy-name/skills/review-change/scripts/check.sh"},
		{name: "skill root without a trailing separator", in: "skills/review-change", want: "skills/review-change"},
		{name: "unrelated content", in: "docs/check.sh and skills/other/check.sh", want: "docs/check.sh and skills/other/check.sh"},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got := RebaseSkillReferences([]byte(testCase.in), rebaseSourceRoot, rebaseNativeRoot)
			if string(got) != testCase.want {
				t.Fatalf("rebased %q = %q, want %q", testCase.in, got, testCase.want)
			}
		})
	}
}

func TestSkillRebasesOrderNestedTreesFirst(t *testing.T) {
	t.Parallel()

	pkg := Package{
		Source: "github:example/nested",
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Skills: []manifest.SkillArtifact{
			{ID: "outer", Path: "skills/outer"},
			{ID: "inner", Path: "skills/outer/inner"},
		}}},
	}
	rebases, err := SkillRebases(pkg, ".claude/skills")
	if err != nil {
		t.Fatal(err)
	}
	want := []SkillRebase{
		{SourceRoot: "skills/outer/inner", NativeRoot: ".claude/skills/acr__example__nested__inner"},
		{SourceRoot: "skills/outer", NativeRoot: ".claude/skills/acr__example__nested__outer"},
	}
	if !reflect.DeepEqual(rebases, want) {
		t.Fatalf("rebases = %#v, want %#v", rebases, want)
	}

	content := []byte("skills/outer/inner/run.sh and skills/outer/run.sh")
	for _, rebase := range rebases {
		content = RebaseSkillReferences(content, rebase.SourceRoot, rebase.NativeRoot)
	}
	wantContent := ".claude/skills/acr__example__nested__inner/run.sh and .claude/skills/acr__example__nested__outer/run.sh"
	if string(content) != wantContent {
		t.Fatalf("rebased nested trees = %q, want %q", content, wantContent)
	}
}

func TestSkillRebasesRejectsAnUnparsableSource(t *testing.T) {
	t.Parallel()

	pkg := Package{
		Source:   "example/no-scheme",
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Skills: []manifest.SkillArtifact{{ID: "outer", Path: "skills/outer"}}}},
	}
	if _, err := SkillRebases(pkg, ".claude/skills"); err == nil {
		t.Fatal("SkillRebases accepted a source without a scheme")
	}
}
