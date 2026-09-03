package realize

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// hostileTargets is the journaled target set the kill-after-N cases use: one
// native file plus both transactional state files, so N=1 is a mid-transaction
// kill and N=len is a kill after the last rename but before journal retirement.
func hostileTargets() []Intent {
	return []Intent{testIntent(".agent/hostile.md", "managed\n", OwnershipGenerated)}
}

func hostileStateFinalizer(Ledger) ([]StateFile, error) {
	return []StateFile{
		{Path: "agents.yaml", Content: []byte("schemaVersion: 2\nagents: [codex]\n"), Mode: 0o644},
		{Path: ".agents/registry.lock", Content: []byte("schemaVersion: 2\n"), Mode: 0o644},
	}, nil
}

func hostileEngine() *Engine { return newEngine(newPlanner(fakeGitInspector{})) }

// TestHostileKillAfterRenameRecoversThroughBothEntryPoints kills a real child
// process after the first and after the last journaled rename, then proves the
// surviving journal is canonical (never .staging-*) and that recovery converges
// through the realize entry point and through a second migrate-shaped apply.
func TestHostileKillAfterRenameRecoversThroughBothEntryPoints(t *testing.T) {
	if os.Getenv("ACR_TEST_HOSTILE_KILL_CHILD") == "1" {
		hostileKillChild()
		return
	}

	renameCount := hostileRenameCount(t)
	if renameCount < 3 {
		t.Fatalf("fixture produced %d journaled renames, want at least 3", renameCount)
	}

	for _, test := range []struct {
		name       string
		killAfter  int
		recoverVia string
	}{
		{name: "first rename recovered by realize", killAfter: 1, recoverVia: "realize"},
		{name: "first rename recovered by migrate", killAfter: 1, recoverVia: "migrate"},
		{name: "last rename recovered by realize", killAfter: renameCount, recoverVia: "realize"},
		{name: "last rename recovered by migrate", killAfter: renameCount, recoverVia: "migrate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := t.TempDir()
			seed := hostileWriteSeed(t, project)
			before := hostileHashTree(t, project)

			command := exec.Command(os.Args[0], "-test.run=^TestHostileKillAfterRenameRecoversThroughBothEntryPoints$")
			command.Env = append(os.Environ(),
				"ACR_TEST_HOSTILE_KILL_CHILD=1",
				"ACR_TEST_PROJECT="+project,
				"ACR_TEST_TRANSACTION_ID=tx-hostile",
				"ACR_TEST_KILL_AFTER_RENAME="+strconv.Itoa(test.killAfter),
			)
			var exitErr *exec.ExitError
			if err := command.Run(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
				t.Fatalf("child error = %v, want exit 86 (killed after rename %d)", err, test.killAfter)
			}

			journal := filepath.Join(project, transactionDirectory, "tx-hostile")
			if _, err := os.Stat(filepath.Join(journal, journalManifestFilename)); err != nil {
				t.Fatalf("kill after rename %d left no canonical journal: %v", test.killAfter, err)
			}
			for _, residue := range hostileStagingResidue(t, project) {
				t.Fatalf("kill after rename %d left staging residue %s", test.killAfter, residue)
			}

			switch test.recoverVia {
			case "realize":
				if _, err := hostileEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, func(Ledger) error { return nil }); err != nil {
					t.Fatalf("realize recovery: %v", err)
				}
			case "migrate":
				if _, err := hostileEngine().RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, func(Ledger) ([]StateFile, error) { return nil, nil }); err != nil {
					t.Fatalf("migrate recovery: %v", err)
				}
			}

			if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal survived recovery: %v", err)
			}
			restored := hostileHashTree(t, project)
			delete(restored, ".agents")
			delete(restored, ".agents/.acr-transactions")
			delete(restored, ".agents/.acr-transactions/.lock")
			delete(before, ".agents")
			delete(before, ".agents/.acr-transactions")
			delete(before, ".agents/.acr-transactions/.lock")
			if !hostileMapsEqual(before, restored) {
				t.Fatalf("recovery did not restore the before-images\nbefore=%v\nafter=%v", before, restored)
			}
			if got := readFile(t, project, "seed.md"); got != seed {
				t.Fatalf("untouched seed file changed to %q", got)
			}

			converged, err := hostileEngine().RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, hostileTargets(), ModeApply, hostileStateFinalizer)
			if err != nil {
				t.Fatalf("convergence apply: %v", err)
			}
			plan, err := hostileEngine().RunStateFiles(project, converged.NextLedger, hostileTargets(), ModeDryRun, hostileStateFinalizer)
			if err != nil {
				t.Fatalf("post-convergence plan: %v", err)
			}
			if plan.HasChanges() {
				t.Fatalf("plan after convergence has changes: %#v", plan.Operations)
			}
		})
	}
}

