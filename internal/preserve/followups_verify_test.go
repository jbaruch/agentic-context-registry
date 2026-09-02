package preserve

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestFollowupsNormalizedFragmentAcceptedIffCompilerProducedIt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		observed []byte
		hostile  []byte
	}{
		{
			name:     "trailing newline added after snapshot",
			observed: []byte("user text"),
			hostile:  []byte("user text\n"),
		},
		{
			name:     "CRLF unmanaged rewritten as LF",
			observed: []byte("# user\r\nkeep me\r\n"),
			hostile:  []byte("# user\nkeep me\n"),
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if bytes.Contains(test.observed, test.hostile) {
				t.Fatalf("hostile fragment %q already occurs in observed %q", test.hostile, test.observed)
			}

			compiled := compileObservedMarkdown(t, "AGENTS.md", test.observed, testMarkdownInsertion("rule-a", "managed body\n"))
			if len(compiled.Proof.PreservedContent) != 1 || !bytes.Equal(compiled.Proof.PreservedContent[0], test.observed) {
				t.Fatalf("compiler fragment = %q, want the original observed bytes %q", compiled.Proof.PreservedContent, test.observed)
			}

			root := t.TempDir()
			if err := os.WriteFile(root+"/AGENTS.md", test.observed, 0o644); err != nil {
				t.Fatal(err)
			}
			accepted := intentFromCompilation(t, "AGENTS.md", compiled)
			plan, err := realize.NewEngine().Run(root, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, []realize.Intent{accepted}, realize.ModeDryRun, nil)
			if err != nil || plan.HasConflicts() {
				t.Fatalf("compiler-produced fragment plan = %#v, %v; want accepted", plan, err)
			}

			hostile := accepted
			hostile.PreservedContent = [][]byte{append([]byte(nil), test.hostile...)}
			if !bytes.Contains(hostile.Content, test.hostile) {
				hostile.Content = append(append([]byte(nil), test.hostile...), hostile.Content...)
			}
			rejected, err := realize.NewEngine().Run(root, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, []realize.Intent{hostile}, realize.ModeDryRun, nil)
			var conflict *realize.ConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("normalized-only fragment plan = %#v, %v; want a conflict", rejected, err)
			}
			found := false
			for _, operation := range rejected.Operations {
				if operation.Kind == realize.OperationConflict && bytes.Contains([]byte(operation.Reason), []byte("preserved unmanaged content")) {
					found = true
				}
			}
			if !found {
				t.Fatalf("conflicts = %#v, want observed-fragment rejection", rejected.Operations)
			}
		})
	}
}

func intentFromCompilation(t *testing.T, path string, compiled adapter.SharedCompilation) realize.Intent {
	t.Helper()
	entries := make([]realize.Entry, 0, len(compiled.Managed))
	for _, result := range compiled.Managed {
		entries = append(entries, realize.Entry{
			Source: result.Owner.Source, ArtifactID: result.Owner.ArtifactID, ArtifactKind: result.Kind,
			SourcePath: result.Owner.SourcePath, Adapter: "test-adapter", AdapterVersion: "1.0.0",
			ManagedHash: result.ManagedHash,
		})
	}
	intent := realize.Intent{
		Action: compiled.Action, Path: path, Entries: entries,
		ObservedHash: compiled.Proof.ObservedHash, ManagedIntact: compiled.Proof.ManagedIntact,
		PreservedContent: compiled.Proof.PreservedContent,
	}
	if compiled.Candidate != nil {
		intent.Content = compiled.Candidate.Content
		intent.Mode = uint32(compiled.Candidate.Mode.Perm())
		intent.Ownership = compiled.Candidate.Ownership
	}
	return intent
}
