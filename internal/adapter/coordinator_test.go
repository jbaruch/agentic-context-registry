package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func testPackage(ruleID string) Package {
	return Package{
		Source: "github:owner/pkg",
		Manifest: manifest.Manifest{
			Artifacts: manifest.Artifacts{Rules: []manifest.RuleArtifact{{ID: ruleID, Path: "rules/" + ruleID + ".md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}}},
		},
	}
}

// ruleFileAdapter renders every rule artifact as a whole generated file at
// rules/<id>.md, independent of any real native format; it exists only to
// exercise the #10 boundary end to end.
func ruleFileAdapter(id, version string) stubAdapter {
	return stubAdapter{
		descriptor: testDescriptor(id, version),
		artifacts:  []ArtifactKind{ArtifactRule},
		plan: func(_ context.Context, request PlanRequest) (NativePlan, error) {
			var items []PlanItem
			for _, pkg := range request.Packages {
				for _, rule := range pkg.Manifest.Artifacts.Rules {
					items = append(items, PlanItem{
						Owner:  OwnerRef{Source: pkg.Source, ArtifactID: rule.ID, SourcePath: rule.Path, Kind: ArtifactRule},
						Target: "rules/" + rule.ID + ".md", Kind: OutputGeneratedFile, Mode: 0o644,
					})
				}
			}
			return NativePlan{Adapter: testDescriptor(id, version), Items: items}, nil
		},
		render: func(_ context.Context, request RenderRequest) ([]Output, error) {
			outputs := make([]Output, 0, len(request.Plan.Items))
			for _, item := range request.Plan.Items {
				outputs = append(outputs, Output{
					Target: item.Target, Mode: item.Mode, Kind: OutputGeneratedFile,
					File: &GeneratedFile{Owner: item.Owner, Content: []byte("managed by " + item.Owner.ArtifactID + "\n")},
				})
			}
			return outputs, nil
		},
	}
}

func TestCoordinatorReportsUnsupportedCombinationBeforeAnyAdapterCall(t *testing.T) {
	t.Parallel()

	planCalls, renderCalls := 0, 0
	noHooks := stubAdapter{
		descriptor: testDescriptor("no-hooks", "1.0.0"),
		artifacts:  []ArtifactKind{ArtifactRule},
		plan:       func(context.Context, PlanRequest) (NativePlan, error) { planCalls++; return NativePlan{}, nil },
		render:     func(context.Context, RenderRequest) ([]Output, error) { renderCalls++; return nil, nil },
	}
	pkg := Package{Source: "github:owner/pkg", Manifest: manifest.Manifest{
		Artifacts: manifest.Artifacts{Hooks: []manifest.HookArtifact{{ID: "hook-a", Path: "hooks/a.sh", Event: manifest.HookSessionStart}}},
	}}
	coordinator, err := NewCoordinator(nil, noHooks)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{pkg}, realize.Ledger{})
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) || len(intents) != 0 {
		t.Fatalf("Realize() = %#v, %v, want *UnsupportedError with no intents", intents, err)
	}
	if planCalls != 0 || renderCalls != 0 {
		t.Fatalf("Plan/Render called after an unsupported-combination preflight failure: plan=%d render=%d", planCalls, renderCalls)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("project tree changed = %v, %v, want untouched", entries, err)
	}
}

// NEW-4 (reviewer): Realize/RealizeWithNotices accepted arbitrary variadic
// targetOptions map counts but forwarded only the first to compileOutputs,
// so a second map's overrides (Force, ConfigFormat, ExplicitDemotion)
// silently disappeared. Passing two must reject the call before any adapter
// runs, not drop the second map.
func TestCoordinatorRejectsMoreThanOneTargetOptionsMapBeforeAnyAdapterCall(t *testing.T) {
	t.Parallel()

	planCalls, renderCalls := 0, 0
	fixture := stubAdapter{
		descriptor: testDescriptor("fixture", "1.0.0"),
		artifacts:  []ArtifactKind{ArtifactRule},
		plan:       func(context.Context, PlanRequest) (NativePlan, error) { planCalls++; return NativePlan{}, nil },
		render:     func(context.Context, RenderRequest) ([]Output, error) { renderCalls++; return nil, nil },
	}
	coordinator, err := NewCoordinator(nil, fixture)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	first := map[string]TargetOptions{"AGENTS.md": {Force: true}}
	second := map[string]TargetOptions{"AGENTS.md": {ExplicitDemotion: true}}
	intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{}, first, second)
	if err == nil || len(intents) != 0 {
		t.Fatalf("Realize(two targetOptions maps) = %#v, %v, want an error and no intents", intents, err)
	}
	if planCalls != 0 || renderCalls != 0 {
		t.Fatalf("Plan/Render called after a rejected multi-map targetOptions call: plan=%d render=%d", planCalls, renderCalls)
	}
}

