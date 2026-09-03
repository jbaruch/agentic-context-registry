package realize

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakeGitInspector struct {
	state gitContext
	err   error
}

func setTransactionWriter(t *testing.T, writer operationWriter) {
	t.Helper()
	original := transactionWriter
	transactionWriter = writer
	t.Cleanup(func() { transactionWriter = original })
}

func setTransactionParents(t *testing.T, creator parentDirectoryCreator) {
	t.Helper()
	original := transactionParents
	transactionParents = creator
	t.Cleanup(func() { transactionParents = original })
}

func applyJournaledTestPlan(projectDirectory string, plan Plan, finalize Finalizer) (err error) {
	claim, err := claimTransactions(projectDirectory)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, claim.Close()) }()
	if err := cleanStagingTransactions(projectDirectory); err != nil {
		return err
	}
	if err := recoverPendingTransaction(projectDirectory); err != nil {
		return err
	}
	return applyPlanJournaled(projectDirectory, plan, finalize)
}

func (fake fakeGitInspector) Inspect(_ string, _ []string) (gitContext, error) {
	return fake.state, fake.err
}

func TestEngineModesAndIdempotency(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	engine := newEngine(newPlanner(fakeGitInspector{}))
	intent := testIntent(".agent/rules.md", "managed\n", OwnershipGenerated)
	current := Ledger{SchemaVersion: CurrentLedgerSchemaVersion}

	dryPlan, err := engine.Run(root, current, []Intent{intent}, ModeDryRun, nil)
	if err != nil || len(dryPlan.Operations) != 1 || dryPlan.Operations[0].Kind != OperationCreate {
		t.Fatalf("Run(dry-run) = %#v, %v", dryPlan, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".agent", "rules.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run created target: %v", err)
	}
	if _, err := engine.Run(root, current, []Intent{intent}, ModeCheck, nil); err == nil {
		t.Fatal("Run(check) error = nil, want unapplied changes")
	} else {
		var changes *ChangesError
		if !errors.As(err, &changes) {
			t.Fatalf("Run(check) error = %T %v, want ChangesError", err, err)
		}
	}

	var persisted Ledger
	applyPlan, err := engine.Run(root, current, []Intent{intent}, ModeApply, func(ledger Ledger) error {
		persisted = ledger
		return nil
	})
	if err != nil || !applyPlan.HasChanges() {
		t.Fatalf("Run(apply) = %#v, %v", applyPlan, err)
	}
	if got := readFile(t, root, intent.Path); got != "managed\n" {
		t.Fatalf("realized content = %q", got)
	}

	second, err := engine.Run(root, persisted, []Intent{intent}, ModeCheck, nil)
	if err != nil || second.HasChanges() || len(second.Operations) != 0 {
		t.Fatalf("second realization = %#v, %v, want empty plan", second, err)
	}
}

func TestEntryOrderDoesNotCreateDrift(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	engine := newEngine(newPlanner(fakeGitInspector{}))
	intent := testIntent("generated.md", "managed\n", OwnershipGenerated)
	entryA := testEntry("github:owner/a", "alpha", "managed-a\n")
	entryB := testEntry("github:owner/b", "bravo", "managed-b\n")
	intent.Entries = []Entry{entryA, entryB}
	var persisted Ledger
	if _, err := engine.Run(root, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, []Intent{intent}, ModeApply, func(ledger Ledger) error {
		persisted = ledger
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	intent.Entries = []Entry{entryB, entryA}
	plan, err := engine.Run(root, persisted, []Intent{intent}, ModeCheck, nil)
	if err != nil || plan.HasChanges() || len(plan.Operations) != 0 {
		t.Fatalf("reordered entries produced drift: plan = %#v, err = %v", plan, err)
	}
}

func TestChangesErrorCountsLedgerOnlyChange(t *testing.T) {
	t.Parallel()

	err := (&ChangesError{Plan: Plan{LedgerChanged: true}}).Error()
	if err != "realization has 1 unapplied change(s)" {
		t.Fatalf("ChangesError.Error() = %q", err)
	}
}

func TestApplyRollsBackEveryFileWhenWriteFails(t *testing.T) {
	root := t.TempDir()
	planner := newPlanner(fakeGitInspector{})
	current := Ledger{SchemaVersion: CurrentLedgerSchemaVersion}
	plan, err := planner.Plan(root, current, []Intent{
		testIntent("generated/a.txt", "a\n", OwnershipGenerated),
		testIntent("generated/b.txt", "b\n", OwnershipGenerated),
	})
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected write failure")
	writes := 0
	setTransactionWriter(t, func(projectRoot *os.Root, operation Operation) (bool, error) {
		writes++
		replaced, err := writeOperation(projectRoot, operation)
		if err != nil {
			return replaced, err
		}
		if writes == 2 {
			return true, injected
		}
		return true, nil
	})
	err = applyJournaledTestPlan(root, plan, func(Ledger) error {
		t.Fatal("finalizer called after write failure")
		return nil
	})
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("applyPlanJournaled() error = %v", err)
	}
	for _, relative := range []string{"generated/a.txt", "generated/b.txt"} {
		if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(relative))); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("rollback left %s: %v", relative, statErr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(root, "generated")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rollback left generated directory: %v", statErr)
	}
}

