package adapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func markdownStub(id string, owner OwnerRef, blockID, body string) stubAdapter {
	return stubAdapter{
		descriptor: testDescriptor(id, "1.0.0"),
		artifacts:  []ArtifactKind{ArtifactRule},
		plan: func(context.Context, PlanRequest) (NativePlan, error) {
			return NativePlan{Adapter: testDescriptor(id, "1.0.0"), Items: []PlanItem{{Owner: owner, Target: "AGENTS.md", Kind: OutputMarkdownInclude, Mode: 0o644}}}, nil
		},
		render: func(context.Context, RenderRequest) ([]Output, error) {
			return []Output{{
				Target: "AGENTS.md", Kind: OutputMarkdownInclude, Mode: 0o644,
				Markdown: []MarkdownInsertion{{Owner: owner, BlockID: blockID, Body: []byte(body)}},
			}}, nil
		},
	}
}

// F1: dropping adapter-a's last contribution must not become a whole-target
// removal while adapter-b still owns the same shared file.
func TestFixRoundSharedTargetIsNotRemovedWhileAnotherAdapterStillOwnsIt(t *testing.T) {
	t.Parallel()

	ownerA := OwnerRef{Source: "github:owner/a", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}
	ownerB := OwnerRef{Source: "github:owner/b", ArtifactID: "rule-b", SourcePath: "rules/b.md", Kind: ArtifactRule}
	observed := "# notes\n" +
		"<!-- ACR:BEGIN block-a -->\nfrom A\n<!-- ACR:END block-a -->\n" +
		"user middle\n" +
		"<!-- ACR:BEGIN block-b -->\nfrom B\n<!-- ACR:END block-b -->\n"
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{{
		Path: "AGENTS.md", Mode: 0o644, Ownership: realize.OwnershipShared, OutputHash: hashContent([]byte(observed)),
		Entries: []realize.Entry{
			{Source: ownerA.Source, ArtifactID: ownerA.ArtifactID, ArtifactKind: realize.ArtifactManagedBlock, SourcePath: ownerA.SourcePath, Adapter: "adapter-a", AdapterVersion: "1.0.0", ManagedHash: hashContent([]byte("from A\n"))},
			{Source: ownerB.Source, ArtifactID: ownerB.ArtifactID, ArtifactKind: realize.ArtifactManagedBlock, SourcePath: ownerB.SourcePath, Adapter: "adapter-b", AdapterVersion: "1.0.0", ManagedHash: hashContent([]byte("from B\n"))},
		},
	}}}

	root := t.TempDir()
	writeTestFile(t, root, "AGENTS.md", observed)
	coordinator, err := NewCoordinator(testCompiler(), markdownStub("adapter-b", ownerB, "block-b", "from B v2\n"))
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-b")}, previous)
	if err != nil || len(intents) != 1 {
		t.Fatalf("Realize() = %#v, %v", intents, err)
	}
	intent := intents[0]
	if intent.Action == realize.ActionRemove {
		t.Fatalf("intent = %#v, a still-owned shared target must not be a final removal", intent)
	}
	content := string(intent.Content)
	if strings.Contains(content, "from A") || strings.Contains(content, "block-a") {
		t.Fatalf("content = %q, want adapter-a's departed block gone", content)
	}
	if !strings.Contains(content, "from B v2") || !strings.Contains(content, "user middle") {
		t.Fatalf("content = %q, want adapter-b's block and unmanaged bytes kept", content)
	}
	if intent.Ownership != realize.OwnershipShared || len(intent.Entries) != 1 || intent.Entries[0].Adapter != "adapter-b" {
		t.Fatalf("intent = %#v, want shared ownership with only adapter-b remaining", intent)
	}

	var applied realize.Ledger
	if _, err := realize.NewEngine().Run(root, previous, intents, realize.ModeApply, func(ledger realize.Ledger) error {
		applied = ledger
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(applied.Targets) != 1 || applied.Targets[0].Ownership != realize.OwnershipShared {
		t.Fatalf("applied ledger = %#v, want the shared target retained", applied)
	}
	written := readTestFile(t, root, "AGENTS.md")
	if strings.Contains(written, "from A") || !strings.Contains(written, "from B v2") || !strings.Contains(written, "user middle") {
		t.Fatalf("realized file = %q", written)
	}
}

func readTestFile(t *testing.T, root, relative string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// F2: rendering the same planned generated-file twice is an extra contribution,
// not a set-union no-op.
func TestFixRoundRejectsPlanItemRenderedTwice(t *testing.T) {
	t.Parallel()

	owner := OwnerRef{Source: "github:owner/pkg", ArtifactID: "rule-a", SourcePath: "rules/rule-a.md", Kind: ArtifactRule}
	twice := stubAdapter{
		descriptor: testDescriptor("hostile", "1.0.0"),
		artifacts:  []ArtifactKind{ArtifactRule},
		plan: func(context.Context, PlanRequest) (NativePlan, error) {
			return NativePlan{Adapter: testDescriptor("hostile", "1.0.0"), Items: []PlanItem{{Owner: owner, Target: "rules/rule-a.md", Kind: OutputGeneratedFile, Mode: 0o644}}}, nil
		},
		render: func(context.Context, RenderRequest) ([]Output, error) {
			file := &GeneratedFile{Owner: owner, Content: []byte("once\n")}
			return []Output{
				{Target: "rules/rule-a.md", Kind: OutputGeneratedFile, Mode: 0o644, File: file},
				{Target: "rules/rule-a.md", Kind: OutputGeneratedFile, Mode: 0o644, File: file},
			}, nil
		},
	}
	coordinator, err := NewCoordinator(nil, twice)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
	if err == nil || len(intents) != 0 {
		t.Fatalf("Realize() = %#v, %v, want rejection of a plan item rendered twice", intents, err)
	}
	if !strings.Contains(err.Error(), "unplanned extra") && !strings.Contains(err.Error(), "does not exactly match the plan") {
		t.Fatalf("error = %v, want a plan/render correspondence failure", err)
	}
}

// F3: a two-hop symlink chain (in-root link → in-root link → outside) must
// not let ReadFile escape the project root.
func TestFixRoundRootSnapshotRejectsSymlinkChainEscapingRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, outside, "secret.md", "outside bytes\n")
	if err := os.Symlink(outside, filepath.Join(dir, "hop")); err != nil {
		t.Fatalf("create symlink on supported platform: %v", err)
	}
	if err := os.Symlink("hop", filepath.Join(dir, "chain")); err != nil {
		t.Fatalf("create relative symlink chain: %v", err)
	}
	snapshot, err := NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	_, err = snapshot.ReadFile("chain/secret.md")
	if err == nil {
		t.Fatal("ReadFile through a two-hop symlink chain succeeded, want rejection")
	}
}