func TestCoordinatorNeverReturnsIntentsAfterAnyStageError(t *testing.T) {
	t.Parallel()

	injected := errors.New("injected failure")
	cases := map[string]stubAdapter{
		"plan": {descriptor: testDescriptor("fixture", "1.0.0"), artifacts: []ArtifactKind{ArtifactRule}, plan: func(context.Context, PlanRequest) (NativePlan, error) { return NativePlan{}, injected }},
		"render": {
			descriptor: testDescriptor("fixture", "1.0.0"), artifacts: []ArtifactKind{ArtifactRule},
			render: func(context.Context, RenderRequest) ([]Output, error) { return nil, injected },
		},
		"validate": {
			descriptor: testDescriptor("fixture", "1.0.0"), artifacts: []ArtifactKind{ArtifactRule},
			plan: func(_ context.Context, request PlanRequest) (NativePlan, error) {
				return NativePlan{
					Adapter: testDescriptor("fixture", "1.0.0"),
					Items:   []PlanItem{{Target: "rules/rule-a.md", Kind: OutputGeneratedFile, Mode: 0o644, Owner: OwnerRef{Source: "github:owner/pkg", ArtifactID: "rule-a", SourcePath: "rules/rule-a.md", Kind: ArtifactRule}}},
				}, nil
			},
			render: func(_ context.Context, request RenderRequest) ([]Output, error) {
				item := request.Plan.Items[0]
				return []Output{{Target: item.Target, Mode: item.Mode, Kind: OutputGeneratedFile, File: &GeneratedFile{Owner: item.Owner, Content: []byte("x")}}}, nil
			},
			validate: func(context.Context, ValidateRequest) error { return injected },
		},
	}
	for name, candidate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			coordinator, err := NewCoordinator(nil, candidate)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
			if err == nil || len(intents) != 0 {
				t.Fatalf("Realize() = %#v, %v, want an error and no intents", intents, err)
			}
		})
	}
}

