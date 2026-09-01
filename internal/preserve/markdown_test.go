package preserve

import (
	"bytes"
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestSharedCompilerCreatesGeneratedOnlyMarkdown(t *testing.T) {
	t.Parallel()
	insertion := testMarkdownInsertion("rule-a", "managed\n")
	got, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target: adapter.SharedTarget{Path: "CLAUDE.md"}, Desired: []adapter.MarkdownInsertion{insertion},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != realize.ActionEnsure || got.Candidate == nil || got.Candidate.Ownership != realize.OwnershipGenerated || len(got.Proof.PreservedContent) != 0 {
		t.Fatalf("compilation = %#v", got)
	}
	if !bytes.Contains(got.Candidate.Content, []byte("source=github:owner/pkg artifact=rule-a adapter=test-adapter prefix=none")) || len(got.Managed) != 1 {
		t.Fatalf("candidate = %q, managed = %#v", got.Candidate.Content, got.Managed)
	}
}

func TestMarkdownSplicesWithoutTouchingUnmanagedBytes(t *testing.T) {
	t.Parallel()
	initial := compileMissingMarkdown(t, testMarkdownInsertion("rule-a", "version one\n"))
	prefix := []byte{0xef, 0xbb, 0xbf, '#', ' ', 'u', 's', 'e', 'r', '\r', '\n'}
	suffix := []byte("mixed\nend-without-newline")
	content := append(append(append([]byte(nil), prefix...), initial.Candidate.Content...), suffix...)
	previous := markdownTarget("AGENTS.md", content, realize.OwnershipShared, initial.Managed)
	observed := observedFile("AGENTS.md", content)

	updated, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target:  adapter.SharedTarget{Path: "AGENTS.md", Observed: &observed, Previous: &previous},
		Desired: []adapter.MarkdownInsertion{testMarkdownInsertion("rule-a", "version two\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Candidate == nil || !bytes.HasPrefix(updated.Candidate.Content, prefix) || !bytes.HasSuffix(updated.Candidate.Content, suffix) {
		t.Fatalf("candidate did not retain prefix/suffix: %q", updated.Candidate.Content)
	}
	if len(updated.Proof.PreservedContent) != 2 || !bytes.Equal(updated.Proof.PreservedContent[0], prefix) || !bytes.Equal(updated.Proof.PreservedContent[1], suffix) {
		t.Fatalf("preservation proof = %q", updated.Proof.PreservedContent)
	}
	if !bytes.Contains(updated.Candidate.Content, []byte("version two")) || bytes.Contains(updated.Candidate.Content, []byte("version one")) {
		t.Fatalf("candidate = %q", updated.Candidate.Content)
	}
}

func TestMarkdownMissingFinalNewlineRoundTrip(t *testing.T) {
	t.Parallel()
	original := []byte("user text")
	observed := observedFile("notes.md", original)
	insertion := testMarkdownInsertion("rule-a", "managed\n")
	inserted, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target: adapter.SharedTarget{Path: "notes.md", Observed: &observed}, Desired: []adapter.MarkdownInsertion{insertion},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inserted.Candidate == nil || !bytes.Contains(inserted.Candidate.Content, []byte("prefix=lf")) {
		t.Fatalf("inserted candidate = %q", inserted.Candidate.Content)
	}
	previous := markdownTarget("notes.md", inserted.Candidate.Content, realize.OwnershipShared, inserted.Managed)
	withBlock := observedFile("notes.md", inserted.Candidate.Content)
	removed, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target: adapter.SharedTarget{Path: "notes.md", Observed: &withBlock, Previous: &previous},
	})
	if err != nil {
		t.Fatal(err)
	}
	if removed.Action != realize.ActionRemove || removed.Candidate == nil || !bytes.Equal(removed.Candidate.Content, original) {
		t.Fatalf("removed = %#v, content = %q", removed, removed.Candidate.Content)
	}
}

