package adapter

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// These tests cover finding F2 (reviewer): an adapter's rendered Output
// values must correspond exactly to its own NativePlan.Items, so a plan
// with no items can never smuggle an arbitrary rendered output through
// compilation, and one adapter's output can never mask another's
// planned-but-never-rendered target.

func TestCoordinatorRejectsExtraUnplannedOutput(t *testing.T) {
	t.Parallel()

	owner := OwnerRef{Source: "github:owner/pkg", ArtifactID: "rule-a", SourcePath: "rules/rule-a.md", Kind: ArtifactRule}
	hostile := stubAdapter{
		descriptor: testDescriptor("hostile", "1.0.0"),
		artifacts:  []ArtifactKind{ArtifactRule},
		plan: func(context.Context, PlanRequest) (NativePlan, error) {
			return NativePlan{Adapter: testDescriptor("hostile", "1.0.0"), Items: []PlanItem{{Owner: owner, Target: "rules/rule-a.md", Kind: OutputGeneratedFile, Mode: 0o644}}}, nil
		},
		render: func(context.Context, RenderRequest) ([]Output, error) {
			return []Output{
				{Target: "rules/rule-a.md", Kind: OutputGeneratedFile, Mode: 0o644, File: &GeneratedFile{Owner: owner, Content: []byte("planned\n")}},
				{Target: "unplanned.md", Kind: OutputGeneratedFile, Mode: 0o644, File: &GeneratedFile{Owner: owner, Content: []byte("sneaked in\n")}},
			}, nil
		},
	}
	coordinator, err := NewCoordinator(nil, hostile)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
	if err == nil || len(intents) != 0 {
		t.Fatalf("Realize() = %#v, %v, want rejection of the unplanned extra output", intents, err)
	}
	if !strings.Contains(err.Error(), "hostile") {
		t.Fatalf("error = %v, want it to name the offending adapter", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("project tree changed = %v, %v, want untouched", entries, err)
	}
}

func TestCoordinatorRejectsPlannedButNeverRenderedTargetEvenWhenAnotherAdapterSharesIt(t *testing.T) {
	t.Parallel()

	ownerA := OwnerRef{Source: "github:owner/a", ArtifactID: "rule-a", SourcePath: "rules/rule-a.md", Kind: ArtifactRule}
	ownerB := OwnerRef{Source: "github:owner/b", ArtifactID: "rule-b", SourcePath: "rules/rule-b.md", Kind: ArtifactRule}
	// silentA promises AGENTS.md in its plan but never actually renders
	// anything for it.
	silentA := stubAdapter{
		descriptor: testDescriptor("adapter-a", "1.0.0"),
		artifacts:  []ArtifactKind{ArtifactRule},
		plan: func(context.Context, PlanRequest) (NativePlan, error) {
			return NativePlan{Adapter: testDescriptor("adapter-a", "1.0.0"), Items: []PlanItem{{Owner: ownerA, Target: "AGENTS.md", Kind: OutputMarkdownInclude, Mode: 0o644}}}, nil
		},
		render: func(context.Context, RenderRequest) ([]Output, error) { return nil, nil },
	}
	// activeB legitimately shares the same target (allowed) with its own
	// plan/render correspondence intact.
	activeB := stubAdapter{
		descriptor: testDescriptor("adapter-b", "1.0.0"),
		artifacts:  []ArtifactKind{ArtifactRule},
		plan: func(context.Context, PlanRequest) (NativePlan, error) {
			return NativePlan{Adapter: testDescriptor("adapter-b", "1.0.0"), Items: []PlanItem{{Owner: ownerB, Target: "AGENTS.md", Kind: OutputMarkdownInclude, Mode: 0o644}}}, nil
		},
		render: func(context.Context, RenderRequest) ([]Output, error) {
			return []Output{{Target: "AGENTS.md", Kind: OutputMarkdownInclude, Mode: 0o644, Markdown: []MarkdownInsertion{{Owner: ownerB, BlockID: "block-b", Body: []byte("from B\n")}}}}, nil
		},
	}
	coordinator, err := NewCoordinator(testCompiler(), silentA, activeB)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
	if err == nil || len(intents) != 0 {
		t.Fatalf("Realize() = %#v, %v, want rejection: adapter-a planned AGENTS.md but never rendered it, regardless of adapter-b's independent contribution", intents, err)
	}
	if !strings.Contains(err.Error(), "adapter-a") {
		t.Fatalf("error = %v, want it to name adapter-a, not the unrelated adapter-b", err)
	}
}

func TestCoordinatorRejectsRenderDriftFromPlan(t *testing.T) {
	t.Parallel()

	planned := PlanItem{
		Owner:  OwnerRef{Source: "github:owner/pkg", ArtifactID: "rule-a", SourcePath: "rules/rule-a.md", Kind: ArtifactRule},
		Target: "rules/rule-a.md", Kind: OutputGeneratedFile, Mode: 0o644,
	}
	cases := map[string]Output{
		"owner drift": {
			Target: planned.Target, Kind: planned.Kind, Mode: planned.Mode,
			File: &GeneratedFile{Owner: OwnerRef{Source: "github:owner/pkg", ArtifactID: "rule-b", SourcePath: "rules/rule-b.md", Kind: ArtifactRule}, Content: []byte("x\n")},
		},
		"kind drift": {
			Target: planned.Target, Kind: OutputMarkdownInclude, Mode: planned.Mode,
			Markdown: []MarkdownInsertion{{Owner: planned.Owner, BlockID: "b", Body: []byte("x\n")}},
		},
		"mode drift": {
			Target: planned.Target, Kind: planned.Kind, Mode: 0o600,
			File: &GeneratedFile{Owner: planned.Owner, Content: []byte("x\n")},
		},
		"target drift": {
			Target: "rules/somewhere-else.md", Kind: planned.Kind, Mode: planned.Mode,
			File: &GeneratedFile{Owner: planned.Owner, Content: []byte("x\n")},
		},
	}
	for name, rendered := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			drift := stubAdapter{
				descriptor: testDescriptor("hostile", "1.0.0"),
				artifacts:  []ArtifactKind{ArtifactRule},
				plan: func(context.Context, PlanRequest) (NativePlan, error) {
					return NativePlan{Adapter: testDescriptor("hostile", "1.0.0"), Items: []PlanItem{planned}}, nil
				},
				render: func(context.Context, RenderRequest) ([]Output, error) {
					return []Output{rendered}, nil
				},
			}
			coordinator, err := NewCoordinator(testCompiler(), drift)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
			if err == nil || len(intents) != 0 {
				t.Fatalf("Realize() = %#v, %v, want rejection of %s between plan and render", intents, err, name)
			}
		})
	}
}

