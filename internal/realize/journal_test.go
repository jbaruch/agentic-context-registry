package realize

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestInterruptedMigrationRecoversBeforeRetry(t *testing.T) {
	if os.Getenv("ACR_TEST_JOURNAL_CHILD") == "1" {
		transactionID = func() (string, error) { return os.Getenv("ACR_TEST_TRANSACTION_ID"), nil }
		killAfter, err := strconv.Atoi(os.Getenv("ACR_TEST_KILL_AFTER_RENAME"))
		if err != nil || killAfter < 1 {
			os.Exit(88)
		}
		renames := 0
		transactionRenameHook = func(string) {
			renames++
			if renames == killAfter {
				os.Exit(86)
			}
		}
		project := os.Getenv("ACR_TEST_PROJECT")
		_, _ = NewEngine().RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, testStateFinalizer)
		os.Exit(87)
	}
	project := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestInterruptedMigrationRecoversBeforeRetry$")
	command.Env = append(os.Environ(),
		"ACR_TEST_JOURNAL_CHILD=1",
		"ACR_TEST_PROJECT="+project,
		"ACR_TEST_TRANSACTION_ID=tx-test-1",
		"ACR_TEST_KILL_AFTER_RENAME=1",
	)
	err := command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("child error = %v", err)
	}
	journal := filepath.Join(project, transactionDirectory, "tx-test-1", journalManifestFilename)
	if info, err := os.Stat(journal); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal manifest = %v, %v", info, err)
	}
	if _, err := NewEngine().RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, testStateFinalizer); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered journal remains: %v", err)
	}
	plan, err := NewEngine().RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeDryRun, testStateFinalizer)
	if err != nil {
		t.Fatal(err)
	}
	if plan.HasChanges() {
		t.Fatalf("third plan = %#v, want empty", plan)
	}

	assertJournalBarrierOrder(t)
}

func assertJournalBarrierOrder(t *testing.T) {
	t.Helper()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "agents.yaml"), []byte("schemaVersion: 2\nagents: [claude-code]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	originalHook := transactionRenameHook
	originalWriter := journalFileWriter
	originalSync := journalDirectorySync
	originalRename := journalRename
	originalID := transactionID
	t.Cleanup(func() {
		transactionRenameHook = originalHook
		journalFileWriter = originalWriter
		journalDirectorySync = originalSync
		journalRename = originalRename
		transactionID = originalID
	})

	var events []string
	relative := func(filename string) string {
		value, err := filepath.Rel(project, filename)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.ToSlash(value)
	}
	transactionID = func() (string, error) { return "order-test", nil }
	transactionRenameHook = func(target string) { events = append(events, "target-rename:"+target) }
	journalDirectorySync = func(directory string) error {
		if err := originalSync(directory); err != nil {
			return err
		}
		events = append(events, "sync-dir:"+relative(directory))
		return nil
	}
	journalRename = func(oldPath, newPath string) error {
		if err := originalRename(oldPath, newPath); err != nil {
			return err
		}
		events = append(events, "journal-rename:"+relative(oldPath)+"->"+relative(newPath))
		return nil
	}
	journalFileWriter = func(filename string, content []byte, mode os.FileMode) error {
		temporary := filename + ".tmp"
		file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		written, writeErr := file.Write(content)
		if writeErr == nil && written != len(content) {
			writeErr = io.ErrShortWrite
		}
		if writeErr != nil {
			return errors.Join(writeErr, file.Close())
		}
		events = append(events, "temp:"+relative(temporary))
		if err := file.Sync(); err != nil {
			return errors.Join(err, file.Close())
		}
		events = append(events, "sync-file:"+relative(temporary))
		if err := file.Close(); err != nil {
			return err
		}
		if err := os.Rename(temporary, filename); err != nil {
			return err
		}
		events = append(events, "file-rename:"+relative(temporary)+"->"+relative(filename))
		return syncDirectory(filepath.Dir(filename))
	}

	if _, err := NewEngine().RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, testStateFinalizer); err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{
		"temp:.agents/.acr-transactions/.staging-order-test/before/000001.tmp",
		"sync-file:.agents/.acr-transactions/.staging-order-test/before/000001.tmp",
		"file-rename:.agents/.acr-transactions/.staging-order-test/before/000001.tmp->.agents/.acr-transactions/.staging-order-test/before/000001",
		"temp:.agents/.acr-transactions/.staging-order-test/manifest.json.tmp",
		"sync-file:.agents/.acr-transactions/.staging-order-test/manifest.json.tmp",
		"file-rename:.agents/.acr-transactions/.staging-order-test/manifest.json.tmp->.agents/.acr-transactions/.staging-order-test/manifest.json",
		"sync-dir:.agents/.acr-transactions/.staging-order-test/before",
		"sync-dir:.agents/.acr-transactions/.staging-order-test",
		"journal-rename:.agents/.acr-transactions/.staging-order-test->.agents/.acr-transactions/order-test",
		"sync-dir:.agents/.acr-transactions",
		"target-rename:.agents/registry.lock",
	}
	if len(events) < len(wantPrefix) || !reflect.DeepEqual(events[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("journal barrier events = %#v, want prefix %#v", events, wantPrefix)
	}
}

