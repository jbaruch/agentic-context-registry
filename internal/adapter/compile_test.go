package adapter

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestCompileGeneratedFileCreatesFromMissingTarget(t *testing.T) {
	t.Parallel()

	project := mapSnapshot{}
	sources := []adapterRender{{
		Descriptor: testDescriptor("fixture", "1.0.0"),
		Outputs: []Output{{
			Target: "generated/rule.md", Kind: OutputGeneratedFile, Mode: 0o644,
			File: &GeneratedFile{Owner: OwnerRef{Source: "github:owner/pkg", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}, Content: []byte("managed\n")},
		}},
	}}
	intents, err := compileOutputs(project, realize.Ledger{}, nil, sources)
	if err != nil || len(intents) != 1 {
		t.Fatalf("compileOutputs() = %#v, %v", intents, err)
	}
	intent := intents[0]
	if intent.Ownership != realize.OwnershipGenerated || string(intent.Content) != "managed\n" || intent.ObservedHash != "" || intent.ManagedIntact || len(intent.PreservedContent) != 0 {
		t.Fatalf("compiled intent = %#v, want plain generated-only create with no merge-binding fields", intent)
	}
	if len(intent.Entries) != 1 || intent.Entries[0].Adapter != "fixture" || intent.Entries[0].AdapterVersion != "1.0.0" || intent.Entries[0].ArtifactKind != realize.ArtifactFile {
		t.Fatalf("compiled entries = %#v", intent.Entries)
	}
}

func TestCompileGeneratedFileRejectsExistingSharedTarget(t *testing.T) {
	t.Parallel()

	project := mapSnapshot{"AGENTS.md": []byte("user content\n")}
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{{
		Path: "AGENTS.md", Mode: 0o644, Ownership: realize.OwnershipShared, OutputHash: hashContent([]byte("user content\n")),
		Entries: []realize.Entry{{Source: "github:owner/pkg", ArtifactID: "rule-a", ArtifactKind: realize.ArtifactManagedBlock, SourcePath: "rules/a.md", Adapter: "fixture", AdapterVersion: "1.0.0", ManagedHash: hashContent([]byte("x"))}},
	}}}
	sources := []adapterRender{{
		Descriptor: testDescriptor("hostile", "1.0.0"),
		Outputs: []Output{{
			Target: "AGENTS.md", Kind: OutputGeneratedFile, Mode: 0o644,
			File: &GeneratedFile{Owner: OwnerRef{Source: "github:owner/pkg", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}, Content: []byte("entirely new bytes\n")},
		}},
	}}
	_, err := compileOutputs(project, previous, nil, sources)
	var malformed *MalformedOutputError
	if !errors.As(err, &malformed) || malformed.Target != "AGENTS.md" {
		t.Fatalf("compileOutputs() error = %v, want *MalformedOutputError rejecting whole-file replace of a shared target", err)
	}
}

func TestCompileGeneratedFileRejectsUnmanagedExistingTarget(t *testing.T) {
	t.Parallel()

	project := mapSnapshot{"rules.md": []byte("hand-authored\n")}
	sources := []adapterRender{{
		Descriptor: testDescriptor("fixture", "1.0.0"),
		Outputs: []Output{{
			Target: "rules.md", Kind: OutputGeneratedFile, Mode: 0o644,
			File: &GeneratedFile{Owner: OwnerRef{Source: "github:owner/pkg", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}, Content: []byte("managed\n")},
		}},
	}}
	_, err := compileOutputs(project, realize.Ledger{}, nil, sources)
	var malformed *MalformedOutputError
	if !errors.As(err, &malformed) {
		t.Fatalf("compileOutputs() error = %v, want *MalformedOutputError rejecting unproven existing target", err)
	}
}

func TestCompileGeneratedFileAllowsReproveningLedgerOwnedTarget(t *testing.T) {
	t.Parallel()

	project := mapSnapshot{"generated.md": []byte("managed v1\n")}
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{{
		Path: "generated.md", Mode: 0o644, Ownership: realize.OwnershipGenerated, OutputHash: hashContent([]byte("managed v1\n")),
		Entries: []realize.Entry{{Source: "github:owner/pkg", ArtifactID: "rule-a", ArtifactKind: realize.ArtifactFile, SourcePath: "rules/a.md", Adapter: "fixture", AdapterVersion: "1.0.0", ManagedHash: hashContent([]byte("managed v1\n"))}},
	}}}
	sources := []adapterRender{{
		Descriptor: testDescriptor("fixture", "1.1.0"),
		Outputs: []Output{{
			Target: "generated.md", Kind: OutputGeneratedFile, Mode: 0o644,
			File: &GeneratedFile{Owner: OwnerRef{Source: "github:owner/pkg", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}, Content: []byte("managed v2\n")},
		}},
	}}
	intents, err := compileOutputs(project, previous, nil, sources)
	if err != nil || len(intents) != 1 || string(intents[0].Content) != "managed v2\n" || intents[0].Entries[0].AdapterVersion != "1.1.0" {
		t.Fatalf("compileOutputs() = %#v, %v", intents, err)
	}
}

