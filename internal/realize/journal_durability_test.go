package realize

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGitExclusionRollbackDurability(t *testing.T) {
	const child = "ACR_TEST_DURABILITY_OWNER"
	const lock = ".agents/registry.lock"
	const generated = "untracked/nested/deep/generated.md"
	const removed = "restore/from/removed.md"
	const link = "restore/from/link"
	const linkText = "../../missing"
	for _, writer := range []string{"realize", "file-transaction"} {
		t.Run(writer, func(t *testing.T) {
			empty := Ledger{SchemaVersion: CurrentLedgerSchemaVersion}
			target := testTarget(generated, "generated\n", OwnershipGenerated)
			if owner := os.Getenv(child); owner != "" {
				exitAtExclusionBoundary(generated)
				if writer == "realize" {
					_, err := NewEngine().RunStateFiles(t.Context(), owner, empty, Ledger{},
						[]Intent{testIntent(generated, "generated\n", OwnershipGenerated)}, ModeApply, func(Ledger) ([]StateFile, error) {
							return []StateFile{{Path: lock, BeforeExists: true, BeforeContent: []byte("before\n"), BeforeMode: 0o644,
								Content: []byte("after\n"), Mode: 0o600}}, nil
						})
					t.Fatalf("realization did not reach local interruption: %v", err)
				}
				desired := Ledger{SchemaVersion: CurrentLedgerSchemaVersion, Targets: []Target{target}}
				err := ApplyFileTransactionWithGitExclusion(t.Context(), owner, desired, func(Ledger) ([]FileTransactionEdit, error) {
					return []FileTransactionEdit{
						{Path: lock, Operation: "splice", Before: []byte("before\n"), BeforeMode: 0o644, After: []byte("after\n"), AfterMode: 0o600},
						{Path: removed, Operation: "remove", Before: []byte("keep\n"), BeforeMode: 0o644},
						{Path: link, Operation: "remove", LinkTarget: linkText},
						{Path: generated, Operation: "splice", BeforeAbsent: true, After: []byte("generated\n"), AfterMode: 0o644},
					}, nil
				}, nil, FileTransactionHooks{})
				t.Fatalf("file transaction did not reach local interruption: %v", err)
			}

			repo := t.TempDir()
			initGit(t, repo)
			git := func(args ...string) {
				t.Helper()
				if out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			writeFile(t, repo, lock, "before\n")
			writeFile(t, repo, gitExcludePath, "# keep an unterminated exclusion line")
			if writer == "file-transaction" {
				writeFile(t, repo, removed, "keep\n")
				if err := os.Symlink(linkText, filepath.Join(repo, link)); err != nil {
					t.Fatal(err)
				}
				writeFile(t, repo, "untracked/nested/deep/keep.md", "keep directory\n")
			}
			git("add", "-f", ".")
			git("-c", "user.name=Example", "-c", "user.email=example@example.org", "commit", "-qm", "Seed")
			owner := filepath.Join(t.TempDir(), "owner")
			git("worktree", "add", "-q", "--detach", owner)
			requireExclusionCrash(t, child, owner)
			if got := readFile(t, owner, lock); got != "after\n" {
				t.Fatalf("writer did not install the local after-state: %q", got)
			}
			if got := readFile(t, repo, gitExcludePath); !strings.Contains(got, generated) {
				t.Fatalf("writer did not install shared exclusion: %q", got)
			}
			if writer == "file-transaction" {
				// Force recovery to recreate both missing removal parents.
				for _, dir := range []string{"restore/from", "restore"} {
					if err := os.Remove(filepath.Join(owner, dir)); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err := inspectTransactions(owner)
			var pending *PendingTransactionError
			if !errors.As(err, &pending) || pending.SchemaVersion != exclusionJournalSchemaVersion {
				t.Fatalf("expected interrupted v3 journal, got %v", err)
			}
			txRoot := filepath.Join(owner, transactionDirectory)
			canonical := filepath.Join(txRoot, pending.ID)
			staged := filepath.Join(txRoot, exclusionRetirementPrefix+pending.ID)
			fenceName := ".git/info/" + gitExclusionJournal
			originalFence := readFile(t, repo, fenceName)
			originalManifest := readFile(t, canonical, journalManifestFilename)
			requireJournal := func(dir, fence string) {
				t.Helper()
				if readFile(t, dir, journalManifestFilename) != originalManifest || readFile(t, repo, fenceName) != fence {
					t.Fatal("failure changed the journal manifest or fence")
				}
			}
			beforeLock := func() fileGeneration {
				t.Helper()
				filename := filepath.Join(owner, lock)
				info, err := os.Stat(filename)
				if err != nil || info.Mode().Perm() != 0o644 || readFile(t, owner, lock) != "before\n" {
					t.Fatalf("rollback did not restore lock bytes and mode: %v", err)
				}
				return fileGenerationOf(info)
			}
			requireAbsent := func(path string) {
				t.Helper()
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("expected %q absent: %v", path, err)
				}
			}
			prepare := func() error {
				return PrepareGitExclusion(t.Context(), owner, func(string) error { return nil })
			}
			fault := errors.New("injected durability failure")
			expectFault := func(err error) {
				t.Helper()
				if !errors.Is(err, fault) {
					t.Fatalf("expected sync failure, got %v", err)
				}
			}
			ownerSync, sharedSync, renameHook := journalDirectorySync, gitExclusionDirectorySync, transactionRenameHook
			t.Cleanup(func() {
				journalDirectorySync, gitExclusionDirectorySync, transactionRenameHook = ownerSync, sharedSync, renameHook
			})
			var synced []string
			var failDir = filepath.Join(owner, ".agents")
			var retirementRenamed, failRetirement, proofRenamed, failShared bool
			journalDirectorySync = func(dir string) error {
				if dir == failDir || (dir == txRoot && retirementRenamed && failRetirement) {
					return fault
				}
				synced = append(synced, dir)
				return ownerSync(dir)
			}
			gitExclusionDirectorySync = func(dir *os.File) error {
				if failShared {
					return fault
				}
				return sharedSync(dir)
			}
			transactionRenameHook = func(name string) {
				switch name {
				case exclusionRetirementPrefix:
					retirementRenamed = true
				case "exclusion-retirement-proof":
					proofRenamed, failShared = true, true
				}
			}

			expectFault(prepare())
			requireJournal(canonical, originalFence)
			restored := beforeLock()
			requireAbsent(filepath.Join(owner, generated))
			if writer == "realize" {
				requireAbsent(filepath.Join(owner, "untracked"))
			} else {
				if got := readFile(t, owner, removed); got != "keep\n" {
					t.Fatalf("removal before-image was not restored: %q", got)
				}
				if got, err := os.Readlink(filepath.Join(owner, link)); err != nil || got != linkText {
					t.Fatalf("dangling symlink was not restored: %q, %v", got, err)
				}
			}
			expectFault(prepare())
			expectFault(RecoverTransactions(t.Context(), owner))
			requireJournal(canonical, originalFence)
			if restored != beforeLock() {
				t.Fatal("canonical retries replayed the already visible restore")
			}

			failDir, failShared = "", true
			synced = nil
			expectFault(prepare())
			requireJournal(canonical, originalFence)
			if writer == "file-transaction" {
				childSync := slices.Index(synced, filepath.Join(owner, "restore/from"))
				parentSync := slices.Index(synced, filepath.Join(owner, "restore"))
				if childSync < 0 || parentSync <= childSync {
					t.Fatalf("recreated directories not synced deepest first: %v", synced)
				}
			}
			failShared, failRetirement = false, true
			expectFault(prepare())
			requireAbsent(canonical)
			requireJournal(staged, originalFence)

			failRetirement = false
			ownerLock := filepath.Join(owner, lock)
			if err := os.Chmod(ownerLock, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := prepare(); err == nil || !strings.Contains(err.Error(), "does not match the journal before-state") {
				t.Fatalf("staged retry did not refuse changed file mode: %v", err)
			}
			requireJournal(staged, originalFence)
			if info, err := os.Stat(ownerLock); err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("staged retry rewrote mismatched owner state: %v", err)
			}
			if err := os.Chmod(ownerLock, 0o644); err != nil {
				t.Fatal(err)
			}
			restored = beforeLock()
			expectFault(prepare())
			proof := readFile(t, repo, fenceName)
			if !proofRenamed || proof == originalFence {
				t.Fatal("proof directory fault did not follow visible proof publication")
			}
			requireJournal(staged, proof)
			failShared, failDir = false, filepath.Join(owner, ".agents")
			expectFault(prepare())
			requireJournal(staged, proof)
			failDir = ""
			if err := prepare(); err != nil {
				t.Fatal(err)
			}
			if restored != beforeLock() {
				t.Fatal("staged retries replayed rollback")
			}
			requireAbsent(staged)

			git("worktree", "remove", owner)
			requireAbsent(filepath.Join(repo, ".git/worktrees/owner"))
			if err := ReconcileGitExclusion(t.Context(), repo, contentHash([]byte(proof))); err != nil {
				t.Fatal(err)
			}
			requireAbsent(filepath.Join(repo, fenceName))
			if got := readFile(t, repo, gitExcludePath); got != "# keep an unterminated exclusion line" {
				t.Fatalf("shared exclusion was not restored exactly: %q", got)
			}
			if _, err := NewEngine().Run(repo, empty, []Intent{testIntent(generated, "generated\n", OwnershipGenerated)}, ModeApply, func(Ledger) error { return nil }); err != nil {
				t.Fatalf("surviving worktree could not retry: %v", err)
			}
		})
	}
}
