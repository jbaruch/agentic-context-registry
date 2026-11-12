package adapter

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type followingSymlinkFS struct{}

func (followingSymlinkFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

func (followingSymlinkFS) ReadDir(string) ([]fs.DirEntry, error) {
	return []fs.DirEntry{followingSymlinkEntry{}}, nil
}

type followingSymlinkEntry struct{}

func (followingSymlinkEntry) Name() string               { return "link.md" }
func (followingSymlinkEntry) IsDir() bool                { return false }
func (followingSymlinkEntry) Type() fs.FileMode          { return fs.ModeSymlink }
func (followingSymlinkEntry) Info() (fs.FileInfo, error) { return followedFileInfo{}, nil }

type followedFileInfo struct{}

func (followedFileInfo) Name() string       { return "target.md" }
func (followedFileInfo) Size() int64        { return 0 }
func (followedFileInfo) Mode() fs.FileMode  { return 0o644 }
func (followedFileInfo) ModTime() time.Time { return time.Time{} }
func (followedFileInfo) IsDir() bool        { return false }
func (followedFileInfo) Sys() any           { return nil }

func TestFSSnapshotReadDirPreservesSymlinkType(t *testing.T) {
	t.Parallel()

	entries, err := NewFSSnapshot(followingSymlinkFS{}).ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Mode&fs.ModeSymlink == 0 {
		t.Fatalf("ReadDir() = %#v, want one symlink entry", entries)
	}
}

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

// A directory is deterministic, portable evidence that ReadFile rejects any
// non-regular leaf, not only symlinks: no platform-specific special-file
// creation, so nothing to skip.
func TestRootSnapshotRejectsDirectoryLeaf(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	_, err = snapshot.ReadFile("subdir")
	if err == nil {
		t.Fatal("ReadFile(directory) succeeded, want rejection of a non-regular file")
	}
}

func TestRootSnapshotRejectsInRootSymlinkedParent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "real/AGENTS.md", "hello\n")
	// The symlink target is relative and root-contained ("real"), unlike
	// TestRootSnapshotRejectsSymlinkedParentEscapingRoot; os.Root rejects an
	// absolute symlink target outright, so only a relative in-root target
	// actually exercises the followed-in-root-parent rejection this test
	// names.
	if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
		t.Fatalf("create symlink on supported platform: %v", err)
	}
	snapshot, err := NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	_, err = snapshot.ReadFile("link/AGENTS.md")
	if err == nil {
		t.Fatal("ReadFile through an in-root symlinked parent succeeded, want rejection")
	}
}

