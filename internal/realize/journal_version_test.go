package realize

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionTwoJournalStillRecovers(t *testing.T) {
	project := t.TempDir()
	const vendor = ".agents/vendor/zeta/alpha"
	writeFile(t, project, vendor+"/rules/rule.md", "vendor rule\n")
	writeFile(t, project, "agents.yaml", "agents: [cursor]\n")
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	hash, err := fingerprintVendorTree(root, vendor, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := []byte("agents: [codex]\n")
	writeJournalFixture(t, project, journalManifest{SchemaVersion: 2, ID: "released-vendor", Entries: []journalEntry{
		{Path: vendor, Operation: "vendor-promote", AfterExists: true, AfterHash: hash},
		{Path: "agents.yaml", BeforeExists: true, BeforeHash: contentHash(before), BeforeSize: int64(len(before)), BeforeMode: 0o644,
			BeforeImage: "before/000000", AfterExists: true, AfterHash: contentHash([]byte("agents: [cursor]\n")), AfterMode: 0o644},
	}}, before)
	if err := RecoverTransactions(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, project, "agents.yaml"); got != string(before) {
		t.Fatalf("v2 recovery restored agents.yaml to %q", got)
	}
	if _, err := root.Stat(vendor); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("v2 recovery left a live promoted vendor tree: %v", err)
	}
	if got := readFile(t, project, recoveredVendorPath("released-vendor", 0)+"/rules/rule.md"); got != "vendor rule\n" {
		t.Fatal("v2 recovery did not conserve the rolled-back vendor bytes")
	}
}

func TestJournalVersionRejectsMetadataFromAnotherFormat(t *testing.T) {
	for _, fixture := range []struct {
		name, manifest, want string
		unsupported          bool
	}{
		{"v1 exclusion draft", `{"schemaVersion":1,"id":"fixture","entries":[{"path":".git/info/exclude","exclusionEdit":{"beforeGeneration":{}}}]}`, "requires schemaVersion 3", false},
		{"v2 null exclusion key", `{"schemaVersion":2,"id":"fixture","entries":[{"path":".git/info/exclude","exclusionEdit":null}]}`, "requires schemaVersion 3", false},
		{"v2 shadowed entries", `{"schemaVersion":2,"id":"fixture","entries":[{"path":".git/info/exclude","exclusionEdit":{}}],"entries":[]}`, "requires schemaVersion 3", false},
		{"v3 missing exclusion", `{"schemaVersion":3,"id":"fixture","entries":[]}`, "requires Git exclusion metadata", false},
		{"v3 unknown field", `{"schemaVersion":3,"id":"fixture","entries":[],"extra":false}`, "unknown field", false},
		{"v3 invalid exclusion operation", `{"schemaVersion":3,"id":"fixture","entries":[{"path":".git/info/exclude","gitExclusion":true,"physicalRoot":"/repository/.git/info","physicalPath":"exclude","operation":"remove","exclusionEdit":{}}]}`, "supported operation", false},
		{"v3 trailing JSON", `{"schemaVersion":3,"id":"fixture","entries":[]} {}`, "decode journal", false},
		{"old then future", `{"schemaVersion":1,"schemaVersion":4,"id":"fixture","entries":[]}`, "exactly one", false},
		{"future then old", `{"schemaVersion":4,"schemaVersion":1,"id":"fixture","entries":[]}`, "exactly one", false},
		{"equal versions", `{"schemaVersion":3,"schemaVersion":3,"id":"fixture","entries":[]}`, "exactly one", false},
		{"uppercase alias", `{"SCHEMAVERSION":3,"id":"fixture","entries":[]}`, "case-exact", false},
		{"alias after exact", `{"schemaVersion":4,"SchemaVersion":3,"id":"fixture","entries":[]}`, "case-exact", false},
		{"alias before exact", `{"schemaversion":1,"schemaVersion":4,"id":"fixture","entries":[]}`, "case-exact", false},
		{"no version", `{"id":"fixture","entries":[{"schemaVersion":4}]}`, "missing schemaVersion", false},
		{"null version", `{"schemaVersion":null,"id":"fixture","entries":[]}`, "must be an integer", false},
		{"string version", `{"schemaVersion":"3","id":"fixture","entries":[]}`, "must be an integer", false},
		{"fractional version", `{"schemaVersion":3.0,"id":"fixture","entries":[]}`, "must be an integer", false},
		{"exponent version", `{"schemaVersion":3e0,"id":"fixture","entries":[]}`, "must be an integer", false},
		{"object version", `{"schemaVersion":{},"id":"fixture","entries":[]}`, "must be an integer", false},
		{"overflow version", `{"schemaVersion":123456789012345678901,"id":"fixture","entries":[]}`, "must be an integer", false},
		{"future shape", `{"schemaVersion":4,"id":"fixture","entries":{"new":"shape"},"newField":true}`, "run the matching ACR version", true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			project := t.TempDir()
			journal := filepath.Join(transactionDirectory, "fixture", journalManifestFilename)
			staging := filepath.Join(transactionDirectory, ".staging-future", "evidence")
			writeFile(t, project, journal, fixture.manifest)
			writeFile(t, project, staging, "uninterpreted staging\n")
			writeFile(t, project, "agents.yaml", "agents: [cursor]\n")
			err := RecoverTransactions(t.Context(), project)
			var version *UnsupportedJournalVersionError
			var conflict *RecoveryConflictError
			if err == nil || !strings.Contains(err.Error(), fixture.want) ||
				errors.As(err, &version) != fixture.unsupported || errors.As(err, &conflict) == fixture.unsupported {
				t.Fatalf("error = %v, want %q", err, fixture.want)
			}
			if got := readFile(t, project, journal); !bytes.Equal([]byte(got), []byte(fixture.manifest)) {
				t.Fatal("rejected journal was modified")
			}
			if got := readFile(t, project, "agents.yaml"); got != "agents: [cursor]\n" {
				t.Fatalf("rejected journal mutated project state: %q", got)
			}
			if got := readFile(t, project, staging); got != "uninterpreted staging\n" {
				t.Fatal("rejected journal altered staging")
			}
		})
	}
}

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
	if err := RecoverTransactions(t.Context(), project); err != nil {
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
			id, journalDir, err := createJournal(t.Context(), project, nil, test.vendors, &gitExclusionClaim{})
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
			if err := RecoverTransactions(t.Context(), project); !errors.As(err, &conflict) {
				t.Fatalf("version 1 vendor journal error = %v, want RecoveryConflictError", err)
			}
			if _, err := os.Stat(filepath.Join(journalDir, vendorStageName(0))); err != nil {
				t.Fatalf("rejected journal lost its staged package: %v", err)
			}
		})
	}
}
