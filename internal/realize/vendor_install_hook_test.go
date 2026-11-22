package realize

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestVendorPromotionSyncFailureRollsBack(t *testing.T) {
	project := t.TempDir()
	first := ".agents/vendor/example/alpha"
	second := ".agents/vendor/example/bravo"
	injected := errors.New("injected directory sync failure after promotion")
	oldSync := journalDirectorySync
	t.Cleanup(func() { journalDirectorySync = oldSync })
	failed := false
	journalDirectorySync = func(directory string) error {
		if !failed && directory == filepath.Dir(filepath.Join(project, second)) {
			if _, err := os.Stat(filepath.Join(project, second)); err == nil {
				failed = true
				return injected
			}
		}
		return oldSync(directory)
	}
	files := []VendorTreeFile{{Path: "README.md", Content: []byte("vendor package\n"), Mode: 0o644}}
	ledger := Ledger{SchemaVersion: CurrentLedgerSchemaVersion}
	_, err := NewEngine().ApplyStateFilesWithVendorTrees(project, ledger, ledger, nil, func(Ledger) ([]StateFile, error) {
		return []StateFile{{Path: "agents.yaml", Content: []byte("agents: []\n"), Mode: 0o644}}, nil
	}, []VendorTreeInstall{{Path: first, Files: files}, {Path: second, Files: files}})
	if !errors.Is(err, injected) || !failed {
		t.Fatalf("apply error = %v, want injected sync failure", err)
	}
	for _, filename := range []string{first, second, ".agents/vendor", transactionDirectory, "agents.yaml"} {
		if _, err := os.Lstat(filepath.Join(project, filename)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("failed promotion left %s: %v", filename, err)
		}
	}
}

func TestVendorRollbackPreservesWriteThroughOpenFile(t *testing.T) {
	project := t.TempDir()
	const destination = ".agents/vendor/example/alpha"
	const stagedFile = "vendor-000000/README.md"
	const before = "vendor\n"
	const edited = "edited\n"
	originalRename, originalSync := journalRename, journalDirectorySync
	t.Cleanup(func() { journalRename, journalDirectorySync = originalRename, originalSync })
	interrupted := errors.New("injected failure retiring transaction")
	var journal string
	journalRename = func(from, to string) error {
		if strings.HasPrefix(filepath.Base(to), ".staging-complete-") {
			journal = from
			return interrupted
		}
		return originalRename(from, to)
	}
	ledger := Ledger{SchemaVersion: CurrentLedgerSchemaVersion}
	_, err := NewEngine().ApplyStateFilesWithVendorTrees(project, ledger, ledger, nil, func(Ledger) ([]StateFile, error) {
		return nil, nil
	}, []VendorTreeInstall{{Path: destination, Files: []VendorTreeFile{{Path: "README.md", Content: []byte(before), Mode: 0o644}}}})
	if !errors.Is(err, interrupted) {
		t.Fatalf("apply error = %v, want interrupted retirement", err)
	}
	journalRename = originalRename
	file, err := os.OpenFile(filepath.Join(project, destination, "README.md"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	journalDirectorySync = func(directory string) error {
		if directory == journal {
			if _, err := file.WriteAt([]byte(edited), 0); err != nil {
				return err
			}
		}
		return originalSync(directory)
	}
	var conflict *RecoveryConflictError
	if err := RecoverTransactions(project); !errors.As(err, &conflict) {
		t.Fatalf("recover with concurrent write = %v, want recovery conflict", err)
	}
	if content, err := os.ReadFile(filepath.Join(journal, stagedFile)); err != nil || string(content) != edited {
		t.Fatalf("concurrent content in retained journal = %q, %v", content, err)
	}
}

// ArmVendorMigrationCrash is visible only to the external test package.
func ArmVendorMigrationCrash(point string) {
	kill := func() {
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			panic(err)
		}
		select {}
	}
	transactionRenameHook = func(target string) {
		if point == target {
			kill()
		}
	}
	vendorTreeSnapshotAfterOpen = func(target string) {
		if point == "stage" && strings.HasPrefix(target, "vendor-000000/") {
			kill()
		}
	}
	journalRename = func(from, to string) error {
		if err := os.Rename(from, to); err != nil {
			return err
		}
		if (point == "publish" && strings.HasPrefix(filepath.Base(from), ".staging-")) ||
			(point == "retire" && strings.HasPrefix(filepath.Base(to), ".staging-complete-")) {
			kill()
		}
		return nil
	}
}
