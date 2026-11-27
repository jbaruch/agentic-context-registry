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
			writeFile(t, repo, "survivor.md", "surviving project output\n")
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
			cleanup := filepath.Join(txRoot, ".staging-complete-"+pending.ID)
			fenceName := ".git/info/" + gitExclusionJournal
			originalFence := readFile(t, repo, fenceName)
			originalManifest := readFile(t, canonical, journalManifestFilename)
			beforeFiles, err := os.ReadDir(filepath.Join(canonical, "before"))
			if err != nil || len(beforeFiles) == 0 {
				t.Fatalf("expected journal before-images: %v", err)
			}
			beforeImages := make(map[string]string, len(beforeFiles))
			for _, entry := range beforeFiles {
				beforeImages[entry.Name()] = readFile(t, canonical, filepath.Join("before", entry.Name()))
			}
			requireJournal := func(dir, fence string) {
				t.Helper()
				if readFile(t, dir, journalManifestFilename) != originalManifest || readFile(t, repo, fenceName) != fence {
					t.Fatal("failure changed the journal manifest or fence")
				}
				for image, content := range beforeImages {
					if readFile(t, dir, filepath.Join("before", image)) != content {
						t.Fatalf("failure changed journal before-image %q", image)
					}
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
			var authorizations int
			prepare := func() error {
				return PrepareGitExclusion(t.Context(), owner, func(string) error {
					authorizations++
					return nil
				})
			}
			fault := errors.New("injected durability failure")
			expectFault := func(err error) {
				t.Helper()
				if !errors.Is(err, fault) {
					t.Fatalf("expected sync failure, got %v", err)
				}
			}
			refuse := func(err error, paths ...string) {
				t.Helper()
				for _, path := range paths {
					if err == nil || !strings.Contains(err.Error(), path) {
						t.Fatalf("expected refusal naming %q, got %v", path, err)
					}
				}
			}
			requireOK := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			ownerSync, sharedSync, renameHook := journalDirectorySync, gitExclusionDirectorySync, transactionRenameHook
			rename := journalRename
			t.Cleanup(func() {
				journalDirectorySync, gitExclusionDirectorySync, transactionRenameHook = ownerSync, sharedSync, renameHook
				journalRename = rename
			})
			var synced []string
			var failDir = filepath.Join(owner, ".agents")
			var retirementRenamed, failRetirement, failShared bool
			var proofPublications int
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
					proofPublications++
					failShared = true
				}
			}

			requireOK(os.Symlink(canonical, cleanup))
			refuse(prepare(), cleanup)
			if target, err := os.Readlink(cleanup); err != nil || target != canonical {
				t.Fatalf("refused preparation changed the symlink: %q, %v", target, err)
			}
			requireOK(os.Remove(cleanup))
			requireOK(os.Mkdir(cleanup, 0o700))
			refuse(prepare(), canonical, cleanup)
			requireOK(os.Remove(cleanup))
			if authorizations != 0 {
				t.Fatal("conflicting cleanup name entered canonical rollback")
			}
			requireJournal(canonical, originalFence)
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
			if proofPublications != 1 || proof == originalFence {
				t.Fatal("proof directory fault did not follow visible proof publication")
			}
			requireJournal(staged, proof)
			requireAbsent(cleanup)
			failShared, failDir = false, filepath.Join(owner, ".agents")
			expectFault(prepare())
			requireJournal(staged, proof)
			failDir = ""
			canonicalAuthorizations := authorizations
			manifest := filepath.Join(staged, journalManifestFilename)
			requireOK(os.Remove(manifest))
			refuse(prepare(), manifest, "refer to the Factory judge")
			requireAbsent(cleanup)
			if readFile(t, repo, fenceName) != proof {
				t.Fatal("damaged evidence stage changed proof")
			}
			requireOK(os.WriteFile(manifest, []byte(originalManifest), 0o600))
			requireOK(os.WriteFile(cleanup, []byte("keep"), 0o600))
			refuse(prepare(), cleanup)
			if got := readFile(t, txRoot, filepath.Base(cleanup)); got != "keep" {
				t.Fatal("refused preparation changed the non-directory")
			}
			requireOK(os.Remove(cleanup))
			requireOK(os.Mkdir(cleanup, 0o700))
			refuse(prepare(), staged, cleanup)
			requireOK(os.Remove(cleanup))

			journalRename = func(from, to string) error {
				if from != staged || to != cleanup {
					t.Fatalf("unexpected cleanup rename %q to %q", from, to)
				}
				return fault
			}
			err = prepare()
			expectFault(err)
			refuse(err, staged, cleanup)
			requireJournal(staged, proof)
			requireAbsent(cleanup)
			journalRename = rename

			// Simulate both possible names after interruption before rename sync.
			journalDirectorySync = func(dir string) error {
				if dir == txRoot {
					if _, err := os.Stat(cleanup); err == nil {
						return fault
					} else if !errors.Is(err, os.ErrNotExist) {
						return err
					}
				}
				return ownerSync(dir)
			}
			for attempt := range 2 {
				err = prepare()
				expectFault(err)
				refuse(err, txRoot, cleanup)
				requireAbsent(staged)
				requireJournal(cleanup, proof)
				// A crash before the sync may restore the old evidence name.
				if attempt == 0 {
					requireOK(os.Rename(cleanup, staged))
					writeFile(t, staged, "blocked/keep", "nested residue\n")
				}
			}
			journalDirectorySync = ownerSync
			failShared = true
			err = prepare()
			expectFault(err)
			refuse(err, filepath.Dir(filepath.Join(repo, fenceName)))
			requireJournal(cleanup, proof)
			failShared = false

			blocked := filepath.Join(cleanup, "blocked")
			if os.Geteuid() != 0 {
				// A real recursive deletion can lose the manifest but leave
				// a nonempty non-writable subtree and the cleanup directory.
				requireOK(os.Chmod(blocked, 0o500))
				err = prepare()
				requireOK(os.Chmod(blocked, 0o700))
				refuse(err, cleanup)
				if !errors.Is(err, os.ErrPermission) {
					t.Fatalf("expected a partial deletion permission error, got %v", err)
				}
				t.Log("recursive deletion removed the manifest and left protected residue")
			} else {
				// Root bypasses the directory permission barrier.
				requireOK(os.Remove(filepath.Join(cleanup, journalManifestFilename)))
			}
			requireAbsent(filepath.Join(cleanup, journalManifestFilename))
			if readFile(t, blocked, "keep") != "nested residue\n" {
				t.Fatal("partial deletion lost the protected residue")
			}
			// A cleanup name cannot replace terminal proof or exact before-images.
			proofPath := filepath.Join(repo, fenceName)
			requireOK(os.WriteFile(proofPath, []byte(originalFence), 0o600))
			refuse(prepare(), cleanup, "lacks terminal retirement proof")
			requireOK(os.WriteFile(proofPath, []byte(proof), 0o600))
			exclude := filepath.Join(repo, gitExcludePath)
			beforeExclude := readFile(t, repo, gitExcludePath)
			requireOK(os.WriteFile(exclude, []byte("changed before-state"), 0o600))
			refuse(prepare(), exclude, "does not match the fence before-state")
			requireOK(os.WriteFile(exclude, []byte(beforeExclude), 0o600))

			unrelated := filepath.Join(txRoot, ".staging-complete-unrelated")
			writeFile(t, unrelated, "keep", "unrelated\n")
			journalDirectorySync = func(dir string) error {
				if dir == txRoot {
					if _, err := os.Lstat(cleanup); errors.Is(err, os.ErrNotExist) {
						return fault
					}
				}
				return ownerSync(dir)
			}
			for attempt := range 3 {
				err = prepare()
				expectFault(err)
				refuse(err, txRoot)
				requireAbsent(cleanup)
				if readFile(t, repo, fenceName) != proof {
					t.Fatal("cleanup failure changed terminal proof")
				}
				if attempt == 0 {
					if readFile(t, unrelated, "keep") != "unrelated\n" {
						t.Fatal("preparation removed unrelated staging")
					}
					requireOK(os.RemoveAll(unrelated))
				} else {
					requireAbsent(txRoot)
				}
			}
			journalDirectorySync = ownerSync
			for range 2 {
				requireOK(prepare())
				requireAbsent(txRoot)
				if readFile(t, repo, fenceName) != proof || proofPublications != 1 || restored != beforeLock() || canonicalAuthorizations != authorizations {
					t.Fatal("cleanup retry rewrote proof or replayed rollback")
				}
			}
			requireAbsent(staged)

			applySurvivor := func() error {
				_, err := NewEngine().Run(repo, empty, []Intent{testIntent(generated, "generated\n", OwnershipGenerated)}, ModeApply, func(Ledger) error { return nil })
				return err
			}
			var busy *TransactionBusyError
			if err := applySurvivor(); !errors.As(err, &busy) {
				t.Fatalf("sibling with registered owner = %v, want transaction_busy", err)
			}
			digest := contentHash([]byte(proof))
			if err := ReconcileGitExclusion(t.Context(), repo, digest); !errors.As(err, &busy) {
				t.Fatal("preparation removed the fence for a still-registered owner")
			}
			git("worktree", "remove", owner)
			requireAbsent(filepath.Join(repo, ".git/worktrees/owner"))
			survivor := []string{gitExcludePath, ".git/index", lock, "survivor.md"}
			beforeReconcile := make(map[string]string, len(survivor))
			for _, name := range survivor {
				beforeReconcile[name] = readFile(t, repo, name)
			}
			requireAbsent(filepath.Join(repo, gitExcludeReceiptPath))
			requireOK(ReconcileGitExclusion(t.Context(), repo, digest))
			requireAbsent(filepath.Join(repo, fenceName))
			requireAbsent(filepath.Join(repo, gitExcludeReceiptPath))
			for _, name := range survivor {
				if readFile(t, repo, name) != beforeReconcile[name] {
					t.Fatalf("fence reconciliation changed %q", name)
				}
			}
			if got := readFile(t, repo, gitExcludePath); got != "# keep an unterminated exclusion line" {
				t.Fatalf("shared exclusion was not restored exactly: %q", got)
			}
			if err := applySurvivor(); err != nil {
				t.Fatalf("surviving worktree could not retry: %v", err)
			}
		})
	}
}
