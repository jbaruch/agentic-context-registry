package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func journeyReconcileExclusionSuccess(t *testing.T) int {
	project := newGitJourneyProject(t)
	const before = "keep-*.local\n"
	const fence = ".git/info/acr-exclusion-journal"
	writeProjectText(t, project.root, gitExcludePath, before)
	runDocsGit(t, project.root, "-c", "user.name=Example", "-c", "user.email=example@example.org", "commit", "--allow-empty", "-qm", "Seed")
	owner := filepath.Join(t.TempDir(), "owner")
	runDocsGit(t, project.root, "worktree", "add", "--quiet", "--detach", owner)
	crash := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestReconcileExclusionJourneyCrash$")
	crash.Env = append(crash.Environ(), "ACR_TEST_RECONCILE_OWNER="+owner)
	var exited *exec.ExitError
	if out, err := crash.CombinedOutput(); !errors.As(err, &exited) || exited.ExitCode() != 85 {
		t.Fatalf("exclusion writer did not crash with a durable fence: %v\n%s", err, out)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(readProjectText(t, project.root, fence))))
	runDocsGit(t, project.root, "worktree", "remove", "--force", owner)
	result := project.runBinary(journeyBuiltBinary(t), 0, "reconcile-exclusion", digest, "--json")
	if envelope := journeyEnvelope(t, result.stdout); envelope["ok"] != true || envelope["command"] != "reconcile-exclusion" {
		t.Fatalf("reconciliation reported the wrong result: %s", result.stdout)
	}
	if _, err := os.Lstat(project.path(fence)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reconciliation did not retire the fence: %v", err)
	}
	if got := readProjectText(t, project.root, gitExcludePath); got != before {
		t.Fatalf("reconciliation changed the exclusion file: %q", got)
	}
	return 1
}

func journeyReconcileExclusionRefusal(t *testing.T) int {
	project := newGitJourneyProject(t)
	before := project.snapshot()
	result := project.runBinary(journeyBuiltBinary(t), 1, "reconcile-exclusion", "sha256:"+strings.Repeat("0", 64), "--json")
	failure := journeyError(t, result.stderr)
	if failure["code"] != "realization_failed" || !strings.Contains(result.stderr, "no Git exclusion fence matches the supplied digest") {
		t.Fatalf("reconciliation did not refuse the unmatched digest: %s", result.stderr)
	}
	project.assertUnchanged(before, "reconcile-exclusion with no matching fence")
	return 1
}

func TestReconcileExclusionJourneyCrash(t *testing.T) {
	owner := os.Getenv("ACR_TEST_RECONCILE_OWNER")
	if owner == "" {
		return
	}
	ledger := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{
		{Path: ".cursor/rules/journey.mdc", Ownership: realize.OwnershipGenerated},
	}}
	err := realize.ApplyFileTransactionWithGitExclusion(t.Context(), owner, ledger, func(realize.Ledger) ([]realize.FileTransactionEdit, error) {
		return nil, nil
	}, nil, realize.FileTransactionHooks{BeforeEdit: func(_ int, edit realize.FileTransactionEdit) error {
		if edit.Path != gitExcludePath {
			return fmt.Errorf("first edit is %q, want %q", edit.Path, gitExcludePath)
		}
		os.Exit(85)
		return nil
	}})
	t.Fatalf("exclusion writer returned instead of crashing: %v", err)
}

