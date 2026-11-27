package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const exclusionJourneyBefore = "keep-*.local\n"
const exclusionJourneyFence = ".git/info/acr-exclusion-journal"

func journeyCrashedExclusion(t *testing.T) (*journeyProject, string) {
	t.Helper()
	binary := journeyBuiltBinary(t)
	project := newGitJourneyProject(t)
	writeProjectText(t, project.root, gitExcludePath, exclusionJourneyBefore)
	runDocsGit(t, project.root, "-c", "user.name=Example", "-c", "user.email=example@example.org", "commit", "--allow-empty", "-qm", "Seed")
	project.runBinary(binary, 0, "init", "--agent", "cursor", "--freshness", "none", "--non-interactive")
	owner := filepath.Join(t.TempDir(), "owner")
	runDocsGit(t, project.root, "worktree", "add", "--quiet", "--detach", owner)
	crash := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestReconcileExclusionJourneyCrash$")
	crash.Env = append(crash.Environ(), "ACR_TEST_RECONCILE_OWNER="+owner)
	var exited *exec.ExitError
	if out, err := crash.CombinedOutput(); !errors.As(err, &exited) || exited.ExitCode() != 85 {
		t.Fatalf("exclusion writer did not crash with a durable fence: %v\n%s", err, out)
	}
	return project, owner
}

func journeyReconcileExclusionSuccess(t *testing.T) int {
	project, owner := journeyCrashedExclusion(t)
	binary := journeyBuiltBinary(t)
	owningProject := *project
	owningProject.root = owner
	text := owningProject.runBinary(binary, 0, "prepare-exclusion")
	if !strings.Contains(text.stdout, "You can now remove it with Git") {
		t.Fatalf("preparation did not report the next step: %s", text.stdout)
	}
	prepared := owningProject.runBinary(binary, 0, "prepare-exclusion", "--json")
	if envelope := journeyEnvelope(t, prepared.stdout); envelope["ok"] != true || envelope["command"] != "prepare-exclusion" ||
		!strings.Contains(prepared.stdout, "You can now remove it with Git") {
		t.Fatalf("JSON preparation did not report the next step: %s", prepared.stdout)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(readProjectText(t, project.root, exclusionJourneyFence))))
	runDocsGit(t, project.root, "worktree", "remove", "--force", owner)
	result := project.runBinary(binary, 0, "reconcile-exclusion", digest, "--json")
	if envelope := journeyEnvelope(t, result.stdout); envelope["ok"] != true || envelope["command"] != "reconcile-exclusion" {
		t.Fatalf("reconciliation reported the wrong result: %s", result.stdout)
	}
	if _, err := os.Lstat(project.path(exclusionJourneyFence)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reconciliation did not retire the fence: %v", err)
	}
	if got := readProjectText(t, project.root, gitExcludePath); got != exclusionJourneyBefore {
		t.Fatalf("reconciliation changed the exclusion file: %q", got)
	}
	return 1
}

func journeyPrepareExclusionRefusal(t *testing.T) int {
	project, _ := journeyCrashedExclusion(t)
	before := project.snapshot()
	result := project.runBinary(journeyBuiltBinary(t), 1, "prepare-exclusion", "--json")
	if failure := journeyError(t, result.stderr); failure["code"] != "transaction_busy" || !strings.Contains(result.stderr, "requires the matching owner") {
		t.Fatalf("preparation did not refuse the non-owner: %s", result.stderr)
	}
	project.assertUnchanged(before, "prepare-exclusion from a surviving worktree")
	return 1
}