func testStateFinalizer(Ledger) ([]StateFile, error) {
	return []StateFile{
		{Path: "agents.yaml", Content: []byte("schemaVersion: 2\nagents: [codex]\n"), Mode: 0o644},
		{Path: ".agents/registry.lock", Content: []byte("schemaVersion: 2\n"), Mode: 0o644},
	}, nil
}

func TestRealizeRecoversPendingMigrationJournalBeforePlan(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "owned.md"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedInterruptedJournal(t, project, "owned.md", []byte("after\n"))

	plan, err := NewEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, func(Ledger) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if plan.HasChanges() {
		t.Fatalf("post-recovery plan has changes: %#v", plan)
	}
	content, err := os.ReadFile(filepath.Join(project, "owned.md"))
	if err != nil || string(content) != "before\n" {
		t.Fatalf("recovered target = %q, %v", content, err)
	}
}

func TestRecoverApplyFailureRestoresJournaledWrites(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "owned.md"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedInterruptedJournal(t, project, "owned.md", []byte("after\n"))

	injected := errors.New("injected apply failure")
	journalDir := filepath.Join(project, transactionDirectory, "test-transaction")
	err := recoverApplyFailure(project, journalDir, injected)
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "all filesystem changes were rolled back") {
		t.Fatalf("recoverApplyFailure() error = %v", err)
	}
	if got := readFile(t, project, "owned.md"); got != "before\n" {
		t.Fatalf("recovered content = %q, want before image", got)
	}
	info, err := os.Stat(filepath.Join(project, "owned.md"))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("recovered mode = %v, %v, want 0644", info.Mode().Perm(), err)
	}
	if _, err := os.Stat(journalDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered journal remains: %v", err)
	}
}

func TestRecoveryConflictPreservesConcurrentEdit(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "owned.md"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedInterruptedJournal(t, project, "owned.md", []byte("after\n"))
	if err := os.WriteFile(filepath.Join(project, "owned.md"), []byte("operator\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := NewEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, func(Ledger) error { return nil })
	var conflict *RecoveryConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want RecoveryConflictError", err)
	}
	content, readErr := os.ReadFile(filepath.Join(project, "owned.md"))
	if readErr != nil || string(content) != "operator\n" {
		t.Fatalf("concurrent edit = %q, %v", content, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(project, transactionDirectory, "test-transaction", journalManifestFilename)); statErr != nil {
		t.Fatalf("journal was not preserved: %v", statErr)
	}
}

func TestRecoveryRejectsJournalPathsOutsideProject(t *testing.T) {
	requireGit(t)

	t.Run("physical root", func(t *testing.T) {
		project := t.TempDir()
		if output, err := exec.Command("git", "init", "-q", project).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, output)
		}
		outside := t.TempDir()
		before := []byte("attacker-controlled\n")
		manifest := journalManifest{
			SchemaVersion: journalSchemaVersion,
			ID:            "crafted",
			Entries: []journalEntry{{
				Path: gitExcludePath, BeforeExists: true, BeforeHash: contentHash(before), BeforeSize: int64(len(before)), BeforeMode: 0o600,
				BeforeImage: "before/000000", AfterExists: false, GitExclusion: true, PhysicalRoot: outside, PhysicalPath: "victim",
			}},
		}
		writeJournalFixture(t, project, manifest, before)

		_, err := NewEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, func(Ledger) error { return nil })
		var conflict *RecoveryConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("error = %v, want RecoveryConflictError", err)
		}
		if _, err := os.Stat(filepath.Join(outside, "victim")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("crafted journal wrote outside project: %v", err)
		}
		if _, err := os.Stat(filepath.Join(project, transactionDirectory, "crafted", journalManifestFilename)); err != nil {
			t.Fatalf("crafted journal was not preserved: %v", err)
		}
	})

	t.Run("before image", func(t *testing.T) {
		project := t.TempDir()
		before := []byte("before\n")
		if err := os.WriteFile(filepath.Join(project, "owned.md"), before, 0o644); err != nil {
			t.Fatal(err)
		}
		manifest := journalManifest{
			SchemaVersion: journalSchemaVersion,
			ID:            "crafted",
			Entries: []journalEntry{{
				Path: "owned.md", BeforeExists: true, BeforeHash: contentHash(before), BeforeSize: int64(len(before)), BeforeMode: 0o644,
				BeforeImage: "../outside", AfterExists: true, AfterHash: contentHash(before), AfterMode: 0o644,
			}},
		}
		writeJournalFixture(t, project, manifest, nil)

		_, err := NewEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, func(Ledger) error { return nil })
		var conflict *RecoveryConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("error = %v, want RecoveryConflictError", err)
		}
	})
}