func TestRollbackPreservesConcurrentChanges(t *testing.T) {
	root := t.TempDir()
	plan, err := newPlanner(fakeGitInspector{}).Plan(root, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, []Intent{
		testIntent("generated/a.txt", "a\n", OwnershipGenerated),
		testIntent("generated/b.txt", "b\n", OwnershipGenerated),
	})
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected write failure")
	writes := 0
	setTransactionWriter(t, func(projectRoot *os.Root, operation Operation) (bool, error) {
		writes++
		replaced, err := writeOperation(projectRoot, operation)
		if err != nil {
			return replaced, err
		}
		if writes == 2 {
			if err := writeFileAtomic(projectRoot, "generated/a.txt", []byte("concurrent\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return true, injected
		}
		return true, nil
	})
	err = applyJournaledTestPlan(root, plan, func(Ledger) error {
		t.Fatal("finalizer called after write failure")
		return nil
	})
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "automatic recovery failed") || !strings.Contains(err.Error(), "recovery_conflict") {
		t.Fatalf("applyPlanJournaled() error = %v", err)
	}
	if got := readFile(t, root, "generated/a.txt"); got != "concurrent\n" {
		t.Fatalf("concurrent content = %q, want preserved", got)
	}
	if got := readFile(t, root, "generated/b.txt"); got != "b\n" {
		t.Fatalf("second transaction output = %q, want preserved until conflict is resolved", got)
	}
	if _, statErr := os.Stat(filepath.Join(root, transactionDirectory)); statErr != nil {
		t.Fatalf("recovery conflict did not preserve journal: %v", statErr)
	}
}

func TestRollbackKeepsConcurrentlyCreatedEmptyParent(t *testing.T) {
	root := t.TempDir()
	plan, err := newPlanner(fakeGitInspector{}).Plan(root, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, []Intent{
		testIntent("external/generated.txt", "managed\n", OwnershipGenerated),
	})
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected ledger failure")
	raced := false
	setTransactionParents(t, func(projectRoot *os.Root, filename string) ([]rootedDirectory, error) {
		return ensureParentDirectoriesWith(projectRoot, filename, func(directory string, mode os.FileMode) error {
			if !raced {
				raced = true
				if err := projectRoot.Mkdir(directory, mode); err != nil {
					t.Fatal(err)
				}
				return projectRoot.Mkdir(directory, mode)
			}
			return projectRoot.Mkdir(directory, mode)
		})
	})
	err = applyJournaledTestPlan(root, plan, func(Ledger) error { return injected })
	if !raced || !errors.Is(err, injected) || !strings.Contains(err.Error(), "all filesystem changes were rolled back") {
		t.Fatalf("applyPlanJournaled() raced = %t, error = %v", raced, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "external"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("concurrently created directory contains rollback leftovers: %#v", entries)
	}
}

func TestApplyRollsBackWhenLedgerFinalizerFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	filename := filepath.Join(root, "managed.txt")
	if err := os.WriteFile(filename, []byte("before\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	currentTarget := testTarget("managed.txt", "before\n", OwnershipGenerated)
	currentTarget.Mode = 0o640
	current := testLedger(currentTarget)
	intent := testIntent("managed.txt", "after\n", OwnershipGenerated)
	plan, err := newPlanner(fakeGitInspector{}).Plan(root, current, []Intent{intent})
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected ledger failure")
	err = applyJournaledTestPlan(root, plan, func(Ledger) error { return injected })
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("applyPlanJournaled() error = %v", err)
	}
	if got := readFile(t, root, "managed.txt"); got != "before\n" {
		t.Fatalf("content after rollback = %q", got)
	}
	info, err := os.Stat(filename)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode after rollback = %v, %v", info.Mode().Perm(), err)
	}
}

func TestApplyRejectsTargetChangedAfterPlanning(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "managed.txt", "before\n")
	current := testLedger(testTarget("managed.txt", "before\n", OwnershipGenerated))
	plan, err := newPlanner(fakeGitInspector{}).Plan(root, current, []Intent{testIntent("managed.txt", "after\n", OwnershipGenerated)})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "managed.txt", "concurrent user edit\n")
	finalized := false
	err = applyJournaledTestPlan(root, plan, func(Ledger) error {
		finalized = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed after planning") || finalized {
		t.Fatalf("applyPlanJournaled() error = %v, finalized = %t", err, finalized)
	}
	if got := readFile(t, root, "managed.txt"); got != "concurrent user edit\n" {
		t.Fatalf("stale plan overwrote target = %q", got)
	}
}

