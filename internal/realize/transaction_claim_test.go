package realize

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

func closeTestClaim(t *testing.T, claim *transactionClaim) {
	t.Helper()
	if err := claim.Close(); err != nil {
		t.Fatal(err)
	}
}

func requireBusyClaim(t *testing.T, project string) {
	t.Helper()
	claim, err := claimTransactions(project)
	if claim != nil {
		closeTestClaim(t, claim)
	}
	var busy *TransactionBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("overlapping mutation claim accepted: error = %v", err)
	}
}

func TestClaimReleaseKeepsSuccessorExclusive(t *testing.T) {
	project := t.TempDir()
	ops := defaultTransactionClaimOps()
	var successor *transactionClaim
	ops.flock = func(fd, operation int) error {
		err := syscall.Flock(fd, operation)
		if err == nil && operation == syscall.LOCK_UN {
			successor, err = claimTransactions(project)
		}
		return err
	}
	holder, err := claimTransactionsWithOps(project, &ops)
	if err != nil {
		t.Fatal(err)
	}
	// The holder has finished all mutations. A successor enters at unlock.
	closeTestClaim(t, holder)
	if successor == nil {
		t.Fatal("successor never acquired")
	}
	defer closeTestClaim(t, successor)
	requireBusyClaim(t, project)
}

func TestClaimFailedCreatorCannotDetachOwner(t *testing.T) {
	project := t.TempDir()
	ops := defaultTransactionClaimOps()
	var owner *transactionClaim
	ops.flock = func(fd, operation int) error {
		var err error
		owner, err = claimTransactions(project)
		if err != nil {
			return err
		}
		return syscall.Flock(fd, operation)
	}
	failed, err := claimTransactionsWithOps(project, &ops)
	if failed != nil {
		closeTestClaim(t, failed)
	}
	var busy *TransactionBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("failed creator = %v", err)
	}
	defer closeTestClaim(t, owner)
	requireBusyClaim(t, project)
}

func TestClaimOpenedBeforeRetirementCannotEnterMutation(t *testing.T) {
	project := t.TempDir()
	holder, err := claimTransactions(project)
	if err != nil {
		t.Fatal(err)
	}
	ops := defaultTransactionClaimOps()
	var successor *transactionClaim
	first := true
	ops.flock = func(fd, operation int) error {
		if first && operation != syscall.LOCK_UN {
			first = false
			closeTestClaim(t, holder)
			successor, err = claimTransactions(project)
			if err != nil {
				return err
			}
		}
		return syscall.Flock(fd, operation)
	}
	contender, err := claimTransactionsWithOps(project, &ops)
	if contender != nil {
		closeTestClaim(t, contender)
	}
	defer closeTestClaim(t, successor)
	var busy *TransactionBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("detached contender entered mutation: %v", err)
	}
	requireBusyClaim(t, project)
}

func TestConvergedEngineCleansClaimPreservesRegistry(t *testing.T) {
	project := t.TempDir()
	parent := filepath.Join(project, ".agents")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(parent, "registry.lock")
	content := []byte("existing registry bytes\n")
	if err := os.WriteFile(registry, content, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, transactionDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, transactionLockPath), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(newPlanner(fakeGitInspector{}))
	plan, err := engine.RunStateFiles(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, func(Ledger) ([]StateFile, error) {
		return []StateFile{{Path: ".agents/registry.lock", Content: content, Mode: 0o640}}, nil
	})
	if err != nil || plan.HasChanges() {
		t.Fatalf("converged run = %#v, %v", plan, err)
	}
	after, err := os.Stat(registry)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(registry)
	if err != nil || string(got) != string(content) || !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("registry changed: before=%v after=%v bytes=%q err=%v", before, after, got, err)
	}
	if _, err := os.Stat(filepath.Join(project, transactionDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty transaction residue remains: %v", err)
	}
	if _, err := os.Stat(parent); err != nil {
		t.Fatalf("caller parent removed: %v", err)
	}
}

