package adapter

import "testing"

func TestRebaseSkillReferences(t *testing.T) {
	t.Parallel()

	content := []byte("Run `skills/review-change/scripts/check.sh`. Keep `docs/check.sh`.\n")
	got := RebaseSkillReferences(content, "skills/review-change", ".codex/skills/acr__example__all-agents__review-change")
	want := "Run `.codex/skills/acr__example__all-agents__review-change/scripts/check.sh`. Keep `docs/check.sh`.\n"
	if string(got) != want {
		t.Fatalf("rebased skill = %q, want %q", got, want)
	}
}