func TestCompileRejectsMalformedTaggedUnion(t *testing.T) {
	t.Parallel()

	sources := []adapterRender{{
		Descriptor: testDescriptor("fixture", "1.0.0"),
		Outputs: []Output{{
			Target: "x.md", Kind: OutputGeneratedFile,
			File:   &GeneratedFile{Content: []byte("a")},
			Config: &ConfigMerge{Format: ConfigJSON},
		}},
	}}
	_, err := compileOutputs(mapSnapshot{}, realize.Ledger{}, nil, sources)
	var malformed *MalformedOutputError
	if !errors.As(err, &malformed) {
		t.Fatalf("compileOutputs() error = %v, want *MalformedOutputError for a multi-payload output", err)
	}
}

func TestCompileRejectsMismatchedKindsForSameTarget(t *testing.T) {
	t.Parallel()

	sources := []adapterRender{{
		Descriptor: testDescriptor("fixture", "1.0.0"),
		Outputs: []Output{
			{Target: "shared.md", Kind: OutputGeneratedFile, File: &GeneratedFile{Content: []byte("a")}},
			{Target: "shared.md", Kind: OutputMarkdownInclude, Markdown: []MarkdownInsertion{{BlockID: "a", Body: []byte("b")}}},
		},
	}}
	_, err := compileOutputs(mapSnapshot{}, realize.Ledger{}, testCompiler(), sources)
	var malformed *MalformedOutputError
	if !errors.As(err, &malformed) {
		t.Fatalf("compileOutputs() error = %v, want *MalformedOutputError for mismatched kinds", err)
	}
}

func TestCompileRejectsDuplicateGeneratedFileTarget(t *testing.T) {
	t.Parallel()

	sources := []adapterRender{{
		Descriptor: testDescriptor("fixture", "1.0.0"),
		Outputs: []Output{
			{Target: "same.md", Kind: OutputGeneratedFile, File: &GeneratedFile{Content: []byte("a")}},
			{Target: "same.md", Kind: OutputGeneratedFile, File: &GeneratedFile{Content: []byte("b")}},
		},
	}}
	_, err := compileOutputs(mapSnapshot{}, realize.Ledger{}, nil, sources)
	var malformed *MalformedOutputError
	if !errors.As(err, &malformed) {
		t.Fatalf("compileOutputs() error = %v, want *MalformedOutputError for a duplicate generated-file target", err)
	}
}

func TestCompileMarkdownFailsClosedWithoutCompiler(t *testing.T) {
	t.Parallel()

	sources := []adapterRender{{
		Descriptor: testDescriptor("fixture", "1.0.0"),
		Outputs:    []Output{{Target: "AGENTS.md", Kind: OutputMarkdownInclude, Markdown: []MarkdownInsertion{{BlockID: "a", Body: []byte("b")}}}},
	}}
	_, err := compileOutputs(mapSnapshot{}, realize.Ledger{}, nil, sources)
	if err == nil || !strings.Contains(err.Error(), "none is registered") {
		t.Fatalf("compileOutputs() error = %v, want fail-closed rejection", err)
	}
}

func TestCompileConfigFailsClosedWithoutCompiler(t *testing.T) {
	t.Parallel()

	sources := []adapterRender{{
		Descriptor: testDescriptor("fixture", "1.0.0"),
		Outputs: []Output{{
			Target: "hooks.json", Kind: OutputConfigMerge,
			Config: &ConfigMerge{Format: ConfigJSON, Entries: []ConfigEntry{{Container: []string{"hooks"}, Kind: ConfigField, Key: "a", EncodedValue: jsonValue("x")}}},
		}},
	}}
	_, err := compileOutputs(mapSnapshot{}, realize.Ledger{}, nil, sources)
	if err == nil || !strings.Contains(err.Error(), "none is registered") {
		t.Fatalf("compileOutputs() error = %v, want fail-closed rejection", err)
	}
}

