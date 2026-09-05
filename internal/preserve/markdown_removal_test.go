package preserve

import (
	"bytes"
	"context"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// TestMarkdownFinalRemovalTransitionsToUnmanaged covers issue #55: the final
// removal from a shared Markdown target kept shared ownership on its
// candidate, and the realization planner refuses that transition, so uninstall
// failed on AGENTS.md and CLAUDE.md.
func TestMarkdownFinalRemovalTransitionsToUnmanaged(t *testing.T) {
	t.Parallel()

	initial := compileMissingMarkdown(t, testMarkdownInsertion("rule-a", "managed\n"))
	prefix := []byte("# user\nkeep\n")
	suffix := []byte("tail\n")
	content := append(append(append([]byte(nil), prefix...), initial.Candidate.Content...), suffix...)
	previous := markdownTarget("AGENTS.md", content, realize.OwnershipShared, initial.Managed)
	observed := observedFile("AGENTS.md", content)

	removed, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target: adapter.SharedTarget{Path: "AGENTS.md", Observed: &observed, Previous: &previous},
	})
	if err != nil {
		t.Fatalf("CompileMarkdown() error = %v", err)
	}
	if removed.Action != realize.ActionRemove || removed.Candidate == nil {
		t.Fatalf("removal = %#v, want a final removal with a candidate", removed)
	}
	if removed.Candidate.Ownership != realize.OwnershipUnmanaged {
		t.Errorf("candidate ownership = %q, want %q", removed.Candidate.Ownership, realize.OwnershipUnmanaged)
	}
	want := append(append([]byte(nil), prefix...), suffix...)
	if !bytes.Equal(removed.Candidate.Content, want) {
		t.Errorf("candidate content = %q, want the unmanaged bytes %q", removed.Candidate.Content, want)
	}
	if observed.Mode.Perm() != removed.Candidate.Mode.Perm() {
		t.Errorf("candidate mode = %v, want the observed %v", removed.Candidate.Mode.Perm(), observed.Mode.Perm())
	}
}

// TestMarkdownRemovalKeepsSharedOwnershipWhileBlocksRemain keeps the
// transition scoped to the final removal.
func TestMarkdownRemovalKeepsSharedOwnershipWhileBlocksRemain(t *testing.T) {
	t.Parallel()

	first := testMarkdownInsertion("rule-a", "managed a\n")
	second := testMarkdownInsertion("rule-b", "managed b\n")
	initial, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target:  adapter.SharedTarget{Path: "AGENTS.md"},
		Desired: []adapter.MarkdownInsertion{first, second},
	})
	if err != nil {
		t.Fatalf("CompileMarkdown() error = %v", err)
	}
	content := append([]byte("# user\n"), initial.Candidate.Content...)
	previous := markdownTarget("AGENTS.md", content, realize.OwnershipShared, initial.Managed)
	observed := observedFile("AGENTS.md", content)

	partial, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target:  adapter.SharedTarget{Path: "AGENTS.md", Observed: &observed, Previous: &previous},
		Desired: []adapter.MarkdownInsertion{first},
	})
	if err != nil {
		t.Fatalf("CompileMarkdown() error = %v", err)
	}
	if partial.Action != realize.ActionEnsure || partial.Candidate == nil {
		t.Fatalf("partial removal = %#v, want an ensure", partial)
	}
	if partial.Candidate.Ownership != realize.OwnershipShared {
		t.Errorf("candidate ownership = %q, want %q while a managed block remains", partial.Candidate.Ownership, realize.OwnershipShared)
	}
	if bytes.Contains(partial.Candidate.Content, []byte("managed b")) {
		t.Errorf("candidate = %q, want the second block gone", partial.Candidate.Content)
	}
}
