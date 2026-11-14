package publish

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isolatedGit keeps the test's Git away from the developer's configuration and
// from any repository above the temporary directory.
func isolatedGit(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CEILING_DIRECTORIES", parent)
	return parent
}

func initRepository(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"init", "-q", "--template="}, append(args, directory)...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s (%v)", output, err)
	}
}

func TestWorktreeTellsNoGitViewFromGitFailures(t *testing.T) {
	t.Run("directory outside any repository", func(t *testing.T) {
		directory := filepath.Join(isolatedGit(t), "plain")
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		inside, err := commandGitSource{}.Worktree(context.Background(), directory)
		if err != nil || inside {
			t.Fatalf("Worktree() = %t, %v; want no Git view and no error", inside, err)
		}
	})

	t.Run("bare repository", func(t *testing.T) {
		directory := filepath.Join(isolatedGit(t), "bare.git")
		initRepository(t, directory, "--bare")
		inside, err := commandGitSource{}.Worktree(context.Background(), directory)
		if err != nil || inside {
			t.Fatalf("Worktree() = %t, %v; want no Git view and no error", inside, err)
		}
	})

	t.Run("git executable missing", func(t *testing.T) {
		directory := filepath.Join(isolatedGit(t), "plain")
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", t.TempDir())
		inside, err := commandGitSource{}.Worktree(context.Background(), directory)
		if err != nil || inside {
			t.Fatalf("Worktree() = %t, %v; want no Git view and no error", inside, err)
		}
	})

	t.Run("work tree", func(t *testing.T) {
		directory := filepath.Join(isolatedGit(t), "tree")
		initRepository(t, directory)
		if err := os.WriteFile(filepath.Join(directory, "a.md"), []byte("a"), 0o644); err != nil {
			t.Fatal(err)
		}
		inside, err := commandGitSource{}.Worktree(context.Background(), directory)
		if err != nil || !inside {
			t.Fatalf("Worktree() = %t, %v", inside, err)
		}
	})

	t.Run("unreadable repository configuration is an error", func(t *testing.T) {
		directory := filepath.Join(isolatedGit(t), "broken")
		initRepository(t, directory)
		config, err := os.OpenFile(filepath.Join(directory, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := config.WriteString("[core\n"); err != nil {
			t.Fatal(err)
		}
		if err := config.Close(); err != nil {
			t.Fatal(err)
		}
		inside, err := commandGitSource{}.Worktree(context.Background(), directory)
		var failure *gitError
		if err == nil || inside || !errors.As(err, &failure) {
			t.Fatalf("Worktree() inside = %t, error = %v; want the Git failure", inside, err)
		}
		_, filterErr := packageListing(context.Background(), directory, commandGitSource{})
		var refusal *Error
		if !errors.As(filterErr, &refusal) || refusal.Code != CodeGitAccess {
			t.Fatalf("packageListing() error = %v, want %s rather than an unfiltered walk", filterErr, CodeGitAccess)
		}
	})
}

func TestWorktreeReportsCancellationNotAbsenceOfGit(t *testing.T) {
	directory := filepath.Join(isolatedGit(t), "tree")
	initRepository(t, directory)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (commandGitSource{}).Worktree(ctx, directory); !errors.Is(err, context.Canceled) {
		t.Fatalf("Worktree() error = %v, want context.Canceled", err)
	}
}

func TestWorktreeLeavesAMissingDirectoryToTheManifestLoad(t *testing.T) {
	missing := filepath.Join(isolatedGit(t), "missing")
	inside, err := commandGitSource{}.Worktree(context.Background(), missing)
	if err != nil || inside {
		t.Fatalf("Worktree() = %t, %v; want no Git view and no error", inside, err)
	}
}

func TestWorktreeReportsFilesystemFailures(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink("loop", directory); err != nil {
		t.Fatal(err)
	}
	if _, err := (commandGitSource{}).Worktree(context.Background(), directory); err == nil {
		t.Fatal("Worktree() silently disabled Git filtering for an unreadable root")
	}
}

func TestNotAGitWorkTreeDoesNotHideOtherFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"missing executable", &gitError{cause: exec.ErrNotFound}, true},
		{"outside repository", &gitError{exitCode: 128, diagnostic: "fatal: not a git repository (or any of the parent directories): .git"}, true},
		{"wrong exit code", &gitError{exitCode: 1, diagnostic: "fatal: not a git repository"}, false},
		{"unsafe repository", &gitError{exitCode: 128, diagnostic: "fatal: detected dubious ownership in repository at '/tmp/not a git repository'"}, false},
		{"bad config", &gitError{exitCode: 128, diagnostic: "fatal: bad config line 1 in file .git/config"}, false},
		{"permission denied", os.ErrPermission, false},
		{"canceled", context.Canceled, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := notAGitWorkTree(test.err); got != test.want {
				t.Fatalf("notAGitWorkTree(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestWorktreeOwnershipRefusalDoesNotEnableFallback(t *testing.T) {
	repo, _ := packageRepository(t, false)
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
	_, err := PackageListing(context.Background(), repo)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != CodeGitAccess || !strings.Contains(err.Error(), "dubious ownership") {
		t.Fatalf("ownership refusal = %v", err)
	}
}