func TestApplyRejectsModeChangedAfterPlanning(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "managed.txt", "before\n")
	current := testLedger(testTarget("managed.txt", "before\n", OwnershipGenerated))
	plan, err := newPlanner(fakeGitInspector{}).Plan(root, current, []Intent{testIntent("managed.txt", "after\n", OwnershipGenerated)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "managed.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	finalized := false
	err = applyJournaledTestPlan(root, plan, func(Ledger) error {
		finalized = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed after planning") || finalized {
		t.Fatalf("applyPlanJournaled() error = %v, finalized = %t", err, finalized)
	}
	if got := readFile(t, root, "managed.txt"); got != "before\n" {
		t.Fatalf("mode-only stale plan changed content = %q", got)
	}
	info, err := os.Stat(filepath.Join(root, "managed.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode-only stale plan changed mode = %v", info.Mode().Perm())
	}
}

func TestWriteRejectsModeChangedAfterPreflight(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "managed.txt", "before\n")
	current := testLedger(testTarget("managed.txt", "before\n", OwnershipGenerated))
	plan, err := newPlanner(fakeGitInspector{}).Plan(root, current, []Intent{testIntent("managed.txt", "after\n", OwnershipGenerated)})
	if err != nil {
		t.Fatal(err)
	}
	finalized := false
	setTransactionWriter(t, func(projectRoot *os.Root, operation Operation) (bool, error) {
		if err := projectRoot.Chmod(operation.Path, 0o600); err != nil {
			t.Fatal(err)
		}
		return writeOperation(projectRoot, operation)
	})
	err = applyJournaledTestPlan(root, plan, func(Ledger) error {
		finalized = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed immediately before") || finalized {
		t.Fatalf("applyPlanJournaled() error = %v, finalized = %t", err, finalized)
	}
	if got := readFile(t, root, "managed.txt"); got != "before\n" {
		t.Fatalf("mode race changed content = %q", got)
	}
}

func TestPreserveRejectsModeDrift(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "managed.txt", "managed\n")
	current := testLedger(testTarget("managed.txt", "managed\n", OwnershipGenerated))
	if err := os.Chmod(filepath.Join(root, "managed.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := newEngine(newPlanner(fakeGitInspector{})).Run(root, current, []Intent{{
		Action: ActionPreserve,
		Path:   "managed.txt",
	}}, ModeCheck, nil)
	if err == nil || !plan.HasConflicts() {
		t.Fatalf("preserve mode drift = %#v, %v; want conflict", plan, err)
	}
}

func TestGeneratedRemovalDeletesOnlyProvenOwnedFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, ".agent/managed.md", "managed\n")
	writeFile(t, root, ".agent/user.md", "user\n")
	current := testLedger(testTarget(".agent/managed.md", "managed\n", OwnershipGenerated))
	engine := newEngine(newPlanner(fakeGitInspector{}))
	var persisted Ledger
	plan, err := engine.Run(root, current, nil, ModeApply, func(ledger Ledger) error {
		persisted = ledger
		return nil
	})
	if err != nil || !hasOperation(plan, OperationRemove, ".agent/managed.md") {
		t.Fatalf("removal plan = %#v, %v", plan, err)
	}
	if len(persisted.Targets) != 0 {
		t.Fatalf("persisted ownership = %#v, want empty", persisted)
	}
	if _, err := os.Stat(filepath.Join(root, ".agent", "managed.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed file still exists: %v", err)
	}
	if got := readFile(t, root, ".agent/user.md"); got != "user\n" {
		t.Fatalf("unmanaged sibling changed = %q", got)
	}
}

func TestPlannerProtectsUnmanagedAndModifiedOutput(t *testing.T) {
	t.Parallel()

	t.Run("unmanaged", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "rules.md", "user\n")
		plan, err := newPlanner(fakeGitInspector{}).Plan(root, Ledger{SchemaVersion: 1}, []Intent{testIntent("rules.md", "managed\n", OwnershipGenerated)})
		if err != nil {
			t.Fatal(err)
		}
		assertConflict(t, plan, "unmanaged")
		if got := readFile(t, root, "rules.md"); got != "user\n" {
			t.Fatalf("unmanaged content changed = %q", got)
		}
	})

	t.Run("modified generated", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "rules.md", "user changed managed bytes\n")
		current := testLedger(testTarget("rules.md", "managed\n", OwnershipGenerated))
		plan, err := newPlanner(fakeGitInspector{}).Plan(root, current, []Intent{testIntent("rules.md", "managed v2\n", OwnershipGenerated)})
		if err != nil {
			t.Fatal(err)
		}
		assertConflict(t, plan, "modified")
	})

	t.Run("modified removal", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "rules.md", "modified\n")
		current := testLedger(testTarget("rules.md", "managed\n", OwnershipGenerated))
		plan, err := newPlanner(fakeGitInspector{}).Plan(root, current, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertConflict(t, plan, "remove modified")
	})

	t.Run("mode-modified removal", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "rules.md", "managed\n")
		current := testLedger(testTarget("rules.md", "managed\n", OwnershipGenerated))
		if err := os.Chmod(filepath.Join(root, "rules.md"), 0o600); err != nil {
			t.Fatal(err)
		}
		plan, err := newPlanner(fakeGitInspector{}).Plan(root, current, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertConflict(t, plan, "remove modified")
	})
}

