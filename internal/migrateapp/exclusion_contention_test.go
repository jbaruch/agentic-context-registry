package migrateapp

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestFinalizeLinkedWorktreeExclusionRetirement(t *testing.T) {
	const child = "ACR_TEST_FINALIZE_RETIREMENT_OWNER"
	const exclude = ".git/info/exclude"
	const fence = ".git/info/acr-exclusion-journal"
	if owner := os.Getenv(child); owner != "" {
		setFinalizationHooks(t, realize.FileTransactionHooks{AfterEdit: func(_ int, edit realize.FileTransactionEdit) error {
			if edit.GitExclusion {
				os.Exit(85)
			}
			return nil
		}})
		_, err := finalize(t, owner, false)
		t.Fatalf("finalization did not stop after the shared exclusion edit: %v", err)
	}

	repository := t.TempDir()
	fixture := writeCodexRuleConsumerWithGit(t, "")
	if err := os.Rename(filepath.Join(fixture, ".git"), filepath.Join(repository, ".git")); err != nil {
		t.Fatal(err)
	}
	runGitFixture(t, repository, "commit", "--allow-empty", "-qm", "seed")
	worktree := filepath.Join(t.TempDir(), "owner")
	runGitFixture(t, repository, "worktree", "add", "--detach", "-q", worktree)
	owner := filepath.Join(worktree, "consumer")
	if err := os.Rename(fixture, owner); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, repository, exclude, "keep-*.local")
	coexist(t, owner)
	writeProjectFile(t, owner, "AGENTS.md", readProjectFile(t, owner, "AGENTS.md")+
		"## Agent Rules <!-- tessl-managed -->\n\n@.tessl/RULES.md follow the [instructions](.tessl/RULES.md)\n")
	coexist(t, owner)
	runGitFixture(t, owner, "add", "tessl.json", ".tessl", ".agents", "agents.yaml")
	runGitFixture(t, owner, "commit", "-qm", "recovery inputs")
	before := hashTreeWithModes(t, owner)
	beforeExclude := readProjectFile(t, repository, exclude)
	crash := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestFinalizeLinkedWorktreeExclusionRetirement$")
	crash.Env = append(crash.Environ(), child+"="+owner)
	var exited *exec.ExitError
	if out, err := crash.CombinedOutput(); !errors.As(err, &exited) || exited.ExitCode() != 85 {
		t.Fatalf("finalization did not interrupt the transaction: %v\n%s", err, out)
	}
	if got := readProjectFile(t, repository, exclude); !strings.Contains(got, "/consumer/AGENTS.md") {
		t.Fatalf("finalization did not change the live exclusion: %q", got)
	}
	if err := realize.PrepareGitExclusion(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	if !mapsEqual(before, hashTreeWithModes(t, owner)) {
		t.Fatal("owner preparation did not restore the pre-finalization project")
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(readProjectFile(t, repository, fence))))
	runGitFixture(t, repository, "worktree", "remove", "--force", worktree)
	if err := realize.ReconcileGitExclusion(t.Context(), repository, digest); err != nil {
		t.Fatal(err)
	}
	if got := readProjectFile(t, repository, exclude); got != beforeExclude {
		t.Fatalf("preparation and reconciliation did not preserve the exclusion before-image: %q", got)
	}
	if _, err := os.Stat(filepath.Join(repository, fence)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reconciliation left the prepared fence: %v", err)
	}
}

