package realize

import (
	"errors"
	"os"
	"os/exec"
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

// A pruning failure is visible, but cannot roll back committed file and state
// changes or recreate previously pruned directories with broader permissions.
func TestApplyPlanReportsPostCommitPruningFailure(t *testing.T) {
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
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "realization committed") || !strings.Contains(err.Error(), ".agent/a/b") || !strings.Contains(err.Error(), "remove the empty directories and retry") {
		t.Fatalf("applyPlanJournaled error = %v, want the prune failure", err)
	}
	if !finalized {
		t.Fatal("the ledger was not finalized before pruning")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed removal was undone: %v", err)
	}
	if notes, err := inspectTransactions(project); err != nil || len(notes) != 0 {
		t.Fatalf("transactions after commit = %#v, %v", notes, err)
	}
}

func TestDroppedFileRemovalRecoveryPreservesPrivateDirectories(t *testing.T) {
	const target = ".claude/scripts/private/check.sh"
	const lock = ".agents/registry.lock"
	content := []byte("#!/bin/sh\nexit 0\n")
	beforeLock := []byte("previous ledger\n")
	afterLock := []byte("next ledger\n")
	if project := os.Getenv("ACR_TEST_DROPPED_RECOVERY_PROJECT"); project != "" {
		transactionRenameHook = func(filename string) {
			if filename == lock {
				os.Exit(86)
			}
		}
		if err := applyJournaledTestPlan(project, Plan{Operations: []Operation{
			{Kind: OperationRemove, Path: target, BeforeHash: contentHash(content), remove: true, beforeExists: true, beforeMode: 0o640},
			{Kind: OperationUpdate, Path: lock, BeforeHash: contentHash(beforeLock), AfterHash: contentHash(afterLock), content: afterLock, Mode: 0o644, beforeExists: true, beforeMode: 0o600, stateFile: true},
		}}, nil); err != nil {
			t.Fatal(err)
		}
		os.Exit(87)
	}
	project := t.TempDir()
	filename := filepath.Join(project, filepath.FromSlash(target))
	directory := filepath.Dir(filename)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, content, 0o640); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(project, filepath.FromSlash(lock))
	if err := os.WriteFile(lockPath, beforeLock, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestDroppedFileRemovalRecoveryPreservesPrivateDirectories$")
	command.Env = append(os.Environ(), "ACR_TEST_DROPPED_RECOVERY_PROJECT="+project)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("interrupted child = %v: %s", err, output)
	}
	if _, err := os.Stat(filename); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child did not remove the file: %v", err)
	}
	if got, err := os.ReadFile(lockPath); err != nil || string(got) != string(afterLock) {
		t.Fatalf("child did not apply the later lock write: %q, %v", got, err)
	}
	if err := RecoverTransactions(project); err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		path    string
		content []byte
		mode    os.FileMode
	}{{filename, content, 0o640}, {lockPath, beforeLock, 0o600}} {
		if got, err := os.ReadFile(file.path); err != nil || string(got) != string(file.content) {
			t.Fatalf("restored %s = %q, %v", file.path, got, err)
		}
		if info, err := os.Stat(file.path); err != nil || info.Mode().Perm() != file.mode {
			t.Fatalf("restored mode for %s = %v, %v", file.path, info, err)
		}
	}
	if info, err := os.Stat(directory); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private directory after recovery = %v, %v; want 0700", info, err)
	}
}