func hostileKillChild() {
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
	_, _ = hostileEngine().RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, hostileTargets(), ModeApply, hostileStateFinalizer)
	os.Exit(87)
}

func hostileRenameCount(t *testing.T) int {
	t.Helper()
	project := t.TempDir()
	hostileWriteSeed(t, project)
	plan, err := hostileEngine().RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, hostileTargets(), ModeDryRun, hostileStateFinalizer)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, operation := range plan.Operations {
		if operation.Kind != OperationPreserve {
			count++
		}
	}
	return count
}

func hostileWriteSeed(t *testing.T, project string) string {
	t.Helper()
	const seed = "operator content\n"
	if err := os.WriteFile(filepath.Join(project, "seed.md"), []byte(seed), 0o640); err != nil {
		t.Fatal(err)
	}
	return seed
}

func hostileStagingResidue(t *testing.T, project string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(project, transactionDirectory))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatal(err)
	}
	var residue []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".staging-") {
			residue = append(residue, entry.Name())
		}
	}
	return residue
}

// TestHostileUnsupportedJournalVersionFailsClosedInEveryMode pins both rejected
// schema versions against all three modes and proves nothing on disk moved.
func TestHostileUnsupportedJournalVersionFailsClosedInEveryMode(t *testing.T) {
	for _, version := range []int{0, 999} {
		for _, mode := range []Mode{ModeDryRun, ModeCheck, ModeApply} {
			t.Run(fmt.Sprintf("v%d/%s", version, mode), func(t *testing.T) {
				project := t.TempDir()
				hostileWriteSeed(t, project)
				journal := filepath.Join(project, transactionDirectory, "old")
				if err := os.MkdirAll(journal, 0o700); err != nil {
					t.Fatal(err)
				}
				manifest := []byte(`{"schemaVersion":` + strconv.Itoa(version) + `,"id":"old","entries":[]}` + "\n")
				if err := os.WriteFile(filepath.Join(journal, journalManifestFilename), manifest, 0o600); err != nil {
					t.Fatal(err)
				}
				before := hostileHashTree(t, project)

				_, err := hostileEngine().RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, hostileTargets(), mode, hostileStateFinalizer)
				var unsupported *UnsupportedJournalVersionError
				if !errors.As(err, &unsupported) {
					t.Fatalf("error = %v, want UnsupportedJournalVersionError", err)
				}

				after := hostileHashTree(t, project)
				// A mutating run legitimately creates the claim file before it
				// reads the journal; nothing else may move.
				delete(after, ".agents/.acr-transactions/.lock")
				delete(before, ".agents/.acr-transactions/.lock")
				if !hostileMapsEqual(before, after) {
					t.Fatalf("unsupported journal run mutated the tree\nbefore=%v\nafter=%v", before, after)
				}
				if _, statErr := os.Stat(filepath.Join(project, ".agent", "hostile.md")); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("unsupported journal run opened a target: %v", statErr)
				}
			})
		}
	}
}

