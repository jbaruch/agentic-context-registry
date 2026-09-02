package preserve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestJSONConfigMergePreservesUnownedKeysValuesAndOrder(t *testing.T) {
	t.Parallel()
	entry := testConfigEntry(adapter.ConfigJSON, []string{"hooks"}, adapter.ConfigField, "managed", `{"event":"Stop"}`)
	initial := compileMissingConfig(t, adapter.ConfigJSON, entry)
	content := []byte("{\n  \"before\": {\"x\": 1},\n  \"hooks\": {\"managed\": {\"event\":\"Stop\"}},\n  \"after\": [true, null]\n}\n")
	previous := configTarget("settings.json", content, realize.OwnershipShared, initial.Managed)
	observed := observedFile("settings.json", content)
	entry.EncodedValue = []byte(`{"event":"SessionEnd"}`)
	compiled, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.json", Observed: &observed, Previous: &previous},
		Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := compiled.Candidate.Content
	before := []byte(`"before": {"x": 1}`)
	after := []byte(`"after": [true, null]`)
	if !bytes.Contains(got, before) || !bytes.Contains(got, after) || bytes.Index(got, before) > bytes.Index(got, after) || !bytes.Contains(got, entry.EncodedValue) {
		t.Fatalf("merged JSON = %s", got)
	}
	if !json.Valid(got) || compiled.Candidate.Ownership != realize.OwnershipShared {
		t.Fatalf("compilation = %#v", compiled)
	}
}

func TestJSONConfigCreatesMissingManagedContainerWithoutRewritingHost(t *testing.T) {
	t.Parallel()
	content := []byte("{\n  \"user\": {\"keep\": true}\n}\n")
	observed := observedFile("settings.json", content)
	entry := testConfigEntry(adapter.ConfigJSON, []string{"hooks"}, adapter.ConfigField, "managed", `{"event":"Stop"}`)
	compiled, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.json", Observed: &observed}, Format: adapter.ConfigJSON,
		Desired: []adapter.ConfigEntry{entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(compiled.Candidate.Content, []byte(`"user": {"keep": true}`)) || !bytes.Contains(compiled.Candidate.Content, []byte(`"hooks":{"managed":{"event":"Stop"}}`)) || !json.Valid(compiled.Candidate.Content) {
		t.Fatalf("merged JSON = %s", compiled.Candidate.Content)
	}
}

func TestJSONConfigRemovalLeavesOnlyUnownedEntries(t *testing.T) {
	t.Parallel()
	entry := testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigField, "managed", `{"v":1}`)
	initial := compileMissingConfig(t, adapter.ConfigJSON, entry)
	content := []byte("{\n  \"user\": {\"keep\": true},\n  \"managed\": {\"v\":1},\n  \"tail\": 2\n}\n")
	previous := configTarget("settings.json", content, realize.OwnershipShared, initial.Managed)
	observed := observedFile("settings.json", content)
	removed, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.json", Observed: &observed, Previous: &previous}, Format: adapter.ConfigJSON,
	})
	if err != nil {
		t.Fatal(err)
	}
	if removed.Action != realize.ActionRemove || removed.Candidate == nil || bytes.Contains(removed.Candidate.Content, []byte(`"managed"`)) || !bytes.Contains(removed.Candidate.Content, []byte(`{"keep": true}`)) || !json.Valid(removed.Candidate.Content) {
		t.Fatalf("removed JSON = %#v, %s", removed, removed.Candidate.Content)
	}
}

func TestJSONConfigRemovesMultipleOwnedMembersWithoutDamagingDelimiter(t *testing.T) {
	t.Parallel()
	first := testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigField, "first", `1`)
	second := testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigField, "second", `2`)
	initial := compileMissingConfig(t, adapter.ConfigJSON, first, second)
	content := []byte(`{"first":1,"second":2,"user":3}` + "\n")
	previous := configTarget("settings.json", content, realize.OwnershipShared, initial.Managed)
	observed := observedFile("settings.json", content)
	removed, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.json", Observed: &observed, Previous: &previous}, Format: adapter.ConfigJSON,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(removed.Candidate.Content)); got != `{"user":3}` {
		t.Fatalf("multi-member removal = %s", got)
	}
}