func TestCompileMarkdownWithCompilerCreatesGeneratedOnly(t *testing.T) {
	t.Parallel()

	sources := []adapterRender{{
		Descriptor: testDescriptor("fixture", "1.0.0"),
		Outputs:    []Output{{Target: "SKILL.md", Kind: OutputMarkdownInclude, Mode: 0o644, Markdown: []MarkdownInsertion{{Owner: OwnerRef{Source: "github:owner/pkg", ArtifactID: "rule-a"}, BlockID: "block-a", Body: []byte("body\n")}}}},
	}}
	intents, err := compileOutputs(mapSnapshot{}, realize.Ledger{}, testCompiler(), sources)
	if err != nil || len(intents) != 1 {
		t.Fatalf("compileOutputs() = %#v, %v", intents, err)
	}
	intent := intents[0]
	if intent.Ownership != realize.OwnershipGenerated || !strings.Contains(string(intent.Content), "block-a") || intent.ObservedHash != "" {
		t.Fatalf("compiled intent = %#v", intent)
	}
}

func TestCompileMarkdownWithCompilerMergesIntoExistingSharedTargetAndPreserves(t *testing.T) {
	t.Parallel()

	observed := "# team notes\nuser wrote this\n"
	project := mapSnapshot{"AGENTS.md": []byte(observed)}
	owner := OwnerRef{Source: "github:owner/pkg", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}
	sources := []adapterRender{{
		Descriptor: testDescriptor("fixture", "1.0.0"),
		Outputs:    []Output{{Target: "AGENTS.md", Kind: OutputMarkdownInclude, Mode: 0o644, Markdown: []MarkdownInsertion{{Owner: owner, BlockID: "block-a", Body: []byte("managed\n")}}}},
	}}
	intents, err := compileOutputs(project, realize.Ledger{}, testCompiler(), sources)
	if err != nil || len(intents) != 1 {
		t.Fatalf("compileOutputs() = %#v, %v", intents, err)
	}
	intent := intents[0]
	if intent.Ownership != realize.OwnershipShared || intent.ObservedHash != hashContent([]byte(observed)) || !intent.ManagedIntact || len(intent.PreservedContent) != 1 {
		t.Fatalf("compiled intent = %#v, want a shared merge bound to the observed file with preserved content", intent)
	}
	if !strings.Contains(string(intent.Content), observed) || !strings.Contains(string(intent.Content), "block-a") {
		t.Fatalf("compiled content = %q, want both preserved and managed bytes", intent.Content)
	}

	// AC1 (interface level): a genuinely preservation-bound merge must still
	// be accepted by the engine, proving the new engine-side guard rejects
	// only whole-file replacement, never a correct compiled merge.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(observed), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := realize.NewPlanner().Plan(root, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, []realize.Intent{intent})
	if err != nil || plan.HasConflicts() {
		t.Fatalf("Plan() = %#v, %v, want a conflict-free promotion", plan, err)
	}
}

func TestCompileRejectsDuplicateMarkdownBlockIDAcrossAdapters(t *testing.T) {
	t.Parallel()

	sources := []adapterRender{
		{Descriptor: testDescriptor("adapter-a", "1.0.0"), Outputs: []Output{{Target: "AGENTS.md", Kind: OutputMarkdownInclude, Markdown: []MarkdownInsertion{{BlockID: "shared-id", Body: []byte("a")}}}}},
		{Descriptor: testDescriptor("adapter-b", "1.0.0"), Outputs: []Output{{Target: "AGENTS.md", Kind: OutputMarkdownInclude, Markdown: []MarkdownInsertion{{BlockID: "shared-id", Body: []byte("b")}}}}},
	}
	_, err := compileOutputs(mapSnapshot{}, realize.Ledger{}, testCompiler(), sources)
	var duplicate *DuplicateEntryError
	if !errors.As(err, &duplicate) || duplicate.Identifier != "shared-id" {
		t.Fatalf("compileOutputs() error = %v, want *DuplicateEntryError", err)
	}
	if !strings.Contains(err.Error(), CodeDuplicateConfigEntry) {
		t.Fatalf("compileOutputs() error = %v, want code %q", err, CodeDuplicateConfigEntry)
	}
}