func TestMarkdownPreservesCRLFAndUsesItForNewBlock(t *testing.T) {
	t.Parallel()
	original := []byte("# user\r\nkeep\r\n")
	observed := observedFile("AGENTS.md", original)
	compiled, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target:  adapter.SharedTarget{Path: "AGENTS.md", Observed: &observed},
		Desired: []adapter.MarkdownInsertion{testMarkdownInsertion("rule-a", "managed\nsecond\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(compiled.Candidate.Content, original) {
		t.Fatalf("CRLF prefix changed: %q", compiled.Candidate.Content)
	}
	withoutCRLF := bytes.ReplaceAll(compiled.Candidate.Content, []byte("\r\n"), nil)
	if bytes.Contains(withoutCRLF, []byte{'\n'}) {
		t.Fatalf("candidate introduced bare LF: %q", compiled.Candidate.Content)
	}
}

func TestMarkdownTwoPackageBlocksAreStableAndIdempotent(t *testing.T) {
	t.Parallel()
	first := testMarkdownInsertion("rule-a", "first\n")
	second := testMarkdownInsertion("rule-b", "second\n")
	initial, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target: adapter.SharedTarget{Path: "AGENTS.md"}, Desired: []adapter.MarkdownInsertion{second, first},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Managed) != 2 || !bytes.Contains(initial.Candidate.Content, first.Body) || !bytes.Contains(initial.Candidate.Content, second.Body) {
		t.Fatalf("two-block compilation = %#v", initial)
	}
	previous := markdownTarget("AGENTS.md", initial.Candidate.Content, realize.OwnershipGenerated, initial.Managed)
	observed := observedFile("AGENTS.md", initial.Candidate.Content)
	repeated, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target:  adapter.SharedTarget{Path: "AGENTS.md", Observed: &observed, Previous: &previous},
		Desired: []adapter.MarkdownInsertion{first, second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(repeated.Candidate.Content, initial.Candidate.Content) {
		t.Fatalf("idempotent candidate changed\nfirst: %q\nnext:  %q", initial.Candidate.Content, repeated.Candidate.Content)
	}
}

func TestMarkdownPromotionIsStickyAndReportsCommit(t *testing.T) {
	t.Parallel()
	insertion := testMarkdownInsertion("rule-a", "managed v1\n")
	initial := compileMissingMarkdown(t, insertion)
	previous := markdownTarget("AGENTS.md", initial.Candidate.Content, realize.OwnershipGenerated, initial.Managed)
	changed := append(append([]byte(nil), initial.Candidate.Content...), []byte("user appendix\n")...)
	observed := observedFile("AGENTS.md", changed)
	insertion.Body = []byte("managed v2\n")
	promoted, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target: adapter.SharedTarget{Path: "AGENTS.md", Observed: &observed, Previous: &previous}, Desired: []adapter.MarkdownInsertion{insertion},
	})
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Candidate.Ownership != realize.OwnershipShared || len(promoted.Notices) != 1 || promoted.Notices[0].Code != "shared_file_requires_commit" {
		t.Fatalf("promotion = %#v", promoted)
	}
	shared := markdownTarget("AGENTS.md", promoted.Candidate.Content, realize.OwnershipShared, promoted.Managed)
	sharedObserved := observedFile("AGENTS.md", promoted.Candidate.Content)
	sticky, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target: adapter.SharedTarget{Path: "AGENTS.md", Observed: &sharedObserved, Previous: &shared}, Desired: []adapter.MarkdownInsertion{insertion},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sticky.Candidate.Ownership != realize.OwnershipShared {
		t.Fatalf("sticky ownership = %q", sticky.Candidate.Ownership)
	}
}

func TestExplicitMarkdownDemotionRequiresNoUnmanagedBytes(t *testing.T) {
	t.Parallel()
	insertion := testMarkdownInsertion("rule-a", "managed\n")
	initial := compileMissingMarkdown(t, insertion)
	clean := markdownTarget("AGENTS.md", initial.Candidate.Content, realize.OwnershipShared, initial.Managed)
	cleanObserved := observedFile("AGENTS.md", initial.Candidate.Content)
	demoted, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target:  adapter.SharedTarget{Path: "AGENTS.md", Observed: &cleanObserved, Previous: &clean, ExplicitDemotion: true},
		Desired: []adapter.MarkdownInsertion{insertion},
	})
	if err != nil || demoted.Candidate.Ownership != realize.OwnershipGenerated {
		t.Fatalf("clean demotion = %#v, %v", demoted, err)
	}

	leftover := append(append([]byte(nil), initial.Candidate.Content...), []byte("user\n")...)
	leftoverTarget := markdownTarget("AGENTS.md", leftover, realize.OwnershipShared, initial.Managed)
	leftoverObserved := observedFile("AGENTS.md", leftover)
	_, err = NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target:  adapter.SharedTarget{Path: "AGENTS.md", Observed: &leftoverObserved, Previous: &leftoverTarget, ExplicitDemotion: true},
		Desired: []adapter.MarkdownInsertion{insertion},
	})
	if err == nil || !strings.Contains(err.Error(), CodeOwnershipConflict) {
		t.Fatalf("leftover demotion error = %v", err)
	}
}

