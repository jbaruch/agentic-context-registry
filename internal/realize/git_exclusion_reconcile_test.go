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
)

func TestGitExclusionRetirement(t *testing.T) {
	authorize := func(string) error { return nil } // No local dependencies in these fixtures.
	const retiredVendor = ".agents/vendor/local/retired"
	for _, fixture := range []struct {
		name, boundary, preparationCrash, unwitnessed                          string
		fileWriter, priorReceipt, excludeAbsent, move, legacyFence, draftProof bool
	}{
		{name: "realize and crash before proof", boundary: gitExcludePath, preparationCrash: exclusionRetirementPrefix, legacyFence: true},
		{name: "file writer and crash after proof", boundary: gitExcludePath, preparationCrash: "exclusion-retirement-proof", fileWriter: true, priorReceipt: true},
		{name: "moved owner with mixed vendor moves and crash during fence retirement", boundary: retiredVendor, priorReceipt: true, move: true},
		{name: "file writer with original exclusion absence", boundary: gitExcludePath, fileWriter: true, excludeAbsent: true},
		{name: "draft proof with staged journal", boundary: gitExcludePath, preparationCrash: exclusionRetirementPrefix, draftProof: true},
		{name: "draft proof without journal", boundary: gitExcludePath, draftProof: true},
		{name: "unprepared forced removal", boundary: gitExcludePath, unwitnessed: "remove"},
		{name: "unprepared forced removal with regular replacement", boundary: gitExcludePath, fileWriter: true, unwitnessed: "remove-file"},
		{name: "unprepared relocation and prune", boundary: gitExcludePath, unwitnessed: "relocate"},
		{name: "unprepared relocation and prune with regular replacement", boundary: gitExcludePath, fileWriter: true, unwitnessed: "relocate-file", legacyFence: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			const childProject = "ACR_TEST_ORPHAN_EXCLUSION_PROJECT"
			const retireChild = "ACR_TEST_ORPHAN_FENCE_DIGEST"
			const prepareChild = "ACR_TEST_PREPARE_EXCLUSION_BOUNDARY"
			const target = ".cursor/rules/orphan-fixture.mdc"
			empty := Ledger{SchemaVersion: CurrentLedgerSchemaVersion}
			desired := Ledger{SchemaVersion: CurrentLedgerSchemaVersion, Targets: []Target{testTarget(target, "generated\n", OwnershipGenerated)}}
			intent := testIntent(target, "generated\n", OwnershipGenerated)
			finalize := func(Ledger) error { return nil }
			const vendor = ".agents/vendor/local/fixture"
			vendorWriter := func(project string) error {
				_, err := NewEngine().ApplyStateFilesWithVendorTrees(t.Context(), project, empty, Ledger{}, []Intent{intent},
					func(Ledger) ([]StateFile, error) { return nil, nil },
					func() (VendorTreeChanges, error) {
						retirement, err := PlanVendorTreeRetirement(project, retiredVendor)
						if err != nil {
							return VendorTreeChanges{}, err
						}
						return VendorTreeChanges{Installs: []VendorTreeInstall{{
							Path: vendor, Files: []VendorTreeFile{{Path: "rules/fixture.md", Content: []byte("fixture rule\n"), Mode: 0o644}},
						}}, Retirements: []VendorTreeRetirement{retirement}}, nil
					})
				return err
			}
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
				if boundary := os.Getenv(prepareChild); boundary != "" {
					exitAtExclusionBoundary(boundary)
					t.Fatalf("preparation did not reach interruption: %v", PrepareGitExclusion(t.Context(), project, authorize))
				}
				if digest := os.Getenv(retireChild); digest != "" {
					exitAtExclusionBoundary(gitExclusionJournal)
					t.Fatalf("fence retirement did not reach interruption: %v", ReconcileGitExclusion(t.Context(), project, digest))
				}
				exitAtExclusionBoundary(fixture.boundary)
				if fixture.fileWriter {
					t.Fatalf("file transaction did not reach interruption: %v", fileApply(project))
				}
				if fixture.move {
					t.Fatalf("mixed vendor realization did not reach interruption: %v", vendorWriter(project))
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
			if fixture.move {
				writeFile(t, owner, retiredVendor+"/rules/old.md", "retired fixture\n")
			}
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
				(matchesGitSnapshot(beforeReceipt, afterReceipt) == fixture.move) {
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
			writeDraftProof := func(fence gitExclusionFence) fileSnapshot {
				t.Helper()
				fence.SchemaVersion, fence.Retirement = gitExclusionFenceVersion, ""
				record, err := json.Marshal(fence)
				if err != nil {
					t.Fatal(err)
				}
				fence.Retirement = contentHash(record) // Draft proof predated the durability domain.
				data, err := json.Marshal(fence)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fencePath, data, 0o600); err != nil {
					t.Fatal(err)
				}
				return snapshot(fenceName)
			}
			if !originalFence.exists || originalFence.mode != 0o600 {
				t.Fatalf("crash did not leave a private fence: mode %o", originalFence.mode)
			}
			var recorded gitExclusionFence
			if err := json.Unmarshal(originalFence.content, &recorded); err != nil || recorded.SchemaVersion != gitExclusionFenceVersion || recorded.Retirement != "" ||
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
			if fixture.legacyFence {
				recorded.SchemaVersion = 1
				legacy, err := json.Marshal(recorded)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fencePath, legacy, 0o600); err != nil {
					t.Fatal(err)
				}
				originalFence = snapshot(fenceName)
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
			checkSiblingBusy("owning project")
			refuse(reconcile(originalFence.hash), "owning project")
			refuse(PrepareGitExclusion(t.Context(), sibling, authorize), "requires the matching owner")
			registration := recorded.Registration.Path
			if fixture.unwitnessed != "" {
				// Make the shared before-images identical to prepared removal.
				// Neither this nor a matching digest can prove retirement.
				restore(excludePath, before)
				restore(receiptPath, beforeReceipt)
				survivor := filepath.Join(worktrees, "relocated")
				canonical := filepath.Join(owner, transactionDirectory, pending.ID, journalManifestFilename)
				originalJournal, err := os.ReadFile(canonical)
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(fixture.unwitnessed, "relocate") {
					if err := os.Rename(owner, survivor); err != nil {
						t.Fatal(err)
					}
					git("worktree", "prune", "--expire", "now")
				} else {
					git("worktree", "remove", "--force", owner)
				}
				if _, err := os.Stat(registration); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("fixture did not remove the Git registration: %v", err)
				}
				if strings.HasSuffix(fixture.unwitnessed, "-file") {
					if err := os.WriteFile(owner, []byte("unrelated file"), 0o640); err != nil {
						t.Fatal(err)
					}
				}
				after, afterReceipt = snapshot(gitExcludePath), snapshot(gitExcludeReceiptPath)
				checkSiblingBusy("no terminal retirement proof")
				err = reconcile(originalFence.hash)
				refuse(err, originalFence.hash)
				refuse(err, "locate or restore the matching owner")
				if strings.Contains(err.Error(), "acr reconcile-exclusion") {
					t.Fatalf("unwitnessed retirement advertised reconciliation: %v", err)
				}
				checkSiblingBusy("locate or restore the matching owner")
				if strings.HasSuffix(fixture.unwitnessed, "-file") {
					if got, err := os.ReadFile(owner); err != nil || string(got) != "unrelated file" {
						t.Fatalf("regular replacement changed: %q, %v", got, err)
					}
				}
				if strings.HasPrefix(fixture.unwitnessed, "relocate") {
					saved := filepath.Join(survivor, transactionDirectory, pending.ID, journalManifestFilename)
					if got, err := os.ReadFile(saved); err != nil || !bytes.Equal(got, originalJournal) {
						t.Fatalf("surviving canonical journal changed: %v", err)
					}
				}
				return
			}
			if fixture.move {
				moved := filepath.Join(worktrees, "moved")
				git("worktree", "move", owner, moved)
				checkSiblingBusy("owning project")
				owner = moved
				if got := readFile(t, owner, vendor+"/rules/fixture.md"); got != "fixture rule\n" {
					t.Fatalf("mixed v3 journal did not install the vendor fixture: %q", got)
				}
				writeFile(t, owner, recoveredVendorPath(pending.ID, 1)+"/rules/old.md", "edited after retirement\n")
				sync := journalDirectorySync
				t.Cleanup(func() { journalDirectorySync = sync })
				fault := errors.New("injected vendor move parent sync failure")
				liveParent, preservedParent := ".agents/vendor/local", recoveredVendorDirectory(pending.ID)
				// First fail the move syncs, then fail them again after both
				// moves are already visible and need no replay.
				for _, parent := range []string{liveParent, preservedParent, liveParent, preservedParent} {
					journalDirectorySync = func(dir string) error {
						if dir == filepath.Join(owner, parent) {
							return fault
						}
						return sync(dir)
					}
					if err := PrepareGitExclusion(t.Context(), owner, authorize); !errors.Is(err, fault) {
						t.Fatalf("vendor move parent %s was not synced: %v", parent, err)
					}
					if !matchesGitSnapshot(originalFence, snapshot(fenceName)) {
						t.Fatal("vendor rollback sync failure changed the fence")
					}
					_, err := inspectTransactions(owner)
					if !errors.As(err, &pending) {
						t.Fatalf("vendor rollback fault lost the canonical journal: %v", err)
					}
				}
				journalDirectorySync = sync
			}
			if fixture.preparationCrash != "" {
				t.Setenv(prepareChild, fixture.preparationCrash)
				requireExclusionCrash(t, childProject, owner)
				evidence := snapshot(fenceName)
				var interrupted gitExclusionFence
				if err := json.Unmarshal(evidence.content, &interrupted); err != nil ||
					(interrupted.Retirement == "") != (fixture.preparationCrash == exclusionRetirementPrefix) {
					t.Fatalf("interruption left incorrect proof state: %v", err)
				}
				if _, err := inspectTransactions(owner); err != nil {
					t.Fatalf("preparation did not retire the canonical journal: %v", err)
				}
				originalFence, after, afterReceipt = evidence, snapshot(gitExcludePath), snapshot(gitExcludeReceiptPath)
				if fixture.draftProof {
					originalFence = writeDraftProof(interrupted)
				}
				checkSiblingBusy("acr prepare-exclusion")
				refuse(reconcile(originalFence.hash), "acr prepare-exclusion")
			}
			if err := PrepareGitExclusion(t.Context(), owner, authorize); err != nil {
				t.Fatal(err)
			}
			originalFence = snapshot(fenceName)
			var proof gitExclusionFence
			if err := json.Unmarshal(originalFence.content, &proof); err != nil || proof.SchemaVersion != gitExclusionFenceVersion ||
				proof.Retirement == "" || proof.Journal != pending.ID || proof.Project != recorded.Project || proof.Registration != recorded.Registration {
				t.Fatalf("preparation changed the transaction binding or omitted terminal proof: %v", err)
			}
			if fixture.draftProof && fixture.preparationCrash == "" {
				originalFence, after, afterReceipt = writeDraftProof(proof), snapshot(gitExcludePath), snapshot(gitExcludeReceiptPath)
				const diagnostic = "draft retirement proof cannot certify durable rollback"
				refuse(PrepareGitExclusion(t.Context(), owner, authorize), diagnostic)
				cleanup := filepath.Join(owner, transactionDirectory, ".staging-complete-"+pending.ID)
				writeFile(t, cleanup, "keep", "draft residue\n")
				refuse(PrepareGitExclusion(t.Context(), owner, authorize), diagnostic)
				if readFile(t, cleanup, "keep") != "draft residue\n" ||
					!matchesGitSnapshot(originalFence, snapshot(fenceName)) {
					t.Fatal("refused draft cleanup changed the residue or fence")
				}
				if err := os.RemoveAll(cleanup); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Dir(cleanup)); err != nil {
					t.Fatal(err)
				}
				checkSiblingBusy(diagnostic)
				git("worktree", "remove", owner)
				checkSiblingBusy(diagnostic)
				refusal := reconcile(originalFence.hash)
				refuse(refusal, diagnostic)
				refuse(refusal, originalFence.hash)
				return
			}
			if err := PrepareGitExclusion(t.Context(), owner, authorize); err != nil {
				t.Fatalf("prepared owner could not retry: %v", err)
			}
			after, afterReceipt = snapshot(gitExcludePath), snapshot(gitExcludeReceiptPath)
			for _, pair := range [][2]fileSnapshot{{before, after}, {beforeReceipt, afterReceipt}} {
				if pair[0].exists != pair[1].exists || pair[0].mode != pair[1].mode || !bytes.Equal(pair[0].content, pair[1].content) {
					t.Fatal("preparation did not restore exact shared before-images")
				}
			}
			if _, err := os.Stat(filepath.Join(owner, transactionDirectory)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owner preparation left transaction residue: %v", err)
			}
			if fixture.move {
				refuse(reconcile(originalFence.hash), "the recorded Git registration still exists")
				if got := readFile(t, owner, recoveredVendorPath(pending.ID, 0)+"/rules/fixture.md"); got != "fixture rule\n" {
					t.Fatalf("mixed v3 preparation did not preserve the vendor rollback: %q", got)
				}
				if got := readFile(t, owner, retiredVendor+"/rules/old.md"); got != "edited after retirement\n" {
					t.Fatalf("mixed v3 preparation lost edits made to the retired tree: %q", got)
				}
			} else {
				refuse(reconcile(originalFence.hash), "acr prepare-exclusion")
			}
			replacement := filepath.Join(repository, ".git", "spare-registration")
			if err := os.Mkdir(replacement, 0o700); err != nil {
				t.Fatal(err)
			}
			git("worktree", "remove", "--force", owner)
			if fixture.move {
				if err := os.WriteFile(recorded.Project.Path, []byte("unrelated file at original path"), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			checkSiblingBusy("acr reconcile-exclusion " + originalFence.hash)
			refuse(reconcile(contentHash([]byte("different fence"))), "no Git exclusion fence matches")
			if before.exists {
				writeFile(t, repository, gitExcludePath, "edited after preparation")
				refuse(reconcile(originalFence.hash), "does not match the fence before-state")
				refuse(apply(sibling), "does not match the fence before-state")
				restore(excludePath, before)
				if err := os.Chmod(excludePath, 0o600); err != nil {
					t.Fatal(err)
				}
				refuse(reconcile(originalFence.hash), "does not match the fence before-state")
				restore(excludePath, before)
			} else {
				writeFile(t, repository, gitExcludePath, "unrelated new exclusion file")
				refuse(reconcile(originalFence.hash), "does not match the fence before-state")
				restore(excludePath, before)
			}
			if beforeReceipt.exists {
				if err := os.Chmod(receiptPath, 0o640); err != nil {
					t.Fatal(err)
				}
				refuse(reconcile(originalFence.hash), "acr-exclusion-separator")
				restore(receiptPath, beforeReceipt)
			}
			if err := os.Rename(replacement, registration); err != nil {
				t.Fatal(err)
			}
			refuse(reconcile(originalFence.hash), "no longer matches the recorded owner")
			if err := os.Remove(registration); err != nil {
				t.Fatal(err)
			}
			if fixture.move {
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
				{bytes.Replace(originalFence.content, []byte(`"schema_version":2`), []byte(`"schema_version":4`), 1), "use an ACR reader that supports the recorded version"},
				{bytes.Replace(originalFence.content, []byte(`"journal":"`+pending.ID+`"`), []byte(`"journal":"different"`), 1), "invalid Git exclusion fence"},
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
			if fixture.move {
				t.Setenv(prepareChild, "")
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
			if fixture.move {
				if got, err := os.ReadFile(recorded.Project.Path); err != nil || string(got) != "unrelated file at original path" {
					t.Fatalf("retirement changed the replacement: %q, %v", got, err)
				}
			}
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
