package realize

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Version 1 is the journal that releases without vendor entries read and write.
func TestVersionOneJournalStillRecovers(t *testing.T) {
	project := t.TempDir()
	before, after := []byte("schemaVersion: 2\nagents: [codex]\n"), []byte("schemaVersion: 2\nagents: [claude-code]\n")
	if err := os.WriteFile(filepath.Join(project, "agents.yaml"), after, 0o644); err != nil {
		t.Fatal(err)
	}
	writeJournalFixture(t, project, journalManifest{SchemaVersion: 1, ID: "released", Entries: []journalEntry{{
		Path: "agents.yaml", BeforeExists: true, BeforeHash: contentHash(before), BeforeSize: int64(len(before)), BeforeMode: 0o644,
		BeforeImage: "before/000000", AfterExists: true, AfterHash: contentHash(after), AfterMode: 0o644,
	}}}, before)
	if err := RecoverTransactions(project); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(project, "agents.yaml")); err != nil || string(content) != string(before) {
		t.Fatalf("recovered agents.yaml = %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(project, transactionDirectory, "released")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered journal remains: %v", err)
	}
}

func TestJournalVersionIdentifiesVendorEntries(t *testing.T) {
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, transactionDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	install := VendorTreeChanges{Installs: []VendorTreeInstall{{
		Path: ".agents/vendor/zeta/alpha", Files: []VendorTreeFile{{Path: "rules/rule.md", Content: []byte("rule\n"), Mode: 0o644}},
	}}}
	for _, test := range []struct {
		name    string
		vendors VendorTreeChanges
		version int
	}{
		{name: "realization only", version: journalSchemaVersion},
		{name: "vendor promotion", vendors: install, version: vendorJournalSchemaVersion},
	} {
		t.Run(test.name, func(t *testing.T) {
			id, journalDir, err := createJournal(project, nil, test.vendors)
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(journalDir)
			manifest, err := loadJournal(project, id)
			if err != nil || manifest.SchemaVersion != test.version {
				t.Fatalf("journal version = %d, %v; want %d", manifest.SchemaVersion, err, test.version)
			}
			if test.version == journalSchemaVersion {
				return
			}
			// The version 1 contract has no vendor operations to recover.
			manifest.SchemaVersion = journalSchemaVersion
			encoded, err := marshalJournal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(journalDir, journalManifestFilename), encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			var conflict *RecoveryConflictError
			if err := RecoverTransactions(project); !errors.As(err, &conflict) {
				t.Fatalf("version 1 vendor journal error = %v, want RecoveryConflictError", err)
			}
			if _, err := os.Stat(filepath.Join(journalDir, vendorStageName(0))); err != nil {
				t.Fatalf("rejected journal lost its staged package: %v", err)
			}
		})
	}
}