func TestBuiltExclusionUnpreparedRemovalGuidance(t *testing.T) {
	project, owner := journeyCrashedExclusion(t)
	binary := journeyBuiltBinary(t)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(readProjectText(t, project.root, exclusionJourneyFence))))
	runDocsGit(t, project.root, "worktree", "remove", "--force", owner)
	writeProjectText(t, filepath.Dir(owner), filepath.Base(owner), "unrelated leaf\n")
	before := project.snapshot()
	for _, args := range [][]string{{"realize"}, {"reconcile-exclusion", digest}} {
		for _, format := range [][]string{nil, {"--json"}} {
			result := project.runBinary(binary, 1, append(append([]string{}, args...), format...)...)
			if len(format) != 0 && journeyError(t, result.stderr)["code"] != "transaction_busy" {
				t.Fatalf("JSON lost the busy code: %s", result.stderr)
			}
			for _, fragment := range []string{"transaction_busy", digest, "locate or restore the matching owner", "Git metadata"} {
				if !strings.Contains(result.stderr, fragment) {
					t.Fatalf("diagnostic omitted %q: %s", fragment, result.stderr)
				}
			}
			if strings.Contains(result.stderr, "run 'acr reconcile-exclusion") {
				t.Fatalf("diagnostic advertised impossible reconciliation: %s", result.stderr)
			}
		}
	}
	project.assertUnchanged(before, "reconcile-exclusion after unwitnessed removal")
	if got := readProjectText(t, filepath.Dir(owner), filepath.Base(owner)); got != "unrelated leaf\n" {
		t.Fatalf("regular replacement was changed: %q", got)
	}
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

func TestBuiltPrepareExclusionRejectsMovedLocalOwner(t *testing.T) {
	const child = "ACR_TEST_PREPARE_MOVED_LOCAL"
	if owner := os.Getenv(child); owner != "" {
		ledger := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{
			{Path: ".cursor/rules/interrupted.mdc", Ownership: realize.OwnershipGenerated},
		}}
		err := realize.ApplyFileTransactionWithGitExclusion(t.Context(), owner, ledger, func(realize.Ledger) ([]realize.FileTransactionEdit, error) {
			var edits []realize.FileTransactionEdit
			for _, name := range []string{dependency.ProjectFilename, dependency.LockFilename} {
				state, present, err := realize.ReadStateFile(owner, name)
				if err != nil || !present {
					t.Fatalf("read authorized fixture state %s: %v", name, err)
				}
				edits = append(edits, realize.FileTransactionEdit{Path: name, Operation: "splice",
					Before: state.Content, BeforeMode: state.BeforeMode,
					After: []byte("schemaVersion: 2\ndependencies: []\n"), AfterMode: state.BeforeMode})
			}
			return edits, nil
		}, nil, realize.FileTransactionHooks{AfterEdit: func(_ int, edit realize.FileTransactionEdit) error {
			if edit.Path == dependency.LockFilename {
				os.Exit(85)
			}
			return nil
		}})
		t.Fatalf("file transaction returned instead of interrupting: %v", err)
	}

	binary := journeyBuiltBinary(t)
	project := newGitJourneyProject(t)
	runDocsGit(t, project.root, "-c", "user.name=Example", "-c", "user.email=example@example.org", "commit", "--allow-empty", "-qm", "Seed")
	worktrees := t.TempDir()
	owner := *project
	owner.root = filepath.Join(worktrees, "owner")
	runDocsGit(t, project.root, "worktree", "add", "--quiet", "--detach", owner.root)
	source := t.TempDir()
	pkg := newJourneyPackage(t, "local/retired-owner", "1.0.0")
	if err := dependency.ExtractPackageArchive(pkg.archive, source); err != nil {
		t.Fatal(err)
	}
	owner.runBinary(binary, 0, "install", "file:"+source, "--agent", "cursor", "--freshness", "none", "--non-interactive")
	owner.runBinary(binary, 0, "realize")
	owner.runBinary(binary, 0, "check")
	crash := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestBuiltPrepareExclusionRejectsMovedLocalOwner$")
	crash.Env = append(crash.Environ(), child+"="+owner.root)
	var exited *exec.ExitError
	if out, err := crash.CombinedOutput(); !errors.As(err, &exited) || exited.ExitCode() != 85 {
		t.Fatalf("failed to interrupt the authorized owner after state replacement: %v\n%s", err, out)
	}
	moved := filepath.Join(worktrees, "moved")
	runDocsGit(t, project.root, "worktree", "move", owner.root, moved)
	owner.root = moved
	for _, name := range []string{dependency.ProjectFilename, dependency.LockFilename} {
		if got := readProjectText(t, moved, name); !strings.Contains(got, "dependencies: []") {
			t.Fatalf("live %s still names a local dependency: %q", name, got)
		}
	}
	// Ignore the inert project claim left by the interrupted process. Its
	// removal on close is not a journal or project output mutation.
	beforeOwner := owner.snapshot()
	delete(beforeOwner, ".agents/.acr-transactions/.lock")
	beforeRepo := project.snapshot()
	gitInfo := project.path(".git/info")
	beforeShared := snapshotProjectTree(t, gitInfo)
	for _, format := range [][]string{nil, {"--json"}} {
		refusal := owner.runBinary(binary, 1, append([]string{"prepare-exclusion"}, format...)...)
		if len(format) != 0 && journeyError(t, refusal.stderr)["code"] != cli.CodeLocalSourceUnauthorized {
			t.Fatalf("wrong JSON refusal: %s", refusal.stderr)
		}
		if refusal.stdout != "" || !strings.Contains(refusal.stderr, cli.CodeLocalSourceUnauthorized) ||
			!strings.Contains(refusal.stderr, "preserve the owning worktree, journal and fence") ||
			!strings.Contains(refusal.stderr, "retry 'acr prepare-exclusion'") ||
			strings.Contains(refusal.stderr, "You can now remove it with Git") {
			t.Fatalf("preparation gave the wrong refusal or removal guidance: %s", refusal.output())
		}
		owner.assertUnchanged(beforeOwner, "moved owner refused before rollback")
		project.assertUnchanged(beforeRepo, "shared files and index after moved owner refusal")
		assertTreeUnchanged(t, beforeShared, gitInfo, "fence and shared images after moved owner refusal")
	}
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
			runDocsGit(t, project.root, "init", "-q")
			runDocsGit(t, project.root, "add", "--", "agents.yaml")
			before := project.snapshot()
			monitor := filepath.Join(t.TempDir(), "monitor")
			command := exec.CommandContext(t.Context(), binary, "realize", "--project", project.root)
			// The snapshot also runs Git. Only the CLI under test gets the
			// blocking fsmonitor configuration.
			command.Env = append(command.Environ(),
				"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0="+monitor)
			err, output := interruptGitInspection(t, command, monitor, test.signal)
			assertGitInspectionCLIError(t, err, output, "core.fsmonitor", "context canceled")
			project.assertUnchanged(before, "stalled realization")
		})
	}
}