func TestFinalizationRollbackPreservesSiblingRealizationExclusions(t *testing.T) {
	worktree := t.TempDir()
	alpha, beta := filepath.Join(worktree, "alpha"), filepath.Join(worktree, "beta")
	fixture := writeCodexRuleConsumerWithGit(t, "")
	if err := os.Rename(fixture, beta); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(beta, ".git"), filepath.Join(worktree, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(alpha, 0o755); err != nil {
		t.Fatal(err)
	}
	const userPattern = "keep-*.local"
	const excludePath = ".git/info/exclude"
	writeProjectFile(t, worktree, excludePath, userPattern)

	coexist(t, beta)
	writeProjectFile(t, beta, "AGENTS.md", readProjectFile(t, beta, "AGENTS.md")+
		"## Agent Rules <!-- tessl-managed -->\n\n@.tessl/RULES.md follow the [instructions](.tessl/RULES.md)\n")
	coexist(t, beta)
	runGitFixture(t, beta, "add", "tessl.json", ".tessl", ".agents", "agents.yaml")
	runGitFixture(t, beta, "commit", "-qm", "recovery inputs")

	state := dependency.State{
		Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion},
		Lock:    dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion},
	}
	if err := dependency.WriteState(alpha, state); err != nil {
		t.Fatal(err)
	}
	runAlpha := func(whileClaimed func(), paths ...string) error {
		state, err := dependency.LoadState(alpha)
		if err != nil {
			return err
		}
		ledger, err := realize.DecodeLedger(state.Lock.Realization)
		if err != nil {
			return err
		}
		before, err := os.ReadFile(filepath.Join(alpha, dependency.LockFilename))
		if err != nil {
			return err
		}
		var intents []realize.Intent
		for _, path := range paths {
			content := []byte("Generated by the realization fixture.\n")
			intents = append(intents, realize.Intent{
				Path: path, Content: content, Ownership: realize.OwnershipGenerated,
				Entries: []realize.Entry{{
					Source: "github:example/fixture", ArtifactID: "rule", ArtifactKind: realize.ArtifactFile,
					SourcePath: "rules/source.md", Adapter: "cursor", AdapterVersion: "1.0.0",
					ManagedHash: migrate.HashFinalizationContent(content),
				}},
			})
		}
		_, err = realize.NewEngine().RunStateFiles(t.Context(), alpha, ledger, realize.Ledger{}, intents, realize.ModeApply, func(next realize.Ledger) ([]realize.StateFile, error) {
			if whileClaimed != nil {
				whileClaimed()
			}
			encoded, err := realize.EncodeLedger(next)
			if err != nil {
				return nil, err
			}
			state.Lock.Realization = encoded
			_, lock, err := dependency.MarshalState(state)
			return []realize.StateFile{{
				Path: dependency.LockFilename, Content: lock, Mode: 0o644,
				BeforeExists: true, BeforeContent: before, BeforeMode: 0o644,
			}}, err
		})
		return err
	}
	const existing = ".cursor/rules/existing.mdc"
	const added = ".cursor/rules/added.mdc"
	if err := runAlpha(nil, existing); err != nil {
		t.Fatal(err)
	}
	beforeExclude := readProjectFile(t, worktree, excludePath)
	beforeAlpha := readProjectFile(t, alpha, dependency.LockFilename)
	beforeBeta := readProjectFile(t, beta, dependency.LockFilename)
	injected := errors.New("fail after updating the shared exclusion")
	reached := false
	t.Run("contend while finalization holds the claim", func(t *testing.T) {
		setFinalizationHooks(t, realize.FileTransactionHooks{AfterEdit: func(_ int, edit realize.FileTransactionEdit) error {
			if !edit.GitExclusion {
				return nil
			}
			reached = true
			if got := readProjectFile(t, worktree, excludePath); !strings.Contains(got, "/beta/AGENTS.md") {
				t.Errorf("no live finalization exclusion to roll back: %q", got)
			}
			// Enter the sibling while this transaction is paused after the
			// shared rename and before its journal can be retired.
			var busy *realize.TransactionBusyError
			if err := runAlpha(nil, existing, added); !errors.As(err, &busy) {
				t.Errorf("contending realize = %v, want retryable transaction_busy", err)
			}
			return injected
		}})
		report, err := finalize(t, beta, false)
		if !reached || !errors.Is(err, injected) || len(report.Reanchored) != 0 {
			t.Fatalf("failed finalization = %v, reanchored=%+v, reached=%v", err, report.Reanchored, reached)
		}
	})

	if got := readProjectFile(t, worktree, excludePath); got != beforeExclude {
		t.Fatalf("rollback changed committed sibling exclusion or user bytes:\nhave %q\nwant %q", got, beforeExclude)
	}
	if got := readProjectFile(t, alpha, dependency.LockFilename); got != beforeAlpha {
		t.Fatal("refused realize changed its ledger")
	}
	if got := readProjectFile(t, beta, dependency.LockFilename); got != beforeBeta {
		t.Fatal("rolled-back finalization changed its ledger")
	}
	for _, project := range []string{alpha, beta} {
		if _, err := os.Stat(filepath.Join(project, ".agents/.acr-transactions")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left a journal: %v", project, err)
		}
	}
	if _, err := os.Stat(filepath.Join(alpha, added)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused realization wrote its new output: %v", err)
	}

	if err := runAlpha(func() {
		report, err := finalize(t, beta, false)
		var busy *realize.TransactionBusyError
		if !errors.As(err, &busy) || report.FinalizationReady || len(report.Reanchored) != 0 {
			t.Errorf("contending finalization = %v, ready=%v, reanchored=%+v", err, report.FinalizationReady, report.Reanchored)
		}
		if got := readProjectFile(t, beta, dependency.LockFilename); got != beforeBeta {
			t.Error("contending finalization changed its ledger")
		}
	}, existing, added); err != nil {
		t.Fatalf("realize retry: %v", err)
	}
	if _, err := finalize(t, beta, false); err != nil {
		t.Fatalf("finalize retry: %v", err)
	}
	for path, root := range map[string]string{existing: alpha, added: alpha, "AGENTS.md": beta} {
		excluded := false
		for _, target := range finalizeLedger(t, root).Targets {
			if target.Path == path {
				excluded = target.Excluded
			}
		}
		if !excluded {
			t.Errorf("ledger for %s does not mark %s excluded", root, path)
		}
		runGitFixture(t, root, "check-ignore", "-q", "--", path)
	}
	runGitFixture(t, worktree, "check-ignore", "-q", "--", "keep-notes.local")
	// Remove both blocks after rollback and retry. The original separator
	// must still be owned by the same shared file, not by either project.
	empty := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}
	if err := realize.ApplyFileTransactionWithGitExclusion(t.Context(), beta, empty, func(realize.Ledger) ([]realize.FileTransactionEdit, error) {
		return nil, nil
	}, nil, realize.FileTransactionHooks{}); err != nil {
		t.Fatal(err)
	}
	if err := runAlpha(nil); err != nil {
		t.Fatal(err)
	}
	if got := readProjectFile(t, worktree, excludePath); got != userPattern {
		t.Fatalf("cleanup after finalization rollback = %q, want %q", got, userPattern)
	}
}

