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

func TestInstructionAliasDetectionRejectsReservedLocations(t *testing.T) {
	t.Parallel()
	for _, fixture := range []struct{ stored, alias string }{
		{".git/config", ".GIT/config"},
		{"agents.yaml", "AGENTS.YAML"},
		{".agents/registry.lock", ".AGENTS/registry.lock"},
	} {
		t.Run(fixture.alias, func(t *testing.T) {
			dir := t.TempDir()
			writeTestFile(t, dir, fixture.stored, "reserved bytes\n")
			if _, err := os.Stat(filepath.Join(dir, fixture.alias)); errors.Is(err, fs.ErrNotExist) {
				t.Skip("native probe found a case-sensitive filesystem")
			} else if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(fixture.alias, filepath.Join(dir, "CLAUDE.md")); err != nil {
				t.Fatal(err)
			}
			_, err := ExistingEvidence(instructionSnapshot(t, dir), []string{"CLAUDE.md"}, nil)
			if err == nil || !strings.Contains(err.Error(), "reserved project state") || !strings.Contains(err.Error(), "CLAUDE.md") {
				t.Fatalf("detection error = %v", err)
			}
			body, err := os.ReadFile(filepath.Join(dir, fixture.stored))
			if err != nil || string(body) != "reserved bytes\n" {
				t.Fatalf("reserved bytes changed: %q, %v", body, err)
			}
		})
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

	t.Run("absolute path through a real directory then dotdot", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "AGENTS.md", "# Agents\n")
		if err := os.Mkdir(filepath.Join(dir, "docs"), 0o755); err != nil {
			t.Fatal(err)
		}
		// filepath.Join would clean "docs/.." out of the link text.
		link := dir + "/docs/../AGENTS.md"
		if err := os.Symlink(link, filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertInstructionAlias(t, instructionSnapshot(t, dir), "AGENTS.md")
	})

	t.Run("directory symlink visited twice", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "real/guide.md", "# Guide\n")
		if err := os.Symlink("real", filepath.Join(dir, "d")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("d/../d/guide.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertInstructionAlias(t, instructionSnapshot(t, dir), "real/guide.md")
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

	t.Run("absolute path can leave and reenter the project", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "AGENTS.md", "# Agents\n")
		outside := t.TempDir()
		if err := os.Symlink(dir, filepath.Join(outside, "back")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "sc")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dir+"/sc/back/AGENTS.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertInstructionAlias(t, instructionSnapshot(t, dir), "AGENTS.md")
	})
}

