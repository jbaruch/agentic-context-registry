package realize

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitExclusionOrphanReconciliation(t *testing.T) {
	for _, fixture := range []struct {
		name, boundary                          string
		fileWriter, priorReceipt, excludeAbsent bool
	}{
		{name: "before installed generation", boundary: beforeExclusionGeneration},
		{name: "file transaction before receipt publication", boundary: gitExcludePath, fileWriter: true, priorReceipt: true},
		{name: "after receipt publication and fence retirement interruption", boundary: gitExcludeReceiptPath, priorReceipt: true},
		{name: "file transaction with original exclusion absence", boundary: gitExcludePath, fileWriter: true, excludeAbsent: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			const childProject = "ACR_TEST_ORPHAN_EXCLUSION_PROJECT"
			const retireChild = "ACR_TEST_ORPHAN_FENCE_DIGEST"
			const target = ".cursor/rules/orphan-fixture.mdc"
			empty := Ledger{SchemaVersion: CurrentLedgerSchemaVersion}
			desired := Ledger{SchemaVersion: CurrentLedgerSchemaVersion, Targets: []Target{testTarget(target, "generated\n", OwnershipGenerated)}}
			intent := testIntent(target, "generated\n", OwnershipGenerated)
			finalize := func(Ledger) error { return nil }
			apply := func(project string) error {
				_, err := NewEngine().Run(project, empty, []Intent{intent}, ModeApply, finalize)
				return err
			}
			fileApply := func(project string) error {
				return ApplyFileTransactionWithGitExclusion(t.Context(), project, desired, func(next Ledger) ([]FileTransactionEdit, error) {
					if len(next.Targets) != 1 || !next.Targets[0].Excluded {
						t.Fatalf("file transaction did not plan exclusion: %+v", next)
					}
					return nil, nil
				}, nil, FileTransactionHooks{})
			}
			if project := os.Getenv(childProject); project != "" {
				if digest := os.Getenv(retireChild); digest != "" {
					exitAtExclusionBoundary(gitExclusionJournal)
					t.Fatalf("fence retirement did not reach interruption: %v", ReconcileGitExclusion(t.Context(), project, digest))
				}
				exitAtExclusionBoundary(fixture.boundary)
				if fixture.fileWriter {
					t.Fatalf("file transaction did not reach interruption: %v", fileApply(project))
				}
				t.Fatalf("realization did not reach interruption: %v", apply(project))
			}

			repository := t.TempDir()
			initGit(t, repository)
			git := func(args ...string) {
				t.Helper()
				if out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", repository}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			git("-c", "user.name=Example", "-c", "user.email=example@example.org", "commit", "--allow-empty", "-qm", "Seed")
			worktrees := t.TempDir()
			owner, sibling := filepath.Join(worktrees, "owner"), filepath.Join(worktrees, "sibling")
			git("worktree", "add", "-q", "--detach", owner)
			git("worktree", "add", "-q", "--detach", sibling)
			excludePath := filepath.Join(repository, gitExcludePath)
			receiptPath := filepath.Join(repository, gitExcludeReceiptPath)
			if fixture.excludeAbsent {
				if err := os.Remove(excludePath); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFile(t, repository, gitExcludePath, "# kept\r\n*.local")
				if err := os.Chmod(excludePath, 0o640); err != nil {
					t.Fatal(err)
				}
			}
			if fixture.priorReceipt {
				if err := apply(repository); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(repository)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			snapshot := func(path string) fileSnapshot {
				t.Helper()
				state, err := snapshotFile(root, path)
				if err != nil {
					t.Fatal(err)
				}
				return state
			}
			before, beforeReceipt := snapshot(gitExcludePath), snapshot(gitExcludeReceiptPath)
			requireExclusionCrash(t, childProject, owner)
			after, afterReceipt := snapshot(gitExcludePath), snapshot(gitExcludeReceiptPath)
			if bytes.Equal(before.content, after.content) || !bytes.Contains(after.content, []byte(target)) ||
				(matchesGitSnapshot(beforeReceipt, afterReceipt) == (fixture.boundary == gitExcludeReceiptPath)) {
				t.Fatal("fixture did not interrupt at the expected exclusion/receipt boundary")
			}
			_, err = inspectTransactions(owner)
			var pending *PendingTransactionError
			if !errors.As(err, &pending) || pending.SchemaVersion != 3 {
				t.Fatalf("exclusion-only writer journal = %v, want v3", err)
			}
			fencePath := filepath.Join(repository, ".git", "info", gitExclusionJournal)
			fenceName := ".git/info/" + gitExclusionJournal
			originalFence := snapshot(fenceName)
			if !originalFence.exists || originalFence.mode != 0o600 {
				t.Fatalf("crash did not leave a private fence: mode %o", originalFence.mode)
			}
			var recorded gitExclusionFence
			if err := json.Unmarshal(originalFence.content, &recorded); err != nil || recorded.SchemaVersion != 1 ||
				recorded.Journal != pending.ID || recorded.Registration.Path == "" || recorded.Exclude == nil ||
				recorded.Exclude.Exists != before.exists || !bytes.Equal(recorded.Exclude.Bytes, before.content) ||
				recorded.Exclude.Mode != uint32(before.mode) || recorded.Exclude.Hash != before.hash || recorded.Exclude.Size != len(before.content) {
				t.Fatalf("fence did not retain the expected exclusion before-state: %v", err)
			}
			if (recorded.Receipt == nil) != fixture.excludeAbsent || recorded.Receipt != nil &&
				(recorded.Receipt.Exists != beforeReceipt.exists || !bytes.Equal(recorded.Receipt.Bytes, beforeReceipt.content) ||
					recorded.Receipt.Mode != uint32(beforeReceipt.mode) || recorded.Receipt.Hash != beforeReceipt.hash) {
				t.Fatal("fence did not retain the expected receipt before-state")
			}
			indexPath := ".git/worktrees/sibling/index"
			index := snapshot(indexPath)
			if !index.exists {
				t.Fatal("fixture lacks the sibling Git index")
			}
			reconcile := func(digest string) error {
				return ReconcileGitExclusion(t.Context(), sibling, digest)
			}
			refuse := func(err error, fragment string) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), fragment) {
					t.Fatalf("error = %v, want %q", err, fragment)
				}
			}
			checkSiblingBusy := func(fragment string) {
				t.Helper()
				var busy *TransactionBusyError
				if err := apply(sibling); !errors.As(err, &busy) || !strings.Contains(err.Error(), fragment) {
					t.Fatalf("ordinary sibling realize = %v, want transaction_busy containing %q", err, fragment)
				}
				if err := fileApply(sibling); !errors.As(err, &busy) || !strings.Contains(err.Error(), fragment) {
					t.Fatalf("ordinary sibling file transaction = %v, want transaction_busy containing %q", err, fragment)
				}
				if !matchesGitSnapshot(after, snapshot(gitExcludePath)) || !matchesGitSnapshot(afterReceipt, snapshot(gitExcludeReceiptPath)) ||
					!matchesGitSnapshot(originalFence, snapshot(fenceName)) || !matchesGitSnapshot(index, snapshot(indexPath)) {
					t.Fatal("refused siblings changed shared files, fence or index")
				}
				for _, name := range []string{target, ".agents/registry.lock"} {
					if _, err := os.Lstat(filepath.Join(sibling, name)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("refused sibling changed %s: %v", name, err)
					}
				}
			}
			checkSiblingBusy("owning project")
			refuse(reconcile(originalFence.hash), "not a confirmed removed linked worktree")
			if fixture.boundary == beforeExclusionGeneration {
				if err := os.Rename(fencePath, fencePath+"-backup"); err != nil {
					t.Fatal(err)
				}
				refuse(RecoverTransactions(t.Context(), owner), "no matching shared fence")
				if !matchesGitSnapshot(after, snapshot(gitExcludePath)) {
					t.Fatal("owner recovery without a fence changed the exclusion")
				}
				if err := os.Rename(fencePath+"-backup", fencePath); err != nil {
					t.Fatal(err)
				}
				originalFence = snapshot(fenceName)
			}
			registration := filepath.Join(repository, ".git", "worktrees", filepath.Base(recorded.Registration.Path))
			unregistered := filepath.Join(repository, ".git", "parked-registration")
			if err := os.Rename(registration, unregistered); err != nil {
				t.Fatal(err)
			}
			refuse(reconcile(originalFence.hash), "project still exists but its Git registration is missing")
			if err := os.Rename(unregistered, registration); err != nil {
				t.Fatal(err)
			}
			replacement := filepath.Join(repository, ".git", "spare-registration")
			if err := os.Mkdir(replacement, 0o700); err != nil {
				t.Fatal(err)
			}
			if fixture.boundary == gitExcludeReceiptPath {
				moved := filepath.Join(worktrees, "moved")
				git("worktree", "move", owner, moved)
				writeFile(t, owner, "separate", "reused path\n")
				checkSiblingBusy("owning project")
				refuse(reconcile(originalFence.hash), "not a confirmed removed linked worktree")
				commonDirFile := filepath.Join(registration, "commondir")
				commonInfo, err := os.Stat(commonDirFile)
				if err != nil {
					t.Fatal(err)
				}
				stale := time.Unix(12345, 0)
				if err := os.Chtimes(commonDirFile, stale, stale); err != nil {
					t.Fatal(err)
				}
				refuse(reconcile(originalFence.hash), "no longer has the recorded worktree identity")
				if err := os.Chtimes(commonDirFile, commonInfo.ModTime(), commonInfo.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(registration, registration+"-parked"); err != nil {
					t.Fatal(err)
				}
				refuse(reconcile(originalFence.hash), "directory identity still exists")
				if err := os.Rename(registration+"-parked", registration); err != nil {
					t.Fatal(err)
				}
				git("worktree", "remove", "--force", moved)
			} else {
				git("worktree", "remove", "--force", owner)
			}
			checkSiblingBusy("acr reconcile-exclusion " + originalFence.hash)
			refuse(reconcile(contentHash([]byte("different fence"))), "no Git exclusion fence matches")
			refuse(reconcile(originalFence.hash), "does not match the fence before-state")
			if !matchesGitSnapshot(after, snapshot(gitExcludePath)) ||
				!matchesGitSnapshot(afterReceipt, snapshot(gitExcludeReceiptPath)) || !matchesGitSnapshot(originalFence, snapshot(fenceName)) {
				t.Fatal("refused reconciliation changed live files")
			}
			t.Run("failed Git lookup keeps the fence", func(t *testing.T) {
				t.Setenv("PATH", t.TempDir())
				refuse(reconcile(originalFence.hash), "git")
				if !matchesGitSnapshot(originalFence, snapshot(fenceName)) {
					t.Fatal("failed Git lookup disturbed the fence")
				}
			})

			restore := func(path string, image fileSnapshot) {
				t.Helper()
				if image.exists {
					if err := os.WriteFile(path, image.content, image.mode); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(path, image.mode); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
			restore(excludePath, before)
			if fixture.boundary == gitExcludeReceiptPath {
				refuse(reconcile(originalFence.hash), "acr-exclusion-separator")
			}
			restore(receiptPath, beforeReceipt)
			if beforeReceipt.exists {
				if err := os.Chmod(receiptPath, 0o640); err != nil {
					t.Fatal(err)
				}
				refuse(reconcile(originalFence.hash), "acr-exclusion-separator")
				restore(receiptPath, beforeReceipt)
			}
			if before.exists {
				if err := os.Chmod(excludePath, 0o600); err != nil {
					t.Fatal(err)
				}
				refuse(reconcile(originalFence.hash), "does not match the fence before-state")
				restore(excludePath, before)
			}
			if err := os.Rename(replacement, registration); err != nil {
				t.Fatal(err)
			}
			refuse(reconcile(originalFence.hash), "no longer matches the recorded owner")
			if err := os.Remove(registration); err != nil {
				t.Fatal(err)
			}
			if fixture.boundary == gitExcludeReceiptPath {
				unrelated := t.TempDir()
				initGit(t, unrelated)
				unrelatedInfo := filepath.Join(unrelated, ".git", "info")
				if err := os.Rename(unrelatedInfo, unrelatedInfo+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(fencePath), unrelatedInfo); err != nil {
					t.Fatal(err)
				}
				refuse(ReconcileGitExclusion(t.Context(), unrelated, originalFence.hash), "belongs to a different repository")
			}
			for _, bad := range []struct {
				data       []byte
				diagnostic string
			}{
				{bytes.Replace(originalFence.content, []byte(`"receipt":`), []byte(`"unknown":`), 1), "invalid Git exclusion fence"},
				{bytes.Replace(originalFence.content, []byte(`"schema_version":1`), []byte(`"schema_version":4`), 1), "use an ACR reader that supports the recorded version"},
				{[]byte(`{"project":"/draft","journal":"draft","device":1,"inode":1}`), "unversioned draft"},
			} {
				if err := os.WriteFile(fencePath, bad.data, 0o600); err != nil {
					t.Fatal(err)
				}
				refuse(reconcile(contentHash(bad.data)), bad.diagnostic)
				refuse(apply(sibling), bad.diagnostic)
				if got := snapshot(fenceName); !bytes.Equal(got.content, bad.data) {
					t.Fatal("refused mutation rewrote the bad fence")
				}
			}
			if err := os.Remove(fencePath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(excludePath, fencePath); err != nil {
				t.Fatal(err)
			}
			refuse(reconcile(originalFence.hash), "restore access to the original record")
			refuse(apply(sibling), "restore access to the original record")
			if err := os.Remove(fencePath); err != nil {
				t.Fatal(err)
			}
			refuse(reconcile(originalFence.hash), "no Git exclusion fence matches")
			restore(fencePath, originalFence)
			claimRoot, _, err := resolveGitExclude(t.Context(), sibling)
			if err != nil {
				t.Fatal(err)
			}
			var claim gitExclusionClaim
			if err := claim.lock(claimRoot); err != nil {
				t.Fatal(err)
			}
			refuse(reconcile(originalFence.hash), "transaction_busy")
			if err := claim.close(); err != nil {
				t.Fatal(err)
			}
			reconciled, reconciledReceipt := snapshot(gitExcludePath), snapshot(gitExcludeReceiptPath)
			if fixture.boundary == gitExcludeReceiptPath {
				t.Setenv(retireChild, originalFence.hash)
				requireExclusionCrash(t, childProject, sibling)
			} else if err := reconcile(originalFence.hash); err != nil {
				t.Fatal(err)
			}
			if snapshot(fenceName).exists || !matchesGitSnapshot(reconciled, snapshot(gitExcludePath)) ||
				!matchesGitSnapshot(reconciledReceipt, snapshot(gitExcludeReceiptPath)) || !matchesGitSnapshot(index, snapshot(indexPath)) {
				t.Fatal("reconciliation did not retire only the verified fence")
			}
			refuse(reconcile(originalFence.hash), "no Git exclusion fence matches")
			if err := fileApply(sibling); err != nil {
				t.Fatalf("sibling file transaction still refused after reconciliation: %v", err)
			}
			if err := apply(sibling); err != nil {
				t.Fatalf("sibling realization still refused after reconciliation: %v", err)
			}
			if !matchesGitSnapshot(index, snapshot(indexPath)) || readFile(t, sibling, target) != "generated\n" {
				t.Fatal("sibling did not realize the expected output without changing its index")
			}
		})
	}
}
