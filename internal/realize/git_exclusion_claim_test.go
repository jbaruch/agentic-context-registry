package realize

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initGit(t *testing.T, root string) {
	t.Helper()
	if output, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
}

const beforeExclusionGeneration = "before-generation-is-journaled"

func exitAtExclusionBoundary(boundary string) {
	if boundary == beforeExclusionGeneration {
		write := journalFileWriter
		journalFileWriter = func(filename string, content []byte, mode os.FileMode) error {
			if filepath.Base(filename) == journalManifestFilename && bytes.Contains(content, []byte(`"afterGeneration"`)) {
				os.Exit(85)
			}
			return write(filename, content, mode)
		}
	}
	transactionRenameHook = func(path string) {
		if path == boundary {
			os.Exit(85)
		}
	}
}

func requireExclusionCrash(t *testing.T, env, project string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), binary, "-test.run=^"+strings.ReplaceAll(t.Name(), "/", "$/^")+"$")
	command.Env = append(command.Environ(), env+"="+project)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 85 {
		t.Fatalf("interrupted realize = %v: %s", err, output)
	}
}

func TestGitExclusionCrashBlocksSiblingUntilRecovery(t *testing.T) {
	for _, boundary := range []string{beforeExclusionGeneration, gitExcludePath, gitExcludeReceiptPath} {
		t.Run(filepath.Base(boundary), func(t *testing.T) {
			t.Run("same worktree", func(t *testing.T) { testGitExclusionCrashRecovery(t, false, boundary) })
			t.Run("moved linked worktree", func(t *testing.T) { testGitExclusionCrashRecovery(t, true, boundary) })
		})
	}
}