// TestRootSnapshotDetectsReplacementBetweenOpenAndRead deterministically
// exercises the os.SameFile binding: readFile's afterOpen seam simulates a
// concurrent replacement (remove + recreate under the same name, giving the
// new file a different inode) at the exact point between Open and the
// pre-read Lstat, so this never depends on winning a real race.
func TestRootSnapshotDetectsReplacementBetweenOpenAndRead(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "target.md", "original\n")
	snapshot, err := NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	replaced := false
	_, err = snapshot.readFile("target.md", func() {
		replaced = true
		target := filepath.Join(dir, "target.md")
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("replaced\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if !replaced {
		t.Fatal("afterOpen hook never ran")
	}
	if err == nil {
		t.Fatal("readFile did not detect a mid-flight replacement, want rejection")
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

func TestInstructionSymlinkAliasAcceptsInProjectRegularFile(t *testing.T) {
	t.Parallel()

	t.Run("relative AGENTS.md", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "AGENTS.md", "# Agents\n")
		if err := os.Symlink("AGENTS.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		snapshot := instructionSnapshot(t, dir)
		assertInstructionAlias(t, snapshot, "AGENTS.md")
		evidence, err := ExistingEvidence(snapshot, []string{"CLAUDE.md", "AGENTS.md"}, nil)
		if err != nil || strings.Join(evidence, ",") != "AGENTS.md,CLAUDE.md" {
			t.Fatalf("evidence = %#v, %v", evidence, err)
		}
	})

	t.Run("dot slash and nested file", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "docs/guide.md", "# Guide\n")
		if err := os.Symlink("./docs/guide.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertInstructionAlias(t, instructionSnapshot(t, dir), "docs/guide.md")
	})

	t.Run("real directory then dotdot", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "AGENTS.md", "# Agents\n")
		if err := os.Mkdir(filepath.Join(dir, "docs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("docs/../AGENTS.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertInstructionAlias(t, instructionSnapshot(t, dir), "AGENTS.md")
	})

	t.Run("chain through an in-project directory symlink", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "notes/guide.md", "# Guide\n")
		if err := os.Symlink("notes", filepath.Join(dir, "dirlink")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("dirlink/guide.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertInstructionAlias(t, instructionSnapshot(t, dir), "notes/guide.md")
	})

	t.Run("two-symlink chain", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "AGENTS.md", "# Agents\n")
		if err := os.Symlink("AGENTS.md", filepath.Join(dir, "mid")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("mid", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertInstructionAlias(t, instructionSnapshot(t, dir), "AGENTS.md")
	})

	t.Run("absolute path inside the project", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "AGENTS.md", "# Agents\n")
		if err := os.Symlink(filepath.Join(dir, "AGENTS.md"), filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertInstructionAlias(t, instructionSnapshot(t, dir), "AGENTS.md")
	})

	t.Run("absolute path through the other spelling of the project", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		real := filepath.Join(parent, "real")
		if err := os.Mkdir(real, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, real, "AGENTS.md", "# Agents\n")
		via := filepath.Join(parent, "via-link")
		if err := os.Symlink(real, via); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(real, "AGENTS.md"), filepath.Join(via, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertInstructionAlias(t, instructionSnapshot(t, via), "AGENTS.md")
	})

	t.Run("forty hops still resolves", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "AGENTS.md", "# Agents\n")
		linkChain(t, dir, 40)
		assertInstructionAlias(t, instructionSnapshot(t, dir), "AGENTS.md")
	})
}

func TestInstructionSymlinkRefusalsNameTheResolvedTarget(t *testing.T) {
	t.Parallel()

	t.Run("dangling", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Symlink("missing.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", "missing.md", "")
	})

	t.Run("relative escape", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Symlink("../secret.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", "../secret.md", "")
	})

	t.Run("absolute escape", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		outside := filepath.Join(t.TempDir(), "secret.md")
		if err := os.WriteFile(outside, []byte("SECRET-BYTES"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", outside, "SECRET-BYTES")
	})

	t.Run("directory symlink escape does not lexical-clean back inside", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "AGENTS.md", "# Agents\n")
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "file.md"), []byte("SECRET-BYTES"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "linkdir")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("linkdir/file.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", outside+"/file.md", "SECRET-BYTES")
	})

	t.Run("shortcut dotdot does not drop the outside symlink", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "AGENTS.md", "# Agents\n")
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("SECRET-BYTES"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "shortcut")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("shortcut/../AGENTS.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		_, alias, err := readInstructionCandidate(instructionSnapshot(t, dir), "CLAUDE.md")
		if err == nil || alias == "AGENTS.md" || strings.Contains(err.Error(), "SECRET-BYTES") {
			t.Fatalf("alias %q err %v, want a refusal that does not accept AGENTS.md", alias, err)
		}
	})

	t.Run("chain to a directory", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("subdir", filepath.Join(dir, "mid")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("mid", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", "subdir", "")
	})

	t.Run("fifo", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("pipe", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", "pipe", "")
	})

	t.Run("cycle and self-cycle", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Symlink("other", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("CLAUDE.md", filepath.Join(dir, "other")); err != nil {
			t.Fatal(err)
		}
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", "CLAUDE.md", "")

		self := t.TempDir()
		if err := os.Symlink("CLAUDE.md", filepath.Join(self, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertSymlinkRefusal(t, instructionSnapshot(t, self), "CLAUDE.md", "CLAUDE.md", "")
	})

	t.Run("hop limit", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "AGENTS.md", "# Agents\n")
		linkChain(t, dir, 41)
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", "hop-40", "")
	})

	t.Run("other instruction hosts stay refused", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "notes.md", "# Notes\n")
		if err := os.Symlink("notes.md", filepath.Join(dir, "AGENTS.md")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../notes.md", filepath.Join(dir, ".claude", "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		snapshot := instructionSnapshot(t, dir)
		assertSymlinkRefusal(t, snapshot, "AGENTS.md", "notes.md", "")
		assertSymlinkRefusal(t, snapshot, ".claude/CLAUDE.md", "notes.md", "")
	})

	t.Run("directory leaf keeps the target-less ReadFile message", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, _, err := readInstructionCandidate(instructionSnapshot(t, dir), "subdir")
		var non *NonRegularFileError
		if !errors.As(err, &non) || non.Target != "" || strings.Contains(err.Error(), "symlink target resolves") {
			t.Fatalf("directory leaf error = %v, want the target-less refusal", err)
		}
	})
}

