package realize

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPruneEmptyParentsReturnsLstatFailure(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	err = pruneEmptyParents(root, ".agent/a/file.md")
	if !errors.Is(err, os.ErrClosed) || !strings.Contains(err.Error(), ".agent/a") {
		t.Fatalf("pruneEmptyParents error = %v, want the Lstat failure and directory", err)
	}
}

func skipWhenPermissionsDoNotBind(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind the superuser")
	}
}

func TestPruneEmptyParentsStopsOnExpectedResults(t *testing.T) {
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".agent", "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".agent", "a", "keep.md"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// b is empty and is pruned; a still holds keep.md, which ends the walk
	// without an error; .agent is top-level and never removed.
	if err := pruneEmptyParents(root, ".agent/a/b/file.md"); err != nil {
		t.Fatalf("not-empty ended the walk with %v", err)
	}
	if _, err := os.Lstat(filepath.Join(project, ".agent", "a", "b")); !os.IsNotExist(err) {
		t.Fatalf("emptied directory remains: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(project, ".agent", "a")); err != nil {
		t.Fatalf("non-empty directory was removed: %v", err)
	}
	if err := pruneEmptyParents(root, ".agent/gone/deeper/file.md"); err != nil {
		t.Fatalf("a directory that is already gone ended the walk with %v", err)
	}
}

func TestPruneEmptyParentsReturnsAFailureToRemove(t *testing.T) {
	skipWhenPermissionsDoNotBind(t)
	project := t.TempDir()
	locked := filepath.Join(project, ".agent", "a")
	if err := os.MkdirAll(filepath.Join(locked, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0o755); err != nil {
			t.Error(err)
		}
	})
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	err = pruneEmptyParents(root, ".agent/a/b/file.md")
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), ".agent/a/b") {
		t.Fatalf("pruneEmptyParents error = %v, want the failed directory named", err)
	}
}

// TestApplyPlanRollsBackWhenPruningFails proves a prune failure other than
// not-empty or not-exist rolls the whole transaction back instead of finalizing
// the ledger over a directory the removal was meant to clear.
func TestApplyPlanRollsBackWhenPruningFails(t *testing.T) {
	skipWhenPermissionsDoNotBind(t)
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, filepath.FromSlash(transactionDirectory)), 0o755); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(project, ".agent", "a")
	target := filepath.Join(locked, "b", "file.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("generated\n")
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0o755); err != nil {
			t.Error(err)
		}
	})
	plan := Plan{Operations: []Operation{{
		Kind: OperationRemove, Path: ".agent/a/b/file.md", BeforeHash: contentHash(content),
		remove: true, beforeExists: true, beforeMode: 0o644,
	}}}
	finalized := false
	err := applyPlanJournaled(project, plan, func(Ledger) error { finalized = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "prune directories emptied by removing") {
		t.Fatalf("applyPlanJournaled error = %v, want the prune failure", err)
	}
	if finalized {
		t.Fatal("the ledger was finalized after a failed prune")
	}
	if restored, readErr := os.ReadFile(target); readErr != nil || string(restored) != string(content) {
		t.Fatalf("removed file after rollback = %q, %v", restored, readErr)
	}
	if notes, err := inspectTransactions(project); err != nil || len(notes) != 0 {
		t.Fatalf("transactions after rollback = %#v, %v", notes, err)
	}
}