func TestCoordinatorEndToEndAppliesThroughRealizeEngine(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	coordinator, err := NewCoordinator(nil, ruleFileAdapter("fixture", "1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
	if err != nil || len(intents) != 1 {
		t.Fatalf("Realize() = %#v, %v", intents, err)
	}

	var persisted realize.Ledger
	plan, err := realize.NewEngine().Run(root, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, intents, realize.ModeApply, func(ledger realize.Ledger) error {
		persisted = ledger
		return nil
	})
	if err != nil || !plan.HasChanges() {
		t.Fatalf("Run(apply) = %#v, %v", plan, err)
	}
	content, err := os.ReadFile(filepath.Join(root, "rules", "rule-a.md"))
	if err != nil || string(content) != "managed by rule-a\n" {
		t.Fatalf("realized content = %q, %v", content, err)
	}
	if len(persisted.Targets) != 1 || persisted.Targets[0].Entries[0].Adapter != "fixture" {
		t.Fatalf("persisted ledger = %#v", persisted)
	}

	unchanged, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, persisted)
	if err != nil {
		t.Fatal(err)
	}
	second, err := realize.NewEngine().Run(root, persisted, unchanged, realize.ModeCheck, nil)
	if err != nil || second.HasChanges() {
		t.Fatalf("second Run(check) = %#v, %v, want an empty plan", second, err)
	}
}

func TestCoordinatorAdapterVersionBumpProducesReviewablePlanWithoutWrites(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	original, err := NewCoordinator(nil, ruleFileAdapter("fixture", "1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	firstIntents, err := original.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
	if err != nil {
		t.Fatal(err)
	}
	var persisted realize.Ledger
	if _, err := realize.NewEngine().Run(root, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, firstIntents, realize.ModeApply, func(ledger realize.Ledger) error {
		persisted = ledger
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "rules", "rule-a.md"))
	if err != nil {
		t.Fatal(err)
	}

	upgraded, err := NewCoordinator(nil, ruleFileAdapter("fixture", "1.1.0"))
	if err != nil {
		t.Fatal(err)
	}
	secondIntents, err := upgraded.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, persisted)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondIntents) != 1 || secondIntents[0].Entries[0].AdapterVersion != "1.1.0" || string(secondIntents[0].Content) != string(before) {
		t.Fatalf("version-bump intents = %#v, want identical bytes with the new adapter version", secondIntents)
	}

	dry, err := realize.NewEngine().Run(root, persisted, secondIntents, realize.ModeDryRun, nil)
	if err != nil || !dry.HasChanges() || !dry.LedgerChanged {
		t.Fatalf("Run(dry-run) = %#v, %v, want a non-empty, ledger-changing plan", dry, err)
	}
	if _, err := realize.NewEngine().Run(root, persisted, secondIntents, realize.ModeCheck, nil); err == nil {
		t.Fatal("Run(check) error = nil, want ChangesError for the pending adapter version bump")
	}
	after, err := os.ReadFile(filepath.Join(root, "rules", "rule-a.md"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("dry-run/check wrote the file: before=%q after=%q", before, after)
	}

	var upgradedLedger realize.Ledger
	apply, err := realize.NewEngine().Run(root, persisted, secondIntents, realize.ModeApply, func(ledger realize.Ledger) error {
		upgradedLedger = ledger
		return nil
	})
	if err != nil || !apply.HasChanges() {
		t.Fatalf("Run(apply) = %#v, %v", apply, err)
	}
	if len(upgradedLedger.Targets) != 1 || upgradedLedger.Targets[0].Entries[0].AdapterVersion != "1.1.0" {
		t.Fatalf("persisted ledger after apply = %#v, want adapterVersion 1.1.0", upgradedLedger)
	}
	if upgradedLedger.Targets[0].OutputHash != persisted.Targets[0].OutputHash {
		t.Fatalf("metadata-only upgrade changed the output hash = %q, want %q", upgradedLedger.Targets[0].OutputHash, persisted.Targets[0].OutputHash)
	}

	// A third run against the now-1.1.0-stamped ledger, with the same
	// upgraded adapter still selected, must be a true no-op.
	thirdIntents, err := upgraded.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, upgradedLedger)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := realize.NewEngine().Run(root, upgradedLedger, thirdIntents, realize.ModeCheck, nil)
	if err != nil || empty.HasChanges() || len(empty.Operations) != 0 {
		t.Fatalf("third run after the upgrade was applied = %#v, %v, want a fully empty plan", empty, err)
	}
}

// F1a (reviewer): ExplicitDemotion/Force must be a coordinator-owned,
// per-target options input the caller supplies to Realize, never data an
// adapter can set. These tests thread TargetOptions through Coordinator
// into the SharedCompiler and prove the engine's own demotion mechanics
// (OperationDemote, sticky ownership) respond to it end to end.

func TestCoordinatorAppliesExplicitDemotionThroughEngine(t *testing.T) {
	t.Parallel()

	owner := OwnerRef{Source: "github:owner/a", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}
	body := "body\n"
	observed := "<!-- ACR:BEGIN block-a -->\n" + body + "<!-- ACR:END block-a -->\n"
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{{
		Path: "AGENTS.md", Mode: 0o644, Ownership: realize.OwnershipShared, OutputHash: hashContent([]byte(observed)),
		Entries: []realize.Entry{{Source: owner.Source, ArtifactID: owner.ArtifactID, ArtifactKind: realize.ArtifactManagedBlock, SourcePath: owner.SourcePath, Adapter: "adapter-a", AdapterVersion: "1.0.0", ManagedHash: hashContent([]byte(body))}},
	}}}

	root := t.TempDir()
	writeTestFile(t, root, "AGENTS.md", observed)
	coordinator, err := NewCoordinator(testCompiler(), markdownStub("adapter-a", owner, "block-a", body))
	if err != nil {
		t.Fatal(err)
	}

	options := map[string]TargetOptions{"AGENTS.md": {ExplicitDemotion: true}}
	intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, previous, options)
	if err != nil || len(intents) != 1 {
		t.Fatalf("Realize() = %#v, %v", intents, err)
	}
	if intents[0].Ownership != realize.OwnershipGenerated || !intents[0].ExplicitDemotion {
		t.Fatalf("intent = %#v, want a generated-only intent with ExplicitDemotion set", intents[0])
	}

	var applied realize.Ledger
	plan, err := realize.NewEngine().Run(root, previous, intents, realize.ModeApply, func(ledger realize.Ledger) error {
		applied = ledger
		return nil
	})
	if err != nil || !plan.HasChanges() {
		t.Fatalf("Run(apply) = %#v, %v", plan, err)
	}
	if !hasOperationKind(plan, realize.OperationDemote) {
		t.Fatalf("plan operations = %#v, want a demote operation", plan.Operations)
	}
	if len(applied.Targets) != 1 || applied.Targets[0].Ownership != realize.OwnershipGenerated {
		t.Fatalf("applied ledger = %#v, want generated-only ownership after demotion", applied)
	}
}

