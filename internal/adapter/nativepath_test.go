package adapter

import (
	"testing"

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

func TestRebaseSkillReferences(t *testing.T) {
	t.Parallel()

	content := []byte("Run `skills/review-change/scripts/check.sh`. Keep `docs/check.sh`.\n")
	got := RebaseSkillReferences(content, "skills/review-change", ".codex/skills/acr__example__all-agents__review-change")
	want := "Run `.codex/skills/acr__example__all-agents__review-change/scripts/check.sh`. Keep `docs/check.sh`.\n"
	if string(got) != want {
		t.Fatalf("rebased skill = %q, want %q", got, want)
	}
}
