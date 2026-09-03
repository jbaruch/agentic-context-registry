package realize

import (
	"os/exec"
	"strings"
	"testing"
)

func TestMergeLedgersCombinesDisjointTargetsAndRejectsDuplicates(t *testing.T) {
	t.Parallel()

	base := testLedger(testTarget("base.md", "base\n", OwnershipGenerated))
	carried := testLedger(testTarget("carried.md", "carried\n", OwnershipGenerated))

	merged, err := MergeLedgers(base, carried)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Targets) != 2 || merged.Targets[0].Path != "base.md" || merged.Targets[1].Path != "carried.md" {
		t.Fatalf("MergeLedgers() = %#v, want both targets in canonical order", merged)
	}
	if merged.SchemaVersion != CurrentLedgerSchemaVersion {
		t.Fatalf("merged schemaVersion = %d, want %d", merged.SchemaVersion, CurrentLedgerSchemaVersion)
	}

	if _, err := MergeLedgers(base, base); err == nil || !strings.Contains(err.Error(), "base.md") {
		t.Fatalf("MergeLedgers(duplicate) error = %v, want the repeated path named", err)
	}
}

// TestRetainedTargetsKeepTheirGitExclusions runs against a real initialized
// repository rather than a fake inspector: the exclusion block is only written
// when Git inspection is live, so a fake would let a scoped ledger silently
// un-exclude the omitted agent's outputs and still pass.
func TestRetainedTargetsKeepTheirGitExclusions(t *testing.T) {
	t.Parallel()
	requireGit(t)

	root := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	retained := testLedger(testTarget("other-agent/rule.md", "managed\n", OwnershipGenerated))

	plan, err := NewPlanner().Plan(root, Ledger{SchemaVersion: CurrentLedgerSchemaVersion},
		[]Intent{testIntent("selected/rule.md", "managed\n", OwnershipGenerated)}, retained)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.NextLedger.Targets) != 1 || plan.NextLedger.Targets[0].Path != "selected/rule.md" {
		t.Fatalf("retained target entered the next ledger: %#v", plan.NextLedger)
	}
	if hasOperation(plan, OperationRemove, "other-agent/rule.md") {
		t.Fatalf("retained target was planned for removal: %#v", plan.Operations)
	}
	if err := applyJournaledTestPlan(root, plan, func(Ledger) error { return nil }); err != nil {
		t.Fatal(err)
	}
	exclusions := readFile(t, root, gitExcludePath)
	if !strings.Contains(exclusions, "/other-agent/rule.md") || !strings.Contains(exclusions, "/selected/rule.md") {
		t.Fatalf("Git exclusions = %q, want both the selected and the retained target", exclusions)
	}
}

func TestPlanRejectsAnOverlappingOrRepeatedRetainedLedger(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	planner := newPlanner(fakeGitInspector{})
	intent := testIntent("managed.md", "managed\n", OwnershipGenerated)
	empty := Ledger{SchemaVersion: CurrentLedgerSchemaVersion}
	overlap := testLedger(testTarget("managed.md", "managed\n", OwnershipGenerated))

	if _, err := planner.Plan(root, empty, []Intent{intent}, overlap); err == nil || !strings.Contains(err.Error(), "adapter intent") {
		t.Fatalf("Plan(retained overlapping an intent) error = %v, want a refusal", err)
	}
	if _, err := planner.Plan(root, overlap, nil, overlap); err == nil || !strings.Contains(err.Error(), "compared ownership ledger") {
		t.Fatalf("Plan(retained overlapping the ledger) error = %v, want a refusal", err)
	}
	if _, err := planner.Plan(root, empty, []Intent{intent}, empty, empty); err == nil || !strings.Contains(err.Error(), "at most one retained") {
		t.Fatalf("Plan(two retained ledgers) error = %v, want a refusal", err)
	}
	if _, err := planner.Plan(root, empty, []Intent{intent}, Ledger{}); err == nil || !strings.Contains(err.Error(), "retained ownership ledger is invalid") {
		t.Fatalf("Plan(unversioned retained ledger) error = %v, want a refusal", err)
	}
}