func TestCompileRejectsDuplicateStructuralConfigEntryAcrossAdapters(t *testing.T) {
	t.Parallel()

	entry := func() ConfigEntry {
		return ConfigEntry{Container: []string{"hooks"}, Kind: ConfigField, Key: "session-start", EncodedValue: jsonValue(map[string]any{"event": "SessionStart"})}
	}
	sources := []adapterRender{
		{Descriptor: testDescriptor("adapter-a", "1.0.0"), Outputs: []Output{{Target: "hooks.json", Kind: OutputConfigMerge, Config: &ConfigMerge{Format: ConfigJSON, Entries: []ConfigEntry{entry()}}}}},
		{Descriptor: testDescriptor("adapter-b", "1.0.0"), Outputs: []Output{{Target: "hooks.json", Kind: OutputConfigMerge, Config: &ConfigMerge{Format: ConfigJSON, Entries: []ConfigEntry{entry()}}}}},
	}
	_, err := compileOutputs(mapSnapshot{}, realize.Ledger{}, testCompiler(), sources)
	var duplicate *DuplicateEntryError
	if !errors.As(err, &duplicate) {
		t.Fatalf("compileOutputs() error = %v, want *DuplicateEntryError", err)
	}
}

func TestCompileConfigMergeFromMultipleAdaptersOnSameTargetSucceeds(t *testing.T) {
	t.Parallel()

	sources := []adapterRender{
		{Descriptor: testDescriptor("adapter-a", "1.0.0"), Outputs: []Output{{Target: "hooks.json", Kind: OutputConfigMerge, Mode: 0o644, Config: &ConfigMerge{Format: ConfigJSON, Entries: []ConfigEntry{{Owner: OwnerRef{Source: "github:owner/a"}, Container: []string{"hooks"}, Kind: ConfigField, Key: "from-a", EncodedValue: jsonValue("x")}}}}}},
		{Descriptor: testDescriptor("adapter-b", "1.0.0"), Outputs: []Output{{Target: "hooks.json", Kind: OutputConfigMerge, Mode: 0o644, Config: &ConfigMerge{Format: ConfigJSON, Entries: []ConfigEntry{{Owner: OwnerRef{Source: "github:owner/b"}, Container: []string{"hooks"}, Kind: ConfigField, Key: "from-b", EncodedValue: jsonValue("y")}}}}}},
	}
	intents, err := compileOutputs(mapSnapshot{}, realize.Ledger{}, testCompiler(), sources)
	if err != nil || len(intents) != 1 || len(intents[0].Entries) != 2 {
		t.Fatalf("compileOutputs() = %#v, %v", intents, err)
	}
	if intents[0].Entries[0].Adapter == intents[0].Entries[1].Adapter {
		t.Fatalf("compiled entries = %#v, want one entry per contributing adapter", intents[0].Entries)
	}
}

func TestCompileOutputsSortedByTarget(t *testing.T) {
	t.Parallel()

	sources := []adapterRender{{
		Descriptor: testDescriptor("fixture", "1.0.0"),
		Outputs: []Output{
			{Target: "z.md", Kind: OutputGeneratedFile, File: &GeneratedFile{Content: []byte("z")}},
			{Target: "a.md", Kind: OutputGeneratedFile, File: &GeneratedFile{Content: []byte("a")}},
		},
	}}
	intents, err := compileOutputs(mapSnapshot{}, realize.Ledger{}, nil, sources)
	if err != nil || len(intents) != 2 || intents[0].Path != "a.md" || intents[1].Path != "z.md" {
		t.Fatalf("compileOutputs() = %#v, %v", intents, err)
	}
}

// The following tests cover the SharedCompiler request/response seam
// (SharedTarget carrying Observed/Previous, and SharedCompilation carrying
// Action/Candidate/Managed/Proof): partial removal, final removal, a
// tampered old entry, and a JSON array element located by managedHash
// rather than by array position.