func testGitExclusionCrashRecovery(t *testing.T, move bool, boundary string) {
	t.Helper()
	const childProject = "ACR_TEST_INTERRUPTED_EXCLUSION_PROJECT"
	const target = ".cursor/rules/generated.mdc"
	const vendorPath = ".agents/vendor/example/alpha"
	const vendorContent = "vendor package\n"
	const userPattern = "keep-*.local"
	intent := testIntent(target, "generated rule\n", OwnershipGenerated)
	empty := Ledger{SchemaVersion: CurrentLedgerSchemaVersion}
	if project := os.Getenv(childProject); project != "" {
		exitAtExclusionBoundary(boundary)
		_, err := NewEngine().ApplyStateFilesWithVendorTrees(t.Context(), project, empty, Ledger{}, []Intent{intent}, func(Ledger) ([]StateFile, error) {
			return nil, nil
		}, func() (VendorTreeChanges, error) {
			return VendorTreeChanges{Installs: []VendorTreeInstall{{Path: vendorPath, Files: []VendorTreeFile{
				{Path: "README.md", Content: []byte(vendorContent), Mode: 0o644},
			}}}}, nil
		})
		t.Fatalf("realization did not reach the exclusion write: %v", err)
	}

	worktree := t.TempDir()
	initGit(t, worktree)
	writeFile(t, worktree, gitExcludePath, userPattern)
	if err := os.Chmod(filepath.Join(worktree, gitExcludePath), 0o640); err != nil {
		t.Fatal(err)
	}
	alphaRoot, betaRoot := worktree, worktree
	git := func(args ...string) {
		t.Helper()
		if output, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", worktree}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if move {
		fixture := t.TempDir()
		alphaRoot, betaRoot = filepath.Join(fixture, "owner"), filepath.Join(fixture, "sibling")
		git("-c", "user.name=Example", "-c", "user.email=example@example.org", "commit", "--allow-empty", "-qm", "Seed")
		git("worktree", "add", "-q", "--detach", alphaRoot)
		git("worktree", "add", "-q", "--detach", betaRoot)
	}
	alpha, beta := filepath.Join(alphaRoot, "alpha"), filepath.Join(betaRoot, "beta")
	for _, project := range []string{alpha, beta} {
		if err := os.Mkdir(project, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var betaLedger, priorLedger Ledger
	apply := func(project string, next *Ledger) error {
		_, err := NewEngine().Run(project, empty, []Intent{intent}, ModeApply, func(ledger Ledger) error {
			*next = ledger
			return nil
		})
		return err
	}
	prior := filepath.Join(worktree, "prior")
	if err := os.Mkdir(prior, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := apply(prior, &priorLedger); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, worktree, gitExcludePath)
	beforeReceipt := readFile(t, worktree, gitExcludeReceiptPath)
	requireExclusionCrash(t, childProject, alpha)
	crashed := readFile(t, worktree, gitExcludePath)
	crashedReceipt := readFile(t, worktree, gitExcludeReceiptPath)
	if (crashedReceipt != beforeReceipt) != (boundary == gitExcludeReceiptPath) {
		t.Fatalf("receipt did not stop at the %s boundary", boundary)
	}
	if !strings.Contains(crashed, "/alpha/"+target) {
		t.Fatalf("interrupted realization did not write its exclusion: %q", crashed)
	}
	if got := readFile(t, alpha, vendorPath+"/README.md"); got != vendorContent {
		t.Fatalf("interrupted realization did not promote the vendor tree: %q", got)
	}
	_, pendingErr := inspectTransactions(alpha)
	var pending *PendingTransactionError
	if !errors.As(pendingErr, &pending) {
		t.Fatalf("interrupted realization did not leave a pending journal: %v", pendingErr)
	}
	manifest, err := loadJournal(alpha, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != exclusionJournalSchemaVersion {
		t.Fatalf("mixed transaction journal schema = %d, want %d", manifest.SchemaVersion, exclusionJournalSchemaVersion)
	}
	assertSiblingRefused := func() {
		t.Helper()
		var busy *TransactionBusyError
		if err := apply(beta, &betaLedger); !errors.As(err, &busy) || !strings.Contains(err.Error(), "pending journal") {
			t.Fatalf("sibling realize returned %v, want recoverable exclusion contention", err)
		}
		if got := readFile(t, worktree, gitExcludePath); got != crashed || len(betaLedger.Targets) != 0 {
			t.Fatalf("refused sibling changed the exclusion or ledger: %q, %+v", got, betaLedger)
		}
		if got := readFile(t, worktree, gitExcludeReceiptPath); got != crashedReceipt {
			t.Fatal("refused sibling changed the exclusion receipt")
		}
		for _, path := range []string{target, transactionDirectory} {
			if _, err := os.Lstat(filepath.Join(beta, path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused sibling left %s: %v", path, err)
			}
		}
	}
	assertSiblingRefused()
	if move {
		moved := filepath.Join(filepath.Dir(alphaRoot), "moved owner")
		if boundary == gitExcludeReceiptPath {
			// Recent Git rewrites the backlink when converting to relative
			// worktree paths. The administrative directory still identifies it.
			git("-c", "worktree.useRelativePaths=true", "worktree", "move", alphaRoot, moved)
		} else {
			git("worktree", "move", alphaRoot, moved)
		}
		assertSiblingRefused()
		if err := os.MkdirAll(alpha, 0o755); err != nil {
			t.Fatal(err)
		}
		assertSiblingRefused()
		alpha = filepath.Join(moved, "alpha")
		alias := filepath.Join(filepath.Dir(moved), "owner-alias")
		if err := os.Symlink(moved, alias); err != nil {
			t.Fatal(err)
		}
		alpha = filepath.Join(alias, "alpha")
	}

	if boundary == beforeExclusionGeneration {
		var conflict *RecoveryConflictError
		if err := RecoverTransactions(t.Context(), alpha); !errors.As(err, &conflict) {
			t.Fatalf("unpublished generation recovery = %v, want recovery_conflict", err)
		}
		assertSiblingRefused()
		if err := os.Rename(filepath.Join(worktree, gitExcludePath), filepath.Join(t.TempDir(), "saved-exclude")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(worktree, gitExcludePath), []byte(before), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecoverTransactions(t.Context(), alpha); err != nil {
		t.Fatal(err)
	}
	if boundary == beforeExclusionGeneration && readFile(t, worktree, gitExcludeReceiptPath) != beforeReceipt {
		t.Fatal("recovery restamped the separator receipt after manual reconciliation")
	}
	if got := readFile(t, worktree, gitExcludePath); got != before {
		t.Fatalf("recovered exclusion = %q, want exact before-image %q", got, before)
	}
	if info, err := os.Stat(filepath.Join(worktree, gitExcludePath)); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("recovered exclusion mode = %v, %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(alpha, vendorPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery left vendor tree at its live path: %v", err)
	}
	if got := readFile(t, alpha, recoveredVendorPath(pending.ID, 0)+"/README.md"); got != vendorContent {
		t.Fatalf("recovered vendor package = %q, want %q", got, vendorContent)
	}
	if _, err := os.Stat(filepath.Join(alpha, transactionDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered project retained its journal: %v", err)
	}
	var alphaLedger Ledger
	if err := apply(beta, &betaLedger); err != nil {
		t.Fatalf("retry sibling: %v", err)
	}
	if err := apply(alpha, &alphaLedger); err != nil {
		t.Fatalf("retry recovered project: %v", err)
	}
	for _, ledger := range []Ledger{alphaLedger, betaLedger} {
		if len(ledger.Targets) != 1 || !ledger.Targets[0].Excluded {
			t.Fatalf("retried ledger lacks exclusion: %+v", ledger)
		}
	}
	for _, path := range []string{"alpha/" + target, "beta/" + target, "keep-notes.local"} {
		if output, err := exec.Command("git", "-C", worktree, "check-ignore", "-q", "--", path).CombinedOutput(); err != nil {
			t.Errorf("check-ignore %s: %v: %s", path, err, output)
		}
	}
	for _, installed := range []struct {
		project string
		ledger  Ledger
	}{{prior, priorLedger}, {alpha, alphaLedger}, {beta, betaLedger}} {
		if _, err := NewEngine().Run(installed.project, installed.ledger, nil, ModeApply, func(Ledger) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	want := userPattern
	if boundary == beforeExclusionGeneration {
		want += "\n" // The manually restored file cannot inherit the prior receipt.
	}
	if got := readFile(t, worktree, gitExcludePath); got != want {
		t.Fatalf("cleanup after recovery = %q, want %q", got, want)
	}
}

func TestGitExclusionRecoveryPreservesUnprovenAfterState(t *testing.T) {
	for _, scenario := range []struct {
		name, change, reason      string
		absent, published, legacy bool
	}{
		{name: "first append replaced", change: "replace", reason: "no recorded installation generation"},
		{name: "first append untouched", reason: "no recorded installation generation"},
		{name: "originally absent", change: "replace", reason: "no recorded installation generation", absent: true},
		{name: "legacy journal", change: "replace", reason: "no recorded installation generation", legacy: true},
		{name: "published generation edited and restored in place", change: "edit", reason: "different file generation", published: true},
		{name: "replacement after preflight", change: "late replace", reason: "changed during recovery", published: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			const childProject = "ACR_TEST_UNPROVEN_EXCLUSION_PROJECT"
			const userPattern = "keep-*.local"
			const target = ".cursor/rules/generated.mdc"
			const targetContent = "generated rule\n"
			const ledgerPath = ".agents/registry.lock"
			intent := testIntent(target, targetContent, OwnershipGenerated)
			empty := Ledger{SchemaVersion: CurrentLedgerSchemaVersion}
			beforeLedger, err := json.Marshal(empty)
			if err != nil {
				t.Fatal(err)
			}
			if project := os.Getenv(childProject); project != "" {
				boundary := beforeExclusionGeneration
				if scenario.published {
					boundary = gitExcludePath
				}
				exitAtExclusionBoundary(boundary)
				_, err := NewEngine().RunStateFiles(t.Context(), project, empty, Ledger{}, []Intent{intent}, ModeApply, func(ledger Ledger) ([]StateFile, error) {
					content, err := json.Marshal(ledger)
					return []StateFile{{Path: ledgerPath, BeforeExists: true, BeforeContent: beforeLedger, BeforeMode: 0o644, Content: content, Mode: 0o644}}, err
				})
				t.Fatalf("realization did not reach the exclusion write: %v", err)
			}

			worktree := t.TempDir()
			initGit(t, worktree)
			worktree, err = filepath.EvalSymlinks(worktree)
			if err != nil {
				t.Fatal(err)
			}
			alpha, beta := filepath.Join(worktree, "owner"), filepath.Join(worktree, "sibling")
			if err := os.Mkdir(beta, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, alpha, ledgerPath, string(beforeLedger))
			writeFile(t, worktree, "staged", "index fixture\n")
			if output, err := exec.CommandContext(t.Context(), "git", "-C", worktree, "add", "-f", "staged").CombinedOutput(); err != nil {
				t.Fatalf("git add fixture: %v: %s", err, output)
			}
			exclude := filepath.Join(worktree, gitExcludePath)
			if scenario.absent {
				if err := os.Remove(exclude); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			} else {
				writeFile(t, worktree, gitExcludePath, userPattern)
				if err := os.Chmod(exclude, 0o640); err != nil {
					t.Fatal(err)
				}
			}
			requireExclusionCrash(t, childProject, alpha)
			after := readFile(t, worktree, gitExcludePath)
			installed, err := os.Stat(exclude)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(after, "/owner/"+target) || !scenario.absent && (!strings.HasPrefix(after, userPattern+"\n") || installed.Mode().Perm() != 0o640) {
				t.Fatalf("interruption did not leave the expected exclusion, got %q mode %o", after, installed.Mode().Perm())
			}
			if readFile(t, alpha, target) != targetContent || readFile(t, alpha, ledgerPath) == string(beforeLedger) {
				t.Fatal("fixture did not interrupt after native target and ledger writes")
			}
			_, err = inspectTransactions(alpha)
			var pending *PendingTransactionError
			if !errors.As(err, &pending) {
				t.Fatalf("missing interrupted journal: %v", err)
			}
			journal := filepath.Join(alpha, transactionDirectory, pending.ID)
			manifest, err := loadJournal(alpha, pending.ID)
			if err != nil {
				t.Fatal(err)
			}
			var exclusion *journalEntry
			for i := range manifest.Entries {
				if manifest.Entries[i].GitExclusion {
					exclusion = &manifest.Entries[i]
					break
				}
			}
			if exclusion == nil || exclusion.ExclusionEdit == nil {
				t.Fatal("journal did not contain the shared exclusion transition")
			}
			generation := exclusion.ExclusionEdit.AfterGeneration
			if scenario.published && (generation == (fileGeneration{}) || generation != fileGenerationOf(installed)) || !scenario.published && generation != (fileGeneration{}) {
				t.Fatalf("unexpected recorded installation generation: %+v", generation)
			}
			if scenario.legacy {
				exclusion.ExclusionEdit = nil
				manifest.SchemaVersion = journalSchemaVersion
				encoded, err := marshalJournal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := writeSyncedFile(filepath.Join(journal, journalManifestFilename), encoded, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			replace := func() {
				t.Helper()
				replacement := filepath.Join(filepath.Dir(exclude), "replacement")
				if err := os.WriteFile(replacement, []byte(after), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(replacement, installed.Mode().Perm()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, exclude); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(exclude)
				if err != nil || os.SameFile(installed, info) {
					t.Fatalf("fixture did not install a different inode: %v", err)
				}
			}
			switch scenario.change {
			case "replace":
				replace()
			case "edit":
				for _, content := range []string{"external edit\n", after} {
					if err := os.WriteFile(exclude, []byte(content), installed.Mode().Perm()); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Chtimes(exclude, installed.ModTime(), installed.ModTime()); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(exclude)
				if err != nil || !os.SameFile(installed, info) || !info.ModTime().Equal(installed.ModTime()) || fileGenerationOf(info).Ctime == generation.Ctime {
					t.Fatalf("fixture did not restore bytes and mtime on the edited inode: %v", err)
				}
			}

			files := []string{
				exclude, filepath.Join(worktree, gitExcludeReceiptPath), filepath.Join(filepath.Dir(exclude), gitExclusionJournal),
				filepath.Join(worktree, ".git", "index"), filepath.Join(alpha, target), filepath.Join(alpha, ledgerPath),
				filepath.Join(beta, target), filepath.Join(beta, ledgerPath), filepath.Join(beta, transactionDirectory),
			}
			capture := func() map[string]exclusionRecoveryFixtureState {
				t.Helper()
				states := make(map[string]exclusionRecoveryFixtureState)
				for _, file := range files {
					states[file] = exclusionRecoveryState(t, file)
				}
				if err := filepath.WalkDir(journal, func(path string, _ fs.DirEntry, err error) error {
					if err == nil {
						states[path] = exclusionRecoveryState(t, path)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return states
			}
			want := capture()
			if want[exclude].content != after || want[exclude].mode.Perm() != installed.Mode().Perm() {
				t.Fatal("external edit did not reproduce exact after-state bytes and mode")
			}
			if scenario.change == "late replace" {
				sync := journalDirectorySync
				t.Cleanup(func() { journalDirectorySync = sync })
				journalDirectorySync = func(directory string) error {
					if directory == journal {
						journalDirectorySync = sync
						replace()
						want[exclude] = exclusionRecoveryState(t, exclude)
					}
					return sync(directory)
				}
			}
			applySibling := func() error {
				_, err := NewEngine().Run(beta, empty, []Intent{intent}, ModeApply, func(Ledger) error { return nil })
				return err
			}
			for attempt := range 2 {
				var conflict *RecoveryConflictError
				err := RecoverTransactions(t.Context(), alpha)
				reason := scenario.reason
				if scenario.change == "late replace" && attempt != 0 {
					reason = "different file generation"
				}
				if !errors.As(err, &conflict) || !strings.Contains(err.Error(), reason) {
					t.Fatalf("owner retry = %v, want recovery_conflict (%s)", err, reason)
				}
				for _, part := range []string{exclude, journal, "preserve the live file separately", "Recreating the after-state bytes will not unblock recovery"} {
					if !strings.Contains(err.Error(), part) {
						t.Fatalf("recovery guidance %q does not include %q", err, part)
					}
				}
				var busy *TransactionBusyError
				if err := applySibling(); !errors.As(err, &busy) || !strings.Contains(err.Error(), "pending journal") {
					t.Fatalf("sibling retry = %v, want transaction_busy", err)
				}
				got := capture()
				if len(got) != len(want) {
					t.Fatalf("refusals changed the journal inventory: got %d entries, want %d", len(got), len(want))
				}
				for file, expected := range want {
					if state, ok := got[file]; !ok || state != expected {
						t.Errorf("refusals changed the bytes, mode, or generation of %s", file)
					}
				}
			}
			backup := filepath.Join(t.TempDir(), "saved-exclude")
			if err := os.Rename(exclude, backup); err != nil {
				t.Fatal(err)
			}
			if exclusion.BeforeExists {
				writeFile(t, worktree, gitExcludePath, readFile(t, journal, exclusion.BeforeImage))
				if err := os.Chmod(exclude, os.FileMode(exclusion.BeforeMode)); err != nil {
					t.Fatal(err)
				}
			}
			reconciled := exclusionRecoveryState(t, exclude)
			if err := RecoverTransactions(t.Context(), alpha); err != nil {
				t.Fatalf("recover after preserving and reconciling the exclusion: %v", err)
			}
			if exclusionRecoveryState(t, exclude) != reconciled || exclusionRecoveryState(t, filepath.Join(worktree, gitExcludeReceiptPath)).exists {
				t.Fatal("recovery rewrote the reconciled file or minted separator proof")
			}
			if exclusionRecoveryState(t, filepath.Join(alpha, target)).exists || readFile(t, alpha, ledgerPath) != string(beforeLedger) ||
				exclusionRecoveryState(t, journal).exists || exclusionRecoveryState(t, filepath.Join(filepath.Dir(exclude), gitExclusionJournal)).exists {
				t.Fatal("recovery did not roll back the transaction and retire the journal and fence")
			}
			if err := applySibling(); err != nil {
				t.Fatalf("sibling retry after owner recovery: %v", err)
			}
			if saved := exclusionRecoveryState(t, backup); saved.content != after || saved.mode.Perm() != installed.Mode().Perm() {
				t.Fatal("explicit reconciliation did not retain the saved after-state")
			}
		})
	}
}

type exclusionRecoveryFixtureState struct {
	exists     bool
	content    string
	mode       fs.FileMode
	generation fileGeneration
}

func exclusionRecoveryState(t *testing.T, path string) exclusionRecoveryFixtureState {
	t.Helper()
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return exclusionRecoveryFixtureState{}
	}
	if err != nil {
		t.Fatal(err)
	}
	state := exclusionRecoveryFixtureState{exists: true, mode: info.Mode(), generation: fileGenerationOf(info)}
	if !info.IsDir() {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		state.content = string(content)
	}
	return state
}

func TestGitExclusionRetirementSyncFailureKeepsFence(t *testing.T) {
	for _, restoreCanonical := range []bool{false, true} {
		name := "retry directory sync"
		if restoreCanonical {
			name = "recover restored canonical journal"
		}
		t.Run(name, func(t *testing.T) {
			worktree := t.TempDir()
			initGit(t, worktree)
			const userPattern = "keep-*.local"
			const target = ".cursor/rules/generated.mdc"
			writeFile(t, worktree, gitExcludePath, userPattern)
			alpha, beta := filepath.Join(worktree, "alpha"), filepath.Join(worktree, "beta")
			for _, project := range []string{alpha, beta} {
				if err := os.Mkdir(project, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// The marker stores a physical path even when TempDir uses a symlink.
			journalParent, err := filepath.EvalSymlinks(worktree)
			if err != nil {
				t.Fatal(err)
			}
			journalParent = filepath.Join(journalParent, "alpha", transactionDirectory)
			syncFailure := errors.New("injected retirement directory sync failure")
			originalRename, originalSync := journalRename, journalDirectorySync
			t.Cleanup(func() { journalRename, journalDirectorySync = originalRename, originalSync })
			var canonical, retired string
			failSync := true
			journalRename = func(from, to string) error {
				if err := originalRename(from, to); err != nil {
					return err
				}
				if strings.HasPrefix(filepath.Base(to), ".staging-complete-") && retired == "" {
					canonical, retired = from, to
				}
				return nil
			}
			journalDirectorySync = func(directory string) error {
				if failSync && retired != "" &&
					(directory == journalParent || directory == filepath.Dir(retired)) {
					return syncFailure
				}
				return originalSync(directory)
			}
			ledgers := map[string]Ledger{}
			apply := func(project string) error {
				_, err := NewEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion},
					[]Intent{testIntent(target, "generated rule\n", OwnershipGenerated)}, ModeApply,
					func(ledger Ledger) error { ledgers[project] = ledger; return nil })
				return err
			}
			if err := apply(alpha); !errors.Is(err, syncFailure) {
				t.Fatalf("owner realization = %v, want retirement sync failure", err)
			}
			marker := filepath.Join(worktree, ".git", "info", gitExclusionJournal)
			data, err := os.ReadFile(marker)
			if err != nil {
				t.Fatalf("failed retirement lost its shared fence: %v", err)
			}
			var fence gitExclusionFence
			if err := json.Unmarshal(data, &fence); err != nil || filepath.Join(fence.Project.Path, transactionDirectory, fence.Journal) != filepath.Join(journalParent, filepath.Base(canonical)) {
				t.Fatalf("failed retirement lost its shared fence: %q, %v", data, err)
			}
			if _, err := os.Lstat(canonical); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("canonical journal should be absent until simulated crash: %v", err)
			}
			afterAlpha := readFile(t, worktree, gitExcludePath)
			alphaReceipt := readFile(t, worktree, gitExcludeReceiptPath)
			if err := apply(beta); !errors.Is(err, syncFailure) {
				t.Fatalf("sibling retry with an absent journal = %v, want sync failure", err)
			}
			if got := readFile(t, worktree, gitExcludePath); got != afterAlpha || len(ledgers[beta].Targets) != 0 {
				t.Fatalf("refused sibling wrote the exclusion or ledger: %q, %+v", got, ledgers[beta])
			}
			if got := readFile(t, worktree, gitExcludeReceiptPath); got != alphaReceipt {
				t.Fatal("refused sibling changed the exclusion receipt")
			}
			if _, err := os.Lstat(filepath.Join(beta, target)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused sibling wrote an output: %v", err)
			}
			if err := RecoverTransactions(t.Context(), alpha); !errors.Is(err, syncFailure) {
				t.Fatalf("owner cleanup = %v, want sync failure", err)
			}
			if _, err := os.Lstat(retired); err != nil {
				t.Fatalf("owner cleanup removed the journal before syncing its rename: %v", err)
			}
			failSync = false
			if restoreCanonical {
				// Model a crash losing the unsynced retirement rename.
				if err := os.Rename(retired, canonical); err != nil {
					t.Fatal(err)
				}
				var busy *TransactionBusyError
				if err := apply(beta); !errors.As(err, &busy) {
					t.Fatalf("sibling bypassed the restored journal: %v", err)
				}
				if err := RecoverTransactions(t.Context(), alpha); err != nil {
					t.Fatal(err)
				}
				if got := readFile(t, worktree, gitExcludePath); got != userPattern {
					t.Fatalf("recovered exclusion = %q, want original operator pattern", got)
				}
				if err := apply(alpha); err != nil {
					t.Fatalf("retry recovered owner: %v", err)
				}
			}
			if err := apply(beta); err != nil {
				t.Fatalf("retry sibling after durable retirement: %v", err)
			}
			if err := RecoverTransactions(t.Context(), alpha); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("durable retirement left its exclusion fence: %v", err)
			}
			for _, path := range []string{"alpha/" + target, "beta/" + target, "keep-notes.local"} {
				if output, err := exec.Command("git", "-C", worktree, "check-ignore", "-q", "--", path).CombinedOutput(); err != nil {
					t.Errorf("check-ignore %s: %v: %s", path, err, output)
				}
			}
		})
	}
}