func TestCoordinatorWithoutExplicitDemotionOptionStaysShared(t *testing.T) {
	t.Parallel()

	owner := OwnerRef{Source: "github:owner/a", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}
	body := "body\n"
	observed := "<!-- ACR:BEGIN block-a -->\n" + body + "<!-- ACR:END block-a -->\n"
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{{
		Path: "AGENTS.md", Mode: 0o644, Ownership: realize.OwnershipShared, OutputHash: hashContent([]byte(observed)),
		Entries: []realize.Entry{{Source: owner.Source, ArtifactID: owner.ArtifactID, ArtifactKind: realize.ArtifactManagedBlock, SourcePath: owner.SourcePath, Adapter: "adapter-a", AdapterVersion: "1.0.0", ManagedHash: hashContent([]byte(body))}},
	}}}

	root := t.TempDir()
	writeTestFile(t, root, "AGENTS.md", observed)
	coordinator, err := NewCoordinator(testCompiler(), markdownStub("adapter-a", owner, "block-a", body))
	if err != nil {
		t.Fatal(err)
	}

	// No TargetOptions this run: the compiler must not demote on its own
	// initiative, and the engine must refuse a demotion nobody authorized.
	intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, previous)
	if err != nil || len(intents) != 1 {
		t.Fatalf("Realize() = %#v, %v", intents, err)
	}
	if intents[0].Ownership != realize.OwnershipShared || intents[0].ExplicitDemotion {
		t.Fatalf("intent = %#v, want ownership to stay shared without an ExplicitDemotion option", intents[0])
	}

	before := readTestFile(t, root, "AGENTS.md")
	finalized := false
	plan, err := realize.NewEngine().Run(root, previous, intents, realize.ModeApply, func(realize.Ledger) error {
		finalized = true
		return nil
	})
	var conflict *realize.ConflictError
	if !errors.As(err, &conflict) || finalized {
		t.Fatalf("Run(apply) = %#v, %v, finalized = %t; want a ConflictError with the finalizer never called", plan, err, finalized)
	}
	if after := readTestFile(t, root, "AGENTS.md"); after != before {
		t.Fatalf("file changed despite the conflict: before=%q after=%q", before, after)
	}
}

// F1a (reviewer): Force must reach the compiler the same way ExplicitDemotion
// does — a coordinator-owned, per-target option threaded from the caller's
// TargetOptions, never something an adapter can set. forceCapturingCompiler
// wraps the real test compiler and records the SharedTarget.Force it
// actually observed for each target path, so these tests prove the value
// Coordinator.Realize hands the compiler rather than inferring it from
// output shape.
type forceCapturingCompiler struct {
	SharedCompiler
	seen map[string]bool
}

func newForceCapturingCompiler() *forceCapturingCompiler {
	return &forceCapturingCompiler{SharedCompiler: testCompiler(), seen: map[string]bool{}}
}

func (compiler *forceCapturingCompiler) CompileMarkdown(ctx context.Context, request MarkdownCompileRequest) (SharedCompilation, error) {
	compiler.seen[request.Target.Path] = request.Target.Force
	return compiler.SharedCompiler.CompileMarkdown(ctx, request)
}

func TestCoordinatorAppliesForceOptionThroughToCompiler(t *testing.T) {
	t.Parallel()

	owner := OwnerRef{Source: "github:owner/a", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}
	root := t.TempDir()
	compiler := newForceCapturingCompiler()
	coordinator, err := NewCoordinator(compiler, markdownStub("adapter-a", owner, "block-a", "body\n"))
	if err != nil {
		t.Fatal(err)
	}

	options := map[string]TargetOptions{"AGENTS.md": {Force: true}}
	if _, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{}, options); err != nil {
		t.Fatal(err)
	}
	if force, seen := compiler.seen["AGENTS.md"]; !seen || !force {
		t.Fatalf("compiler observed Force = %v, seen = %t for AGENTS.md, want true", force, seen)
	}
}

func TestCoordinatorWithoutForceOptionLeavesCompilerForceFalse(t *testing.T) {
	t.Parallel()

	owner := OwnerRef{Source: "github:owner/a", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}
	root := t.TempDir()
	compiler := newForceCapturingCompiler()
	coordinator, err := NewCoordinator(compiler, markdownStub("adapter-a", owner, "block-a", "body\n"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{}); err != nil {
		t.Fatal(err)
	}
	if force, seen := compiler.seen["AGENTS.md"]; !seen || force {
		t.Fatalf("compiler observed Force = %v, seen = %t for AGENTS.md, want false", force, seen)
	}
}

func hasOperationKind(plan realize.Plan, kind realize.OperationKind) bool {
	for _, operation := range plan.Operations {
		if operation.Kind == kind {
			return true
		}
	}
	return false
}