func writeJournalFixture(t *testing.T, project string, manifest journalManifest, before []byte) {
	t.Helper()
	journal := filepath.Join(project, transactionDirectory, manifest.ID)
	if err := os.MkdirAll(filepath.Join(journal, "before"), 0o700); err != nil {
		t.Fatal(err)
	}
	if before != nil {
		if err := os.WriteFile(filepath.Join(journal, "before", "000000"), before, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(journal, journalManifestFilename), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedJournalVersionFailsClosed(t *testing.T) {
	for _, version := range []int{0, 999} {
		version := version
		t.Run(decimal(version), func(t *testing.T) {
			project := t.TempDir()
			journal := filepath.Join(project, transactionDirectory, "old")
			if err := os.MkdirAll(journal, 0o700); err != nil {
				t.Fatal(err)
			}
			manifest := []byte(`{"schemaVersion":` + decimal(version) + `,"id":"old","entries":[]}` + "\n")
			if err := os.WriteFile(filepath.Join(journal, journalManifestFilename), manifest, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := NewEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeDryRun, nil)
			var unsupported *UnsupportedJournalVersionError
			if !errors.As(err, &unsupported) {
				t.Fatalf("error = %v, want UnsupportedJournalVersionError", err)
			}
			stored, readErr := os.ReadFile(filepath.Join(journal, journalManifestFilename))
			if readErr != nil || string(stored) != string(manifest) {
				t.Fatalf("journal changed = %q, %v", stored, readErr)
			}
		})
	}
}

func TestReadOnlyCommandPendingJournalWritesNothing(t *testing.T) {
	for _, mode := range []Mode{ModeDryRun, ModeCheck} {
		project := t.TempDir()
		journal := filepath.Join(project, transactionDirectory, "pending")
		if err := os.MkdirAll(journal, 0o700); err != nil {
			t.Fatal(err)
		}
		manifest := []byte(`{"schemaVersion":1,"id":"pending","entries":[]}` + "\n")
		if err := os.WriteFile(filepath.Join(journal, journalManifestFilename), manifest, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, filename := range []string{filepath.Join(project, transactionDirectory, "pending", journalManifestFilename)} {
			if err := os.Chmod(filename, 0o444); err != nil {
				t.Fatal(err)
			}
		}
		for _, directory := range []string{journal, filepath.Dir(journal), filepath.Join(project, ".agents"), project} {
			if err := os.Chmod(directory, 0o555); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			for _, directory := range []string{project, filepath.Join(project, ".agents"), filepath.Dir(journal), journal} {
				_ = os.Chmod(directory, 0o755)
			}
		})
		_, err := NewEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, mode, nil)
		var pending *PendingTransactionError
		if !errors.As(err, &pending) {
			t.Fatalf("%s error = %v, want PendingTransactionError", mode, err)
		}
		if _, statErr := os.Stat(filepath.Join(project, transactionLockPath)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("%s created transaction lock: %v", mode, statErr)
		}
	}
}

func TestTransactionLockUnavailableFailsClosed(t *testing.T) {
	original := transactionFlock
	defer func() { transactionFlock = original }()
	for _, test := range []struct {
		name string
		err  error
		busy bool
	}{{"enolck", syscall.ENOLCK, false}, {"eopnotsupp", syscall.EOPNOTSUPP, false}, {"busy", syscall.EWOULDBLOCK, true}} {
		t.Run(test.name, func(t *testing.T) {
			project := t.TempDir()
			transactionFlock = func(int, int) error { return test.err }
			_, err := NewEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, func(Ledger) error { return nil })
			if test.busy {
				var busy *TransactionBusyError
				if !errors.As(err, &busy) {
					t.Fatalf("error = %v, want TransactionBusyError", err)
				}
			} else {
				var unavailable *TransactionLockUnavailableError
				if !errors.As(err, &unavailable) {
					t.Fatalf("error = %v, want TransactionLockUnavailableError", err)
				}
			}
			if _, statErr := os.Stat(filepath.Join(project, transactionDirectory)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed claim left transaction directory: %v", statErr)
			}
		})
	}
}

func TestEngineRunReturnsClaimCloseError(t *testing.T) {
	original := transactionFlock
	t.Cleanup(func() { transactionFlock = original })
	injected := errors.New("injected unlock failure")
	transactionFlock = func(_ int, operation int) error {
		if operation == syscall.LOCK_UN {
			return injected
		}
		return nil
	}
	project := t.TempDir()
	_, err := NewEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, func(Ledger) error { return nil })
	if !errors.Is(err, injected) {
		t.Fatalf("Run() error = %v, want claim close failure", err)
	}
}