func TestLinkedWorktreeExclusionOwnershipAcrossRealizeAndFinalize(t *testing.T) {
	for _, prefix := range []string{"", "consumer/"} {
		t.Run("scope="+prefix, func(t *testing.T) {
			repository := t.TempDir()
			runGitFixture(t, repository, "init", "-q")
			runGitFixture(t, repository, "commit", "-q", "--allow-empty", "-m", "seed")
			worktrees := t.TempDir()
			alpha, beta := filepath.Join(worktrees, "alpha"), filepath.Join(worktrees, "beta")
			for _, root := range []string{alpha, beta} {
				runGitFixture(t, repository, "worktree", "add", "--detach", "-q", root)
			}
			alpha, beta = filepath.Join(alpha, prefix), filepath.Join(beta, prefix)
			for _, root := range []string{alpha, beta} {
				if err := os.MkdirAll(root, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			const exclude = ".git/info/exclude"
			const user = "# user comment\r\n*.local"
			writeProjectFile(t, repository, exclude, user)
			if err := os.Chmod(filepath.Join(repository, exclude), 0o640); err != nil {
				t.Fatal(err)
			}
			empty := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}
			engine := realize.NewEngine()
			generate := func(root, path string) realize.Ledger {
				t.Helper()
				content := []byte("Generated by ACR.\n")
				intent := realize.Intent{Path: path, Content: content, Ownership: realize.OwnershipGenerated,
					Entries: []realize.Entry{{
						Source: "github:example/fixture", ArtifactID: "rule", ArtifactKind: realize.ArtifactFile,
						SourcePath: "rules/source.md", Adapter: "cursor", AdapterVersion: "1.0.0",
						ManagedHash: migrate.HashFinalizationContent(content),
					}},
				}
				var applied realize.Ledger
				_, err := engine.Run(root, empty, []realize.Intent{intent}, realize.ModeApply, func(next realize.Ledger) error {
					applied = next
					return nil
				})
				if err != nil || len(applied.Targets) != 1 || !applied.Targets[0].Excluded {
					t.Fatalf("realize %s: ledger = %+v, error = %v", root, applied, err)
				}
				return applied
			}
			const alphaPath, betaPath = "alpha.mdc", "beta.mdc"
			alphaLedger := generate(alpha, alphaPath)
			alphaOnly := readProjectFile(t, repository, exclude)
			betaLedger := generate(beta, betaPath)
			for _, root := range []string{alpha, beta} {
				// Git applies the union in both worktrees, irrespective of
				// which worktree has the actual file.
				for _, path := range []string{alphaPath, betaPath} {
					runGitFixture(t, root, "check-ignore", "-q", "--", path)
				}
			}
			both := readProjectFile(t, repository, exclude)
			if !strings.HasPrefix(both, alphaOnly) || !strings.Contains(both, "/"+prefix+betaPath+"\n") {
				t.Fatalf("B changed A's block or wrote an unanchored pattern: %q", both)
			}
			remove := func(root string, ledger realize.Ledger) {
				t.Helper()
				if _, err := engine.Run(root, ledger, nil, realize.ModeApply, func(realize.Ledger) error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			remove(beta, betaLedger)
			if got := readProjectFile(t, repository, exclude); got != alphaOnly {
				t.Fatalf("empty realization in B changed A's bytes: %q", got)
			}
			runGitFixture(t, alpha, "check-ignore", "-q", "--", alphaPath)

			state := dependency.State{
				Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion},
				Lock:    dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion},
			}
			if err := dependency.WriteState(beta, state); err != nil {
				t.Fatal(err)
			}
			for _, phase := range []struct {
				ledger realize.Ledger
				before string
				after  string
			}{
				{betaLedger, alphaOnly, both},
				{empty, both, alphaOnly},
			} {
				if len(phase.ledger.Targets) != 0 {
					phase.ledger.Targets[0].Excluded = false
					writeProjectFile(t, beta, betaPath, "Generated by ACR.\n")
				}
				encoded, err := realize.EncodeLedger(phase.ledger)
				if err != nil {
					t.Fatal(err)
				}
				state.Lock.Realization = encoded
				if _, edits, note, err := plannedReanchors(t.Context(), beta, phase.ledger, migrate.FinalizePlan{}); err != nil || !edits || note != nil {
					t.Fatalf("finalization preview: edit = %v, note = %+v, error = %v", edits, note, err)
				}
				if got := readProjectFile(t, repository, exclude); got != phase.before {
					t.Fatalf("finalization preview wrote exclusions: %q", got)
				}
				if _, err := applyFinalization(t.Context(), beta, &state, migrate.FinalizePlan{}); err != nil {
					t.Fatal(err)
				}
				if got := readProjectFile(t, repository, exclude); got != phase.after {
					t.Fatalf("B's finalization wrote %q, want %q", got, phase.after)
				}
				if len(phase.ledger.Targets) != 0 {
					if got := finalizeLedger(t, beta); len(got.Targets) != 1 || !got.Targets[0].Excluded {
						t.Fatalf("finalization failed to persist exclusion: %+v", got)
					}
					runGitFixture(t, beta, "check-ignore", "-q", "--", betaPath)
				}
				runGitFixture(t, alpha, "check-ignore", "-q", "--", alphaPath)
			}
			remove(alpha, alphaLedger)
			if got := readProjectFile(t, repository, exclude); got != user {
				t.Fatalf("exclusion round trip = %q, want %q", got, user)
			}
			info, err := os.Stat(filepath.Join(repository, exclude))
			if err != nil || info.Mode().Perm() != 0o640 {
				t.Fatalf("wrong final exclusion permissions: %v, %v", info, err)
			}
		})
	}
}