func TestJSONConfigArrayElementUsesManagedHashNotPosition(t *testing.T) {
	t.Parallel()
	entry := testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigElement, "managed-key", `{"id":"managed"}`)
	initial := compileMissingConfig(t, adapter.ConfigJSON, entry)
	content := []byte(`[{"id":"user"},{"id":"managed"}]` + "\n")
	previous := configTarget("settings.json", content, realize.OwnershipShared, initial.Managed)
	observed := observedFile("settings.json", content)
	entry.EncodedValue = []byte(`{"id":"managed","v":2}`)
	updated, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.json", Observed: &observed, Previous: &previous},
		Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(updated.Candidate.Content, []byte(`{"id":"user"}`)) || !bytes.Contains(updated.Candidate.Content, entry.EncodedValue) || bytes.Index(updated.Candidate.Content, []byte("user")) > bytes.Index(updated.Candidate.Content, []byte("managed")) {
		t.Fatalf("updated JSON array = %s", updated.Candidate.Content)
	}
}

func TestJSONConfigRejectsEscapeEquivalentDuplicateKeys(t *testing.T) {
	t.Parallel()
	content := []byte(`{"a":1,"\u0061":2}`)
	observed := observedFile("settings.json", content)
	_, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.json", Observed: &observed}, Format: adapter.ConfigJSON,
		Desired: []adapter.ConfigEntry{testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigField, "managed", `true`)},
	})
	if err == nil || !strings.Contains(err.Error(), CodeDuplicateConfigEntry) {
		t.Fatalf("duplicate JSON error = %v", err)
	}
}

func TestGeneratedJSONPromotionPreservesUserEntry(t *testing.T) {
	t.Parallel()
	entry := testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigField, "managed", `1`)
	initial := compileMissingConfig(t, adapter.ConfigJSON, entry)
	previous := configTarget("settings.json", initial.Candidate.Content, realize.OwnershipGenerated, initial.Managed)
	content := []byte(`{"managed":1,"user":{"keep":true}}` + "\n")
	observed := observedFile("settings.json", content)
	promoted, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.json", Observed: &observed, Previous: &previous},
		Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Candidate.Ownership != realize.OwnershipShared || len(promoted.Notices) != 1 || !bytes.Contains(promoted.Candidate.Content, []byte(`{"keep":true}`)) {
		t.Fatalf("promotion = %#v", promoted)
	}
	unmanagedSpan := []byte(`{"keep":true}`)
	if len(promoted.Proof.PreservedContent) != 1 || len(promoted.Proof.PreservedContent[0]) < len(unmanagedSpan) || !bytes.Equal(promoted.Proof.PreservedContent[0], unmanagedSpan) {
		t.Fatalf("promotion proof = %q, want exact unmanaged value span %q", promoted.Proof.PreservedContent, unmanagedSpan)
	}
}

func TestGeneratedConfigReformatWithoutUnmanagedContentConflicts(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		format      adapter.ConfigFormat
		path        string
		reformatted []byte
	}{
		"json": {format: adapter.ConfigJSON, path: "settings.json", reformatted: []byte("{\n  \"managed\": 1\n}\n")},
		"toml": {format: adapter.ConfigTOML, path: "settings.toml", reformatted: []byte("managed=1\n")},
	}
	for name, test := range tests {
		test := test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entry := testConfigEntry(test.format, nil, adapter.ConfigField, "managed", `1`)
			initial := compileMissingConfig(t, test.format, entry)
			previous := configTarget(test.path, initial.Candidate.Content, realize.OwnershipGenerated, initial.Managed)
			observed := observedFile(test.path, test.reformatted)
			compiled, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
				Target: adapter.SharedTarget{Path: test.path, Observed: &observed, Previous: &previous},
				Format: test.format, Desired: []adapter.ConfigEntry{entry},
			})
			if err != nil {
				t.Fatal(err)
			}
			if compiled.Proof.ManagedIntact || len(compiled.Proof.PreservedContent) != 0 {
				t.Fatalf("reformat proof = %#v, want non-intact with no fabricated fragments", compiled.Proof)
			}
			if compiled.Candidate == nil || compiled.Candidate.Ownership != realize.OwnershipGenerated || len(compiled.Notices) != 0 {
				t.Fatalf("reformat compilation = %#v, want generated ownership without promotion notice", compiled)
			}

			assertConfigCompilationConflictsWithoutWrite(t, test.path, test.reformatted, previous, compiled)
		})
	}
}