func TestBuiltRealizeStopsAtFinalGitInspection(t *testing.T) {
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
	for _, name := range []string{"interrupt", "failed exit"} {
		t.Run(name, func(t *testing.T) {
			project := newJourneyProject(t, nil)
			runDocsGit(t, project.root, "init", "-q")
			writeProjectText(t, project.root, gitExcludePath, "keep-*.local\n")
			project.runBinary(binary, 0, "install", "file:"+source, "--agent", "cursor", "--freshness", "none", "--non-interactive")
			runDocsGit(t, project.root, "add", "--", "agents.yaml")
			before := project.snapshot()
			if _, err := os.Stat(project.path(".cursor/rules/acr__local__example__boundaries.mdc")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fixture must require a generated output write: %v", err)
			}

			shimDir := t.TempDir()
			const shim = `#!/bin/bash
set -euo pipefail
# The same receipt inspection runs earlier without the project claim.
if [[ "${1-}" == -C && "${5-}" == ls-files && "${8-}" == ":(icase,literal).agents/.acr-applied.json" && -f "$2/.agents/.acr-transactions/.lock" ]]; then
    if [[ -n "${ACR_TEST_GIT_HOLDER-}" ]]; then
        exec "$ACR_TEST_GIT_HOLDER"
    fi
    printf 'synthetic corrupt index: inspect index before retry\n' >&2
    exit 23
fi
exec "$ACR_TEST_REAL_GIT" "$@"
`
			if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), binary, "realize", "--project", project.root)
			command.Env = append(command.Environ(),
				"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"), "ACR_TEST_REAL_GIT="+git,
				"ACR_TEST_GIT_HOLDER=")
			const inspection = "inspect tracked receipt spellings"
			if name == "interrupt" {
				holder := filepath.Join(shimDir, "hold-git")
				command.Env = append(command.Env, "ACR_TEST_GIT_HOLDER="+holder)
				err, output := interruptGitInspection(t, command, holder, os.Interrupt)
				assertGitInspectionCLIError(t, err, output, inspection, "core.fsmonitor", "context canceled")
			} else {
				output, err := command.CombinedOutput()
				assertGitInspectionCLIError(t, err, string(output), inspection, "verify the Git worktree",
					"exit status 23", "synthetic corrupt index: inspect index before retry")
				if bytes.Contains(output, []byte("core.fsmonitor")) {
					t.Errorf("ordinary Git failure reported as a pipe stall: %s", output)
				}
			}
			project.assertUnchanged(before, "failed final Git inspection")
		})
	}
}

