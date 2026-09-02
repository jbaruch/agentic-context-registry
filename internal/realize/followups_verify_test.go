package realize

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestFollowupsPlannerRejectsFragmentAbsentFromObservedSnapshot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	observed := "user-authored content\n"
	fabricated := []byte("fabricated preservation proof\n")
	writeFile(t, root, "AGENTS.md", observed)
	intent := Intent{
		Action: ActionEnsure, Path: "AGENTS.md",
		Content: []byte("managed replacement\n" + string(fabricated)), Mode: 0o644, Ownership: OwnershipShared,
		Entries:      []Entry{testEntry("github:owner/plugin", "artifact", "managed replacement\n")},
		ObservedHash: contentHash([]byte(observed)), ManagedIntact: true,
		PreservedContent: [][]byte{fabricated},
	}
	finalized := false

	plan, err := newEngine(newPlanner(fakeGitInspector{})).Run(root, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, []Intent{intent}, ModeApply, func(Ledger) error {
		finalized = true
		return nil
	})

	var conflict *ConflictError
	if !errors.As(err, &conflict) || finalized {
		t.Fatalf("Run(apply) = %#v, %v, finalized = %t; want a conflict without finalizing", plan, err, finalized)
	}
	assertConflict(t, plan, "existing unmanaged target requires a shared merge bound to its observed hash and preserved unmanaged content")
	if got := readFile(t, root, "AGENTS.md"); got != observed {
		t.Fatalf("fabricated preservation proof replaced observed content with %q", got)
	}
}

func TestFollowupsPreservationCommentsNoLongerClaimRedTests(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("preservation_boundary_test.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(content)
	for _, banned := range []string{"red by design", "until #6", "expected to fail"} {
		if strings.Contains(source, banned) {
			t.Fatalf("preservation_boundary_test.go still claims failing tests: %q", banned)
		}
	}
}
