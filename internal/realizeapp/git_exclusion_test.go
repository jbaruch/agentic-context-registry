package realizeapp

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestPrepareExclusionClearanceAndLocalGrant(t *testing.T) {
	const child = "ACR_TEST_PREPARE_GRANT_OWNER"
	const journalID = "prepare-grant-fixture"
	const stateFile = "state.txt"
	if owner := os.Getenv(child); owner != "" {
		desired := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{
			{Path: "output.md", Ownership: realize.OwnershipGenerated},
		}}
		err := realize.ApplyFileTransactionWithGitExclusion(t.Context(), owner, desired, func(realize.Ledger) ([]realize.FileTransactionEdit, error) {
			return []realize.FileTransactionEdit{{Path: stateFile, Operation: "splice",
				Before: []byte("before\n"), BeforeMode: 0o644, After: []byte("after\n"), AfterMode: 0o644}}, nil
		}, func() error {
			os.Exit(85)
			return nil
		}, realize.FileTransactionHooks{TransactionID: func() (string, error) { return journalID, nil }})
		t.Fatalf("transaction did not reach interruption: %v", err)
	}

	repo, source, worktrees := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("ACR_STATE_HOME", t.TempDir())
	git := func(project string, args ...string) string {
		t.Helper()
		return gitFixtureCommand(t, project, append([]string{"-c", "user.name=Example", "-c", "user.email=example@example.org"}, args...)...)
	}
	git(repo, "init", "-q")
	git(repo, "commit", "--allow-empty", "-qm", "Seed")
	owner, moved := filepath.Join(worktrees, "owner"), filepath.Join(worktrees, "moved")
	git(repo, "worktree", "add", "-q", "--detach", owner)
	writeFixture(t, filepath.Join(source, "agent-plugin.yaml"), []byte("schemaVersion: 1\nname: owner/plugin\nversion: 1.0.0\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  rules:\n    - id: guidance\n      path: guidance.md\n      activation:\n        mode: always\n"), 0o644)
	writeFixture(t, filepath.Join(source, "guidance.md"), []byte("Example rule\n"), 0o644)
	installer := dependency.NewService(dependency.NewResolver(nil))
	if _, err := installer.InstallLocal(t.Context(), owner, source, false); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(owner, stateFile), []byte("before\n"), 0o644)
	git(owner, "add", "-f", "agents.yaml", ".agents/registry.lock", stateFile)
	git(owner, "commit", "-qm", "Record owner inputs")
	crash := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPrepareExclusionClearanceAndLocalGrant$")
	crash.Env = append(crash.Environ(), child+"="+owner)
	var exited *exec.ExitError
	if out, err := crash.CombinedOutput(); !errors.As(err, &exited) || exited.ExitCode() != 85 {
		t.Fatalf("transaction did not interrupt: %v\n%s", err, out)
	}
	read := func(filename string) string {
		t.Helper()
		data, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	app := NewApplication(nil)
	const clearance = "You can now remove it with Git"
	refuse := func(project, diagnostic string) {
		t.Helper()
		for _, jsonOutput := range []bool{false, true} {
			args := []string{"prepare-exclusion", "--project", project}
			if jsonOutput {
				args = append(args, "--json")
			}
			stdout, stderr, code := runCLI(t, app, args...)
			if code == cli.ExitSuccess || stdout != "" || !strings.Contains(stderr, diagnostic) || strings.Contains(stderr, clearance) {
				t.Fatalf("expected refusal without clearance (JSON %t), got exit %d, stdout %q, stderr %q", jsonOutput, code, stdout, stderr)
			}
			if jsonOutput {
				var envelope struct {
					OK     *bool           `json:"ok"`
					Result json.RawMessage `json:"result"`
				}
				if err := json.Unmarshal([]byte(stderr), &envelope); err != nil || envelope.OK == nil || *envelope.OK || len(envelope.Result) != 0 {
					t.Fatalf("expected only a JSON error, got %q: %v", stderr, err)
				}
			}
		}
	}
	fence := filepath.Join(repo, ".git/info/acr-exclusion-journal")
	original := read(fence)
	const txRoot = ".agents/.acr-transactions"
	manifest := read(filepath.Join(owner, txRoot, journalID, "manifest.json"))
	git(repo, "worktree", "move", owner, moved)
	refuse(moved, cli.CodeLocalSourceUnauthorized)
	if read(filepath.Join(moved, stateFile)) != "after\n" || read(fence) != original ||
		read(filepath.Join(moved, txRoot, journalID, "manifest.json")) != manifest {
		t.Fatal("refusal changed the moved owner's live state, journal or fence")
	}

	git(repo, "worktree", "move", moved, owner)
	prepare := func() {
		t.Helper()
		stdout, stderr, code := runCLI(t, app, "prepare-exclusion", "--project", owner, "--json")
		if code != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, clearance) {
			t.Fatalf("missing removal clearance: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	}
	prepare()
	proof := read(fence)
	if os.Geteuid() != 0 {
		cleanup := filepath.Join(owner, txRoot, ".staging-complete-"+journalID)
		blocked := filepath.Join(cleanup, "blocked")
		writeFixture(t, filepath.Join(blocked, "keep"), []byte("incomplete cleanup\n"), 0o600)
		if err := os.Chmod(blocked, 0o500); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if err := os.Chmod(blocked, 0o700); err != nil {
					t.Error(err)
				}
			}()
			refuse(owner, cleanup)
		}()
		prepare()
	}
	if read(fence) != proof || read(filepath.Join(owner, stateFile)) != "before\n" {
		t.Fatal("cleanup retry changed the proof or restored state")
	}
	git(repo, "worktree", "remove", owner)
}