func TestPlannerPromotesAndKeepsSharedOwnershipSticky(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	currentContent := "managed v1\nuser appendix\n"
	writeFile(t, root, "AGENTS.md", currentContent)
	current := testLedger(testTarget("AGENTS.md", "managed v1\n", OwnershipGenerated))
	intent := testIntent("AGENTS.md", "managed v2\nuser appendix\n", OwnershipShared)
	intent.ObservedHash = contentHash([]byte(currentContent))
	intent.ManagedIntact = true
	intent.PreservedContent = [][]byte{[]byte("user appendix\n")}
	engine := newEngine(newPlanner(fakeGitInspector{}))
	var promoted Ledger
	plan, err := engine.Run(root, current, []Intent{intent}, ModeApply, func(ledger Ledger) error {
		promoted = ledger
		return nil
	})
	if err != nil || len(plan.Operations) != 1 || plan.Operations[0].Kind != OperationPromote {
		t.Fatalf("promotion plan = %#v, %v", plan, err)
	}
	if promoted.Targets[0].Ownership != OwnershipShared || readFile(t, root, "AGENTS.md") != "managed v2\nuser appendix\n" {
		t.Fatalf("promotion result = %#v, %q", promoted, readFile(t, root, "AGENTS.md"))
	}

	demotion := testIntent("AGENTS.md", "managed v3\n", OwnershipGenerated)
	demotion.ObservedHash = promoted.Targets[0].OutputHash
	demotion.ManagedIntact = true
	blocked, err := engine.Run(root, promoted, []Intent{demotion}, ModeDryRun, nil)
	if err == nil {
		t.Fatal("automatic demotion succeeded")
	}
	assertConflict(t, blocked, "sticky")
	demotion.ExplicitDemotion = true
	allowed, err := engine.Run(root, promoted, []Intent{demotion}, ModeDryRun, nil)
	if err != nil || len(allowed.Operations) != 1 || allowed.Operations[0].Kind != OperationDemote {
		t.Fatalf("explicit demotion = %#v, %v", allowed, err)
	}
}

func TestSharedRemovalRequiresRenderedMerge(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	content := "user\nmanaged-a\nmanaged-b\n"
	writeFile(t, root, "config.txt", content)
	target := testTarget("config.txt", content, OwnershipShared)
	target.Entries = []Entry{testEntry("github:owner/a", "a", "managed-a\n"), testEntry("github:owner/b", "b", "managed-b\n")}
	current := testLedger(target)
	planner := newPlanner(fakeGitInspector{})

	blocked, err := planner.Plan(root, current, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertConflict(t, blocked, "shared target removal")

	intent := testIntent("config.txt", "user\nmanaged-b\n", OwnershipShared)
	intent.Entries = []Entry{testEntry("github:owner/b", "b", "managed-b\n")}
	intent.ObservedHash = contentHash([]byte(content))
	intent.ManagedIntact = true
	intent.PreservedContent = [][]byte{[]byte("user\n")}
	plan, err := planner.Plan(root, current, []Intent{intent})
	if err != nil || len(plan.Operations) != 1 || plan.Operations[0].Kind != OperationMerge {
		t.Fatalf("rendered shared removal = %#v, %v", plan, err)
	}
	if len(plan.NextLedger.Targets[0].Entries) != 1 || plan.NextLedger.Targets[0].Entries[0].Source != "github:owner/b" {
		t.Fatalf("next ownership entries = %#v", plan.NextLedger.Targets[0].Entries)
	}

	remove := Intent{
		Action: ActionRemove, Path: "config.txt", Content: []byte("user\n"),
		ObservedHash: contentHash([]byte(content)), ManagedIntact: true,
		PreservedContent: [][]byte{[]byte("user\n")},
	}
	unsafe := remove
	unsafe.ObservedHash = contentHash([]byte("stale\n"))
	blocked, err = planner.Plan(root, current, []Intent{unsafe})
	if err != nil {
		t.Fatal(err)
	}
	assertConflict(t, blocked, "exact observed file")

	fabricated := remove
	fabricated.Content = []byte("fabricated preservation proof\n")
	fabricated.PreservedContent = [][]byte{[]byte("fabricated preservation proof\n")}
	blocked, err = planner.Plan(root, current, []Intent{fabricated})
	if err != nil {
		t.Fatal(err)
	}
	assertConflict(t, blocked, "shared target removal must be bound to the exact observed file")

	engine := newEngine(planner)
	var unmanaged Ledger
	plan, err = engine.Run(root, current, []Intent{remove}, ModeApply, func(ledger Ledger) error {
		unmanaged = ledger
		return nil
	})
	if err != nil || len(plan.Operations) != 1 || plan.Operations[0].Kind != OperationRemove || plan.Operations[0].remove || len(unmanaged.Targets) != 0 {
		t.Fatalf("final shared removal = %#v, ledger = %#v, err = %v", plan, unmanaged, err)
	}
	if got := readFile(t, root, "config.txt"); got != "user\n" {
		t.Fatalf("final shared removal content = %q", got)
	}
	unchanged, err := engine.Run(root, unmanaged, nil, ModeCheck, nil)
	if err != nil || unchanged.HasChanges() {
		t.Fatalf("post-removal plan = %#v, %v; want unmanaged file ignored", unchanged, err)
	}
}

func TestGitExclusionFollowsOwnershipAndTracking(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, gitExcludePath, "# user pattern\n*.local\n")
	git := fakeGitInspector{state: gitContext{enabled: true, tracked: map[string]bool{}}}
	engine := newEngine(newPlanner(git))
	intent := testIntent(".agent/generated.md", "managed\n", OwnershipGenerated)
	var generated Ledger
	plan, err := engine.Run(root, Ledger{SchemaVersion: 1}, []Intent{intent}, ModeApply, func(ledger Ledger) error {
		generated = ledger
		return nil
	})
	if err != nil || !generated.Targets[0].Excluded || !hasOperation(plan, OperationMerge, gitExcludePath) {
		t.Fatalf("generated exclusion plan = %#v, ledger = %#v, err = %v", plan, generated, err)
	}
	exclude := readFile(t, root, gitExcludePath)
	if !strings.Contains(exclude, "# user pattern\n*.local\n") || !strings.Contains(exclude, "/.agent/generated.md") {
		t.Fatalf("exclude content = %q", exclude)
	}
	unchanged, err := engine.Run(root, generated, []Intent{intent}, ModeCheck, nil)
	if err != nil || unchanged.HasChanges() || len(unchanged.Operations) != 0 {
		t.Fatalf("unchanged excluded target = %#v, %v; want empty plan", unchanged, err)
	}

	currentContent := "managed\nuser\n"
	writeFile(t, root, intent.Path, currentContent)
	promote := testIntent(intent.Path, currentContent, OwnershipShared)
	promote.ObservedHash = contentHash([]byte(currentContent))
	promote.ManagedIntact = true
	promote.PreservedContent = [][]byte{[]byte("user\n")}
	var shared Ledger
	plan, err = engine.Run(root, generated, []Intent{promote}, ModeApply, func(ledger Ledger) error {
		shared = ledger
		return nil
	})
	if err != nil || shared.Targets[0].Excluded || !hasOperation(plan, OperationPromote, intent.Path) || !hasOperation(plan, OperationMerge, gitExcludePath) {
		t.Fatalf("promotion exclusion plan = %#v, ledger = %#v, err = %v", plan, shared, err)
	}
	exclude = readFile(t, root, gitExcludePath)
	if strings.Contains(exclude, "/.agent/generated.md") || !strings.Contains(exclude, "*.local") {
		t.Fatalf("exclude after promotion = %q", exclude)
	}
}

