package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
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
// When root is a Git repository it also records that repository's semantic
// state, under the git: keys snapshotGitState writes.
//
// Git's own storage is excluded from the walk. Background maintenance creates
// and removes files such as .git/objects/maintenance.lock while a command runs,
// which made before/after comparisons fail for reasons that had nothing to do
// with the behavior under test (#76). Excluding the storage also hid the index
// and the refs, so a staged mode change or a moved HEAD read as "unchanged";
// snapshotGitState is what puts that evidence back without the noise.
func snapshotProjectTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := snapshotGitState(t, root)
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

// snapshotGitState records what Git stores about a project as facts rather than
// as bytes: the commit HEAD names, the branch it is on, every index entry with
// its mode and blob, the working-tree status, and the resolved exclude file.
//
// Reading Git's answers rather than Git's files is what separates the evidence
// from the noise. A maintenance lock appearing and disappearing changes none of
// these; staging a mode change, staging content, or moving HEAD changes them
// all, and none of that is visible in the product tree.
//
// The exclude file is resolved through Git, so a linked worktree — whose .git
// is a file and whose info/exclude lives in the common directory — is covered
// by the same key as an ordinary checkout.
func snapshotGitState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := map[string]string{}
	if _, err := os.Lstat(filepath.Join(root, ".git")); err != nil {
		if !os.IsNotExist(err) {
			t.Fatalf("stat %s/.git: %v", root, err)
		}
		state["git:repository"] = "absent"
		return state
	}
	state["git:repository"] = "present"

	if head, err := gitQuery(root, "rev-parse", "HEAD"); err == nil {
		state["git:HEAD"] = head
	} else {
		state["git:HEAD"] = "unborn"
	}
	if branch, err := gitQuery(root, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		state["git:branch"] = branch
	} else {
		state["git:branch"] = "detached"
	}

	staged, err := gitQuery(root, "ls-files", "--stage")
	if err != nil {
		t.Fatalf("read the Git index of %s: %v", root, err)
	}
	for _, line := range strings.Split(staged, "\n") {
		if line == "" {
			continue
		}
		// "<mode> <blob> <stage>\t<path>"
		fields, path, found := strings.Cut(line, "\t")
		if !found {
			t.Fatalf("unreadable index entry %q", line)
		}
		state["git:index "+filepath.ToSlash(path)] = strings.Join(strings.Fields(fields), " ")
	}

	status, err := gitQuery(root, "status", "--porcelain")
	if err != nil {
		t.Fatalf("read the Git status of %s: %v", root, err)
	}
	for _, line := range strings.Split(status, "\n") {
		if len(line) < 4 {
			continue
		}
		state["git:status "+line[3:]] = line[:2]
	}

	excludePath, err := gitQuery(root, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		t.Fatalf("resolve the Git exclude path of %s: %v", root, err)
	}
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(root, excludePath)
	}
	contents, err := os.ReadFile(excludePath)
	switch {
	case err == nil:
		digest := sha256.Sum256(contents)
		state["git:exclude"] = hex.EncodeToString(digest[:])
	case os.IsNotExist(err):
		state["git:exclude"] = "absent"
	default:
		t.Fatalf("read %s: %v", excludePath, err)
	}
	return state
}

// gitQuery asks Git one question with a fixed identity, fixed dates and no
// global configuration, so the answer is the repository's and not the machine's.
// The identity is there for the probes that have to write a commit to prove the
// snapshot notices one.
func gitQuery(root string, arguments ...string) (string, error) {
	command := exec.Command("git", arguments...)
	command.Dir = root
	command.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=ACR Test", "GIT_AUTHOR_EMAIL=acr@example.invalid",
		"GIT_COMMITTER_NAME=ACR Test", "GIT_COMMITTER_EMAIL=acr@example.invalid",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, stderr.String())
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
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
		// The two below are the review's counterexample: nothing in the product
		// tree changes, so only the Git state can catch them.
		{
			name:  "staged product mode",
			apply: func() { mustGitQuery(t, root, "update-index", "--chmod=+x", "product.md") },
			undo:  func() { mustGitQuery(t, root, "update-index", "--chmod=-x", "product.md") },
		},
		{
			name: "moved HEAD",
			apply: func() {
				mustGitQuery(t, root, "commit", "--allow-empty", "-qm", "advance HEAD without touching the tree")
			},
			undo: func() { mustGitQuery(t, root, "reset", "--hard", "--quiet", "HEAD~1") },
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

// TestSnapshotResolvesTheExcludeFileOfALinkedWorktree covers the checkout shape
// where .git is a file: the exclude file lives in the common directory, so the
// product walk never sees it and only the resolved path proves it is watched.
func TestSnapshotResolvesTheExcludeFileOfALinkedWorktree(t *testing.T) {
	root := t.TempDir()
	reverify2Put(t, root, "product.md", "# Product\n", 0o644)
	verify8GitCommit(t, root)

	linked := filepath.Join(t.TempDir(), "linked")
	mustGitQuery(t, root, "worktree", "add", "--quiet", "-b", "linked", linked)
	t.Cleanup(func() {
		if _, err := gitQuery(root, "worktree", "remove", "--force", linked); err != nil {
			t.Errorf("remove the linked worktree: %v", err)
		}
	})

	if info, err := os.Lstat(filepath.Join(linked, ".git")); err != nil || info.IsDir() {
		t.Fatalf("linked worktree .git = %v, %v, want a file", info, err)
	}
	baseline := snapshotProjectTree(t, linked)
	if baseline["git:repository"] != "present" {
		t.Fatalf("linked worktree state = %v, want a repository", baseline)
	}
	if baseline["git:exclude"] == "absent" {
		t.Fatalf("linked worktree exclude = %q, want the resolved common file", baseline["git:exclude"])
	}

	excludePath, err := gitQuery(linked, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(linked, excludePath)
	}
	if strings.HasPrefix(filepath.Clean(excludePath), filepath.Clean(linked)+string(filepath.Separator)) {
		t.Fatalf("resolved exclude %q sits inside the linked worktree; the walk would already cover it", excludePath)
	}
	if err := os.WriteFile(excludePath, []byte("# generated by acr\n.claude/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if after := snapshotProjectTree(t, linked); reflect.DeepEqual(after, baseline) {
		t.Fatal("editing the resolved exclude file of a linked worktree changed nothing in the snapshot")
	}
}

// mustGitQuery runs one Git command that has to succeed for the test to mean
// anything.
func mustGitQuery(t *testing.T, root string, arguments ...string) string {
	t.Helper()
	output, err := gitQuery(root, arguments...)
	if err != nil {
		t.Fatal(err)
	}
	return output
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
