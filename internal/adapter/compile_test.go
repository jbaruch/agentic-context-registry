package adapter

import (
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