func TestGitExclusionAndFileRollbackTogether(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	writeFile(t, root, gitExcludePath, "user-pattern")
	git := commandGitInspector{}
	plan, err := newPlanner(git).Plan(root, Ledger{SchemaVersion: 1}, []Intent{testIntent("generated/rule.md", "managed\n", OwnershipGenerated)})
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("state write failed")
	err = applyJournaledTestPlan(root, plan, func(Ledger) error { return injected })
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("applyPlanJournaled() error = %v", err)
	}
	if got := readFile(t, root, gitExcludePath); got != "user-pattern" {
		t.Fatalf("exclude rollback = %q, want exact original", got)
	}
	if _, err := os.Stat(filepath.Join(root, "generated", "rule.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generated file survived rollback: %v", err)
	}
}

func TestTrackedGeneratedFileIsNotExcluded(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	git := fakeGitInspector{state: gitContext{enabled: true, tracked: map[string]bool{"tracked.md": true}}}
	plan, err := newPlanner(git).Plan(root, Ledger{SchemaVersion: 1}, []Intent{testIntent("tracked.md", "managed\n", OwnershipGenerated)})
	if err != nil {
		t.Fatal(err)
	}
	if plan.NextLedger.Targets[0].Excluded || hasOperation(plan, OperationMerge, gitExcludePath) {
		t.Fatalf("tracked target exclusion plan = %#v", plan)
	}
}