func TestClaimMissingAndReplacedPathRetry(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "replaced"}[replacement], func(t *testing.T) {
			project := t.TempDir()
			lockName := filepath.Join(project, transactionLockPath)
			ops := defaultTransactionClaimOps()
			first := true
			disposed := false
			var stale os.FileInfo
			ops.flock = func(fd, operation int) error {
				err := syscall.Flock(fd, operation)
				if err == nil && operation != syscall.LOCK_UN && first {
					first = false
					stale, err = os.Stat(lockName)
					if err != nil {
						return err
					}
					if err := os.Remove(lockName); err != nil {
						return err
					}
					if replacement {
						return os.WriteFile(lockName, nil, 0o600)
					}
				}
				return err
			}
			opens := 0
			ops.open = func(name string, flags int, mode os.FileMode) (*os.File, error) {
				opens++
				if opens > 1 && !disposed {
					t.Error("retried before closing stale descriptor")
				}
				return os.OpenFile(name, flags, mode)
			}
			ops.close = func(f *os.File) error { disposed = true; return f.Close() }
			claim, err := claimTransactionsWithOps(project, &ops)
			if err != nil {
				t.Fatal(err)
			}
			defer closeTestClaim(t, claim)
			current, err := claim.file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			// In the missing case inode reuse after disposal is allowed, so prove a
			// new open and current identity instead of requiring a different number.
			named, err := os.Stat(lockName)
			if err != nil || !os.SameFile(current, named) || opens != 2 {
				t.Fatalf("retry did not bind current path: opens=%d err=%v", opens, err)
			}
			if replacement && os.SameFile(stale, current) {
				t.Fatal("accepted replaced inode")
			}
			requireBusyClaim(t, project)
		})
	}
}

