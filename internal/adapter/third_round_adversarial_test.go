package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// F1a: ExplicitDemotion with leftover unmanaged bytes must not wipe them
// or silently reclassify the file as generated-only.
func TestThirdRoundExplicitDemotionWithLeftoverUnmanagedStaysShared(t *testing.T) {
	t.Parallel()

	owner := OwnerRef{Source: "github:owner/a", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}
	body := "body\n"
	observed := "user leftover\n<!-- ACR:BEGIN block-a -->\n" + body + "<!-- ACR:END block-a -->\n"
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
	if intents[0].Ownership != realize.OwnershipShared {
		t.Fatalf("intent = %#v, leftover unmanaged bytes must keep ownership shared even with ExplicitDemotion", intents[0])
	}
	if _, err := realize.NewEngine().Run(root, previous, intents, realize.ModeApply, func(realize.Ledger) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, root, "AGENTS.md"); !strings.Contains(got, "user leftover") {
		t.Fatalf("demotion wiped unmanaged bytes = %q", got)
	}
}

// F1b: extensionless target named exactly "hooks" through create, update,
// and trusted-format final removal.
func TestThirdRoundExtensionlessHooksTargetCreateUpdateRemove(t *testing.T) {
	t.Parallel()

	owner := OwnerRef{Source: "github:owner/a", ArtifactID: "hook-a", SourcePath: "hooks/a.sh", Kind: ArtifactHook}
	const target = "hooks"
	entry := func(id string) ConfigEntry {
		return ConfigEntry{Owner: owner, Container: []string{"hooks"}, Kind: ConfigField, Key: "mine", EncodedValue: jsonValue(map[string]any{"id": id})}
	}
	sources := func(id string) []adapterRender {
		return []adapterRender{{
			Descriptor: testDescriptor("adapter-a", "1.0.0"),
			Outputs: []Output{{
				Target: target, Kind: OutputConfigMerge, Mode: 0o644,
				Config: &ConfigMerge{Format: ConfigJSON, Entries: []ConfigEntry{entry(id)}},
			}},
		}}
	}

	create, err := compileOutputs(context.Background(), mapSnapshot{}, realize.Ledger{}, testCompiler(), sources("v1"))
	if err != nil || len(create) != 1 || create[0].Ownership != realize.OwnershipGenerated {
		t.Fatalf("create = %#v, %v", create, err)
	}
	root := t.TempDir()
	var ledger realize.Ledger
	if _, err := realize.NewEngine().Run(root, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, create, realize.ModeApply, func(next realize.Ledger) error {
		ledger = next
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, target)); err != nil {
		t.Fatalf("create did not write %s: %v", target, err)
	}

	observed, err := os.ReadFile(filepath.Join(root, target))
	if err != nil {
		t.Fatal(err)
	}
	update, err := compileOutputs(context.Background(), mapSnapshot{target: observed}, ledger, testCompiler(), sources("v2"))
	if err != nil || len(update) != 1 {
		t.Fatalf("update = %#v, %v", update, err)
	}
	if _, err := realize.NewEngine().Run(root, ledger, update, realize.ModeApply, func(next realize.Ledger) error {
		ledger = next
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	updated := readTestFile(t, root, target)
	if !strings.Contains(updated, `"id": "v2"`) && !strings.Contains(updated, `"id":"v2"`) {
		t.Fatalf("update content = %q, want id v2", updated)
	}

	observed, err = os.ReadFile(filepath.Join(root, target))
	if err != nil {
		t.Fatal(err)
	}
	options := map[string]TargetOptions{target: {ConfigFormat: ConfigJSON}}
	remove, err := compileOutputs(context.Background(), mapSnapshot{target: observed}, ledger, testCompiler(), nil, options)
	if err != nil || len(remove) != 1 || remove[0].Action != realize.ActionRemove {
		t.Fatalf("remove = %#v, %v, want ActionRemove", remove, err)
	}
	var after realize.Ledger
	if _, err := realize.NewEngine().Run(root, ledger, remove, realize.ModeApply, func(next realize.Ledger) error {
		after = next
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(after.Targets) != 0 {
		t.Fatalf("ledger after removal = %#v, want empty", after)
	}
}

// F2: a planned config-merge item rendered with zero entries is extra/missing,
// not a silent removal of a shared target.
func TestThirdRoundPlannedConfigMergeWithZeroEntriesRejected(t *testing.T) {
	t.Parallel()

	owner := OwnerRef{Source: "github:owner/pkg", ArtifactID: "hook-a", SourcePath: "hooks/a.sh", Kind: ArtifactHook}
	hostile := stubAdapter{
		descriptor: testDescriptor("hostile", "1.0.0"),
		artifacts:  []ArtifactKind{ArtifactRule},
		plan: func(context.Context, PlanRequest) (NativePlan, error) {
			return NativePlan{Adapter: testDescriptor("hostile", "1.0.0"), Items: []PlanItem{{Owner: owner, Target: "hooks.json", Kind: OutputConfigMerge, Mode: 0o644}}}, nil
		},
		render: func(context.Context, RenderRequest) ([]Output, error) {
			return []Output{{
				Target: "hooks.json", Kind: OutputConfigMerge, Mode: 0o644,
				Config: &ConfigMerge{Format: ConfigJSON, Entries: []ConfigEntry{}},
			}}, nil
		},
	}
	root := t.TempDir()
	writeTestFile(t, root, "hooks.json", "{}\n")
	coordinator, err := NewCoordinator(testCompiler(), hostile)
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
	if err == nil || len(intents) != 0 {
		t.Fatalf("Realize() = %#v, %v, want rejection of a planned empty config-merge", intents, err)
	}
	if got := readTestFile(t, root, "hooks.json"); got != "{}\n" {
		t.Fatalf("file changed = %q", got)
	}
}

// F3: relative in-root symlink parent (alias -> real) must still be rejected.
func TestThirdRoundRootSnapshotRejectsRelativeInRootSymlinkParent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "real/AGENTS.md", "hello\n")
	if err := os.Symlink("real", filepath.Join(dir, "alias")); err != nil {
		t.Fatalf("create relative in-root symlink: %v", err)
	}
	snapshot, err := NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	_, err = snapshot.ReadFile("alias/AGENTS.md")
	if err == nil {
		t.Fatal("ReadFile through a relative in-root symlink parent succeeded, want rejection")
	}
}

// NEW-1: three adapters, identical OwnerRef, one departs — remaining two
// keep their own adapter stamps; the departed block is gone.
func TestThirdRoundThreeAdaptersSameOwnerOneDeparts(t *testing.T) {
	t.Parallel()

	shared := OwnerRef{Source: "github:owner/shared-pkg", ArtifactID: "shared-artifact", SourcePath: "shared/path.md", Kind: ArtifactRule}
	block := func(id, adapterID, body string) adapterRender {
		return adapterRender{
			Descriptor: testDescriptor(adapterID, "1.0.0"),
			Outputs:    []Output{{Target: "AGENTS.md", Kind: OutputMarkdownInclude, Mode: 0o644, Markdown: []MarkdownInsertion{{Owner: shared, BlockID: id, Body: []byte(body)}}}},
		}
	}
	all := []adapterRender{block("block-a", "adapter-a", "from A\n"), block("block-b", "adapter-b", "from B\n"), block("block-c", "adapter-c", "from C\n")}
	first, err := compileOutputs(context.Background(), mapSnapshot{}, realize.Ledger{}, testCompiler(), all)
	if err != nil || len(first) != 1 || len(first[0].Entries) != 3 {
		t.Fatalf("first compile = %#v, %v", first, err)
	}
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{{
		Path: "AGENTS.md", Mode: 0o644, Ownership: realize.OwnershipShared, OutputHash: hashContent(first[0].Content),
		Entries: first[0].Entries,
	}}}
	remaining, err := compileOutputs(context.Background(), mapSnapshot{"AGENTS.md": first[0].Content}, previous, testCompiler(), []adapterRender{all[1], all[2]})
	if err != nil || len(remaining) != 1 || len(remaining[0].Entries) != 2 {
		t.Fatalf("remaining compile = %#v, %v, want two entries after A departed", remaining, err)
	}
	content := string(remaining[0].Content)
	if strings.Contains(content, "from A") || strings.Contains(content, "block-a") {
		t.Fatalf("content = %q, want A's block gone", content)
	}
	byAdapter := map[string]realize.Entry{}
	for _, entry := range remaining[0].Entries {
		byAdapter[entry.Adapter] = entry
	}
	if byAdapter["adapter-b"].ManagedHash != hashContent([]byte("from B\n")) || byAdapter["adapter-c"].ManagedHash != hashContent([]byte("from C\n")) {
		t.Fatalf("entries = %#v, want B and C to keep their own adapter stamps", remaining[0].Entries)
	}
	if _, keptA := byAdapter["adapter-a"]; keptA {
		t.Fatalf("adapter-a still stamped after departing: %#v", remaining[0].Entries)
	}
}

// cancelDuringCompile cancels the caller's context the moment the compiler
// is entered, then returns ctx.Err() — proving cancellation mid-compile
// yields no intents and no writes.
type cancelDuringCompile struct {
	cancel context.CancelFunc
}

func (c cancelDuringCompile) CompileMarkdown(ctx context.Context, _ MarkdownCompileRequest) (SharedCompilation, error) {
	c.cancel()
	if err := ctx.Err(); err != nil {
		return SharedCompilation{}, err
	}
	return SharedCompilation{}, errors.New("cancelDuringCompile: context still live after cancel")
}

func (c cancelDuringCompile) CompileConfig(ctx context.Context, _ ConfigCompileRequest) (SharedCompilation, error) {
	c.cancel()
	if err := ctx.Err(); err != nil {
		return SharedCompilation{}, err
	}
	return SharedCompilation{}, errors.New("cancelDuringCompile: context still live after cancel")
}

// NEW-2: cancel the context mid-compile; no intents, no writes.
func TestThirdRoundCancelDuringCompileWritesNothing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := OwnerRef{Source: "github:owner/a", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}
	root := t.TempDir()
	writeTestFile(t, root, "sentinel.txt", "untouched\n")
	coordinator, err := NewCoordinator(cancelDuringCompile{cancel: cancel}, markdownStub("adapter-a", owner, "block-a", "body\n"))
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(ctx, NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
	if !errors.Is(err, context.Canceled) || len(intents) != 0 {
		t.Fatalf("Realize() = %#v, %v, want context.Canceled and no intents", intents, err)
	}
	if got := readTestFile(t, root, "sentinel.txt"); got != "untouched\n" {
		t.Fatalf("tree mutated = %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(root, "AGENTS.md")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("AGENTS.md appeared after cancelled compile: %v", statErr)
	}
}