func TestCommandGitInspectorReadsTrackingWithoutChangingIndex(t *testing.T) {
	t.Parallel()
	requireGit(t)

	root := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	writeFile(t, root, "tracked.md", "tracked\n")
	writeFile(t, root, "untracked.md", "untracked\n")
	if output, err := exec.Command("git", "-C", root, "add", "--", "tracked.md").CombinedOutput(); err != nil {
		t.Fatalf("git add fixture: %v: %s", err, output)
	}
	indexPath := filepath.Join(root, ".git", "index")
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	state, err := (commandGitInspector{}).Inspect(root, []string{"tracked.md", "untracked.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !state.enabled || !state.tracked["tracked.md"] || state.tracked["untracked.md"] {
		t.Fatalf("Inspect() = %#v", state)
	}
	indexAfter, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(indexAfter, indexBefore) {
		t.Fatal("Git tracking inspection modified the index")
	}
}

func TestLinkedGitWorktreeUsesResolvedExclusionPath(t *testing.T) {
	t.Parallel()
	requireGit(t)

	runGit := func(args ...string) []byte {
		t.Helper()
		output, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
		}
		return output
	}
	repository := t.TempDir()
	runGit("init", "-q", repository)
	writeFile(t, repository, "seed.md", "seed\n")
	runGit("-C", repository, "add", "--", "seed.md")
	runGit("-C", repository, "-c", "user.name=ACR Test", "-c", "user.email=acr@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "seed")

	linked := filepath.Join(t.TempDir(), "linked")
	runGit("-C", repository, "worktree", "add", "-q", "-b", "linked-review", linked)
	metadata, err := os.Lstat(filepath.Join(linked, ".git"))
	if err != nil || !metadata.Mode().IsRegular() {
		t.Fatalf("linked .git metadata = %v, %v; want regular gitfile", metadata, err)
	}

	state, err := (commandGitInspector{}).Inspect(linked, []string{"seed.md", ".agent/generated.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !state.enabled || !state.tracked["seed.md"] || state.tracked[".agent/generated.md"] || state.excludeRoot == "" || state.excludePath != "exclude" {
		t.Fatalf("Inspect(linked worktree) = %#v", state)
	}

	engine := NewEngine()
	var persisted Ledger
	plan, err := engine.Run(linked, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, []Intent{
		testIntent(".agent/generated.md", "managed\n", OwnershipGenerated),
	}, ModeApply, func(ledger Ledger) error {
		persisted = ledger
		return nil
	})
	if err != nil || len(persisted.Targets) != 1 || !persisted.Targets[0].Excluded || !hasOperation(plan, OperationMerge, gitExcludePath) {
		t.Fatalf("Run(linked worktree) = %#v, ledger = %#v, err = %v", plan, persisted, err)
	}
	exclude, err := os.ReadFile(filepath.Join(state.excludeRoot, state.excludePath))
	if err != nil || !strings.Contains(string(exclude), "/.agent/generated.md") {
		t.Fatalf("resolved exclude content = %q, %v", exclude, err)
	}
	runGit("-C", linked, "check-ignore", "-q", "--", ".agent/generated.md")
}

func TestGitExcludeBlockRoundTripPreservesMissingTrailingNewline(t *testing.T) {
	t.Parallel()

	original := []byte("user-pattern")
	withBlock, err := rewriteGitExclude(original, []string{"generated/rule.md"})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := rewriteGitExclude(withBlock, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(original) {
		t.Fatalf("exclude round trip = %q, want %q", restored, original)
	}
}

func TestGitExcludeRejectsAmbiguousMarkerText(t *testing.T) {
	t.Parallel()

	content := []byte("user-# BEGIN ACR GENERATED OUTPUTS\n# END ACR GENERATED OUTPUTS\n")
	if _, err := rewriteGitExclude(content, []string{"generated/rule.md"}); err == nil || !strings.Contains(err.Error(), "complete lines") {
		t.Fatalf("rewriteGitExclude() error = %v, want ambiguous-marker rejection", err)
	}
}

func TestCommittedSharedFileKeepsSharedClassification(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	content := "user\nmanaged\n"
	writeFile(t, root, "AGENTS.md", content)
	target := testTarget("AGENTS.md", content, OwnershipShared)
	current := testLedger(target)
	intent := testIntent("AGENTS.md", content, OwnershipShared)
	intent.ObservedHash = target.OutputHash
	intent.ManagedIntact = true
	intent.PreservedContent = [][]byte{[]byte("user\n")}
	git := fakeGitInspector{state: gitContext{enabled: true, tracked: map[string]bool{"AGENTS.md": true}}}
	plan, err := newPlanner(git).Plan(root, current, []Intent{intent})
	if err != nil || plan.HasChanges() || plan.NextLedger.Targets[0].Ownership != OwnershipShared {
		t.Fatalf("clone shared plan = %#v, %v", plan, err)
	}
}

func TestExplicitPreserveOperation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "kept.txt", "managed\n")
	current := testLedger(testTarget("kept.txt", "managed\n", OwnershipGenerated))
	plan, err := newPlanner(fakeGitInspector{}).Plan(root, current, []Intent{{Action: ActionPreserve, Path: "kept.txt"}})
	if err != nil || len(plan.Operations) != 1 || plan.Operations[0].Kind != OperationPreserve || plan.HasChanges() {
		t.Fatalf("preserve plan = %#v, %v", plan, err)
	}
}

func TestLedgerEncodingAndValidation(t *testing.T) {
	t.Parallel()

	absent, err := DecodeLedger(nil)
	if err != nil || absent.SchemaVersion != CurrentLedgerSchemaVersion || len(absent.Targets) != 0 {
		t.Fatalf("DecodeLedger(nil) = %#v, %v", absent, err)
	}
	if _, err := DecodeLedger(map[string]any{}); err == nil || !strings.Contains(err.Error(), "schemaVersion") {
		t.Fatalf("DecodeLedger(empty object) error = %v, want required schemaVersion rejection", err)
	}

	ledger := testLedger(testTarget(".agent/rules.md", "managed\n", OwnershipGenerated))
	encoded, err := EncodeLedger(ledger)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeLedger(encoded)
	if err != nil || !reflect.DeepEqual(decoded, ledger) {
		t.Fatalf("DecodeLedger() = %#v, %v, want %#v", decoded, err, ledger)
	}
	invalid := ledger
	invalid.Targets = append([]Target(nil), ledger.Targets...)
	invalid.Targets[0].Path = "../outside"
	if err := ValidateLedger(invalid); err == nil || !strings.Contains(err.Error(), "project-relative") {
		t.Fatalf("ValidateLedger() error = %v, want path rejection", err)
	}
}

func TestPlannerRejectsSymlinkedTargetParent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".agent")); err != nil {
		t.Fatalf("create symlink on supported platform: %v", err)
	}
	_, err := newPlanner(fakeGitInspector{}).Plan(root, Ledger{SchemaVersion: 1}, []Intent{testIntent(".agent/rules.md", "managed\n", OwnershipGenerated)})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Plan() error = %v, want symlink rejection", err)
	}
}