func TestMarkdownRejectsEditedOwnedAndAmbiguousMarkersEvenWithForce(t *testing.T) {
	t.Parallel()
	insertion := testMarkdownInsertion("rule-a", "managed\n")
	initial := compileMissingMarkdown(t, insertion)
	previous := markdownTarget("AGENTS.md", initial.Candidate.Content, realize.OwnershipShared, initial.Managed)
	edited := bytes.Replace(initial.Candidate.Content, []byte("managed"), []byte("edited"), 1)
	editedObserved := observedFile("AGENTS.md", edited)
	_, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target:  adapter.SharedTarget{Path: "AGENTS.md", Observed: &editedObserved, Previous: &previous, Force: true},
		Desired: []adapter.MarkdownInsertion{insertion},
	})
	if err == nil || !strings.Contains(err.Error(), "was edited") {
		t.Fatalf("edited block error = %v", err)
	}

	ambiguous := []byte("<!-- acr:begin copied by user -->\n")
	ambiguousObserved := observedFile("AGENTS.md", ambiguous)
	_, err = NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target:  adapter.SharedTarget{Path: "AGENTS.md", Observed: &ambiguousObserved, Force: true},
		Desired: []adapter.MarkdownInsertion{insertion},
	})
	if err == nil || !strings.Contains(err.Error(), CodeMarkerConflict) {
		t.Fatalf("ambiguous marker error = %v", err)
	}
}

func TestExistingZeroByteMarkdownFailsClosed(t *testing.T) {
	t.Parallel()
	empty := observedFile("CLAUDE.md", nil)
	_, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target: adapter.SharedTarget{Path: "CLAUDE.md", Observed: &empty}, Desired: []adapter.MarkdownInsertion{testMarkdownInsertion("rule-a", "managed\n")},
	})
	if err == nil || !strings.Contains(err.Error(), "zero-byte") {
		t.Fatalf("zero-byte error = %v", err)
	}
}

func compileMissingMarkdown(t *testing.T, insertion adapter.MarkdownInsertion) adapter.SharedCompilation {
	t.Helper()
	compiled, err := NewCompiler().CompileMarkdown(context.Background(), adapter.MarkdownCompileRequest{
		Target: adapter.SharedTarget{Path: "AGENTS.md"}, Desired: []adapter.MarkdownInsertion{insertion},
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func testMarkdownInsertion(artifactID, body string) adapter.MarkdownInsertion {
	owner := adapter.OwnerRef{Source: "github:owner/pkg", ArtifactID: artifactID, SourcePath: "rules/" + artifactID + ".md", Kind: adapter.ArtifactRule}
	adapterID := "test-adapter"
	return adapter.MarkdownInsertion{
		Owner: owner, AdapterID: adapterID, BlockID: adapter.CanonicalMarkdownBlockID(owner, adapterID), Body: []byte(body),
	}
}

func markdownTarget(path string, content []byte, ownership realize.Ownership, managed []adapter.ManagedResult) realize.Target {
	entries := make([]realize.Entry, 0, len(managed))
	for _, result := range managed {
		entries = append(entries, realize.Entry{
			Source: result.Owner.Source, ArtifactID: result.Owner.ArtifactID, SourcePath: result.Owner.SourcePath,
			ArtifactKind: result.Kind, Adapter: "test-adapter", AdapterVersion: "1.0.0", ManagedHash: result.ManagedHash,
		})
	}
	return realize.Target{Path: path, Mode: 0o644, Ownership: ownership, OutputHash: hashBytes(content), Entries: entries}
}

func observedFile(path string, content []byte) adapter.ObservedFile {
	return adapter.ObservedFile{Path: path, Content: append([]byte(nil), content...), Mode: fs.FileMode(0o644), Hash: hashBytes(content)}
}