func TestConvergedRunRemovesEmptyTransactionClaimResidue(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	agentsRoot := filepath.Join(project, ".agents")
	if err := os.Mkdir(agentsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	registryLock := filepath.Join(agentsRoot, "registry.lock")
	if err := os.WriteFile(registryLock, []byte("schemaVersion: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := NewEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, func(Ledger) error { return nil })
	if err != nil || plan.HasChanges() {
		t.Fatalf("converged Run() = %#v, %v", plan, err)
	}
	if _, err := os.Stat(filepath.Join(project, transactionDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("converged run left transaction claim residue: %v", err)
	}
	content, err := os.ReadFile(registryLock)
	if err != nil || string(content) != "schemaVersion: 1\n" {
		t.Fatalf("existing registry lock = %q, %v", content, err)
	}
}

func TestConcurrentRecoveryIsSerializedByJournalClaim(t *testing.T) {
	project := t.TempDir()
	holder, err := claimTransactions(project)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(project, transactionLockPath))
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, func(Ledger) error { return nil })
	var busy *TransactionBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("contender error = %v, want TransactionBusyError", err)
	}
	after, err := os.ReadFile(filepath.Join(project, transactionLockPath))
	if err != nil || string(after) != string(before) {
		t.Fatalf("contender changed claim = %q, %v", after, err)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPartialBeforeImageIsNeverRestored(t *testing.T) {
	for _, test := range []struct {
		name    string
		corrupt func(*testing.T, string)
	}{
		{
			name: "truncated before-image",
			corrupt: func(t *testing.T, project string) {
				beforeImage := filepath.Join(project, transactionDirectory, "test-transaction", "before", "000000")
				if err := os.WriteFile(beforeImage, []byte("truncated"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing canonical manifest",
			corrupt: func(t *testing.T, project string) {
				manifest := filepath.Join(project, transactionDirectory, "test-transaction", journalManifestFilename)
				if err := os.Remove(manifest); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := t.TempDir()
			if err := os.WriteFile(filepath.Join(project, "owned.md"), []byte("before\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			seedInterruptedJournal(t, project, "owned.md", []byte("after\n"))
			test.corrupt(t, project)
			_, err := NewEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, func(Ledger) error { return nil })
			var conflict *RecoveryConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("error = %v, want RecoveryConflictError", err)
			}
			content, readErr := os.ReadFile(filepath.Join(project, "owned.md"))
			if readErr != nil || string(content) != "after\n" {
				t.Fatalf("target changed = %q, %v", content, readErr)
			}
		})
	}
}

func TestStaleTransactionStagingIsNonBlocking(t *testing.T) {
	project := t.TempDir()
	staging := filepath.Join(project, transactionDirectory, ".staging-old")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	plan, err := NewEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeDryRun, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.TransactionNotes) != 1 || plan.TransactionNotes[0].Code != "stale_transaction_staging" {
		t.Fatalf("notes = %#v", plan.TransactionNotes)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("dry-run removed staging: %v", err)
	}
	if _, err := NewEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, func(Ledger) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("apply retained staging: %v", err)
	}
}

func seedInterruptedJournal(t *testing.T, project, target string, after []byte) {
	t.Helper()
	claim, err := claimTransactions(project)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotFile(root, target)
	if err != nil {
		t.Fatal(err)
	}
	originalID := transactionID
	transactionID = func() (string, error) { return "test-transaction", nil }
	operation := Operation{Kind: OperationUpdate, Path: target, BeforeHash: snapshot.hash, AfterHash: contentHash(after), Mode: 0o644, content: after, beforeExists: true, beforeMode: 0o644}
	_, _, err = createJournal(project, []preparedOperation{{operation: operation, root: root, path: target, snapshot: snapshot}})
	transactionID = originalID
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeOperation(root, operation); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := claim.Close(); err != nil {
		t.Fatal(err)
	}
}

func decimal(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 4)
	for value > 0 {
		digits = append(digits, byte('0'+value%10))
		value /= 10
	}
	for left, right := 0, len(digits)-1; left < right; left, right = left+1, right-1 {
		digits[left], digits[right] = digits[right], digits[left]
	}
	return string(digits)
}