func TestPlannerRejectsWholeFileSharedReplaceWithEmptyPreservedContent(t *testing.T) {
	t.Parallel()

	t.Run("unowned existing target", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		content := "user\nunmanaged content\n"
		writeFile(t, root, "AGENTS.md", content)
		intent := testIntent("AGENTS.md", "entirely new managed bytes\n", OwnershipShared)
		intent.ObservedHash = contentHash([]byte(content))
		intent.ManagedIntact = true
		plan, err := newPlanner(fakeGitInspector{}).Plan(root, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, []Intent{intent})
		if err != nil {
			t.Fatal(err)
		}
		assertConflict(t, plan, "shared merge")
		if got := readFile(t, root, "AGENTS.md"); got != content {
			t.Fatalf("unmanaged content changed = %q", got)
		}
	})

	t.Run("previously shared target", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		content := "managed v1\nuser appendix\n"
		writeFile(t, root, "AGENTS.md", content)
		current := testLedger(testTarget("AGENTS.md", content, OwnershipShared))
		intent := testIntent("AGENTS.md", "managed v2 entirely replaces the file\n", OwnershipShared)
		intent.ObservedHash = contentHash([]byte(content))
		intent.ManagedIntact = true
		plan, err := newPlanner(fakeGitInspector{}).Plan(root, current, []Intent{intent})
		if err != nil {
			t.Fatal(err)
		}
		assertConflict(t, plan, "shared merge")
		if got := readFile(t, root, "AGENTS.md"); got != content {
			t.Fatalf("shared content changed = %q", got)
		}

		// F6 (reviewer): a Plan-only conflict does not by itself prove
		// Engine.Run(ModeApply) refuses the write; exercise apply directly,
		// with the ledger finalizer never called and the tree byte-identical
		// before and after the attempt.
		beforeTree := treeSnapshot(t, root)
		finalized := false
		applyPlan, applyErr := newEngine(newPlanner(fakeGitInspector{})).Run(root, current, []Intent{intent}, ModeApply, func(Ledger) error {
			finalized = true
			return nil
		})
		var conflict *ConflictError
		if !errors.As(applyErr, &conflict) || finalized {
			t.Fatalf("Run(apply) = %#v, %v, finalized = %t; want a ConflictError with the finalizer never called", applyPlan, applyErr, finalized)
		}
		if got := treeSnapshot(t, root); !reflect.DeepEqual(got, beforeTree) {
			t.Fatalf("tree after apply attempt = %#v, want %#v", got, beforeTree)
		}
	})

	t.Run("promotion from generated", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		content := "managed\nuser appendix\n"
		writeFile(t, root, "AGENTS.md", content)
		current := testLedger(testTarget("AGENTS.md", "managed\n", OwnershipGenerated))
		intent := testIntent("AGENTS.md", "an entirely different body\n", OwnershipShared)
		intent.ObservedHash = contentHash([]byte(content))
		intent.ManagedIntact = true
		plan, err := newPlanner(fakeGitInspector{}).Plan(root, current, []Intent{intent})
		if err != nil {
			t.Fatal(err)
		}
		assertConflict(t, plan, "promoted through a hash-bound merge")
		if got := readFile(t, root, "AGENTS.md"); got != content {
			t.Fatalf("promoted content changed = %q", got)
		}
	})

	t.Run("empty fragment does not count as preserved", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		content := "user\nmanaged\n"
		writeFile(t, root, "AGENTS.md", content)
		intent := testIntent("AGENTS.md", "managed only, no user content\n", OwnershipShared)
		intent.ObservedHash = contentHash([]byte(content))
		intent.ManagedIntact = true
		intent.PreservedContent = [][]byte{{}, nil}
		plan, err := newPlanner(fakeGitInspector{}).Plan(root, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, []Intent{intent})
		if err != nil {
			t.Fatal(err)
		}
		assertConflict(t, plan, "shared merge")
	})

	t.Run("genuine merge with a real preserved fragment is accepted", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		content := "user appendix\n"
		writeFile(t, root, "AGENTS.md", content)
		intent := testIntent("AGENTS.md", "managed\nuser appendix\n", OwnershipShared)
		intent.ObservedHash = contentHash([]byte(content))
		intent.ManagedIntact = true
		intent.PreservedContent = [][]byte{[]byte("user appendix\n")}
		plan, err := newPlanner(fakeGitInspector{}).Plan(root, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, []Intent{intent})
		if err != nil || plan.HasConflicts() {
			t.Fatalf("genuine merge plan = %#v, %v, want no conflict", plan, err)
		}
	})
}

func TestPlannerRejectsFabricatedPreservedFragment(t *testing.T) {
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

func TestPlannerRejectsReservedAdapterTargets(t *testing.T) {
	t.Parallel()

	for _, targetPath := range []string{"agents.yaml", ".agents", ".agents/registry.lock", ".git", ".git/config"} {
		t.Run(targetPath, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			intent := testIntent(targetPath, "generated\n", OwnershipGenerated)
			_, err := newPlanner(fakeGitInspector{}).Plan(root, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, []Intent{intent})
			if err == nil || !strings.Contains(err.Error(), "reserved project state path") {
				t.Fatalf("Plan() error = %v, want reserved-path rejection", err)
			}
		})
	}
}