func TestReadInstructionCandidateWithoutClassifierDoesNotAlias(t *testing.T) {
	t.Parallel()

	dir := instructionProject(t, "AGENTS.md", "target\n")
	if err := os.Symlink("AGENTS.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	observed, alias, err := readInstructionCandidate(NewFSSnapshot(os.DirFS(dir)), "CLAUDE.md")
	if err != nil || alias != "" || observed.Path != "CLAUDE.md" || string(observed.Content) != "target\n" {
		t.Fatalf("FSSnapshot alias = %q observed %#v err %v", alias, observed, err)
	}
}

func instructionProject(t *testing.T, relative, content string) string {
	t.Helper()
	dir := t.TempDir()
	writeTestFile(t, dir, relative, content)
	return dir
}

func instructionSnapshot(t *testing.T, dir string) *RootSnapshot {
	t.Helper()
	snapshot, err := NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	})
	return snapshot
}

func assertInstructionAlias(t *testing.T, snapshot *RootSnapshot, want string) {
	t.Helper()
	observed, alias, err := readInstructionCandidate(snapshot, claudeInstructionAlias)
	if err != nil || alias != want || observed.Path != "" {
		t.Fatalf("alias = %q observed %#v err %v, want %q", alias, observed, err, want)
	}
	_, err = snapshot.ReadFile(claudeInstructionAlias)
	var non *NonRegularFileError
	if !errors.As(err, &non) || non.Target != "" || strings.Contains(err.Error(), "symlink target resolves") {
		t.Fatalf("ReadFile(%s) = %v, want a target-less symlink refusal", claudeInstructionAlias, err)
	}
}

func assertSymlinkRefusal(t *testing.T, snapshot *RootSnapshot, filename, target, absent string) {
	t.Helper()
	_, alias, err := readInstructionCandidate(snapshot, filename)
	if err == nil || alias != "" {
		t.Fatalf("candidate %s alias %q err %v, want a refusal", filename, alias, err)
	}
	message := err.Error()
	for _, want := range []string{filename, target, "symlink target resolves to"} {
		if !strings.Contains(message, want) {
			t.Fatalf("refusal %q, want %q", message, want)
		}
	}
	if absent != "" && strings.Contains(message, absent) {
		t.Fatalf("refusal includes file contents %q: %s", absent, message)
	}
}

func linkChain(t *testing.T, dir string, hops int) {
	t.Helper()
	next := "AGENTS.md"
	for index := hops - 1; index >= 1; index-- {
		name := fmt.Sprintf("hop-%d", index)
		if err := os.Symlink(next, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		next = name
	}
	if err := os.Symlink(next, filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
}