func TestStickySharedConfigWithoutUnmanagedContentHasNoFabricatedProof(t *testing.T) {
	t.Parallel()
	const path = "settings.json"
	entry := testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigField, "managed", `1`)
	initial := compileMissingConfig(t, adapter.ConfigJSON, entry)
	generated := configTarget(path, initial.Candidate.Content, realize.OwnershipGenerated, initial.Managed)
	withUser := []byte(`{"managed":1,"user":2}` + "\n")
	promoted, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: path, Observed: ptrObserved(path, withUser), Previous: &generated},
		Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	previous := configTarget(path, promoted.Candidate.Content, realize.OwnershipShared, promoted.Managed)
	withoutUser := []byte(`{"managed":1}` + "\n")
	compiled, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: path, Observed: ptrObserved(path, withoutUser), Previous: &previous},
		Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Candidate == nil || compiled.Candidate.Ownership != realize.OwnershipShared {
		t.Fatalf("sticky compilation = %#v, want shared ownership", compiled)
	}
	if len(compiled.Proof.PreservedContent) != 0 || len(compiled.Notices) != 0 {
		t.Fatalf("sticky proof/notices = %q/%#v, want no synthesized fragment or promotion", compiled.Proof.PreservedContent, compiled.Notices)
	}

	assertConfigCompilationConflictsWithoutWrite(t, path, withoutUser, previous, compiled)
}

func TestStructuredConfigDemotionAndForcePreserveOwnershipChecks(t *testing.T) {
	t.Parallel()
	entry := testConfigEntry(adapter.ConfigJSON, nil, adapter.ConfigField, "managed", `1`)
	initial := compileMissingConfig(t, adapter.ConfigJSON, entry)
	shared := configTarget("settings.json", initial.Candidate.Content, realize.OwnershipShared, initial.Managed)
	cleanObserved := observedFile("settings.json", initial.Candidate.Content)
	demoted, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.json", Observed: &cleanObserved, Previous: &shared, ExplicitDemotion: true},
		Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
	})
	if err != nil || demoted.Candidate.Ownership != realize.OwnershipGenerated {
		t.Fatalf("clean config demotion = %#v, %v", demoted, err)
	}

	withUser := []byte(`{"managed":1,"user":2}` + "\n")
	withUserTarget := configTarget("settings.json", withUser, realize.OwnershipShared, initial.Managed)
	withUserObserved := observedFile("settings.json", withUser)
	_, err = NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.json", Observed: &withUserObserved, Previous: &withUserTarget, ExplicitDemotion: true, Force: true},
		Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
	})
	if err == nil || !strings.Contains(err.Error(), "zero unmanaged") {
		t.Fatalf("forced demotion error = %v", err)
	}

	edited := []byte(`{"managed":9}` + "\n")
	editedObserved := observedFile("settings.json", edited)
	_, err = NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.json", Observed: &editedObserved, Previous: &shared, Force: true},
		Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
	})
	if err == nil || !strings.Contains(err.Error(), "was edited or removed") {
		t.Fatalf("forced edited-entry error = %v", err)
	}
}

