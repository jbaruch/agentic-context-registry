package dependency

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// A child exits after both project edits, before journal retirement and grant
// activation. The retry uses the built production CLI, not a recovery mock.
func TestLocalInterruptedRefreshRecovery(t *testing.T) {
	if os.Getenv("ACR_LOCAL_RECOVERY_CRASH_CHILD") == "1" {
		project, source := os.Getenv("ACR_LOCAL_RECOVERY_PROJECT"), os.Getenv("ACR_LOCAL_RECOVERY_SOURCE")
		before, err := LoadState(project)
		if err != nil {
			t.Fatal(err)
		}
		_, locked, cleanup, err := snapshotLocal(source)
		if err != nil {
			t.Fatal(err)
		}
		if err = cleanup(); err != nil {
			t.Fatal(err)
		}
		locked.Path = source
		desired := cloneState(before)
		desired.Lock.Dependencies = []LockedDependency{locked}
		projectData, lockData, err := MarshalState(desired)
		if err != nil {
			t.Fatal(err)
		}
		var edits []realize.FileTransactionEdit
		for i, name := range []string{ProjectFilename, LockFilename} {
			data, err := os.ReadFile(filepath.Join(project, name))
			if err != nil {
				t.Fatal(err)
			}
			after := projectData
			if i == 1 {
				after = lockData
			}
			edits = append(edits, realize.FileTransactionEdit{Path: name, Operation: "splice", Before: data, BeforeMode: 0644, After: after, AfterMode: 0644})
		}
		err = changeLocalAuthorization(project, locked.Source, &localAuthorization{SchemaVersion: 1, Path: source, SourceRoot: source}, func() error {
			return realize.ApplyFileTransactionWithHooks(project, edits, nil, realize.FileTransactionHooks{TransactionID: func() (string, error) { return "tx-local-refresh-crash", nil }, AfterEdit: func(i int, _ realize.FileTransactionEdit) error {
				if i == 1 {
					os.Exit(77)
				}
				return nil
			}})
		})
		t.Fatalf("crash helper returned: %v", err)
	}
	project, source, service, _ := localFixture(t)
	if _, err := service.InstallLocal(context.Background(), project, source, false); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(project)
	if err != nil {
		t.Fatal(err)
	}
	state.Project.Agents = []string{"codex"}
	state.Project.Freshness = "none"
	if err = WriteState(project, state); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "guidance.md"), []byte("edited for recovery\n"), 0644); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestLocalInterruptedRefreshRecovery$")
	child.Env = append(os.Environ(), "ACR_LOCAL_RECOVERY_CRASH_CHILD=1", "ACR_LOCAL_RECOVERY_PROJECT="+project, "ACR_LOCAL_RECOVERY_SOURCE="+source)
	output, err := child.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 77 {
		t.Fatalf("helper: %s %v", output, err)
	}
	crashed, err := LoadState(project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authorizeLocal(project, crashed.Project.Dependencies[0]); err == nil {
		t.Fatal("crash left active authorization")
	}
	binary := filepath.Join(t.TempDir(), "acr")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/acr")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %s %v", output, err)
	}
	install := exec.Command(binary, "install", "file:"+source, "--project", project)
	output, err = install.CombinedOutput()
	t.Logf("retry install err=%v output=%s", err, output)
	if err != nil {
		t.Fatal(err)
	}
	repaired, err := LoadState(project)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Lock.Dependencies[0].ContentHash != crashed.Lock.Dependencies[0].ContentHash {
		t.Fatal("retry retained recovered before-state instead of refreshed hash")
	}
	if _, err := authorizeLocal(project, repaired.Project.Dependencies[0]); err != nil {
		t.Fatalf("retry failed to activate authorization: %v", err)
	}
	for _, verb := range []string{"realize", "check"} {
		command := exec.Command(binary, verb, "--project", project)
		output, err := command.CombinedOutput()
		if err != nil || strings.Contains(string(output), "pending_transaction") {
			t.Fatalf("%s after explicit retry: %s %v", verb, output, err)
		}
	}
	if got := readTestFile(t, filepath.Join(source, "guidance.md")); got != "edited for recovery\n" {
		t.Fatalf("source mutated: %q", got)
	}
}