func TestInstructionSymlinkRefusalsNameTheTarget(t *testing.T) {
	t.Parallel()
	t.Run("outside hardlink to inside target", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "project")
		writeTestFile(t, dir, "AGENTS.md", "inside bytes\n")
		outside := filepath.Join(parent, "outside.md")
		if err := os.Link(filepath.Join(dir, "AGENTS.md"), outside); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../outside.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", canonicalInstructionTarget(t, outside), errSymlinkEscapes, "inside bytes")
	})

	for _, spelling := range []string{"relative", "absolute", "chain", "absolute chain"} {
		for _, suffix := range []string{"/", "/.", "//", "/.."} {
			t.Run("regular file suffix/"+spelling+suffix, func(t *testing.T) {
				t.Parallel()
				dir := instructionProject(t, "AGENTS.md", "# Agents\n")
				target := "AGENTS.md" + suffix
				if strings.Contains(spelling, "chain") {
					if err := os.Symlink("AGENTS.md", filepath.Join(dir, "mid")); err != nil {
						t.Fatal(err)
					}
					target = "mid" + suffix
				}
				if strings.HasPrefix(spelling, "absolute") {
					// Keep the suffix verbatim; filepath.Join would clean it away.
					target = dir + "/" + target
				}
				if err := os.Symlink(target, filepath.Join(dir, "CLAUDE.md")); err != nil {
					t.Fatal(err)
				}
				if _, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); !errors.Is(err, syscall.ENOTDIR) {
					t.Fatalf("OS read error = %v, want ENOTDIR", err)
				}
				snapshot := instructionSnapshot(t, dir)
				assertSymlinkRefusal(t, snapshot, "CLAUDE.md", target, errSymlinkNotRegular, "")
				evidence, err := ExistingEvidence(snapshot, []string{"CLAUDE.md", "AGENTS.md"}, nil)
				var non *NonRegularFileError
				if !errors.Is(err, errSymlinkNotRegular) || !errors.As(err, &non) || non.Path != "CLAUDE.md" || non.Target != target || len(evidence) != 0 {
					t.Fatalf("detection evidence = %v, error = %v, want a named non-regular target refusal", evidence, err)
				}
			})
		}
	}

	t.Run("dangling", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Symlink("missing.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		snapshot := instructionSnapshot(t, dir)
		assertSymlinkRefusal(t, snapshot, "CLAUDE.md", "missing.md", errSymlinkDangling, "")
		_, _, err := readInstructionCandidate(snapshot, "CLAUDE.md")
		if errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("dangling alias looks absent: %v", err)
		}
		if evidence, err := ExistingEvidence(snapshot, []string{"CLAUDE.md"}, nil); err == nil || errors.Is(err, fs.ErrNotExist) || len(evidence) != 0 {
			t.Fatalf("dangling detection evidence = %v, err = %v", evidence, err)
		}
		if roots, err := existingInstructionRoots(snapshot, []string{"CLAUDE.md"}, "CLAUDE.md"); err == nil || errors.Is(err, fs.ErrNotExist) || len(roots) != 0 {
			t.Fatalf("dangling instruction roots = %v, err = %v", roots, err)
		}
	})

	t.Run("relative escape", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		dir := filepath.Join(parent, "project")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, parent, "secret.md", "SECRET-BYTES")
		if err := os.Symlink("../secret.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", canonicalInstructionTarget(t, filepath.Join(parent, "secret.md")), errSymlinkEscapes, "SECRET-BYTES")
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
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", canonicalInstructionTarget(t, outside), errSymlinkEscapes, "SECRET-BYTES")
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
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", canonicalInstructionTarget(t, outside+"/file.md"), errSymlinkEscapes, "SECRET-BYTES")
	})

	t.Run("shortcut dotdot does not drop the outside symlink", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "AGENTS.md", "# Agents\n")
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("SECRET-BYTES"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Dir(outside), "AGENTS.md", "SECRET-BYTES")
		if err := os.Symlink(outside, filepath.Join(dir, "shortcut")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("shortcut/../AGENTS.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", canonicalInstructionTarget(t, filepath.Join(outside, "..", "AGENTS.md")), errSymlinkEscapes, "SECRET-BYTES")
	})

	t.Run("absolute dotdot does not drop the outside symlink", func(t *testing.T) {
		t.Parallel()
		dir := instructionProject(t, "AGENTS.md", "# Agents\n")
		outside := t.TempDir()
		if err := os.Mkdir(filepath.Join(outside, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "AGENTS.md"), []byte("SECRET-BYTES"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "sub"), filepath.Join(dir, "sc")); err != nil {
			t.Fatal(err)
		}
		// filepath.Join would clean "sc/.." and store a link to the in-project file.
		link := dir + "/sc/../AGENTS.md"
		if err := os.Symlink(link, filepath.Join(dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		got, err := os.Readlink(filepath.Join(dir, "CLAUDE.md"))
		if err != nil || got != link {
			t.Fatalf("CLAUDE.md -> %q, %v, want the unclean absolute target", got, err)
		}
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", canonicalInstructionTarget(t, outside+"/AGENTS.md"), errSymlinkEscapes, "SECRET-BYTES")
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
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", "mid", errSymlinkNotRegular, "")
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
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", "pipe", errSymlinkNotRegular, "")
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
		assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", "other", errSymlinkHops, "")

		self := t.TempDir()
		if err := os.Symlink("CLAUDE.md", filepath.Join(self, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		assertSymlinkRefusal(t, instructionSnapshot(t, self), "CLAUDE.md", "CLAUDE.md", errSymlinkHops, "")
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
		assertSymlinkRefusal(t, snapshot, "AGENTS.md", "notes.md", nil, "")
		assertSymlinkRefusal(t, snapshot, ".claude/CLAUDE.md", "../notes.md", nil, "")
	})

	t.Run("directory leaf keeps the target-less ReadFile message", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, _, err := readInstructionCandidate(instructionSnapshot(t, dir), "subdir")
		var non *NonRegularFileError
		if !errors.As(err, &non) || non.Target != "" || strings.Contains(err.Error(), "symlink target") {
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
	if !errors.As(err, &non) || non.Target != "" || strings.Contains(err.Error(), "symlink target") {
		t.Fatalf("ReadFile(%s) = %v, want a target-less symlink refusal", claudeInstructionAlias, err)
	}
}

func assertSymlinkRefusal(t *testing.T, snapshot *RootSnapshot, filename, target string, reason error, absent string) {
	t.Helper()
	_, alias, err := readInstructionCandidate(snapshot, filename)
	if err == nil || alias != "" {
		t.Fatalf("candidate %s alias %q err %v, want a refusal", filename, alias, err)
	}
	var non *NonRegularFileError
	if !errors.As(err, &non) || non.Path != filename || non.Target != target {
		t.Fatalf("refusal = %v, want host %q and target %q", err, filename, target)
	}
	message := err.Error()
	for _, want := range []string{filename, target, "symlink target", "replace the link"} {
		if !strings.Contains(message, want) {
			t.Fatalf("refusal %q, want %q", message, want)
		}
	}
	if reason != nil && (!errors.Is(err, reason) || !strings.Contains(message, reason.Error())) {
		t.Fatalf("refusal %q, want reason %v", message, reason)
	}
	if reason == nil && !strings.Contains(message, "symlinks are not supported at this instruction host") {
		t.Fatalf("refusal %q, want unsupported instruction-host reason", message)
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

func canonicalInstructionTarget(t *testing.T, filename string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(filename)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func TestInstructionSymlinkNativeReadAgreement(t *testing.T) {
	t.Parallel()
	for _, hops := range []int{1, 2, 31, 32, 33, 39, 40, 41, 64, 300} {
		t.Run(fmt.Sprintf("%d links", hops), func(t *testing.T) {
			t.Parallel()
			// Match classification’s canonical project prefix so /var does not
			// consume an extra native hop on macOS.
			dir := canonicalInstructionTarget(t, instructionProject(t, "AGENTS.md", "# Agents\n"))
			linkChain(t, dir, hops)
			content, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
			switch {
			case err == nil:
				if string(content) != "# Agents\n" {
					t.Fatalf("native alias content = %q", content)
				}
				assertInstructionAlias(t, instructionSnapshot(t, dir), "AGENTS.md")
			case errors.Is(err, syscall.ELOOP):
				assertSymlinkRefusal(t, instructionSnapshot(t, dir), "CLAUDE.md", "hop-1", errSymlinkHops, "# Agents")
			default:
				t.Fatalf("unexpected native alias read error: %v", err)
			}
		})
	}
}

func TestInstructionSymlinkExternalPrefixReentry(t *testing.T) {
	t.Parallel()
	dir := instructionProject(t, "AGENTS.md", "# Agents\n")
	outside := t.TempDir()
	if err := os.Symlink(dir, filepath.Join(outside, "back")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "back", "AGENTS.md"), filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	assertInstructionAlias(t, instructionSnapshot(t, dir), "AGENTS.md")
}
