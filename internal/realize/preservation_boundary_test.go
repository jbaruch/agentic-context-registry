package realize

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemoveChangedGeneratedTargetRetainsUnmanagedContent is red by design
// until #6: today's generated-only removal conflicts on any hash mismatch
// instead of writing back unmanaged bytes, dropping the ledger target, and
// removing the Git exclusion in the same transaction.
func TestRemoveChangedGeneratedTargetRetainsUnmanagedContent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, gitExcludePath, "# user pattern\n*.local\n")
	git := fakeGitInspector{state: gitContext{enabled: true, tracked: map[string]bool{}}}
	engine := newEngine(newPlanner(git))
	create := testIntent(".agent/generated.md", "managed\n", OwnershipGenerated)
	var generated Ledger
	if _, err := engine.Run(root, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, []Intent{create}, ModeApply, func(ledger Ledger) error {
		generated = ledger
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	currentContent := "managed\nuser notes\n"
	unmanaged := "user notes\n"
	writeFile(t, root, create.Path, currentContent)
	remove := Intent{
		Action:           ActionRemove,
		Path:             create.Path,
		Content:          []byte(unmanaged),
		Ownership:        OwnershipShared,
		ObservedHash:     contentHash([]byte(currentContent)),
		ManagedIntact:    true,
		PreservedContent: [][]byte{[]byte(unmanaged)},
	}

	var persisted Ledger
	plan, err := engine.Run(root, generated, []Intent{remove}, ModeApply, func(ledger Ledger) error {
		persisted = ledger
		return nil
	})
	var removal Operation
	for _, operation := range plan.Operations {
		if operation.Kind == OperationRemove && operation.Path == create.Path {
			removal = operation
		}
	}
	if err != nil || removal.Kind != OperationRemove || removal.remove || removal.AfterHash != contentHash([]byte(unmanaged)) || !hasOperation(plan, OperationMerge, gitExcludePath) || len(persisted.Targets) != 0 {
		t.Fatalf("apply changed generated-only removal = %#v, ledger = %#v, err = %v", plan, persisted, err)
	}
	if got := readFile(t, root, create.Path); got != unmanaged {
		t.Fatalf("retained content = %q, want unmanaged bytes only", got)
	}
	exclude := readFile(t, root, gitExcludePath)
	if strings.Contains(exclude, "/.agent/generated.md") || !strings.Contains(exclude, "*.local") {
		t.Fatalf("exclude after generated-only removal = %q", exclude)
	}
}

func TestRemoveChangedGeneratedTargetWithoutProofConflicts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, gitExcludePath, "# user pattern\n*.local\n")
	git := fakeGitInspector{state: gitContext{enabled: true, tracked: map[string]bool{}}}
	engine := newEngine(newPlanner(git))
	create := testIntent(".agent/generated.md", "managed\n", OwnershipGenerated)
	var generated Ledger
	if _, err := engine.Run(root, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, []Intent{create}, ModeApply, func(ledger Ledger) error {
		generated = ledger
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	currentContent := "managed\nuser notes\n"
	writeFile(t, root, create.Path, currentContent)
	remove := Intent{
		Action:           ActionRemove,
		Path:             create.Path,
		Content:          []byte("user notes\n"),
		Ownership:        OwnershipShared,
		ObservedHash:     contentHash([]byte(currentContent)),
		ManagedIntact:    true,
		PreservedContent: [][]byte{},
	}

	finalized := false
	plan, err := engine.Run(root, generated, []Intent{remove}, ModeApply, func(Ledger) error {
		finalized = true
		return nil
	})
	var conflict *ConflictError
	if err == nil || !errors.As(err, &conflict) || !plan.HasConflicts() || finalized {
		t.Fatalf("empty-proof generated-only removal = %#v, finalized = %t, err = %v; want conflict", plan, finalized, err)
	}
	if got := readFile(t, root, create.Path); got != currentContent {
		t.Fatalf("empty-proof removal changed content = %q", got)
	}
}

func TestTrackedTargetIsNeverDeletedByRemoval(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "user\nmanaged\n"
	writeFile(t, root, "AGENTS.md", content)
	target := testTarget("AGENTS.md", content, OwnershipShared)
	current := testLedger(target)
	remove := Intent{
		Action:           ActionRemove,
		Path:             "AGENTS.md",
		Content:          []byte("user\n"),
		ObservedHash:     contentHash([]byte(content)),
		ManagedIntact:    true,
		PreservedContent: [][]byte{[]byte("user\n")},
	}
	git := fakeGitInspector{state: gitContext{enabled: true, tracked: map[string]bool{"AGENTS.md": true}}}
	engine := newEngine(newPlanner(git))
	var persisted Ledger
	plan, err := engine.Run(root, current, []Intent{remove}, ModeApply, func(ledger Ledger) error {
		persisted = ledger
		return nil
	})
	if err != nil || len(plan.Operations) != 1 || plan.Operations[0].Kind != OperationRemove || plan.Operations[0].remove || len(persisted.Targets) != 0 {
		t.Fatalf("tracked shared removal = %#v, ledger = %#v, err = %v", plan, persisted, err)
	}
	if got := readFile(t, root, "AGENTS.md"); got != "user\n" {
		t.Fatalf("tracked shared removal content = %q", got)
	}
	if hasOperation(plan, OperationMerge, gitExcludePath) {
		t.Fatalf("tracked shared removal mutated Git exclusions: %#v", plan)
	}
}

func TestTrackedGeneratedTargetIsNeverDeletedByRemoval(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	content := "generated\n"
	writeFile(t, root, "tracked.md", content)
	current := testLedger(testTarget("tracked.md", content, OwnershipGenerated))
	git := fakeGitInspector{state: gitContext{enabled: true, tracked: map[string]bool{"tracked.md": true}}}
	var persisted Ledger
	plan, err := newEngine(newPlanner(git)).Run(root, current, nil, ModeApply, func(ledger Ledger) error {
		persisted = ledger
		return nil
	})
	if err != nil || len(persisted.Targets) != 0 || !hasOperation(plan, OperationRemove, "tracked.md") {
		t.Fatalf("tracked generated removal = %#v, ledger = %#v, err = %v", plan, persisted, err)
	}
	if got := readFile(t, root, "tracked.md"); got != content {
		t.Fatalf("tracked generated removal content = %q, want retained bytes", got)
	}
	if plan.Operations[0].remove {
		t.Fatalf("tracked generated removal scheduled deletion: %#v", plan)
	}
}

func TestTrackedGeneratedTargetDropsManagedContentOnRenderedRemoval(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	content := "managed\n"
	writeFile(t, root, "tracked.md", content)
	current := testLedger(testTarget("tracked.md", content, OwnershipGenerated))
	git := fakeGitInspector{state: gitContext{enabled: true, tracked: map[string]bool{"tracked.md": true}}}
	remove := Intent{
		Action: ActionRemove, Path: "tracked.md", Content: nil, Ownership: OwnershipUnmanaged,
		ObservedHash: contentHash([]byte(content)), ManagedIntact: true,
	}
	var persisted Ledger
	plan, err := newEngine(newPlanner(git)).Run(root, current, []Intent{remove}, ModeApply, func(ledger Ledger) error {
		persisted = ledger
		return nil
	})
	if err != nil || len(persisted.Targets) != 0 || !hasOperation(plan, OperationRemove, "tracked.md") {
		t.Fatalf("tracked rendered removal = %#v, ledger = %#v, err = %v", plan, persisted, err)
	}
	if got := readFile(t, root, "tracked.md"); got != "" {
		t.Fatalf("tracked rendered removal content = %q, want empty retained file", got)
	}
	if plan.Operations[0].remove {
		t.Fatalf("tracked rendered removal scheduled deletion: %#v", plan)
	}
}

func TestPromotionRemovesExclusionInSameTransaction(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, gitExcludePath, "# user pattern\n*.local\n")
	git := fakeGitInspector{state: gitContext{enabled: true, tracked: map[string]bool{}}}
	engine := newEngine(newPlanner(git))
	create := testIntent(".agent/generated.md", "managed v1\n", OwnershipGenerated)
	var generated Ledger
	if _, err := engine.Run(root, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, []Intent{create}, ModeApply, func(ledger Ledger) error {
		generated = ledger
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	currentContent := "managed v1\nuser appendix\n"
	writeFile(t, root, create.Path, currentContent)
	promote := testIntent(create.Path, "managed v2\nuser appendix\n", OwnershipShared)
	promote.ObservedHash = contentHash([]byte(currentContent))
	promote.ManagedIntact = true
	promote.PreservedContent = [][]byte{[]byte("user appendix\n")}

	plan, err := newPlanner(git).Plan(root, generated, []Intent{promote})
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected ledger failure")
	err = applyPlan(root, plan, func(Ledger) error { return injected })
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("promote rollback error = %v", err)
	}
	if got := readFile(t, root, create.Path); got != currentContent {
		t.Fatalf("file after failed finalizer = %q, want rolled back", got)
	}
	exclude := readFile(t, root, gitExcludePath)
	if !strings.Contains(exclude, "/.agent/generated.md") || !strings.Contains(exclude, "*.local") {
		t.Fatalf("exclude after failed finalizer = %q, want generated exclusion restored", exclude)
	}

	var shared Ledger
	plan, err = engine.Run(root, generated, []Intent{promote}, ModeApply, func(ledger Ledger) error {
		shared = ledger
		return nil
	})
	if err != nil || len(shared.Targets) != 1 || shared.Targets[0].Ownership != OwnershipShared || shared.Targets[0].Excluded || !hasOperation(plan, OperationPromote, create.Path) || !hasOperation(plan, OperationMerge, gitExcludePath) {
		t.Fatalf("promotion plan = %#v, ledger = %#v, err = %v", plan, shared, err)
	}
	if got := readFile(t, root, create.Path); got != "managed v2\nuser appendix\n" {
		t.Fatalf("promoted content = %q", got)
	}
	exclude = readFile(t, root, gitExcludePath)
	if strings.Contains(exclude, "/.agent/generated.md") || !strings.Contains(exclude, "*.local") {
		t.Fatalf("exclude after promotion = %q", exclude)
	}
}

// TestExplicitDemotionRequiresCleanUnmanagedContent is red by design until
// #6: ExplicitDemotion currently reclassifies even when PreservedContent
// still names leftover unmanaged bytes.
func TestExplicitDemotionRequiresCleanUnmanagedContent(t *testing.T) {
	t.Parallel()

	t.Run("leftover unmanaged", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		content := "managed\nuser leftover\n"
		writeFile(t, root, "AGENTS.md", content)
		current := testLedger(testTarget("AGENTS.md", content, OwnershipShared))
		demotion := testIntent("AGENTS.md", content, OwnershipGenerated)
		demotion.ObservedHash = contentHash([]byte(content))
		demotion.ManagedIntact = true
		demotion.ExplicitDemotion = true
		demotion.PreservedContent = [][]byte{[]byte("user leftover\n")}

		plan, err := newEngine(newPlanner(fakeGitInspector{})).Run(root, current, []Intent{demotion}, ModeDryRun, nil)
		if err == nil || !plan.HasConflicts() {
			t.Fatalf("demotion with leftover unmanaged = %#v, %v; want conflict", plan, err)
		}
		if got := readFile(t, root, "AGENTS.md"); got != content {
			t.Fatalf("leftover demotion changed content = %q", got)
		}
	})

	t.Run("clean unmanaged", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		content := "managed\n"
		writeFile(t, root, "AGENTS.md", content)
		current := testLedger(testTarget("AGENTS.md", content, OwnershipShared))
		demotion := testIntent("AGENTS.md", content, OwnershipGenerated)
		demotion.ObservedHash = contentHash([]byte(content))
		demotion.ManagedIntact = true
		demotion.ExplicitDemotion = true

		plan, err := newEngine(newPlanner(fakeGitInspector{})).Run(root, current, []Intent{demotion}, ModeDryRun, nil)
		if err != nil || len(plan.Operations) != 1 || plan.Operations[0].Kind != OperationDemote {
			t.Fatalf("clean explicit demotion = %#v, %v", plan, err)
		}
		if got := readFile(t, root, "AGENTS.md"); got != content {
			t.Fatalf("dry-run demotion wrote content = %q", got)
		}
	})
}