func assertGitInspectionCLIError(t *testing.T, err error, output string, want ...string) {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Errorf("realize = %v, want exit 1: %s", err, output)
	}
	for _, fragment := range append(want, "git ls-files", "retry") {
		if !strings.Contains(output, fragment) {
			t.Errorf("realize diagnostic %q is missing %q", output, fragment)
		}
	}
	if strings.Contains(output, "WaitDelay expired") {
		t.Errorf("expected signal or ordinary Git failure, got a pipe deadline: %s", output)
	}
}

func newGitInspectionHolder(t *testing.T, holder string) (hold, guard, lifetime *os.File) {
	t.Helper()
	holdPath, lifePath := holder+".hold", holder+".lifetime"
	for _, path := range []string{holdPath, lifePath} {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	open := func(path string, flag int) *os.File {
		f, err := os.OpenFile(path, flag, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				t.Error(err)
			}
		})
		return f
	}
	// The owner is the only hold writer. Its exit releases the holder.
	hold = open(holdPath, os.O_RDWR)
	// Keep the read side from seeing EOF before the helper opens its end.
	guard = open(lifePath, os.O_RDWR)
	lifetime = open(lifePath, os.O_RDONLY)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/bash
set -euo pipefail
ACR_TEST_GIT_HOLD_BASE="$0" exec '` + strings.ReplaceAll(binary, "'", `'\''`) + `' -test.run='^TestGitInspectionHolderProcess$'
`
	if err := os.WriteFile(holder, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return hold, guard, lifetime
}

func TestGitInspectionHolderProcess(t *testing.T) {
	base := os.Getenv("ACR_TEST_GIT_HOLD_BASE")
	if base == "" {
		return
	}
	// Open the lifetime endpoint without needing a surviving owner, so it
	// can carry holder failures as well as READY and EOF.
	life, err := os.OpenFile(base+".lifetime", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open holder lifetime: %v", err)
	}
	defer life.Close()
	fail := func(err error) {
		if _, writeErr := fmt.Fprintln(life, err); writeErr != nil {
			t.Errorf("report holder failure on lifetime FIFO: %v", writeErr)
		}
		t.Fatal(err)
	}
	// A blocking open would wait forever if the owner had already exited.
	// Switch the same read-only descriptor to blocking for the read so a
	// live owner's empty FIFO waits in the kernel, including on Darwin.
	hold, err := syscall.Open(base+".hold", syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		fail(fmt.Errorf("open holder hold FIFO: %w", err))
	}
	defer syscall.Close(hold)
	if err := syscall.SetNonblock(hold, false); err != nil {
		fail(fmt.Errorf("make holder hold FIFO blocking: %w", err))
	}
	if _, err := fmt.Fprintln(life, "READY"); err != nil {
		t.Fatalf("publish holder readiness: %v", err)
	}
	var data [1]byte
	for {
		n, err := syscall.Read(hold, data[:])
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			fail(fmt.Errorf("read holder hold FIFO: %w", err))
		}
		if n != 0 {
			fail(fmt.Errorf("unexpected holder hold data: %q", data[0]))
		}
		return // EOF is the only normal release.
	}
}

func TestGitInspectionHolderLifecycle(t *testing.T) {
	for _, name := range []string{"owner-exits-before-open", "owner-exits-after-ready", "unexpected-hold-data"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "holder")
			hold, guard, lifetime := newGitInspectionHolder(t, path)
			start := func(command *exec.Cmd) {
				t.Helper()
				if err := command.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if command.ProcessState != nil {
						return
					}
					if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
						t.Error(err)
					}
					var exitErr *exec.ExitError
					if err := command.Wait(); err != nil && !errors.As(err, &exitErr) {
						t.Error(err)
					}
				})
			}
			owner := exec.Command("/bin/cat")
			killOwner := func() {
				t.Helper()
				if err := owner.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				var exitErr *exec.ExitError
				if err := owner.Wait(); !errors.As(err, &exitErr) || exitErr.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
					t.Fatalf("hold owner = %v, want SIGKILL", err)
				}
			}
			if name != "unexpected-hold-data" {
				// The open stdin keeps the owner alive. Only its inherited
				// descriptor remains a writer once the parent closes hold.
				if _, err := owner.StdinPipe(); err != nil {
					t.Fatal(err)
				}
				owner.ExtraFiles = []*os.File{hold}
				start(owner)
				if err := hold.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if name == "owner-exits-before-open" {
				killOwner()
			}
			helper := exec.Command(path)
			var output bytes.Buffer
			helper.Stdout, helper.Stderr = &output, &output
			start(helper)
			reader := bufio.NewReader(lifetime)
			if line, err := reader.ReadString('\n'); line != "READY\n" || err != nil {
				t.Fatalf("holder readiness = %q, %v, want READY", line, err)
			}
			if err := guard.Close(); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "owner-exits-after-ready":
				killOwner()
			case "unexpected-hold-data":
				if _, err := hold.WriteString("x"); err != nil {
					t.Fatal(err)
				}
				var exitErr *exec.ExitError
				if err := helper.Wait(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
					t.Fatalf("holder = %v, want exit 1\n%s", err, output.String())
				}
				if line, err := reader.ReadString('\n'); line != "unexpected holder hold data: 'x'\n" || err != nil {
					t.Fatalf("holder failure event = %q, %v\n%s", line, err, output.String())
				}
			}
			if name != "unexpected-hold-data" {
				if err := helper.Wait(); err != nil {
					t.Fatalf("holder did not exit normally after owner death: %v\n%s", err, output.String())
				}
			}
			if line, err := reader.ReadString('\n'); line != "" || !errors.Is(err, io.EOF) {
				t.Fatalf("holder lifetime = %q, %v, want EOF", line, err)
			}
		})
	}
}

// Git does not inherit arbitrary descriptors from ACR. Use private FIFOs for
// readiness and lifetime so a helper can prove it is holding and has stopped
// independently of the Git and CLI output pipes.
func interruptGitInspection(t *testing.T, command *exec.Cmd, holder string, signal os.Signal) (error, string) {
	t.Helper()
	hold, guard, lifetime := newGitInspectionHolder(t, holder)
	outputFile, err := os.CreateTemp(filepath.Dir(holder), "acr-output-")
	if err != nil {
		t.Fatal(err)
	}
	defer outputFile.Close()
	command.Stdout, command.Stderr = outputFile, outputFile
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	output := func() string {
		t.Helper()
		content, err := os.ReadFile(outputFile.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}
	var waitErr error
	exited := make(chan struct{})
	go func() {
		waitErr = command.Wait()
		close(exited)
	}()
	defer func() {
		select {
		case <-exited:
		default:
			if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error(err)
			}
			<-exited
		}
	}()

	events := make(chan error, 2)
	go func() {
		defer close(events)
		reader := bufio.NewReader(lifetime)
		if line, err := reader.ReadString('\n'); err != nil || line != "READY\n" {
			events <- fmt.Errorf("helper readiness = %q, %v, want READY", line, err)
			return
		}
		events <- nil
		if line, err := reader.ReadString('\n'); line != "" || !errors.Is(err, io.EOF) {
			events <- fmt.Errorf("helper lifetime = %q, %v, want EOF", line, err)
			return
		}
		events <- nil
	}()
	defer func() {
		// Release only after the verdict, including on failure. Removing the
		// names prevents a late start from reopening either FIFO.
		if err := errors.Join(os.Remove(holder+".lifetime"), os.Remove(holder+".hold")); err != nil {
			t.Error(err)
		}
		if err := hold.Close(); err != nil {
			t.Error(err)
		}
		if err := guard.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Error(err)
		}
		for range events {
		}
	}()

	// The Go test runner bounds missing events.
	select {
	case err := <-events:
		if err != nil {
			t.Fatal(err)
		}
	case <-exited:
		t.Fatalf("realize exited before Git helper readiness: %v: %s", waitErr, output())
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Signal(signal); err != nil {
		t.Fatalf("send %v after Git helper readiness: %v", signal, err)
	}
	<-exited
	if err := <-events; err != nil {
		t.Fatalf("Git helper did not stop after %v while the hold was unreleased: %v\nrealize: %v: %s", signal, err, waitErr, output())
	}
	return waitErr, output()
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
