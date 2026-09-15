package realize

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

// A one-shot fault makes the Darwin errno classification deterministic even
// when the test runs on a filesystem that reports only ENOENT for retirement.
func TestClaimCreationTurnoverRetries(t *testing.T) {
	for _, site := range []string{"open", ".agents", ".acr-transactions"} {
		for _, fault := range []syscall.Errno{syscall.ENOENT, syscall.EINVAL} {
			t.Run(site+"/"+fault.Error(), func(t *testing.T) {
				project := t.TempDir()
				ops := defaultTransactionClaimOps()
				attempts := 0
				failOnce := func(name string) error {
					attempts++
					if attempts == 1 {
						return &os.PathError{Op: site, Path: name, Err: fault}
					}
					return nil
				}
				if site == "open" {
					ops.open = func(name string, flags int, mode os.FileMode) (*os.File, error) {
						if err := failOnce(name); err != nil {
							return nil, err
						}
						return os.OpenFile(name, flags, mode)
					}
				} else {
					ops.mkdir = func(name string, mode os.FileMode) error {
						if filepath.Base(name) == site {
							if err := failOnce(name); err != nil {
								return err
							}
						}
						return os.Mkdir(name, mode)
					}
				}
				claim, err := claimTransactionsWithOps(project, &ops)
				if err != nil {
					t.Fatalf("one-shot retirement fault was terminal: attempts=%d err=%v", attempts, err)
				}
				defer closeTestClaim(t, claim)
				if attempts != 2 {
					t.Fatalf("creation attempts=%d, want one retry", attempts)
				}
				requireBusyClaim(t, project)
			})
		}
	}
}

func TestClaimCreationErrorsStayBounded(t *testing.T) {
	for _, site := range []string{"open", ".agents", ".acr-transactions"} {
		for _, fault := range []syscall.Errno{syscall.EINVAL, syscall.EACCES, syscall.EIO} {
			t.Run(site+"/"+fault.Error(), func(t *testing.T) {
				project := t.TempDir()
				ops := defaultTransactionClaimOps()
				attempts := 0
				failure := func(name string) error { attempts++; return &os.PathError{Op: site, Path: name, Err: fault} }
				if site == "open" {
					ops.open = func(name string, _ int, _ os.FileMode) (*os.File, error) { return nil, failure(name) }
				} else {
					ops.mkdir = func(name string, mode os.FileMode) error {
						if filepath.Base(name) == site {
							return failure(name)
						}
						return os.Mkdir(name, mode)
					}
				}
				claim, err := claimTransactionsWithOps(project, &ops)
				if claim != nil {
					closeTestClaim(t, claim)
					t.Fatal("accepted a failed creation")
				}
				want := 1
				if fault == syscall.EINVAL {
					want = transactionClaimAttempts
				}
				if attempts != want || !errors.Is(err, fault) || !strings.Contains(err.Error(), project) {
					t.Fatalf("attempts=%d want=%d, lost contextual cause: %v", attempts, want, err)
				}
				if fault == syscall.EINVAL && !strings.Contains(err.Error(), "8 attempts") {
					t.Fatalf("missing retry exhaustion context: %v", err)
				}
			})
		}
	}
}

// In particular, EINVAL from stat is not evidence of a creation/retirement
// race. Fail only once so a mistaken retry cannot encounter the same fault.
func TestClaimInspectFailureDoesNotRetry(t *testing.T) {
	for _, site := range []string{"parent-lstat", "transaction-lstat", "locked-stat", "locked-lstat"} {
		t.Run(site, func(t *testing.T) {
			project := t.TempDir()
			ops := defaultTransactionClaimOps()
			opens, closes, stats := 0, 0, 0
			injected := false
			ops.open = func(name string, flags int, mode os.FileMode) (*os.File, error) {
				opens++
				return os.OpenFile(name, flags, mode)
			}
			ops.close = func(f *os.File) error { closes++; return f.Close() }
			ops.stat = func(f *os.File) (os.FileInfo, error) {
				stats++
				if site == "locked-stat" && stats == 2 {
					injected = true
					return nil, syscall.EINVAL
				}
				return f.Stat()
			}
			ops.lstat = func(name string) (os.FileInfo, error) {
				match := site == "parent-lstat" && filepath.Base(name) == ".agents" || site == "transaction-lstat" && filepath.Base(name) == ".acr-transactions" || site == "locked-lstat" && stats == 2
				if match && !injected {
					injected = true
					return nil, &os.PathError{Op: "lstat", Path: name, Err: syscall.EINVAL}
				}
				return os.Lstat(name)
			}
			claim, err := claimTransactionsWithOps(project, &ops)
			if claim != nil {
				closeTestClaim(t, claim)
			}
			wantOpens := 0
			if strings.HasPrefix(site, "locked-") {
				wantOpens = 1
			}
			if !injected || !errors.Is(err, syscall.EINVAL) || opens != wantOpens || closes != opens {
				t.Fatalf("inspect failure retried or lost: injected=%v opens=%d closes=%d err=%v", injected, opens, closes, err)
			}
		})
	}
}

// Fixed workloads, a start barrier, and invariant-only assertions let the OS
// choose any schedule. Fault occurrence and worker success counts are not
// oracles: the one-shot tests above cover the errno even on a single CPU.
func TestClaimConcurrentRetirementControl(t *testing.T) {
	for _, existingParent := range []bool{false, true} {
		name := "new-parent"
		if existingParent {
			name = "existing-parent"
		}
		t.Run(name, func(t *testing.T) {
			project := t.TempDir()
			if existingParent {
				if err := os.Mkdir(filepath.Join(project, ".agents"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			const workers, iterations = 8, 60
			var active, accepted, busyCount, exhausted atomic.Int32
			var overlap atomic.Bool
			start := make(chan struct{})
			failures := make(chan error, workers*iterations)
			var group sync.WaitGroup
			for worker := 0; worker < workers; worker++ {
				group.Add(1)
				go func() {
					defer group.Done()
					<-start
					for iteration := 0; iteration < iterations; iteration++ {
						claim, err := claimTransactions(project)
						if err != nil {
							var busy *TransactionBusyError
							switch {
							case errors.As(err, &busy):
								busyCount.Add(1)
							case strings.Contains(err.Error(), "8 attempts") && (errors.Is(err, os.ErrNotExist) || errors.Is(err, errTransactionClaimChanged)):
								// Sustained turnover may exhaust the bounded budget. It must retain
								// its cause; this is distinct from a terminal creation error.
								exhausted.Add(1)
							default:
								failures <- err
							}
							continue
						}
						accepted.Add(1)
						if active.Add(1) != 1 {
							overlap.Store(true)
						}
						active.Add(-1) // mutation authority ends before retirement
						if err := claim.Close(); err != nil {
							failures <- err
						}
					}
				}()
			}
			close(start)
			group.Wait()
			close(failures)
			count := 0
			for err := range failures {
				count++
				if count <= 3 {
					t.Errorf("unexpected contention outcome: %v", err)
				}
			}
			if overlap.Load() || active.Load() != 0 {
				t.Errorf("overlapping mutation authority: overlap=%v active=%d", overlap.Load(), active.Load())
			}
			// Once contention ends, production recovery must acquire and clean any
			// harmless residue. No success quota or ordering is imposed on workers.
			if err := RecoverTransactions(project); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(project, transactionDirectory)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("transaction residue remains: %v", err)
			}
			t.Logf("accepted=%d busy=%d bounded-exhaustion=%d unexpected=%d", accepted.Load(), busyCount.Load(), exhausted.Load(), count)
		})
	}
}