func TestCompileMarkdownPartialRemovalDropsDepartedAdapterKeepsOthers(t *testing.T) {
	t.Parallel()

	ownerA := OwnerRef{Source: "github:owner/a", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}
	ownerB := OwnerRef{Source: "github:owner/b", ArtifactID: "rule-b", SourcePath: "rules/b.md", Kind: ArtifactRule}
	observed := "# Team notes\n" +
		"<!-- ACR:BEGIN block-a -->\nfrom A v1\n<!-- ACR:END block-a -->\n" +
		"user middle text\n" +
		"<!-- ACR:BEGIN block-b -->\nfrom B v1\n<!-- ACR:END block-b -->\n" +
		"trailing user text\n"
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{{
		Path: "AGENTS.md", Mode: 0o644, Ownership: realize.OwnershipShared, OutputHash: hashContent([]byte(observed)),
		Entries: []realize.Entry{
			{Source: ownerA.Source, ArtifactID: ownerA.ArtifactID, ArtifactKind: realize.ArtifactManagedBlock, SourcePath: ownerA.SourcePath, Adapter: "adapter-a", AdapterVersion: "1.0.0", ManagedHash: hashContent([]byte("from A v1\n"))},
			{Source: ownerB.Source, ArtifactID: ownerB.ArtifactID, ArtifactKind: realize.ArtifactManagedBlock, SourcePath: ownerB.SourcePath, Adapter: "adapter-b", AdapterVersion: "1.0.0", ManagedHash: hashContent([]byte("from B v1\n"))},
		},
	}}}
	// Only adapter-a still renders this run; adapter-b's package was removed.
	sources := []adapterRender{{
		Descriptor: testDescriptor("adapter-a", "1.0.0"),
		Outputs:    []Output{{Target: "AGENTS.md", Kind: OutputMarkdownInclude, Mode: 0o644, Markdown: []MarkdownInsertion{{Owner: ownerA, BlockID: "block-a", Body: []byte("from A v2\n")}}}},
	}}
	project := mapSnapshot{"AGENTS.md": []byte(observed)}

	intents, err := compileOutputs(project, previous, testCompiler(), sources)
	if err != nil || len(intents) != 1 {
		t.Fatalf("compileOutputs() = %#v, %v", intents, err)
	}
	intent := intents[0]
	content := string(intent.Content)
	if intent.Ownership != realize.OwnershipShared || intent.Action != realize.ActionEnsure {
		t.Fatalf("intent = %#v, want an ensure that stays shared", intent)
	}
	if !strings.Contains(content, "from A v2") || !strings.Contains(content, "user middle text") || !strings.Contains(content, "trailing user text") {
		t.Fatalf("compiled content = %q, want the updated block and both preserved fragments", content)
	}
	if strings.Contains(content, "from B v1") || strings.Contains(content, "block-b") {
		t.Fatalf("compiled content = %q, want the departed adapter's block fully removed", content)
	}
	if len(intent.Entries) != 1 || intent.Entries[0].Adapter != "adapter-a" {
		t.Fatalf("entries = %#v, want only the still-contributing adapter", intent.Entries)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(observed), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := realize.NewPlanner().Plan(root, previous, []realize.Intent{intent})
	if err != nil || plan.HasConflicts() {
		t.Fatalf("Plan() = %#v, %v, want a conflict-free partial removal", plan, err)
	}
}

func TestCompileMarkdownFinalRemovalThroughApplyLeavesUnmanagedContent(t *testing.T) {
	t.Parallel()

	owner := OwnerRef{Source: "github:owner/a", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}
	observed := "# Team notes\n<!-- ACR:BEGIN block-a -->\nfrom A v1\n<!-- ACR:END block-a -->\nuser trailing text\n"
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{{
		Path: "AGENTS.md", Mode: 0o644, Ownership: realize.OwnershipShared, OutputHash: hashContent([]byte(observed)),
		Entries: []realize.Entry{{Source: owner.Source, ArtifactID: owner.ArtifactID, ArtifactKind: realize.ArtifactManagedBlock, SourcePath: owner.SourcePath, Adapter: "adapter-a", AdapterVersion: "1.0.0", ManagedHash: hashContent([]byte("from A v1\n"))}},
	}}}
	// No current output at all for AGENTS.md: compileOutputs must revisit it
	// via the ledger alone.
	intents, err := compileOutputs(mapSnapshot{"AGENTS.md": []byte(observed)}, previous, testCompiler(), nil)
	if err != nil || len(intents) != 1 {
		t.Fatalf("compileOutputs() = %#v, %v", intents, err)
	}
	intent := intents[0]
	if intent.Action != realize.ActionRemove || len(intent.Entries) != 0 {
		t.Fatalf("intent = %#v, want a final removal with no remaining entries", intent)
	}
	if strings.Contains(string(intent.Content), "from A v1") || strings.Contains(string(intent.Content), "block-a") {
		t.Fatalf("compiled content = %q, want the managed block gone", intent.Content)
	}
	if !strings.Contains(string(intent.Content), "user trailing text") {
		t.Fatalf("compiled content = %q, want the unmanaged text preserved", intent.Content)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(observed), 0o644); err != nil {
		t.Fatal(err)
	}
	var appliedLedger realize.Ledger
	plan, err := realize.NewEngine().Run(root, previous, intents, realize.ModeApply, func(ledger realize.Ledger) error {
		appliedLedger = ledger
		return nil
	})
	if err != nil || !plan.HasChanges() {
		t.Fatalf("Run(apply) = %#v, %v", plan, err)
	}
	if len(appliedLedger.Targets) != 0 {
		t.Fatalf("applied ledger = %#v, want the target dropped entirely", appliedLedger)
	}
	written, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil || strings.Contains(string(written), "from A v1") || !strings.Contains(string(written), "user trailing text") {
		t.Fatalf("realized file = %q, %v", written, err)
	}
	second, err := realize.NewEngine().Run(root, appliedLedger, nil, realize.ModeCheck, nil)
	if err != nil || second.HasChanges() {
		t.Fatalf("second Run(check) = %#v, %v, want an empty plan for the now-unmanaged file", second, err)
	}
}