func TestCoordinatorRejectsPlanStampedForAnotherDescriptor(t *testing.T) {
	t.Parallel()

	mismatched := stubAdapter{
		descriptor: testDescriptor("hostile", "1.0.0"),
		artifacts:  []ArtifactKind{ArtifactRule},
		plan: func(context.Context, PlanRequest) (NativePlan, error) {
			return NativePlan{Adapter: testDescriptor("someone-else", "9.9.9")}, nil
		},
	}
	coordinator, err := NewCoordinator(nil, mismatched)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
	if err == nil || len(intents) != 0 {
		t.Fatalf("Realize() = %#v, %v, want rejection of a plan stamped for a different descriptor", intents, err)
	}
}

// F2 (reviewer): a non-nil ConfigMerge with zero Entries rendered zero
// plan/render-correspondence tuples, so an adapter with a completely empty
// plan could render one, pass verifyPlanRenderCorrespondence trivially
// (0 == 0), and reach compileOutputs as a real contribution to any target
// it names - all while its own Validate never sees it, since that
// adapter's plan.Items (and therefore its derived candidates) stay empty.
func TestCoordinatorRejectsEmptyConfigMergeFromEmptyPlanBypass(t *testing.T) {
	t.Parallel()

	// A previously shared config target owned by an unrelated source.
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{{
		Path: "hooks.json", Mode: 0o644, Ownership: realize.OwnershipShared, OutputHash: hashContent([]byte("{}\n")),
		Entries: []realize.Entry{{Source: "github:owner/victim", ArtifactID: "hook-a", ArtifactKind: realize.ArtifactStructuredEntry, SourcePath: "hooks/a.sh", Adapter: "victim-adapter", AdapterVersion: "1.0.0", ManagedHash: hashContent([]byte("{}"))}},
	}}}

	// hostile renders no plan items at all, only an empty-entries
	// ConfigMerge output for the victim's shared target.
	hostile := stubAdapter{
		descriptor: testDescriptor("hostile", "1.0.0"),
		artifacts:  []ArtifactKind{ArtifactRule},
		render: func(context.Context, RenderRequest) ([]Output, error) {
			return []Output{{
				Target: "hooks.json", Kind: OutputConfigMerge, Mode: 0o644,
				Config: &ConfigMerge{Format: ConfigJSON, Entries: nil},
			}}, nil
		},
	}
	coordinator, err := NewCoordinator(testCompiler(), hostile)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeTestFile(t, root, "hooks.json", "{}\n")
	intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, previous)
	var malformed *MalformedOutputError
	if !errors.As(err, &malformed) || len(intents) != 0 {
		t.Fatalf("Realize() = %#v, %v, want rejection of the empty-plan/empty-config-merge bypass", intents, err)
	}
	if got := readTestFile(t, root, "hooks.json"); got != "{}\n" {
		t.Fatalf("victim's file changed = %q", got)
	}
}
