package realize

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func skipWhenPermissionsDoNotBind(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind the superuser")
	}
}

func TestApplyPlanRemovesFileWithoutPruningDirectories(t *testing.T) {
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
	err := applyJournaledTestPlan(project, plan, func(Ledger) error { finalized = true; return nil })
	if err != nil {
		t.Fatalf("applyPlanJournaled: %v", err)
	}
	for _, directory := range []string{locked, filepath.Dir(target)} {
		if info, err := os.Stat(directory); err != nil || !info.IsDir() {
			t.Fatalf("retained directory %s = %v, %v", directory, info, err)
		}
	}
	if info, err := os.Stat(locked); err != nil || info.Mode().Perm() != 0o555 {
		t.Fatalf("ancestor mode = %v, %v; want 0555", info, err)
	}
	if !finalized {
		t.Fatal("the ledger was not finalized")
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
	if err := RecoverTransactions(t.Context(), project); err != nil {
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
