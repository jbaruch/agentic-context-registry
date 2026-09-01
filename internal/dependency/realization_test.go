package dependency

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestRealizationEnginePersistsLedgerThroughRegistryLock(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion},
		Lock:    Lockfile{SchemaVersion: CurrentSchemaVersion},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	content := []byte("managed\n")
	intent := realize.Intent{
		Path: ".agent/rules.md", Content: content, Mode: 0o644, Ownership: realize.OwnershipGenerated,
		Entries: []realize.Entry{{
			Source: "github:owner/plugin", ArtifactID: "rule", ArtifactKind: realize.ArtifactFile,
			SourcePath: "rules/source.md", Adapter: "test-adapter", AdapterVersion: "1.0.0",
			ManagedHash: realizationTestHash(content),
		}},
	}
	current, err := realize.DecodeLedger(state.Lock.Realization)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := realize.NewEngine().Run(root, current, []realize.Intent{intent}, realize.ModeApply, func(ledger realize.Ledger) error {
		encoded, err := realize.EncodeLedger(ledger)
		if err != nil {
			return err
		}
		state.Lock.Realization = encoded
		return WriteState(root, state)
	})
	if err != nil || !plan.HasChanges() {
		t.Fatalf("Run(apply) = %#v, %v", plan, err)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := realize.DecodeLedger(loaded.Lock.Realization)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Targets) != 1 || persisted.Targets[0].Path != intent.Path || persisted.Targets[0].OutputHash != realizationTestHash(content) {
		t.Fatalf("persisted ledger = %#v", persisted)
	}
	second, err := realize.NewEngine().Run(root, persisted, []realize.Intent{intent}, realize.ModeCheck, nil)
	if err != nil || second.HasChanges() {
		t.Fatalf("second realization = %#v, %v", second, err)
	}
}

func realizationTestHash(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}