// F4: container segments and keys that contain the old join bytes (NUL, colon)
// must stay distinct under canonicalEntryKey and compile as separate entries.
func TestFixRoundCanonicalKeySurvivesOldSeparatorBytesAndNUL(t *testing.T) {
	t.Parallel()

	type tuple struct {
		container []string
		kind      ConfigEntryKind
		key       string
	}
	tuples := []tuple{
		{[]string{"hooks\x00x"}, ConfigField, "a"},
		{[]string{"hooks", "x"}, ConfigField, "a"},
		{[]string{"hooks:x"}, ConfigField, "a"},
		{[]string{"hooks"}, ConfigField, "x:a"},
		{[]string{"ab"}, ConfigField, "c\x00"},
		{[]string{"a"}, ConfigField, "bc\x00"},
		{[]string{"5:hooks"}, ConfigField, "k"},
		{[]string{"hooks"}, ConfigField, "5:k"},
	}
	seen := map[string]int{}
	for index, candidate := range tuples {
		key := canonicalEntryKey(candidate.container, candidate.kind, candidate.key)
		if previous, collided := seen[key]; collided {
			t.Fatalf("canonicalEntryKey collision between tuple %d and %d: %q", previous, index, key)
		}
		seen[key] = index
	}

	sources := []adapterRender{{
		Descriptor: testDescriptor("fixture", "1.0.0"),
		Outputs: []Output{{
			Target: "hooks.json", Kind: OutputConfigMerge, Mode: 0o644,
			Config: &ConfigMerge{Format: ConfigJSON, Entries: []ConfigEntry{
				{Owner: OwnerRef{Source: "github:owner/a", ArtifactID: "nul-container"}, Container: []string{"hooks\x00x"}, Kind: ConfigField, Key: "a", EncodedValue: jsonValue("one")},
				{Owner: OwnerRef{Source: "github:owner/a", ArtifactID: "split-container"}, Container: []string{"hooks", "x"}, Kind: ConfigField, Key: "a", EncodedValue: jsonValue("two")},
				{Owner: OwnerRef{Source: "github:owner/a", ArtifactID: "colon-key"}, Container: []string{"hooks"}, Kind: ConfigField, Key: "x:a", EncodedValue: jsonValue("three")},
			}},
		}},
	}}
	intents, err := compileOutputs(mapSnapshot{}, realize.Ledger{}, testCompiler(), sources)
	if err != nil {
		t.Fatalf("compileOutputs() = %v, want NUL/colon-bearing tuples accepted as distinct", err)
	}
	if len(intents) != 1 || len(intents[0].Entries) != 3 {
		t.Fatalf("intents = %#v, want all three distinct entries kept", intents)
	}
}
