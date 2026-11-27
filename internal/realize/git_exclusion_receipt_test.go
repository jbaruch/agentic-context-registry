package realize

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const receiptTestUserPattern = "keep-*.local"

func TestGitSeparatorReceiptDoesNotAdoptEditedGenerations(t *testing.T) {
	for _, edit := range []string{"unchanged", "replace-identical", "edit-restore-mtime", "missing", "malformed", "unsupported-version", "unversioned-draft"} {
		t.Run(edit, func(t *testing.T) {
			project := t.TempDir()
			initGit(t, project)
			writeFile(t, project, gitExcludePath, receiptTestUserPattern)
			exclude := filepath.Join(project, gitExcludePath)
			receipt := filepath.Join(project, gitExcludeReceiptPath)
			if err := os.Chmod(exclude, 0o640); err != nil {
				t.Fatal(err)
			}
			intent := testIntent(".cursor/rules/generated.mdc", "managed\n", OwnershipGenerated)
			empty := Ledger{SchemaVersion: CurrentLedgerSchemaVersion}
			finalize := func(Ledger) error { return nil }
			plan, err := NewEngine().Run(project, empty, []Intent{intent}, ModeApply, finalize)
			if err != nil {
				t.Fatal(err)
			}
			bound := readFile(t, project, gitExcludeReceiptPath)
			if !strings.Contains(bound, `"schema_version":1`) {
				t.Fatal("writer did not version the separator receipt")
			}
			existing := readFile(t, project, gitExcludePath)
			initial, err := os.Stat(exclude)
			if err != nil {
				t.Fatal(err)
			}
			switch edit {
			case "replace-identical":
				replacement := filepath.Join(project, ".git", "info", "replacement")
				if err := os.WriteFile(replacement, []byte(existing), 0o640); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(replacement, initial.ModTime(), initial.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, exclude); err != nil {
					t.Fatal(err)
				}
			case "edit-restore-mtime":
				if err := os.WriteFile(exclude, []byte("changed\n"), 0o640); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(exclude, []byte(existing), 0o640); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(exclude, initial.ModTime(), initial.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(receipt); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				// The otherwise valid record has an extra object member.
				writeFile(t, project, gitExcludeReceiptPath, strings.TrimSuffix(bound, "}")+`,"unexpected":1}`)
			case "unsupported-version":
				writeFile(t, project, gitExcludeReceiptPath, strings.Replace(bound, `"schema_version":1`, `"schema_version":9`, 1))
			case "unversioned-draft":
				writeFile(t, project, gitExcludeReceiptPath, strings.Replace(bound, `"schema_version":1,`, "", 1))
			}
			root, err := os.OpenRoot(project)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			before, err := snapshotFile(root, gitExcludePath)
			if err != nil {
				t.Fatal(err)
			}
			if afterEdit, err := os.Stat(exclude); err != nil || !initial.ModTime().Equal(afterEdit.ModTime()) ||
				os.SameFile(initial, afterEdit) != (edit != "replace-identical") || before.content == nil || string(before.content) != existing {
				t.Fatalf("invalid %s fixture: %v, %v", edit, afterEdit, err)
			}
			beforeReceipt, err := snapshotFile(root, gitExcludeReceiptPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, mode := range []Mode{ModeDryRun, ModeApply} {
				current, err := NewEngine().Run(project, plan.NextLedger, []Intent{intent}, mode, finalize)
				if err != nil {
					t.Fatal(err)
				}
				if edit == "unsupported-version" || edit == "unversioned-draft" {
					if len(current.TransactionNotes) != 1 || current.TransactionNotes[0].Code != "unsupported_git_exclusion_receipt_version" ||
						!strings.Contains(current.TransactionNotes[0].Message, "Use a reader") {
						t.Fatalf("%s did not report actionable unsupported receipt guidance: %+v", mode, current.TransactionNotes)
					}
				}
				after, err := snapshotFile(root, gitExcludePath)
				if err != nil || !matchesGitSnapshot(before, after) {
					t.Fatalf("%s changed info/exclude: %v", mode, err)
				}
				afterReceipt, err := snapshotFile(root, gitExcludeReceiptPath)
				if err != nil || !matchesGitSnapshot(beforeReceipt, afterReceipt) {
					t.Fatalf("%s changed or restamped the receipt: %v", mode, err)
				}
			}
			if _, err := NewEngine().Run(project, plan.NextLedger, nil, ModeApply, finalize); err != nil {
				t.Fatal(err)
			}
			want := receiptTestUserPattern
			if edit != "unchanged" {
				want += "\n"
			}
			if got := readFile(t, project, gitExcludePath); got != want {
				t.Fatalf("uninstall wrote %q, want %q", got, want)
			}
			if edit == "unsupported-version" || edit == "unversioned-draft" {
				after, err := snapshotFile(root, gitExcludeReceiptPath)
				if err != nil || !matchesGitSnapshot(beforeReceipt, after) {
					t.Fatalf("uninstall rewrote the unsupported receipt: %v", err)
				}
			}
			if mode, err := os.Stat(exclude); err != nil || mode.Mode().Perm() != 0o640 {
				t.Fatalf("uninstall changed mode: %v, %v", mode, err)
			}
		})
	}
}

func TestGitExclusionRecoveryRefusesReplacedAfterGeneration(t *testing.T) {
	project := t.TempDir()
	initGit(t, project)
	writeFile(t, project, gitExcludePath, receiptTestUserPattern)
	exclude := filepath.Join(project, gitExcludePath)
	originalHook := transactionRenameHook
	t.Cleanup(func() { transactionRenameHook = originalHook })
	var after []byte
	transactionRenameHook = func(name string) {
		if name != gitExcludePath {
			return
		}
		var err error
		after, err = os.ReadFile(exclude)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(exclude)
		if err != nil {
			t.Fatal(err)
		}
		replacement := filepath.Join(project, ".git", "info", "replacement")
		if err := os.WriteFile(replacement, after, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, exclude); err != nil {
			t.Fatal(err)
		}
	}
	intent := testIntent(".cursor/rules/generated.mdc", "managed\n", OwnershipGenerated)
	_, err := NewEngine().Run(project, Ledger{SchemaVersion: CurrentLedgerSchemaVersion}, []Intent{intent}, ModeApply, func(Ledger) error { return nil })
	var incomplete *IncompleteRecoveryError
	if !errors.As(err, &incomplete) || !strings.Contains(incomplete.RecoveryErr.Error(), "different file generation") {
		t.Fatalf("apply and automatic rollback = %v", err)
	}
	if string(after) != readFile(t, project, gitExcludePath) {
		t.Fatal("recovery overwrote the replacement")
	}
	if _, err := os.Lstat(filepath.Join(project, gitExcludeReceiptPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery bound the mismatched generation: %v", err)
	}
}

func TestGitExclusionRefusesChangedReceiptAtApply(t *testing.T) {
	project := t.TempDir()
	initGit(t, project)
	writeFile(t, project, gitExcludePath, receiptTestUserPattern)
	intent := testIntent(".cursor/rules/generated.mdc", "managed\n", OwnershipGenerated)
	empty := Ledger{SchemaVersion: CurrentLedgerSchemaVersion}
	seed, err := NewEngine().Run(project, empty, []Intent{intent}, ModeApply, func(Ledger) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	original := readFile(t, project, gitExcludePath)
	receipt := readFile(t, project, gitExcludeReceiptPath)
	var conflict *FileTransactionConflictError
	err = ApplyFileTransactionWithGitExclusion(t.Context(), project, empty, func(Ledger) ([]FileTransactionEdit, error) {
		return nil, nil
	}, nil, FileTransactionHooks{BeforeEdit: func(_ int, edit FileTransactionEdit) error {
		if edit.GitExclusion {
			replacement := filepath.Join(project, ".git", "info", "replacement")
			if err := os.WriteFile(replacement, []byte(receipt), 0o600); err != nil {
				return err
			}
			return os.Rename(replacement, filepath.Join(project, gitExcludeReceiptPath))
		}
		return nil
	}})
	if !errors.As(err, &conflict) || !strings.Contains(err.Error(), "changed after planning") {
		t.Fatalf("finalization changed receipt check = %v", err)
	}
	if got := readFile(t, project, gitExcludePath); got != original {
		t.Fatalf("finalization changed the exclusion despite receipt replacement: %q", got)
	}
	if _, err := NewEngine().Run(project, seed.NextLedger, nil, ModeApply, func(Ledger) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, project, gitExcludePath); got != receiptTestUserPattern {
		t.Fatalf("retry did not retire the original valid separator: %q", got)
	}
}