func TestCompileMarkdownDetectsTamperedOldEntry(t *testing.T) {
	t.Parallel()

	owner := OwnerRef{Source: "github:owner/a", ArtifactID: "rule-a", SourcePath: "rules/a.md", Kind: ArtifactRule}
	// The ledger recorded "original body" as the managed block's hash, but
	// the file on disk now carries different bytes inside that same block:
	// someone hand-edited managed content.
	observed := "<!-- ACR:BEGIN block-a -->\nhand-edited body\n<!-- ACR:END block-a -->\n"
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{{
		Path: "AGENTS.md", Mode: 0o644, Ownership: realize.OwnershipShared, OutputHash: hashContent([]byte(observed)),
		Entries: []realize.Entry{{Source: owner.Source, ArtifactID: owner.ArtifactID, ArtifactKind: realize.ArtifactManagedBlock, SourcePath: owner.SourcePath, Adapter: "adapter-a", AdapterVersion: "1.0.0", ManagedHash: hashContent([]byte("original body\n"))}},
	}}}
	sources := []adapterRender{{
		Descriptor: testDescriptor("adapter-a", "1.0.0"),
		Outputs:    []Output{{Target: "AGENTS.md", Kind: OutputMarkdownInclude, Mode: 0o644, Markdown: []MarkdownInsertion{{Owner: owner, BlockID: "block-a", Body: []byte("from A v2\n")}}}},
	}}
	project := mapSnapshot{"AGENTS.md": []byte(observed)}

	intents, err := compileOutputs(project, previous, testCompiler(), sources)
	if err != nil || len(intents) != 1 {
		t.Fatalf("compileOutputs() = %#v, %v", intents, err)
	}
	if intents[0].ManagedIntact {
		t.Fatalf("intent = %#v, want ManagedIntact=false for a tampered old entry", intents[0])
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(observed), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := realize.NewPlanner().Plan(root, previous, intents)
	if err != nil {
		t.Fatal(err)
	}
	assertConflictContains(t, plan, "not bound to intact managed content")
	written, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil || string(written) != observed {
		t.Fatalf("tampered file changed = %q, %v", written, err)
	}
}