func TestBuiltRealizeUsesEnclosingGitWorktree(t *testing.T) {
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	worktree := project.root
	git := func(args ...string) {
		t.Helper()
		if output, err := exec.Command("git", append([]string{"-C", worktree}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	git("init", "-q")
	writeProjectText(t, worktree, gitExcludePath, "*.local\n")
	const nested = "consumer [dev]"
	const child = nested + "/child"
	const generated = ".cursor/rules/acr__local__example__boundaries.mdc"
	const tracked = nested + "/" + generated
	const ignored = child + "/" + generated
	pkg := newJourneyPackage(t, "local/example", "1.0.0")
	source := t.TempDir()
	if err := dependency.ExtractPackageArchive(pkg.archive, source); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{nested, child} {
		project.root = filepath.Join(worktree, directory)
		if err := os.MkdirAll(project.root, 0o755); err != nil {
			t.Fatal(err)
		}
		project.runBinary(binary, 0, "install", "file:"+source, "--agent", "cursor", "--freshness", "none", "--non-interactive")
		project.runBinary(binary, 0, "realize")
	}
	git("check-ignore", "-q", "--", tracked)
	git("check-ignore", "-q", "--", ignored)
	git("add", "-f", "--", tracked)
	before := readProjectText(t, worktree, ".git/index")
	trackedContent := readProjectText(t, worktree, tracked)

	project.root = filepath.Join(worktree, nested)
	project.runBinary(binary, 0, "realize")
	project.runBinary(binary, 0, "check")
	git("check-ignore", "-q", "--", ignored)
	git("ls-files", "--error-unmatch", "--", tracked)
	gitCheck := exec.Command("git", "-C", worktree, "check-ignore", "-q", "--no-index", "--", tracked)
	if output, err := gitCheck.CombinedOutput(); err == nil || gitCheck.ProcessState.ExitCode() != 1 {
		t.Fatalf("force-added file should have no exclusion: %v: %s", err, output)
	}
	if got := readProjectText(t, worktree, ".git/index"); got != before {
		t.Fatal("realize changed the enclosing index")
	}
	if got := readProjectText(t, worktree, tracked); got != trackedContent {
		t.Fatal("realize changed the tracked file")
	}
	if !strings.HasPrefix(readProjectText(t, worktree, gitExcludePath), "*.local\n") {
		t.Fatal("realize changed the existing user pattern")
	}
	const legacy = "# BEGIN ACR PROJECT OUTPUTS [consumer%20%5Bdev%5D%2F]\n/legacy.md\n# END ACR PROJECT OUTPUTS [consumer%20%5Bdev%5D%2F]\n"
	exclude := readProjectText(t, worktree, gitExcludePath) + legacy
	writeProjectText(t, worktree, gitExcludePath, exclude)
	for _, flags := range [][]string{{"--dry-run"}, nil} {
		run := project.runBinary(binary, 0, append([]string{"realize"}, flags...)...)
		if !strings.Contains(run.output(), "ambiguous_legacy_git_exclusion") {
			t.Fatal("realize did not diagnose the ownerless legacy block")
		}
		if got := readProjectText(t, worktree, gitExcludePath); got != exclude {
			t.Fatal("realize changed the ownerless legacy block")
		}
	}
}

func TestBuiltRealizeAndUninstallPreserveGitExcludeUserBytes(t *testing.T) {
	binary := journeyBuiltBinary(t)
	pkg := newJourneyPackage(t, "local/example", "1.0.0")
	source := t.TempDir()
	if err := dependency.ExtractPackageArchive(pkg.archive, source); err != nil {
		t.Fatal(err)
	}
	const marker = "# ACR WORKTREE OUTPUTS v1 ADDED SEPARATOR"
	for _, test := range []struct {
		name, content, ignored, notIgnored string
	}{
		{"pattern suffix", "keep-*.local" + marker + "\n", "keep-a.local" + marker, "keep-a.local"},
		{"standalone comment", "keep-*.local\n" + marker + "\n", "keep-a.local", "drop-a.local"},
		{"unterminated pattern", "# existing comment\r\nkeep-*.local", "keep-a.local", "drop-a.local"},
		{"unterminated comment", "keep-*.local\r\n" + marker, "keep-a.local", "drop-a.local"},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := newJourneyProject(t, nil)
			git := func(want int, args ...string) {
				t.Helper()
				command := exec.Command("git", append([]string{"-C", project.root}, args...)...)
				output, err := command.CombinedOutput()
				if command.ProcessState == nil || command.ProcessState.ExitCode() != want {
					t.Fatalf("git %v: want exit %d, got %v: %s", args, want, err, output)
				}
			}
			git(0, "init", "-q")
			writeProjectText(t, project.root, gitExcludePath, test.content)
			project.runBinary(binary, 0, "install", "file:"+source, "--agent", "cursor", "--freshness", "none", "--non-interactive")
			project.runBinary(binary, 0, "realize")
			const generated = ".cursor/rules/acr__local__example__boundaries.mdc"
			if got := readProjectText(t, project.root, gitExcludePath); !strings.HasPrefix(got, test.content) {
				t.Fatalf("realize changed existing user bytes: %q", got)
			}
			git(0, "check-ignore", "-q", "--", generated)
			git(0, "check-ignore", "-q", "--", test.ignored)
			git(1, "check-ignore", "-q", "--", test.notIgnored)
			project.runBinary(binary, 0, "uninstall", "github:local/example")
			if got := readProjectText(t, project.root, gitExcludePath); got != test.content {
				t.Fatalf("uninstall exclusion = %q, want %q", got, test.content)
			}
			git(1, "check-ignore", "-q", "--", generated)
			git(0, "check-ignore", "-q", "--", test.ignored)
			git(1, "check-ignore", "-q", "--", test.notIgnored)
		})
	}
}

func TestBuiltLinkedUninstallPreservesInterBlockUserBytes(t *testing.T) {
	binary := journeyBuiltBinary(t)
	var sources [2]string
	for index, name := range []string{"local/alpha", "local/beta"} {
		sources[index] = t.TempDir()
		pkg := newJourneyPackage(t, name, "1.0.0")
		if err := dependency.ExtractPackageArchive(pkg.archive, sources[index]); err != nil {
			t.Fatal(err)
		}
	}
	const marker = "# ACR WORKTREE OUTPUTS v1 ADDED SEPARATOR\n"
	const laterEdit = "# added after the first uninstall\r\nlast-*.local"
	const original = "# original comment\r\nkeep-*.local"
	// Verbatim separator emitted by 78f64a0 for original.
	const genuine = "# ACR WORKTREE OUTPUTS v1 ADDED SEPARATOR sha256:3de0a1f5bd5a937d72f150ca932426c5baa7810b15aca0f7b8ee65539a336b44\n"
	for _, scope := range []string{"", "consumer"} {
		const generatedAlpha = ".cursor/rules/acr__local__alpha__boundaries.mdc"
		for _, test := range []struct {
			name, original, edit, old, later string
		}{
			{name: "literal", original: original, edit: marker, later: laterEdit},
			{name: "pattern-and-literal", original: original, edit: "inter-*.local\r\n" + marker},
			{name: "copied-old-format", original: original, old: genuine, edit: genuine},
			{name: "copied-into-receipted-file", original: original, edit: genuine},
			{name: "unchanged-LF", original: "# original comment\nkeep-*.local"},
			{name: "unchanged-CRLF", original: original},
			{name: "unchanged-terminated", original: original + "\r\n"},
			{name: "EOF-negation", original: "keep-*.local\n!/" + filepath.ToSlash(filepath.Join(scope, generatedAlpha))},
		} {
			for _, order := range [][2]int{{0, 1}, {1, 0}} {
				t.Run(fmt.Sprintf("scope=%q/%s/first=%d", scope, test.name, order[0]), func(t *testing.T) {
					repo, fixture := t.TempDir(), t.TempDir()
					runGit := func(root string, want int, args ...string) string {
						t.Helper()
						args = append([]string{"-C", root, "-c", "user.name=ACR Test", "-c", "user.email=acr@example.invalid", "-c", "commit.gpgsign=false"}, args...)
						command := exec.Command("git", args...)
						output, err := command.CombinedOutput()
						if command.ProcessState == nil || command.ProcessState.ExitCode() != want {
							t.Fatalf("git %v: want exit %d, got %v: %s", args, want, err, output)
						}
						return string(output)
					}
					runGit(repo, 0, "init", "-q")
					writeProjectText(t, repo, "tracked", "track this\n")
					runGit(repo, 0, "add", "--", "tracked")
					runGit(repo, 0, "commit", "-q", "-m", "seed")
					exclude := filepath.Join(repo, gitExcludePath)
					receipt := filepath.Join(filepath.Dir(exclude), "acr-exclusion-separator")
					if err := os.WriteFile(exclude, []byte(test.original), 0o640); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(exclude, 0o640); err != nil {
						t.Fatal(err)
					}
					assertExclude := func(want string) {
						t.Helper()
						if got := readProjectText(t, repo, gitExcludePath); got != want {
							t.Fatalf("info/exclude = %q, want %q", got, want)
						}
						info, err := os.Stat(exclude)
						if err != nil {
							t.Fatal(err)
						}
						if info.Mode().Perm() != 0o640 {
							t.Fatalf("info/exclude mode = %o, want 0640", info.Mode().Perm())
						}
					}

					prefix := test.original
					if !strings.HasSuffix(prefix, "\n") {
						prefix += "\n"
					}
					var projects [2]*journeyProject
					var roots, indexPaths, indexes, generated, blocks [2]string
					var realized string
					checkIgnore := func(alive [2]int) {
						t.Helper()
						for i, root := range roots {
							if root == "" {
								continue
							}
							if got, err := os.ReadFile(indexPaths[i]); err != nil || string(got) != indexes[i] {
								t.Fatalf("worktree %d index changed: %v", i, err)
							}
							for j, target := range generated {
								if target != "" {
									runGit(root, alive[j], "check-ignore", "-q", "--", target)
								}
							}
							runGit(root, 0, "check-ignore", "-q", "--", "keep-example.local")
						}
					}
					for index, name := range []string{"alpha", "beta"} {
						linked := filepath.Join(fixture, name)
						runGit(repo, 0, "worktree", "add", "-q", "--detach", linked)
						roots[index] = linked
						indexPaths[index] = strings.TrimSpace(runGit(linked, 0, "rev-parse", "--path-format=absolute", "--git-path", "index"))
						indexBytes, err := os.ReadFile(indexPaths[index])
						if err != nil {
							t.Fatal(err)
						}
						indexes[index] = string(indexBytes)
						generated[index] = filepath.ToSlash(filepath.Join(scope, ".cursor/rules/acr__local__"+name+"__boundaries.mdc"))
						project := newJourneyProject(t, nil)
						projects[index] = project
						project.root = filepath.Join(linked, scope)
						if err := os.MkdirAll(project.root, 0o755); err != nil {
							t.Fatal(err)
						}
						project.runBinary(binary, 0, "install", "file:"+sources[index], "--agent", "cursor", "--freshness", "none", "--non-interactive")
						project.runBinary(binary, 0, "realize")
						before := prefix
						if index != 0 {
							before = realized
						}
						realized = readProjectText(t, repo, gitExcludePath)
						block, appended := strings.CutPrefix(realized, before)
						if !appended || !strings.HasPrefix(block, "# BEGIN ACR WORKTREE OUTPUTS v1 ") ||
							!strings.HasSuffix(block, "]\n") || !strings.Contains(block, "\n/"+generated[index]+"\n") {
							t.Fatalf("block not appended at EOF: %q", realized)
						}
						blocks[index] = block
						assertExclude(before + block)
						checkIgnore([2]int{0, 0})
					}
					if test.old != "" {
						if err := os.Remove(receipt); err != nil {
							t.Fatal(err)
						}
					}
					prefix += test.old
					withEdit := prefix + blocks[0] + test.edit + blocks[1]
					if test.edit != "" {
						writeProjectText(t, repo, gitExcludePath, withEdit)
					}
					for _, project := range projects {
						project.runBinary(binary, 0, "realize", "--dry-run")
						assertExclude(withEdit)
						project.runBinary(binary, 0, "realize")
						assertExclude(withEdit)
					}

					packages := [2]string{"github:local/alpha", "github:local/beta"}
					projects[order[0]].runBinary(binary, 0, "uninstall", packages[order[0]])
					want := prefix + test.edit + blocks[1]
					if order[0] == 1 {
						want = prefix + blocks[0] + test.edit
					}
					assertExclude(want)
					ignored := [2]int{0, 0}
					ignored[order[0]] = 1
					checkIgnore(ignored)
					if test.later != "" {
						writeProjectText(t, repo, gitExcludePath, want+test.later)
					}
					projects[order[1]].runBinary(binary, 0, "uninstall", packages[order[1]])
					if test.edit == "" {
						assertExclude(test.original)
					} else {
						assertExclude(prefix + test.edit + test.later)
					}
					checkIgnore([2]int{1, 1})
					if test.later != "" {
						runGit(repo, 0, "check-ignore", "-q", "--", "last-example.local")
					}
					if test.name == "pattern-and-literal" {
						runGit(repo, 0, "check-ignore", "-q", "--", "inter-example.local")
					}
				})
			}
		}
	}
}

func TestBuiltRealizeStopsStalledGitInspection(t *testing.T) {
	binary := journeyBuiltBinary(t)
	for _, test := range []struct {
		name   string
		signal os.Signal
	}{
		{name: "interrupt", signal: os.Interrupt},
		{name: "hangup", signal: syscall.SIGHUP},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := newJourneyProject(t, nil)
			state := dependency.State{
				Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion, Agents: []string{"cursor"}},
				Lock:    dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion},
			}
			if err := dependency.WriteState(project.root, state); err != nil {
				t.Fatal(err)
			}
			fixtures := t.TempDir()
			monitor := filepath.Join(fixtures, "monitor")
			ready := filepath.Join(fixtures, "ready")
			const script = "#!/bin/bash\nset -euo pipefail\nprintf '%s\\n' \"$$\" > \"$ACR_TEST_FSMONITOR_READY\"\nexec sleep 20\n"
			if err := os.WriteFile(monitor, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{
				{"init", "-q"}, {"add", "--", "agents.yaml"}, {"config", "core.fsmonitor", monitor},
			} {
				if output, err := exec.Command("git", append([]string{"-C", project.root}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, output)
				}
			}
			before := project.snapshot()
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, "realize", "--project", project.root)
			command.Env = append(command.Environ(), "ACR_TEST_FSMONITOR_READY="+ready)
			command.WaitDelay = time.Second
			var output bytes.Buffer
			command.Stdout, command.Stderr = &output, &output
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- command.Wait() }()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			var helperPID string
			for {
				if contents, err := os.ReadFile(ready); err == nil {
					if pid, complete := strings.CutSuffix(string(contents), "\n"); complete {
						helperPID = pid
						break
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				select {
				case err := <-exited:
					t.Fatalf("realize exited before the fsmonitor started: %v: %s", err, &output)
				case <-ctx.Done():
					t.Fatal("the fsmonitor did not start")
				case <-ticker.C:
				}
			}
			if err := command.Process.Signal(test.signal); err != nil {
				t.Fatal(err)
			}
			err := <-exited
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() <= 0 {
				t.Errorf("realize must exit with an error after stopping Git: %v: %s", err, &output)
			}
			for _, want := range []string{"git ls-files", "core.fsmonitor", "retry", "context canceled"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("realize diagnostic %q is missing %q", &output, want)
				}
			}
			status, err := exec.Command("ps", "-o", "stat=", "-p", helperPID).CombinedOutput()
			if err == nil {
				// An exited helper may still be waiting for its new parent to reap it.
				if !strings.HasPrefix(strings.TrimSpace(string(status)), "Z") {
					t.Errorf("fsmonitor helper %s survived cancellation: %s", helperPID, status)
				}
			} else if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || len(status) != 0 {
				t.Errorf("check fsmonitor helper %s status: %v: %s", helperPID, err, status)
			}
			project.assertUnchanged(before, "stalled realization")
		})
	}
}