// TestHostilePipeHeldFlockSerializesContender holds the claim in a real child
// process blocked on a pipe read, so LOCK_NB is proved by the contender's error
// rather than by any elapsed time.
func TestHostilePipeHeldFlockSerializesContender(t *testing.T) {
	if os.Getenv("ACR_TEST_HOSTILE_HOLDER") == "1" {
		hostileFlockHolder()
		return
	}

	project := t.TempDir()
	hostileWriteSeed(t, project)

	releaseRead, releaseWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestHostilePipeHeldFlockSerializesContender$")
	command.Env = append(os.Environ(), "ACR_TEST_HOSTILE_HOLDER=1", "ACR_TEST_PROJECT="+project)
	command.ExtraFiles = []*os.File{releaseRead, readyWrite}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := releaseRead.Close(); err != nil {
		t.Fatal(err)
	}
	if err := readyWrite.Close(); err != nil {
		t.Fatal(err)
	}
	// Blocking read on the holder's ready pipe: the test proceeds on the event,
	// never on a timer.
	ready := make([]byte, 2)
	if _, err := readyRead.Read(ready); err != nil {
		t.Fatalf("holder never signalled the acquired claim: %v", err)
	}

	before := hostileHashTree(t, project)
	_, err = hostileEngine().RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, hostileTargets(), ModeApply, hostileStateFinalizer)
	var busy *TransactionBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("contender error = %v, want TransactionBusyError", err)
	}
	if !strings.Contains(err.Error(), transactionLockPath) {
		t.Fatalf("busy error %q does not name %s", err, transactionLockPath)
	}
	if after := hostileHashTree(t, project); !hostileMapsEqual(before, after) {
		t.Fatalf("losing contender mutated the tree\nbefore=%v\nafter=%v", before, after)
	}
	if _, statErr := os.Stat(filepath.Join(project, ".agent", "hostile.md")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("losing contender opened a target: %v", statErr)
	}

	if _, err := releaseWrite.Write([]byte("g")); err != nil {
		t.Fatal(err)
	}
	if err := releaseWrite.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("holder exit = %v", err)
	}
	if err := readyRead.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := hostileEngine().RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, hostileTargets(), ModeApply, hostileStateFinalizer); err != nil {
		t.Fatalf("apply after the holder released: %v", err)
	}
}

func hostileFlockHolder() {
	claim, err := claimTransactions(os.Getenv("ACR_TEST_PROJECT"))
	if err != nil {
		os.Exit(91)
	}
	ready := os.NewFile(4, "ready")
	if _, err := ready.Write([]byte("ok")); err != nil {
		os.Exit(92)
	}
	if err := ready.Close(); err != nil {
		os.Exit(93)
	}
	release := os.NewFile(3, "release")
	buffer := make([]byte, 1)
	if _, err := release.Read(buffer); err != nil {
		os.Exit(94)
	}
	if err := claim.Close(); err != nil {
		os.Exit(95)
	}
	os.Exit(0)
}