func TestCompileConfigLocatesArrayElementByManagedHashNotPosition(t *testing.T) {
	t.Parallel()

	owner := OwnerRef{Source: "github:owner/a", ArtifactID: "hook-mine", SourcePath: "hooks/mine.sh", Kind: ArtifactHook}
	oldValue := map[string]any{"event": "SessionStart", "id": "mine"}
	oldEncoded, err := json.Marshal(oldValue)
	if err != nil {
		t.Fatal(err)
	}
	otherValue := map[string]any{"event": "Stop", "id": "other"}
	// "mine" sits at index 1, not 0: something else already reordered the
	// array, so position-based lookup would target the wrong element.
	observedContent, err := json.Marshal(map[string]any{"hooks": []any{otherValue, oldValue}})
	if err != nil {
		t.Fatal(err)
	}
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{{
		Path: "hooks.json", Mode: 0o644, Ownership: realize.OwnershipShared, OutputHash: hashContent(observedContent),
		Entries: []realize.Entry{{Source: owner.Source, ArtifactID: owner.ArtifactID, ArtifactKind: realize.ArtifactStructuredEntry, SourcePath: owner.SourcePath, Adapter: "adapter-a", AdapterVersion: "1.0.0", ManagedHash: hashContent(oldEncoded)}},
	}}}
	newEncoded, err := json.Marshal(map[string]any{"event": "SessionStart", "id": "mine", "extra": "v2"})
	if err != nil {
		t.Fatal(err)
	}
	sources := []adapterRender{{
		Descriptor: testDescriptor("adapter-a", "1.1.0"),
		Outputs: []Output{{
			Target: "hooks.json", Kind: OutputConfigMerge, Mode: 0o644,
			Config: &ConfigMerge{Format: ConfigJSON, Entries: []ConfigEntry{{Owner: owner, Container: []string{"hooks"}, Kind: ConfigElement, Key: "mine", EncodedValue: newEncoded}}},
		}},
	}}

	intents, err := compileOutputs(mapSnapshot{"hooks.json": observedContent}, previous, testCompiler(), sources)
	if err != nil || len(intents) != 1 {
		t.Fatalf("compileOutputs() = %#v, %v", intents, err)
	}
	if !intents[0].ManagedIntact {
		t.Fatalf("intent = %#v, want ManagedIntact=true (the recorded hash was found)", intents[0])
	}
	var decoded struct {
		Hooks []map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(intents[0].Content, &decoded); err != nil {
		t.Fatalf("decode compiled content: %v; content = %s", err, intents[0].Content)
	}
	if len(decoded.Hooks) != 2 {
		t.Fatalf("hooks array = %#v, want the element updated in place, not appended", decoded.Hooks)
	}
	foundOther, foundUpdated := false, false
	for _, element := range decoded.Hooks {
		if element["id"] == "other" {
			foundOther = true
		}
		if element["id"] == "mine" {
			foundUpdated = true
			if element["extra"] != "v2" {
				t.Fatalf("updated element = %#v, want extra=v2", element)
			}
		}
	}
	if !foundOther || !foundUpdated {
		t.Fatalf("hooks array = %#v, want both the untouched and the updated element", decoded.Hooks)
	}
}

func TestCompileConfigRemovesArrayElementByManagedHashWhenOwnerDeparts(t *testing.T) {
	t.Parallel()

	mineOwner := OwnerRef{Source: "github:owner/a", ArtifactID: "hook-mine", SourcePath: "hooks/mine.sh", Kind: ArtifactHook}
	otherOwner := OwnerRef{Source: "github:owner/a", ArtifactID: "hook-other", SourcePath: "hooks/other.sh", Kind: ArtifactHook}
	mineValue := map[string]any{"event": "SessionStart", "id": "mine"}
	otherValue := map[string]any{"event": "Stop", "id": "other"}
	mineEncoded, err := json.Marshal(mineValue)
	if err != nil {
		t.Fatal(err)
	}
	otherEncoded, err := json.Marshal(otherValue)
	if err != nil {
		t.Fatal(err)
	}
	observedContent, err := json.Marshal(map[string]any{"hooks": []any{mineValue, otherValue}})
	if err != nil {
		t.Fatal(err)
	}
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{{
		Path: "hooks.json", Mode: 0o644, Ownership: realize.OwnershipShared, OutputHash: hashContent(observedContent),
		Entries: []realize.Entry{
			{Source: mineOwner.Source, ArtifactID: mineOwner.ArtifactID, ArtifactKind: realize.ArtifactStructuredEntry, SourcePath: mineOwner.SourcePath, Adapter: "adapter-a", AdapterVersion: "1.0.0", ManagedHash: hashContent(mineEncoded)},
			{Source: otherOwner.Source, ArtifactID: otherOwner.ArtifactID, ArtifactKind: realize.ArtifactStructuredEntry, SourcePath: otherOwner.SourcePath, Adapter: "adapter-a", AdapterVersion: "1.0.0", ManagedHash: hashContent(otherEncoded)},
		},
	}}}
	// Only "other" is desired this run; "mine"'s artifact was removed from
	// the package.
	sources := []adapterRender{{
		Descriptor: testDescriptor("adapter-a", "1.0.0"),
		Outputs: []Output{{
			Target: "hooks.json", Kind: OutputConfigMerge, Mode: 0o644,
			Config: &ConfigMerge{Format: ConfigJSON, Entries: []ConfigEntry{{Owner: otherOwner, Container: []string{"hooks"}, Kind: ConfigElement, Key: "other", EncodedValue: otherEncoded}}},
		}},
	}}

	intents, err := compileOutputs(mapSnapshot{"hooks.json": observedContent}, previous, testCompiler(), sources)
	if err != nil || len(intents) != 1 {
		t.Fatalf("compileOutputs() = %#v, %v", intents, err)
	}
	var decoded struct {
		Hooks []map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(intents[0].Content, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Hooks) != 1 || decoded.Hooks[0]["id"] != "other" {
		t.Fatalf("hooks array = %#v, want only the still-desired element, located and kept by hash", decoded.Hooks)
	}
	if len(intents[0].Entries) != 1 || intents[0].Entries[0].ArtifactID != "hook-other" {
		t.Fatalf("entries = %#v, want only the departed owner's entry dropped", intents[0].Entries)
	}
}