// TestAdapterVersionBumpEmitsReviewablePlanWithoutWrites is the tester's
// acceptance test (internal/realize/adapter_boundary_test.go in the
// verification patch), kept verbatim: it is stricter than the developer's
// own TestCoordinatorAdapterVersionBumpProducesReviewablePlanWithoutWrites
// because it also asserts the reviewable plan JSON never leaks the rendered
// file body and that agents.yaml/.agents/registry.lock stay byte-identical
// through dry-run, check, and apply.
func TestAdapterVersionBumpEmitsReviewablePlanWithoutWrites(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	body := "distinctive-managed-body-v1\n"
	path := ".agent/rules.md"
	engine := newEngine(newPlanner(fakeGitInspector{}))
	intent := testIntent(path, body, OwnershipGenerated)
	var current Ledger
	if _, err := engine.Run(root, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, []Intent{intent}, ModeApply, func(ledger Ledger) error {
		current = ledger
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if current.Targets[0].Entries[0].AdapterVersion != "1.0.0" {
		t.Fatalf("seed adapterVersion = %q, want 1.0.0", current.Targets[0].Entries[0].AdapterVersion)
	}
	writeFile(t, root, "agents.yaml", "schemaVersion: 1\n")
	writeFile(t, root, ".agents/registry.lock", "schemaVersion: 1\n")
	before := treeSnapshot(t, root)

	bumped := testIntent(path, body, OwnershipGenerated)
	bumped.Entries[0].AdapterVersion = "1.1.0"
	rejectWrites := func(Ledger) error {
		t.Fatal("dry-run or check called the ledger finalizer")
		return nil
	}

	dryPlan, err := engine.Run(root, current, []Intent{bumped}, ModeDryRun, rejectWrites)
	if err != nil || !dryPlan.HasChanges() {
		t.Fatalf("Run(dry-run) = %#v, %v; want non-empty plan", dryPlan, err)
	}
	assertReviewablePlan(t, dryPlan, body)
	if got := treeSnapshot(t, root); !reflect.DeepEqual(got, before) {
		t.Fatalf("dry-run wrote files: got %#v, want %#v", got, before)
	}

	checkPlan, err := engine.Run(root, current, []Intent{bumped}, ModeCheck, rejectWrites)
	var changes *ChangesError
	if !errors.As(err, &changes) || !checkPlan.HasChanges() {
		t.Fatalf("Run(check) = %#v, %v; want ChangesError for unapplied version bump", checkPlan, err)
	}
	assertReviewablePlan(t, checkPlan, body)
	if got := treeSnapshot(t, root); !reflect.DeepEqual(got, before) {
		t.Fatalf("check wrote files: got %#v, want %#v", got, before)
	}

	var applied Ledger
	applyPlan, err := engine.Run(root, current, []Intent{bumped}, ModeApply, func(ledger Ledger) error {
		applied = ledger
		return nil
	})
	if err != nil || !applyPlan.HasChanges() {
		t.Fatalf("Run(apply) = %#v, %v", applyPlan, err)
	}
	if got := readFile(t, root, path); got != body {
		t.Fatalf("apply changed rendered file = %q", got)
	}
	if got := readFile(t, root, "agents.yaml"); got != "schemaVersion: 1\n" {
		t.Fatalf("apply changed agents.yaml = %q", got)
	}
	if got := readFile(t, root, ".agents/registry.lock"); got != "schemaVersion: 1\n" {
		t.Fatalf("apply changed lockfile outside the finalizer = %q", got)
	}
	if len(applied.Targets) != 1 || applied.Targets[0].Entries[0].AdapterVersion != "1.1.0" {
		t.Fatalf("applied ledger = %#v, want adapterVersion 1.1.0", applied)
	}
	if applied.Targets[0].OutputHash != current.Targets[0].OutputHash {
		t.Fatalf("apply changed output hash = %q, want %q", applied.Targets[0].OutputHash, current.Targets[0].OutputHash)
	}
}

func assertReviewablePlan(t *testing.T, plan Plan, body string) {
	t.Helper()
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), strings.TrimSuffix(body, "\n")) {
		t.Fatalf("reviewable plan JSON leaked file body: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"path"`) || !strings.Contains(string(encoded), `"kind"`) {
		t.Fatalf("reviewable plan JSON missing path/kind: %s", encoded)
	}
}

func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(filename string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		snapshot[filepath.ToSlash(relative)] = string(content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func testIntent(targetPath, content string, ownership Ownership) Intent {
	return Intent{
		Path: targetPath, Content: []byte(content), Mode: 0o644, Ownership: ownership,
		Entries: []Entry{testEntry("github:owner/plugin", "artifact", content)},
	}
}

func testEntry(source, id, managed string) Entry {
	return Entry{
		Source: source, ArtifactID: id, ArtifactKind: ArtifactFile, SourcePath: "rules/source.md",
		Adapter: "test", AdapterVersion: "1.0.0", ManagedHash: contentHash([]byte(managed)),
	}
}

func testTarget(targetPath, content string, ownership Ownership) Target {
	return Target{
		Path: targetPath, Mode: 0o644, Ownership: ownership, OutputHash: contentHash([]byte(content)),
		Entries: []Entry{testEntry("github:owner/plugin", "artifact", content)},
	}
}

func testLedger(targets ...Target) Ledger {
	return canonicalLedger(Ledger{SchemaVersion: CurrentLedgerSchemaVersion, Targets: targets})
}

func writeFile(t *testing.T, root, relative, content string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, root, relative string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func assertConflict(t *testing.T, plan Plan, want string) {
	t.Helper()
	if !plan.HasConflicts() {
		t.Fatalf("plan has no conflict: %#v", plan)
	}
	for _, operation := range plan.Operations {
		if operation.Kind == OperationConflict && strings.Contains(operation.Reason, want) {
			return
		}
	}
	t.Fatalf("conflicts = %#v, want reason containing %q", plan.Operations, want)
}

func hasOperation(plan Plan, kind OperationKind, targetPath string) bool {
	for _, operation := range plan.Operations {
		if operation.Kind == kind && operation.Path == targetPath {
			return true
		}
	}
	return false
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("Git is required for realization integration tests; install Git and ensure it is on PATH: %v", err)
	}
}