func TestTOMLConfigMergePreservesCommentsKeysAndOrder(t *testing.T) {
	t.Parallel()
	entry := testConfigEntry(adapter.ConfigTOML, []string{"hooks"}, adapter.ConfigField, "managed", `{ event = "Stop" }`)
	initial := compileMissingConfig(t, adapter.ConfigTOML, entry)
	content := []byte("# user comment\nbefore = 1\n\n[hooks]\nmanaged = { event = \"Stop\" } # keep inline\nafter = 2\n")
	previous := configTarget("settings.toml", content, realize.OwnershipShared, initial.Managed)
	observed := observedFile("settings.toml", content)
	entry.EncodedValue = []byte(`{ event = "SessionEnd" }`)
	updated, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.toml", Observed: &observed, Previous: &previous},
		Format: adapter.ConfigTOML, Desired: []adapter.ConfigEntry{entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := updated.Candidate.Content
	for _, preserved := range [][]byte{[]byte("# user comment"), []byte("before = 1"), []byte("# keep inline"), []byte("after = 2")} {
		if !bytes.Contains(got, preserved) {
			t.Fatalf("merged TOML lost %q: %s", preserved, got)
		}
	}
	if bytes.Index(got, []byte("before")) > bytes.Index(got, []byte("after")) || !bytes.Contains(got, entry.EncodedValue) {
		t.Fatalf("merged TOML = %s", got)
	}
}

func TestTOMLConfigArrayElementUsesManagedHashNotPosition(t *testing.T) {
	t.Parallel()
	entry := testConfigEntry(adapter.ConfigTOML, []string{"plugins"}, adapter.ConfigElement, "managed-key", `"managed"`)
	initial := compileMissingConfig(t, adapter.ConfigTOML, entry)
	content := []byte("plugins = [\"user\", \"managed\"]\n")
	previous := configTarget("settings.toml", content, realize.OwnershipShared, initial.Managed)
	observed := observedFile("settings.toml", content)
	entry.EncodedValue = []byte(`"managed-v2"`)
	updated, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.toml", Observed: &observed, Previous: &previous},
		Format: adapter.ConfigTOML, Desired: []adapter.ConfigEntry{entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(updated.Candidate.Content, []byte(`"user"`)) || !bytes.Contains(updated.Candidate.Content, entry.EncodedValue) || bytes.Index(updated.Candidate.Content, []byte("user")) > bytes.Index(updated.Candidate.Content, []byte("managed-v2")) {
		t.Fatalf("updated TOML array = %s", updated.Candidate.Content)
	}
}

func TestTOMLConfigAppendsAfterTrailingArrayComma(t *testing.T) {
	t.Parallel()
	content := []byte("plugins = [\"user\",]\n")
	observed := observedFile("settings.toml", content)
	entry := testConfigEntry(adapter.ConfigTOML, []string{"plugins"}, adapter.ConfigElement, "managed-key", `"managed"`)
	compiled, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.toml", Observed: &observed}, Format: adapter.ConfigTOML,
		Desired: []adapter.ConfigEntry{entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(compiled.Candidate.Content, []byte(",,")) || !bytes.Contains(compiled.Candidate.Content, []byte(`"user","managed"`)) {
		t.Fatalf("TOML trailing-comma append = %s", compiled.Candidate.Content)
	}
}

func TestStructuredEntryHashSeparatesContainerShapeFromKind(t *testing.T) {
	t.Parallel()
	raw := []byte(`true`)
	field := structuredEntryHash(adapter.ConfigJSON, nil, adapter.ConfigField, "element", raw)
	element := structuredEntryHash(adapter.ConfigJSON, []string{"field"}, adapter.ConfigElement, "ignored", raw)
	if field == element {
		t.Fatalf("structured hashes collide: %s", field)
	}
}

func TestTOMLConfigRejectsDuplicateFullyQualifiedKey(t *testing.T) {
	t.Parallel()
	content := []byte("a.b = 1\n[a]\nb = 2\n")
	observed := observedFile("settings.toml", content)
	_, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.toml", Observed: &observed}, Format: adapter.ConfigTOML,
		Desired: []adapter.ConfigEntry{testConfigEntry(adapter.ConfigTOML, nil, adapter.ConfigField, "managed", `true`)},
	})
	if err == nil || !strings.Contains(err.Error(), CodeDuplicateConfigEntry) {
		t.Fatalf("duplicate TOML error = %v", err)
	}
}

func TestTOMLConfigRejectsScalarUsedAsTablePrefix(t *testing.T) {
	t.Parallel()
	content := []byte("a = 1\na.b = 2\n")
	observed := observedFile("settings.toml", content)
	_, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.toml", Observed: &observed}, Format: adapter.ConfigTOML,
		Desired: []adapter.ConfigEntry{testConfigEntry(adapter.ConfigTOML, nil, adapter.ConfigField, "managed", `true`)},
	})
	if err == nil || !strings.Contains(err.Error(), CodeDuplicateConfigEntry) {
		t.Fatalf("TOML scalar-prefix error = %v", err)
	}
}