func assertConflictContains(t *testing.T, plan realize.Plan, want string) {
	t.Helper()
	if !plan.HasConflicts() {
		t.Fatalf("plan has no conflict: %#v", plan)
	}
	for _, operation := range plan.Operations {
		if operation.Kind == realize.OperationConflict && strings.Contains(operation.Reason, want) {
			return
		}
	}
	t.Fatalf("conflicts = %#v, want reason containing %q", plan.Operations, want)
}

// F4 (reviewer): the old sort/duplicate key omitted a separator between the
// joined container and Key and omitted Kind entirely, so structurally
// distinct tuples like (["ab"], "c") and (["a"], "bc") compared equal.
// canonicalEntryKey's length-prefixed encoding must not collide.

func TestCanonicalEntryKeyHasNoPrefixCollision(t *testing.T) {
	t.Parallel()

	a := canonicalEntryKey([]string{"ab"}, ConfigField, "c")
	b := canonicalEntryKey([]string{"a"}, ConfigField, "bc")
	if a == b {
		t.Fatalf("canonicalEntryKey collided: %q for both ([\"ab\"], field, \"c\") and ([\"a\"], field, \"bc\")", a)
	}
}

func TestCanonicalEntryKeyDistinguishesKind(t *testing.T) {
	t.Parallel()

	field := canonicalEntryKey([]string{"hooks"}, ConfigField, "mine")
	element := canonicalEntryKey([]string{"hooks"}, ConfigElement, "mine")
	if field == element {
		t.Fatalf("canonicalEntryKey did not distinguish ConfigField from ConfigElement: %q", field)
	}
}

func TestCanonicalEntryKeyPermutationsProduceATotalOrder(t *testing.T) {
	t.Parallel()

	type tuple struct {
		container []string
		kind      ConfigEntryKind
		key       string
	}
	tuples := []tuple{
		{[]string{"ab"}, ConfigField, "c"},
		{[]string{"a"}, ConfigField, "bc"},
		{[]string{"hooks"}, ConfigField, "mine"},
		{[]string{"hooks"}, ConfigElement, "mine"},
		{[]string{"a", "b"}, ConfigField, "c"},
		{[]string{"a"}, ConfigField, "b\x00c"},
	}
	keys := make(map[string]int, len(tuples))
	for index, candidate := range tuples {
		key := canonicalEntryKey(candidate.container, candidate.kind, candidate.key)
		if previous, collided := keys[key]; collided {
			t.Fatalf("canonicalEntryKey collision between tuple %d and %d: %q", previous, index, key)
		}
		keys[key] = index
	}
}

func TestCompileConfigDoesNotFalselyRejectPrefixCollidingEntriesAsDuplicates(t *testing.T) {
	t.Parallel()

	sources := []adapterRender{{
		Descriptor: testDescriptor("fixture", "1.0.0"),
		Outputs: []Output{{
			Target: "hooks.json", Kind: OutputConfigMerge, Mode: 0o644,
			Config: &ConfigMerge{Format: ConfigJSON, Entries: []ConfigEntry{
				{Owner: OwnerRef{Source: "github:owner/a", ArtifactID: "one"}, Container: []string{"ab"}, Kind: ConfigField, Key: "c", EncodedValue: jsonValue("first")},
				{Owner: OwnerRef{Source: "github:owner/a", ArtifactID: "two"}, Container: []string{"a"}, Kind: ConfigField, Key: "bc", EncodedValue: jsonValue("second")},
			}},
		}},
	}}
	intents, err := compileOutputs(mapSnapshot{}, realize.Ledger{}, testCompiler(), sources)
	if err != nil {
		t.Fatalf("compileOutputs() = %v, want structurally distinct (container, kind, key) tuples accepted, not rejected as duplicates", err)
	}
	if len(intents) != 1 || len(intents[0].Entries) != 2 {
		t.Fatalf("intents = %#v, want both distinct entries kept", intents)
	}
}
