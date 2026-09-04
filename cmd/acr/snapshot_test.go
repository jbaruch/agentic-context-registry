package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// gitExcludePath is the one file inside .git that ACR writes: uninstall and
// realization own a generated block in it, so a snapshot that dropped it would
// stop proving those promises.
const gitExcludePath = ".git/info/exclude"

// snapshotProjectTree records every product path under root together with the
// evidence a command promise is made of: the content digest, the permission
// bits, the link target of a symlink, and the bare existence of a directory.
//
// Git's own internals are excluded. Background maintenance creates and removes
// files such as .git/objects/maintenance.lock while a command runs, which made
// before/after comparisons fail for reasons that had nothing to do with the
// behavior under test (#76). Product bytes, modes, and .git/info/exclude stay
// inside the snapshot, so nothing a command is allowed to change becomes
// invisible along with the noise.
func snapshotProjectTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == "." {
			return nil
		}
		if isTransientGitInternal(relative) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(filename)
			if err != nil {
				return err
			}
			snapshot[relative] = "link " + filepath.ToSlash(target)
		case entry.IsDir():
			snapshot[relative] = fmt.Sprintf("dir %04o", info.Mode().Perm())
		default:
			contents, err := os.ReadFile(filename)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(contents)
			snapshot[relative] = fmt.Sprintf("file %04o %s", info.Mode().Perm(), hex.EncodeToString(digest[:]))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// isTransientGitInternal reports whether a path is Git's own storage rather
// than product output. The .git directory itself stays in the snapshot so a
// command that deleted a repository is still caught.
func isTransientGitInternal(relative string) bool {
	switch relative {
	case ".git", ".git/info", gitExcludePath:
		return false
	}
	return strings.HasPrefix(relative, ".git/")
}

// assertTreeUnchanged fails when a command that promised to write nothing
// changed the project.
func assertTreeUnchanged(t *testing.T, before map[string]string, root, what string) {
	t.Helper()
	after := snapshotProjectTree(t, root)
	if reflect.DeepEqual(before, after) {
		return
	}
	for path, value := range after {
		if previous, ok := before[path]; !ok {
			t.Errorf("%s added %s (%s)", what, path, value)
		} else if previous != value {
			t.Errorf("%s changed %s: %s -> %s", what, path, previous, value)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			t.Errorf("%s removed %s", what, path)
		}
	}
	t.FailNow()
}

func TestSnapshotIgnoresGitMaintenanceButDetectsProductAndExcludeChanges(t *testing.T) {
	root := t.TempDir()
	reverify2Put(t, root, "product.md", "# Product\n", 0o644)
	verify8GitCommit(t, root)
	reverify2Put(t, root, gitExcludePath, "# git ls-files --others\nuser-pattern\n", 0o644)
	baseline := snapshotProjectTree(t, root)

	// Git maintenance creates and removes its own files while a command runs.
	lock := filepath.Join(root, ".git", "objects", "maintenance.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if withLock := snapshotProjectTree(t, root); !reflect.DeepEqual(withLock, baseline) {
		t.Fatalf("a Git maintenance lock changed the snapshot: %v", diffSnapshots(baseline, withLock))
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if withoutLock := snapshotProjectTree(t, root); !reflect.DeepEqual(withoutLock, baseline) {
		t.Fatalf("removing a Git maintenance lock changed the snapshot: %v", diffSnapshots(baseline, withoutLock))
	}

	// Everything the snapshot exists to catch still fails it. Each mutation is
	// reverted so the next one proves its own difference.
	for _, mutation := range []struct {
		name  string
		apply func()
		undo  func()
	}{
		{
			name:  "product bytes",
			apply: func() { reverify2Put(t, root, "product.md", "# Edited\n", 0o644) },
			undo:  func() { reverify2Put(t, root, "product.md", "# Product\n", 0o644) },
		},
		{
			name:  "product mode",
			apply: func() { chmodOrFail(t, filepath.Join(root, "product.md"), 0o755) },
			undo:  func() { chmodOrFail(t, filepath.Join(root, "product.md"), 0o644) },
		},
		{
			name: "generated exclude block",
			apply: func() {
				reverify2Put(t, root, gitExcludePath, "# git ls-files --others\nuser-pattern\n.claude/\n", 0o644)
			},
			undo: func() { reverify2Put(t, root, gitExcludePath, "# git ls-files --others\nuser-pattern\n", 0o644) },
		},
		{
			name:  "removed product file",
			apply: func() { removeOrFail(t, filepath.Join(root, "product.md")) },
			undo:  func() { reverify2Put(t, root, "product.md", "# Product\n", 0o644) },
		},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			mutation.apply()
			if mutated := snapshotProjectTree(t, root); reflect.DeepEqual(mutated, baseline) {
				t.Fatalf("snapshot missed a %s change", mutation.name)
			}
			mutation.undo()
			if restored := snapshotProjectTree(t, root); !reflect.DeepEqual(restored, baseline) {
				t.Fatalf("restoring %s left the snapshot different: %v", mutation.name, diffSnapshots(baseline, restored))
			}
		})
	}
}

func chmodOrFail(t *testing.T, filename string, mode fs.FileMode) {
	t.Helper()
	if err := os.Chmod(filename, mode); err != nil {
		t.Fatal(err)
	}
}

func removeOrFail(t *testing.T, filename string) {
	t.Helper()
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
}

func diffSnapshots(before, after map[string]string) []string {
	var differences []string
	for path, value := range after {
		if previous, ok := before[path]; !ok {
			differences = append(differences, "+"+path+" ("+value+")")
		} else if previous != value {
			differences = append(differences, "~"+path+" ("+previous+" -> "+value+")")
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			differences = append(differences, "-"+path)
		}
	}
	return differences
}
