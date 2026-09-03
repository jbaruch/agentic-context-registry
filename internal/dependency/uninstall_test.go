package dependency

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

const (
	prunedSource  = "github:owner/plugin"
	retainedSourc = "github:owner/sibling"
)

func prunableState() State {
	return State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Agents: []string{"codex"}, Dependencies: []Declaration{
			{Source: prunedSource, Requested: "latest", Hold: &Hold{Pin: "v1.0.0", Rejected: "v2.0.0"}},
			{Source: retainedSourc, Requested: "v3.0.0"},
		}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{
			{
				Source: prunedSource, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 1, Tag: "v1.0.0",
				Commit: strings.Repeat("a", 40), PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("0", 64),
				Hold: &LockHold{RejectedTag: "v2.0.0"},
			},
			{
				Source: retainedSourc, Requested: "v3.0.0", Kind: ResolutionRelease, ReleaseID: 2, Tag: "v3.0.0",
				Commit: strings.Repeat("b", 40), PackageVersion: "3.0.0", ContentHash: "sha256:" + strings.Repeat("1", 64),
			},
		}},
	}
}

func TestPruneDependencyDropsTheDeclarationHoldAndLockRow(t *testing.T) {
	t.Parallel()

	state := prunableState()
	before := cloneState(state)

	pruned, removed, err := PruneDependency(state, prunedSource)
	if err != nil {
		t.Fatal(err)
	}
	if removed == nil || removed.Source != prunedSource || removed.Hold == nil {
		t.Fatalf("PruneDependency() removed = %#v, want the held lock row", removed)
	}
	if len(pruned.Project.Dependencies) != 1 || pruned.Project.Dependencies[0].Source != retainedSourc {
		t.Fatalf("pruned declarations = %#v", pruned.Project.Dependencies)
	}
	if len(pruned.Lock.Dependencies) != 1 || pruned.Lock.Dependencies[0].Source != retainedSourc {
		t.Fatalf("pruned lock rows = %#v", pruned.Lock.Dependencies)
	}
	if pruned.Project.Agents[0] != "codex" || pruned.Project.Freshness != state.Project.Freshness {
		t.Fatalf("pruned project selections = %#v", pruned.Project)
	}
	if !reflect.DeepEqual(before, state) {
		t.Fatalf("PruneDependency() mutated the caller's state: %#v", state)
	}
}

func TestPruneDependencyRefusesUndeclaredAndInvalidSources(t *testing.T) {
	t.Parallel()

	state := prunableState()

	_, _, err := PruneDependency(state, "github:owner/missing")
	var notDeclared *NotDeclaredError
	if !errors.As(err, &notDeclared) {
		t.Fatalf("PruneDependency(undeclared) error = %v, want *NotDeclaredError", err)
	}
	_, _, err = PruneDependency(state, "vendor:workspace/package")
	if !errors.As(err, &notDeclared) {
		t.Fatalf("PruneDependency(vendor:) error = %v, want *NotDeclaredError", err)
	}
	if _, _, err := PruneDependency(state, "workspace/package"); err == nil || !strings.Contains(err.Error(), "github:owner/repository or vendor:workspace/package") {
		t.Fatalf("PruneDependency(invalid) error = %v, want canonical-source guidance", err)
	}
}

func TestResumeAndUpdateRefuseUndeclaredAsUsage(t *testing.T) {
	t.Parallel()

	for _, command := range []string{"resume", "update"} {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			args := []string{command, "github:owner/missing", "--project", t.TempDir(), "--json"}
			exitCode := cli.New(&stdout, &stderr, NewApplication(&fakeGitHub{}), cli.Build{Version: "test"}).Run(context.Background(), args)

			if exitCode != cli.ExitUsage {
				t.Fatalf("Run(%s) exit = %d, want %d", command, exitCode, cli.ExitUsage)
			}
			if stdout.Len() != 0 {
				t.Fatalf("Run(%s) stdout = %q, want empty", command, stdout.String())
			}
			if !strings.Contains(stderr.String(), `"code":"dependency_not_declared"`) || !strings.Contains(stderr.String(), "acr list") {
				t.Fatalf("Run(%s) stderr = %q, want a dependency_not_declared refusal naming acr list", command, stderr.String())
			}
		})
	}
}
