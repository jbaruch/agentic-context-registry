package adapter

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// NEW-2 (reviewer): compileOutputs discarded the caller's context in favor
// of context.Background() when calling into a SharedCompiler, making
// cancellation ineffective, and intentFromCompilation dropped every
// SharedCompilation.Notices value, making #6's required
// shared_file_requires_commit promotion notice impossible to surface.

// cancellationAwareCompiler returns ctx.Err() verbatim instead of compiling,
// so a test can prove whichever context reached the compiler.
type cancellationAwareCompiler struct{}

func (cancellationAwareCompiler) CompileMarkdown(ctx context.Context, _ MarkdownCompileRequest) (SharedCompilation, error) {
	if err := ctx.Err(); err != nil {
		return SharedCompilation{}, err
	}
	return SharedCompilation{}, errors.New("cancellationAwareCompiler: context was not cancelled")
}

func (cancellationAwareCompiler) CompileConfig(ctx context.Context, _ ConfigCompileRequest) (SharedCompilation, error) {
	if err := ctx.Err(); err != nil {
		return SharedCompilation{}, err
	}
	return SharedCompilation{}, errors.New("cancellationAwareCompiler: context was not cancelled")
}

func TestCompileOutputsPropagatesCallerContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sources := []adapterRender{{
		Descriptor: testDescriptor("fixture", "1.0.0"),
		Outputs:    []Output{{Target: "AGENTS.md", Kind: OutputMarkdownInclude, Mode: 0o644, Markdown: []MarkdownInsertion{{BlockID: "a", Body: []byte("b")}}}},
	}}
	_, err := compileOutputs(ctx, mapSnapshot{}, realize.Ledger{}, cancellationAwareCompiler{}, sources)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("compileOutputs() error = %v, want context.Canceled propagated from the compiler, proving compileOutputs threaded the caller's ctx instead of context.Background()", err)
	}
}

func TestCoordinatorRealizePropagatesCallerContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	owner := OwnerRef{Source: "github:owner/a", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}
	coordinator, err := NewCoordinator(cancellationAwareCompiler{}, markdownStub("adapter-a", owner, "block-a", "body\n"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	intents, err := coordinator.Realize(ctx, NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
	if !errors.Is(err, context.Canceled) || len(intents) != 0 {
		t.Fatalf("Realize() = %#v, %v, want context.Canceled propagated through Coordinator.Realize", intents, err)
	}
}

// noticingCompiler wraps the package's normal reconciling behavior for
// Markdown, additionally returning a shared_file_requires_commit Notice
// whenever it produces a shared (not generated-only) candidate — the same
// shape #6's real promotion path is expected to use.
type noticingCompiler struct {
	reconcilingCompiler
}

const codeSharedFileRequiresCommit = "shared_file_requires_commit"

func (c noticingCompiler) CompileMarkdown(ctx context.Context, request MarkdownCompileRequest) (SharedCompilation, error) {
	compilation, err := c.reconcilingCompiler.CompileMarkdown(ctx, request)
	if err != nil {
		return SharedCompilation{}, err
	}
	if compilation.Candidate != nil && compilation.Candidate.Ownership == realize.OwnershipShared {
		compilation.Notices = append(compilation.Notices, Notice{
			Code: codeSharedFileRequiresCommit, Path: request.Target.Path,
			Message: "commit the now-authoritative shared file; ACR never stages it",
		})
	}
	return compilation, nil
}

func TestCoordinatorRealizeWithNoticesSurfacesSharedFileRequiresCommit(t *testing.T) {
	t.Parallel()

	owner := OwnerRef{Source: "github:owner/a", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}
	observed := "user preface\n"
	root := t.TempDir()
	writeTestFile(t, root, "AGENTS.md", observed)

	coordinator, err := NewCoordinator(noticingCompiler{}, markdownStub("adapter-a", owner, "block-a", "managed\n"))
	if err != nil {
		t.Fatal(err)
	}
	intents, notices, err := coordinator.RealizeWithNotices(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
	if err != nil || len(intents) != 1 || intents[0].Ownership != realize.OwnershipShared {
		t.Fatalf("RealizeWithNotices() = %#v, %v, want one shared intent", intents, err)
	}
	found := false
	for _, notice := range notices {
		if notice.Code == codeSharedFileRequiresCommit && notice.Path == "AGENTS.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("notices = %#v, want a %s notice for AGENTS.md", notices, codeSharedFileRequiresCommit)
	}

	// Realize (without notices) still compiles the same intents; it simply
	// drops the notice, it does not lose the underlying result.
	plainIntents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
	if err != nil || len(plainIntents) != 1 {
		t.Fatalf("Realize() = %#v, %v", plainIntents, err)
	}
}