func TestTOMLConfigRemovalKeepsUnownedCommentsAndFields(t *testing.T) {
	t.Parallel()
	entry := testConfigEntry(adapter.ConfigTOML, nil, adapter.ConfigField, "managed", `1`)
	initial := compileMissingConfig(t, adapter.ConfigTOML, entry)
	content := []byte("# keep\nuser = 2\nmanaged = 1\n")
	previous := configTarget("settings.toml", content, realize.OwnershipShared, initial.Managed)
	observed := observedFile("settings.toml", content)
	removed, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.toml", Observed: &observed, Previous: &previous}, Format: adapter.ConfigTOML,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(removed.Candidate.Content, []byte("managed =")) || !bytes.Contains(removed.Candidate.Content, []byte("# keep")) || !bytes.Contains(removed.Candidate.Content, []byte("user = 2")) {
		t.Fatalf("removed TOML = %s", removed.Candidate.Content)
	}
}

func compileMissingConfig(t *testing.T, format adapter.ConfigFormat, entries ...adapter.ConfigEntry) adapter.SharedCompilation {
	t.Helper()
	compiled, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings." + string(format)}, Format: format, Desired: entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func testConfigEntry(_ adapter.ConfigFormat, container []string, kind adapter.ConfigEntryKind, key, raw string) adapter.ConfigEntry {
	owner := adapter.OwnerRef{Source: "github:owner/pkg", ArtifactID: "hook-" + strings.ReplaceAll(key, "_", "-"), SourcePath: "hooks/entry.sh", Kind: adapter.ArtifactHook}
	return adapter.ConfigEntry{
		Owner: owner, Container: append([]string(nil), container...), Kind: kind, Key: key,
		EncodedValue: []byte(raw), AdapterID: "test-adapter",
	}
}

func configTarget(path string, content []byte, ownership realize.Ownership, managed []adapter.ManagedResult) realize.Target {
	entries := make([]realize.Entry, 0, len(managed))
	for _, result := range managed {
		entries = append(entries, realize.Entry{
			Source: result.Owner.Source, ArtifactID: result.Owner.ArtifactID, SourcePath: result.Owner.SourcePath,
			ArtifactKind: result.Kind, Adapter: "test-adapter", AdapterVersion: "1.0.0", ManagedHash: result.ManagedHash,
		})
	}
	return realize.Target{Path: path, Mode: 0o644, Ownership: ownership, OutputHash: hashBytes(content), Entries: entries}
}

func assertConfigCompilationConflictsWithoutWrite(t *testing.T, path string, content []byte, previous realize.Target, compiled adapter.SharedCompilation) {
	t.Helper()
	root := t.TempDir()
	filename := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, content, 0o644); err != nil {
		t.Fatal(err)
	}
	intent := realize.Intent{
		Action: compiled.Action, Path: path,
		ObservedHash: compiled.Proof.ObservedHash, ManagedIntact: compiled.Proof.ManagedIntact,
		PreservedContent: compiled.Proof.PreservedContent,
	}
	if compiled.Candidate != nil {
		intent.Content = compiled.Candidate.Content
		intent.Mode = uint32(compiled.Candidate.Mode.Perm())
		intent.Ownership = compiled.Candidate.Ownership
		intent.Entries = configTarget(path, compiled.Candidate.Content, compiled.Candidate.Ownership, compiled.Managed).Entries
	}
	finalized := false
	plan, err := realize.NewEngine().Run(root, realize.Ledger{
		SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{previous},
	}, []realize.Intent{intent}, realize.ModeApply, func(realize.Ledger) error {
		finalized = true
		return nil
	})
	var conflict *realize.ConflictError
	if err == nil || !errors.As(err, &conflict) || !plan.HasConflicts() || finalized {
		t.Fatalf("real compiler intent plan = %#v, finalized = %t, err = %v; want conflict", plan, finalized, err)
	}
	for _, operation := range plan.Operations {
		if operation.Kind == realize.OperationPromote {
			t.Fatalf("real compiler intent promoted without unmanaged content: %#v", plan)
		}
	}
	got, readErr := os.ReadFile(filename)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("conflicting real compiler intent changed tree: got %q, want %q", got, content)
	}
}
