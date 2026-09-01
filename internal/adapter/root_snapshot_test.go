package adapter

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestRootSnapshotReadsRegularFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "AGENTS.md", "hello\n")
	snapshot, err := NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	observed, err := snapshot.ReadFile("AGENTS.md")
	if err != nil || string(observed.Content) != "hello\n" || observed.Hash != hashContent([]byte("hello\n")) {
		t.Fatalf("ReadFile() = %#v, %v", observed, err)
	}
}

func TestRootSnapshotReportsMissingAsNotExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	snapshot, err := NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	_, err = snapshot.ReadFile("missing.md")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadFile(missing) error = %v, want fs.ErrNotExist", err)
	}
}

func TestRootSnapshotRejectsSymlinkTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, outside, "secret.txt", "outside bytes\n")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "link.md")); err != nil {
		t.Fatalf("create symlink on supported platform: %v", err)
	}
	snapshot, err := NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	_, err = snapshot.ReadFile("link.md")
	if err == nil {
		t.Fatal("ReadFile(symlink) succeeded, want rejection")
	}
}

func TestRootSnapshotRejectsSymlinkedParentEscapingRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, outside, "escaped.md", "outside bytes\n")
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatalf("create symlink on supported platform: %v", err)
	}
	snapshot, err := NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	_, err = snapshot.ReadFile("link/escaped.md")
	if err == nil {
		t.Fatal("ReadFile through a symlinked parent succeeded, want rejection")
	}
}

func TestRootSnapshotRejectsParentTraversal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, outside, "secret.txt", "outside bytes\n")
	snapshot, err := NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	relative, err := filepath.Rel(dir, filepath.Join(outside, "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = snapshot.ReadFile(relative)
	if err == nil {
		t.Fatalf("ReadFile(%q) succeeded, want rejection of parent traversal", relative)
	}
}

func TestRootSnapshotRejectsAbsolutePath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outsideFile := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	_, err = snapshot.ReadFile(outsideFile)
	if err == nil {
		t.Fatal("ReadFile(absolute path) succeeded, want rejection")
	}
}

func TestRootSnapshotRejectsSpecialFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// CI targets only linux/darwin (see .github/workflows/ci.yml); both
	// support syscall.Mkfifo.
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo unavailable on this platform/runner: %v", err)
	}
	snapshot, err := NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	_, err = snapshot.ReadFile("pipe")
	if err == nil {
		t.Fatal("ReadFile(named pipe) succeeded, want rejection of a non-regular file")
	}
}

func TestRootSnapshotRejectsOversizedRead(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	oversized := make([]byte, maxSnapshotBytes+1)
	if err := os.WriteFile(filepath.Join(dir, "big.md"), oversized, 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	_, err = snapshot.ReadFile("big.md")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("ReadFile(oversized) error = %v, want a size-limit rejection", err)
	}
}