func TestBuiltRealizeStopsPostExitGitDrain(t *testing.T) {
	binary := journeyBuiltBinary(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	pkg := newJourneyPackage(t, "local/example", "1.0.0")
	if err := dependency.ExtractPackageArchive(pkg.archive, source); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		cancel bool
		exit   int
		want   []string
	}{
		{"interrupt", true, 0, []string{"context canceled"}},
		{"interrupt after failed exit", true, 23, []string{"context canceled", "exit status 23", "synthetic corrupt index"}},
		{"failed exit", false, 23, []string{"exit status 23", "WaitDelay expired", "synthetic corrupt index: inspect index before retry"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := newJourneyProject(t, nil)
			if output, err := exec.Command(git, "init", "-q", project.root).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, output)
			}
			writeProjectText(t, project.root, gitExcludePath, "keep-*.local\n")
			project.runBinary(binary, 0, "install", "file:"+source, "--agent", "cursor", "--freshness", "none", "--non-interactive")
			before := project.snapshot()
			if _, err := os.Stat(project.path(".cursor/rules/acr__local__example__boundaries.mdc")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fixture must require a generated output write: %v", err)
			}

			shimDir := t.TempDir()
			ready := filepath.Join(shimDir, "pids")
			const shim = `#!/bin/bash
set -euo pipefail
# The same receipt inspection runs earlier without the project claim.
if [[ "${3-}" == --no-literal-pathspecs && -f "$2/.agents/.acr-transactions/.lock" ]]; then
    if [[ "$ACR_TEST_GIT_EXIT" == 0 ]]; then
        "$ACR_TEST_REAL_GIT" "$@"
        /bin/sleep 30 </dev/null 2>/dev/null &
    else
        printf 'synthetic corrupt index: inspect index before retry\n' >&2
        /bin/sleep 30 </dev/null >/dev/null &
    fi
    printf '%s %s\n' "$$" "$!" > "$ACR_TEST_PIDS"
    exit "$ACR_TEST_GIT_EXIT"
fi
exec "$ACR_TEST_REAL_GIT" "$@"
`
			if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
				t.Fatal(err)
			}
			ctx, stop := context.WithTimeout(t.Context(), 12*time.Second)
			defer stop()
			command := exec.CommandContext(ctx, binary, "realize", "--project", project.root)
			command.Env = append(command.Environ(),
				"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"ACR_TEST_REAL_GIT="+git, "ACR_TEST_PIDS="+ready, "ACR_TEST_GIT_EXIT="+strconv.Itoa(test.exit))
			command.WaitDelay = time.Second
			var output bytes.Buffer
			command.Stdout, command.Stderr = &output, &output
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- command.Wait() }()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			var leader, helper int
			for {
				pids, err := os.ReadFile(ready)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if bytes.HasSuffix(pids, []byte("\n")) {
					if _, err := fmt.Sscan(string(pids), &leader, &helper); err != nil || leader <= 1 || helper <= 1 {
						t.Fatalf("invalid Git test PIDs %q: %v", pids, err)
					}
					break
				}
				select {
				case err := <-exited:
					t.Fatalf("realize exited before reaching the final receipt inspection: %v: %s", err, &output)
				case <-ctx.Done():
					t.Fatal("final receipt inspection did not start")
				case <-ticker.C:
				}
			}
			t.Cleanup(func() {
				if err := syscall.Kill(helper, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					t.Errorf("clean Git test helper: %v", err)
				}
			})
			if test.cancel {
				// ESRCH is observed only after the CLI has reaped the leader,
				// so the signal cannot exercise just the running-Git path.
				for {
					if err := syscall.Kill(leader, 0); errors.Is(err, syscall.ESRCH) {
						break
					} else if err != nil {
						t.Fatal(err)
					}
					select {
					case <-ctx.Done():
						t.Fatal("Git leader was not reaped")
					case <-ticker.C:
					}
				}
				if err := command.Process.Signal(os.Interrupt); err != nil {
					t.Fatal(err)
				}
			}
			var exitErr *exec.ExitError
			if err := <-exited; !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Errorf("realize = %v, want exit 1: %s", err, &output)
			}
			for _, want := range append(test.want, "git ls-files", "core.fsmonitor", "retry") {
				if !strings.Contains(output.String(), want) {
					t.Errorf("realize diagnostic %q is missing %q", &output, want)
				}
			}
			if test.cancel && strings.Contains(output.String(), "WaitDelay expired") {
				t.Errorf("canceled drain waited for the pipe deadline: %s", &output)
			}
			status, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(helper)).Output()
			if err == nil && !strings.HasPrefix(strings.TrimSpace(string(status)), "Z") {
				t.Errorf("Git helper survived: %s", status)
			} else if err != nil && (!errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || len(status) != 0) {
				t.Errorf("check Git helper: %v: %s", err, status)
			}
			project.assertUnchanged(before, "failed post-exit Git drain")
		})
	}
}