// TestHostileNonBusyFlockErrorNamesTheLockAndWritesNothing covers the injected
// ENOLCK / EOPNOTSUPP classes: named remedy, no journal directory, no target.
func TestHostileNonBusyFlockErrorNamesTheLockAndWritesNothing(t *testing.T) {
	original := transactionFlock
	t.Cleanup(func() { transactionFlock = original })

	for _, injected := range []error{syscall.ENOLCK, syscall.EOPNOTSUPP, syscall.EINVAL, syscall.ENOSYS} {
		t.Run(injected.Error(), func(t *testing.T) {
			project := t.TempDir()
			hostileWriteSeed(t, project)
			before := hostileHashTree(t, project)
			transactionFlock = func(int, int) error { return injected }

			_, err := hostileEngine().RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, hostileTargets(), ModeApply, hostileStateFinalizer)
			var unavailable *TransactionLockUnavailableError
			if !errors.As(err, &unavailable) {
				t.Fatalf("error = %v, want TransactionLockUnavailableError", err)
			}
			if !strings.Contains(err.Error(), transactionLockPath) {
				t.Fatalf("error %q does not name %s", err, transactionLockPath)
			}
			if _, statErr := os.Stat(filepath.Join(project, transactionDirectory)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed claim left %s behind: %v", transactionDirectory, statErr)
			}
			if after := hostileHashTree(t, project); !hostileMapsEqual(before, after) {
				t.Fatalf("failed claim mutated the tree\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

// TestHostileCorruptJournalIsNeverRestoredAndWritesNothing wraps each corruption
// class in a full path+mode+sha256 no-writes proof, dotfiles included.
func TestHostileCorruptJournalIsNeverRestoredAndWritesNothing(t *testing.T) {
	for _, test := range []struct {
		name    string
		corrupt func(*testing.T, string)
	}{
		{
			name: "truncated before-image",
			corrupt: func(t *testing.T, project string) {
				image := filepath.Join(project, transactionDirectory, "test-transaction", "before", "000000")
				if err := os.WriteFile(image, []byte("trunc"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "canonical journal without a manifest",
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
			hostileWriteSeed(t, project)
			if err := os.WriteFile(filepath.Join(project, "owned.md"), []byte("before\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			seedInterruptedJournal(t, project, "owned.md", []byte("after\n"))
			test.corrupt(t, project)
			before := hostileHashTree(t, project)

			_, err := hostileEngine().RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, hostileTargets(), ModeApply, hostileStateFinalizer)
			var conflict *RecoveryConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("error = %v, want RecoveryConflictError", err)
			}
			after := hostileHashTree(t, project)
			// The claim file is the one path a mutating run creates before it
			// reads the journal; nothing else may move.
			delete(after, ".agents/.acr-transactions/.lock")
			delete(before, ".agents/.acr-transactions/.lock")
			if !hostileMapsEqual(before, after) {
				t.Fatalf("recovery conflict mutated the tree\nbefore=%v\nafter=%v", before, after)
			}
			if _, statErr := os.Stat(filepath.Join(project, ".agent", "hostile.md")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("recovery conflict opened a new target: %v", statErr)
			}
		})
	}

	t.Run("leftover staging directory", func(t *testing.T) {
		project := t.TempDir()
		hostileWriteSeed(t, project)
		staging := filepath.Join(project, transactionDirectory, ".staging-leftover")
		if err := os.MkdirAll(filepath.Join(staging, "before"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staging, "before", "000000"), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := hostileHashTree(t, project)

		plan, err := hostileEngine().RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, hostileTargets(), ModeDryRun, hostileStateFinalizer)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.TransactionNotes) != 1 || plan.TransactionNotes[0].Code != "stale_transaction_staging" {
			t.Fatalf("read-only notes = %#v, want stale_transaction_staging", plan.TransactionNotes)
		}
		if !plan.HasChanges() {
			t.Fatal("read-only run planned nothing; staging residue must not block planning")
		}
		if after := hostileHashTree(t, project); !hostileMapsEqual(before, after) {
			t.Fatalf("read-only run mutated the tree\nbefore=%v\nafter=%v", before, after)
		}

		if _, err := hostileEngine().RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, hostileTargets(), ModeApply, hostileStateFinalizer); err != nil {
			t.Fatalf("mutating run: %v", err)
		}
		if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("mutating run retained staging residue: %v", err)
		}
		if got := readFile(t, project, ".agent/hostile.md"); got != "managed\n" {
			t.Fatalf("mutating run did not apply after clearing residue: %q", got)
		}
	})
}

func hostileHashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(filename)
			if err != nil {
				return err
			}
			result[relative] = "link→" + filepath.ToSlash(target)
			return nil
		}
		if entry.IsDir() {
			result[relative] = "dir " + info.Mode().Perm().String()
			return nil
		}
		content, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		result[relative] = info.Mode().Perm().String() + " " + hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func hostileMapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