func TestClaimDirectoryTurnover(t *testing.T) {
	for _, boundary := range []string{"parent-mkdir", "transaction-mkdir", "open", "successor-before-rmdir", "successor-after-rmdir"} {
		t.Run(boundary, func(t *testing.T) {
			project := t.TempDir()
			ops := defaultTransactionClaimOps()
			var successor *transactionClaim
			fired := false
			ops.mkdir = func(name string, mode os.FileMode) error {
				match := boundary == "parent-mkdir" && filepath.Base(name) == ".agents" || boundary == "transaction-mkdir" && filepath.Base(name) == ".acr-transactions"
				if match && !fired {
					fired = true
					// The other creator wins mkdir; acquisition must revalidate EEXIST.
					if err := os.Mkdir(name, mode); err != nil {
						return err
					}
				}
				return os.Mkdir(name, mode)
			}
			ops.open = func(name string, flags int, mode os.FileMode) (*os.File, error) {
				if boundary == "open" && !fired {
					fired = true
					if err := os.Remove(filepath.Dir(name)); err != nil {
						return nil, err
					}
				}
				return os.OpenFile(name, flags, mode)
			}
			ops.rmdir = func(name string) error {
				if filepath.Base(name) == ".acr-transactions" && !fired && (boundary == "successor-before-rmdir" || boundary == "successor-after-rmdir") {
					fired = true
					if boundary == "successor-after-rmdir" {
						if err := syscall.Rmdir(name); err != nil {
							return err
						}
					}
					var err error
					successor, err = claimTransactions(project)
					if err != nil {
						return err
					}
				}
				return syscall.Rmdir(name)
			}
			claim, err := claimTransactionsWithOps(project, &ops)
			if err != nil {
				t.Fatal(err)
			}
			closeTestClaim(t, claim)
			if !fired {
				t.Fatal("schedule never ran")
			}
			if successor != nil {
				defer closeTestClaim(t, successor)
				requireBusyClaim(t, project)
			} else if err := RecoverTransactions(project); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClaimRetirementPreservesReplacementAndJournal(t *testing.T) {
	for _, state := range []string{"missing", "replacement", "journal", "empty-parent"} {
		t.Run(state, func(t *testing.T) {
			project := t.TempDir()
			parent := filepath.Join(project, ".agents")
			if err := os.Mkdir(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			claim, err := claimTransactions(project)
			if err != nil {
				t.Fatal(err)
			}
			lockName := filepath.Join(project, transactionLockPath)
			var preserved string
			switch state {
			case "missing":
				if err := os.Remove(lockName); err != nil {
					t.Fatal(err)
				}
			case "replacement":
				if err := os.Remove(lockName); err != nil {
					t.Fatal(err)
				}
				preserved = lockName
			case "journal":
				preserved = filepath.Join(project, transactionDirectory, "pending", "manifest.json")
				if err := os.Mkdir(filepath.Dir(preserved), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if preserved != "" {
				if err := os.WriteFile(preserved, []byte("retain\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			closeTestClaim(t, claim)
			closeTestClaim(t, claim) // idempotent, including after path retirement
			if preserved != "" {
				got, err := os.ReadFile(preserved)
				if err != nil || string(got) != "retain\n" {
					t.Fatalf("removed foreign state: %q %v", got, err)
				}
			}
			if _, err := os.Stat(parent); err != nil {
				t.Fatalf("removed caller parent: %v", err)
			}
		})
	}
}

// Run real mutation callers across the old unlock/unlink window. The pipe-like
// channels hold B in AfterEdit while A finishes Close and C attempts mutation.
func TestClaimProductionCallersNeverOverlap(t *testing.T) {
	for _, schedule := range []string{"release", "failed-creator"} {
		t.Run(schedule, func(t *testing.T) { testClaimProductionSchedule(t, schedule) })
	}
}

func testClaimProductionSchedule(t *testing.T, schedule string) {
	project := t.TempDir()
	edit := func(name string) FileTransactionEdit {
		if err := os.WriteFile(filepath.Join(project, name), []byte("before\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return FileTransactionEdit{Path: name, Operation: "splice", Before: []byte("before\n"), After: []byte("after\n"), BeforeMode: 0o600, AfterMode: 0o600}
	}
	bEdit, cEdit := edit("b.md"), edit("c.md")
	entered := make(chan struct{})
	resume := make(chan struct{})
	finished := make(chan error, 1)
	var active atomic.Int32
	var overlap atomic.Bool
	ops := defaultTransactionClaimOps()
	startSuccessor := func() {
		go func() {
			signalled := false
			err := ApplyFileTransactionWithHooks(project, []FileTransactionEdit{bEdit}, nil, FileTransactionHooks{
				TransactionID: func() (string, error) { return "successor-b", nil },
				AfterEdit: func(int, FileTransactionEdit) error {
					if active.Add(1) != 1 {
						overlap.Store(true)
					}
					close(entered)
					signalled = true
					<-resume
					active.Add(-1)
					return nil
				},
			})
			if !signalled {
				close(entered)
			}
			finished <- err
		}()
		<-entered
	}
	ops.flock = func(fd, operation int) error {
		if schedule == "failed-creator" && operation != syscall.LOCK_UN {
			startSuccessor()
		}
		if err := syscall.Flock(fd, operation); err != nil {
			return err
		}
		if schedule == "release" && operation == syscall.LOCK_UN {
			startSuccessor()
		}
		return nil
	}
	engine := newEngine(newPlanner(fakeGitInspector{}))
	engine.claimOps = &ops
	_, holderErr := engine.Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, nil)
	cErr := ApplyFileTransactionWithHooks(project, []FileTransactionEdit{cEdit}, nil, FileTransactionHooks{
		TransactionID: func() (string, error) { return "contender-c", nil },
		BeforeEdit: func(int, FileTransactionEdit) error {
			if active.Add(1) != 1 {
				overlap.Store(true)
			}
			active.Add(-1)
			return nil
		},
	})
	close(resume)
	bErr := <-finished
	var busy *TransactionBusyError
	holderOK := holderErr == nil
	if schedule == "failed-creator" {
		holderOK = errors.As(holderErr, &busy)
	}
	if !holderOK || bErr != nil || !errors.As(cErr, &busy) || overlap.Load() {
		t.Fatalf("mutation overlap=%v holder=%v successor=%v contender=%v", overlap.Load(), holderErr, bErr, cErr)
	}
	if got := readFile(t, project, "b.md"); got != "after\n" {
		t.Fatalf("successor not applied: %q", got)
	}
	if got := readFile(t, project, "c.md"); got != "before\n" {
		t.Fatalf("contender mutated: %q", got)
	}
}

func TestClaimErrorsRetainCausesAndDisposeDescriptors(t *testing.T) {
	for _, phase := range []string{"parent-stat", "mkdir", "transaction-stat", "path-stat", "open", "descriptor-stat", "locked-stat", "locked-path-stat", "flock", "eintr", "retire-stat", "retire-path-stat", "unlink", "rmdir", "parent-retire-stat", "unlock", "close"} {
		t.Run(phase, func(t *testing.T) {
			project := t.TempDir()
			ops := defaultTransactionClaimOps()
			injected := errors.New("injected filesystem failure")
			if phase == "eintr" {
				injected = syscall.EINTR
			}
			acquired, retiring := false, false
			opened, closed, stats := 0, 0, 0
			ops.lstat = func(name string) (os.FileInfo, error) {
				fail := phase == "parent-stat" && filepath.Base(name) == ".agents" || phase == "transaction-stat" && filepath.Base(name) == ".acr-transactions" || phase == "path-stat" && filepath.Base(name) == ".lock" || phase == "locked-path-stat" && acquired || phase == "retire-path-stat" && retiring || phase == "parent-retire-stat" && retiring && filepath.Base(name) == ".agents"
				if fail {
					return nil, injected
				}
				return os.Lstat(name)
			}
			ops.mkdir = func(name string, mode os.FileMode) error {
				if phase == "mkdir" {
					return injected
				}
				return os.Mkdir(name, mode)
			}
			ops.open = func(name string, flags int, mode os.FileMode) (*os.File, error) {
				if phase == "open" {
					return nil, injected
				}
				f, err := os.OpenFile(name, flags, mode)
				if err == nil {
					opened++
				}
				return f, err
			}
			ops.stat = func(f *os.File) (os.FileInfo, error) {
				stats++
				if phase == "descriptor-stat" && stats == 1 || phase == "locked-stat" && acquired || phase == "retire-stat" && retiring {
					return nil, injected
				}
				return f.Stat()
			}
			ops.flock = func(fd, operation int) error {
				if operation != syscall.LOCK_UN && (phase == "flock" || phase == "eintr") {
					return injected
				}
				if operation == syscall.LOCK_UN && phase == "unlock" {
					return injected
				}
				err := syscall.Flock(fd, operation)
				if err == nil && operation != syscall.LOCK_UN {
					acquired = true
				}
				return err
			}
			ops.close = func(f *os.File) error {
				closed++
				err := f.Close()
				if phase == "close" {
					return errors.Join(err, injected)
				}
				return err
			}
			ops.remove = func(name string) error {
				if phase == "unlink" {
					return injected
				}
				return os.Remove(name)
			}
			ops.rmdir = func(name string) error {
				if phase == "rmdir" {
					return injected
				}
				return syscall.Rmdir(name)
			}
			claim, err := claimTransactionsWithOps(project, &ops)
			if err == nil {
				retiring = true
				err = claim.Close()
				closeTestClaim(t, claim)
			}
			context := project
			if phase == "flock" || phase == "eintr" {
				context = transactionLockPath
			}
			if !errors.Is(err, injected) || !strings.Contains(err.Error(), context) {
				t.Fatalf("lost contextual cause: %v", err)
			}
			if opened != closed {
				t.Fatalf("descriptor leak: opened=%d closed=%d", opened, closed)
			}
		})
	}
}

func TestClaimRetryExhaustionAndDisposalFailures(t *testing.T) {
	for _, phase := range []string{"missing-path", "directory-turnover", "unlock-and-close", "stat-and-close", "flock-and-close"} {
		t.Run(phase, func(t *testing.T) {
			project := t.TempDir()
			ops := defaultTransactionClaimOps()
			injectedUnlock := errors.New("unlock failure")
			injectedClose := errors.New("close failure")
			injectedStat := errors.New("stat failure")
			opens, closes := 0, 0
			ops.open = func(name string, flags int, mode os.FileMode) (*os.File, error) {
				if phase == "directory-turnover" {
					return nil, os.ErrNotExist
				}
				opens++
				return os.OpenFile(name, flags, mode)
			}
			ops.flock = func(fd, operation int) error {
				if phase == "flock-and-close" {
					return syscall.ENOLCK
				}
				if operation == syscall.LOCK_UN && phase == "unlock-and-close" {
					return injectedUnlock
				}
				err := syscall.Flock(fd, operation)
				if err == nil && operation != syscall.LOCK_UN {
					return os.Remove(filepath.Join(project, transactionLockPath))
				}
				return err
			}
			ops.stat = func(f *os.File) (os.FileInfo, error) {
				if phase == "stat-and-close" {
					return nil, injectedStat
				}
				return f.Stat()
			}
			ops.close = func(f *os.File) error {
				closes++
				err := f.Close()
				if strings.HasSuffix(phase, "-close") {
					return errors.Join(err, injectedClose)
				}
				return err
			}
			claim, err := claimTransactionsWithOps(project, &ops)
			if claim != nil {
				closeTestClaim(t, claim)
				t.Fatal("accepted unstable claim")
			}
			if err == nil || !strings.Contains(err.Error(), project) {
				t.Fatalf("missing actionable failure: %v", err)
			}
			switch phase {
			case "missing-path":
				if !errors.Is(err, errTransactionClaimChanged) || opens != transactionClaimAttempts {
					t.Fatalf("unbounded/wrong retry: opens=%d err=%v", opens, err)
				}
			case "directory-turnover":
				if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "8 attempts") {
					t.Fatalf("retry lost ENOENT: %v", err)
				}
			default:
				if opens != 1 || !errors.Is(err, injectedClose) {
					t.Fatalf("retried after disposal failure: opens=%d err=%v", opens, err)
				}
				if phase == "unlock-and-close" && !errors.Is(err, injectedUnlock) || phase == "stat-and-close" && !errors.Is(err, injectedStat) || phase == "flock-and-close" && !errors.Is(err, syscall.ENOLCK) {
					t.Fatalf("lost primary error: %v", err)
				}
			}
			if opens != closes {
				t.Fatalf("descriptor leak: opens=%d closes=%d", opens, closes)
			}
		})
	}
}

func TestClaimCallersJoinPrimaryAndRetirementErrors(t *testing.T) {
	for _, caller := range []string{"engine", "recovery", "file-transaction"} {
		t.Run(caller, func(t *testing.T) {
			project := t.TempDir()
			journal := filepath.Join(project, transactionDirectory, "pending")
			if err := os.MkdirAll(journal, 0o700); err != nil {
				t.Fatal(err)
			}
			// Recovery fails in each real caller before it can mutate this bad journal.
			if err := os.WriteFile(filepath.Join(journal, journalManifestFilename), []byte("invalid"), 0o600); err != nil {
				t.Fatal(err)
			}
			ops := defaultTransactionClaimOps()
			unlockErr, closeErr := errors.New("unlock failed"), errors.New("close failed")
			closed := false
			ops.flock = func(fd, operation int) error {
				if operation == syscall.LOCK_UN {
					return unlockErr
				}
				return syscall.Flock(fd, operation)
			}
			ops.close = func(f *os.File) error { closed = true; return errors.Join(f.Close(), closeErr) }
			var err error
			switch caller {
			case "engine":
				engine := newEngine(newPlanner(fakeGitInspector{}))
				engine.claimOps = &ops
				_, err = engine.Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, nil, ModeApply, nil)
			case "recovery":
				err = recoverTransactionsWithOps(project, &ops)
			case "file-transaction":
				err = ApplyFileTransactionWithHooks(project, nil, nil, FileTransactionHooks{claimOps: &ops})
			}
			var conflict *RecoveryConflictError
			if !errors.As(err, &conflict) || !errors.Is(err, unlockErr) || !errors.Is(err, closeErr) || !closed {
				t.Fatalf("caller lost joined failures: %v closed=%v", err, closed)
			}
			got, readErr := os.ReadFile(filepath.Join(journal, journalManifestFilename))
			if readErr != nil || string(got) != "invalid" {
				t.Fatalf("caller removed journal: %q %v", got, readErr)
			}
		})
	}
}