func TestBuiltSiblingRealizationsShareGitExclusions(t *testing.T) {
	binary := journeyBuiltBinary(t)
	worktree := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if output, err := exec.Command("git", append([]string{"-C", worktree}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	git("init", "-q")
	const userPattern = "keep-*.local\n"
	writeProjectText(t, worktree, gitExcludePath, userPattern)

	source := t.TempDir()
	pkg := newJourneyPackage(t, "local/example", "1.0.0")
	if err := dependency.ExtractPackageArchive(pkg.archive, source); err != nil {
		t.Fatal(err)
	}
	type attempt struct {
		project *journeyProject
		before  map[string]string
		command *exec.Cmd
		output  *bytes.Buffer
	}
	var attempts []attempt
	for _, name := range []string{"alpha", "beta"} {
		project := newJourneyProject(t, nil)
		project.root = filepath.Join(worktree, name)
		if err := os.Mkdir(project.root, 0o755); err != nil {
			t.Fatal(err)
		}
		project.runBinary(binary, 0, "install", "file:"+source, "--agent", "cursor", "--freshness", "none", "--non-interactive")
		command := exec.CommandContext(t.Context(), binary, "realize", "--project", project.root)
		command.Env = environmentWith("ACR_STATE_HOME", project.stateHome)
		output := new(bytes.Buffer)
		command.Stdout, command.Stderr = output, output
		attempts = append(attempts, attempt{project, project.snapshot(), command, output})
	}
	for _, attempt := range attempts {
		if err := attempt.command.Start(); err != nil {
			t.Fatal(err)
		}
	}
	const generated = ".cursor/rules/acr__local__example__boundaries.mdc"
	var retry []*journeyProject
	for _, attempt := range attempts {
		err := attempt.command.Wait()
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 ||
				!strings.Contains(attempt.output.String(), "transaction_busy") {
				t.Errorf("concurrent realize in %s: %v: %s", attempt.project.root, err, attempt.output)
				continue
			}
			attempt.project.assertUnchanged(attempt.before, "refused sibling realization")
			retry = append(retry, attempt.project)
			continue
		}
		// Check reported successes before retry can accidentally repair a lost block.
		git("check-ignore", "-q", "--", filepath.Join(filepath.Base(attempt.project.root), generated))
	}
	if len(retry) == len(attempts) {
		t.Fatal("neither concurrent realization acquired the exclusion claim")
	}
	for _, project := range retry {
		project.runBinary(binary, 0, "realize")
	}
	for _, attempt := range attempts {
		git("check-ignore", "-q", "--", filepath.Join(filepath.Base(attempt.project.root), generated))
		attempt.project.runBinary(binary, 0, "check")
	}
	if got := readProjectText(t, worktree, gitExcludePath); !strings.HasPrefix(got, userPattern) {
		t.Fatalf("shared exclusion lost the user pattern: %q", got)
	}
}
