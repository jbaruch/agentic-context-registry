package realize

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func setFileTransactionID(t *testing.T, id string) {
	t.Helper()
	original := transactionID
	transactionID = func() (string, error) { return id, nil }
	t.Cleanup(func() { transactionID = original })
}

func TestRemovalBeforeImageIsStagedBeforeAnyRename(t *testing.T) {
	project := t.TempDir()
	filename := filepath.Join(project, "tessl.json")
	content := []byte("manifest\n")
	if err := os.WriteFile(filename, content, 0o640); err != nil {
		t.Fatal(err)
	}
	setFileTransactionID(t, "tx-staging")
	original := fileTransactionRename
	defer func() { fileTransactionRename = original }()
	checked := false
	fileTransactionRename = func(root *os.Root, oldname, newname string) error {
		manifestData, err := os.ReadFile(filepath.Join(project, transactionDirectory, "tx-staging", journalManifestFilename))
		if err != nil {
			t.Fatalf("read committed manifest before rename: %v", err)
		}
		var manifest journalManifest
		if err := json.Unmarshal(manifestData, &manifest); err != nil {
			t.Fatal(err)
		}
		if len(manifest.Entries) != 1 {
			t.Fatalf("journal entries = %#v", manifest.Entries)
		}
		entry := manifest.Entries[0]
		if entry.Operation != "remove" || entry.BeforeHash != contentHash(content) || entry.BeforeSize != int64(len(content)) || entry.BeforeMode != 0o640 || entry.RemovedImage == "" {
			t.Fatalf("removal journal entry = %#v", entry)
		}
		before, err := os.ReadFile(filepath.Join(project, transactionDirectory, "tx-staging", filepath.FromSlash(entry.BeforeImage)))
		if err != nil || string(before) != string(content) {
			t.Fatalf("before-image = %q, %v", before, err)
		}
		checked = true
		return original(root, oldname, newname)
	}
	edit := FileTransactionEdit{Path: "tessl.json", Operation: "remove", Before: content, BeforeMode: 0o640}
	if err := ApplyFileTransaction(project, []FileTransactionEdit{edit}); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("removal ran without observing the staging barrier")
	}
}

func TestRemovalFallsBackToBeforeImageOnEXDEV(t *testing.T) {
	project := t.TempDir()
	filename := filepath.Join(project, "tessl.json")
	content := []byte("manifest\n")
	if err := os.WriteFile(filename, content, 0o644); err != nil {
		t.Fatal(err)
	}
	setFileTransactionID(t, "tx-exdev")
	original := fileTransactionRename
	defer func() { fileTransactionRename = original }()
	fileTransactionRename = func(*os.Root, string, string) error { return syscall.EXDEV }
	edit := FileTransactionEdit{Path: "tessl.json", Operation: "remove", Before: content, BeforeMode: 0o644}
	if err := ApplyFileTransaction(project, []FileTransactionEdit{edit}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filename); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("EXDEV fallback retained target: %v", err)
	}
}

func TestRemovalRecoveryRestoresSymlinkFromRemovedArea(t *testing.T) {
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".agents", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	const target = "../../.tessl/plugins/example/orphan/skills/review"
	link := filepath.Join(project, ".agents", "skills", "tessl__review")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	claim, err := claimTransactions(project)
	if err != nil {
		t.Fatal(err)
	}
	setFileTransactionID(t, "tx-link")
	edit := FileTransactionEdit{Path: ".agents/skills/tessl__review", Operation: "remove", BeforeMode: 0o777, LinkTarget: target}
	_, journal, err := createFileTransactionJournal(project, []FileTransactionEdit{edit}, transactionID)
	if err != nil {
		t.Fatal(err)
	}
	removed := filepath.Join(journal, "removed", "000000")
	if err := os.Rename(link, removed); err != nil {
		t.Fatal(err)
	}
	if err := claim.Close(); err != nil {
		t.Fatal(err)
	}
	if err := RecoverTransactions(project); err != nil {
		t.Fatal(err)
	}
	restored, err := os.Readlink(link)
	if err != nil || restored != target {
		t.Fatalf("restored link = %q, %v", restored, err)
	}
}
