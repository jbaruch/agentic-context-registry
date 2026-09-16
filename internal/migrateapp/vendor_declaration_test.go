package migrateapp

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/dependencytest"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
)

func TestRemigrationPreservesMappedDeclarationExtensions(t *testing.T) {
	for _, sourceKind := range []string{"vendor", "github"} {
		for _, extension := range []struct {
			name  string
			extra map[string]any
		}{
			{name: "no-extension"},
			{name: "scalar", extra: map[string]any{"acceptance": "preserve-me"}},
			{name: "nested", extra: map[string]any{"acceptance": map[string]any{"reviewed": true, "owners": []any{"caller", "team"}, "count": 2}}},
		} {
			t.Run(sourceKind+"/"+extension.name, func(t *testing.T) {
				root := writeUnmappedConsumer(t)
				remote := remigrationRemote(t)
				source := "vendor:example/orphan"
				args := []string{"migrate", "tessl", "--vendor-unmapped", "--non-interactive", "--json", "--project", root}
				if sourceKind == "github" {
					root = seedConsumer(t)
					source = "github:example/alpha"
					args = []string{"migrate", "tessl", "--map", "example/alpha=github:example/alpha@v1.0.0", "--non-interactive", "--json", "--project", root}
				}
				retained := seedRetainedState(t, root, remote)
				app := NewApplication(remote, "test")
				requireMigrationSuccess(t, app, args...)
				state := loadRemigrationState(t, root)
				for i := range state.Project.Dependencies {
					if state.Project.Dependencies[i].Source == source {
						state.Project.Dependencies[i].Extra = extension.extra
					}
				}
				state.Project.Extra = map[string]any{"caller-project": "keep"}
				state.Lock.Extra = map[string]any{"caller-lock": "keep"}
				if err := dependency.WriteState(root, state); err != nil {
					t.Fatal(err)
				}
				state = loadRemigrationState(t, root)
				writeFile(t, root, "notes.md", []byte("Caller notes.\n"), 0o640)
				if stdout, stderr, exit := runCLI(t, app, "list", "--json", "--project", root); exit != 0 {
					t.Fatalf("list: %d %s %s", exit, stdout, stderr)
				}
				before := hashTreeWithModes(t, root)
				invocation := cli.Invocation{Command: cli.CommandMigrate, Subcommand: "tessl", ProjectDirectory: root, DryRun: true, VendorUnmapped: true}
				if sourceKind == "github" {
					invocation.Mappings = []string{"example/alpha=github:example/alpha@v1.0.0"}
				}
				preview, err := app.Execute(context.Background(), invocation)
				if err != nil {
					t.Fatal(err)
				}
				report := preview.Value.(migrate.MigrationReport)
				if !reflect.DeepEqual(state.Project, report.Project) {
					t.Errorf("preview lost declaration state: got=%#v want=%#v", report.Project, state.Project)
				}
				for _, phase := range []string{"preview", "apply", "repeat"} {
					phaseArgs := append([]string(nil), args...)
					if phase == "preview" {
						phaseArgs = append(phaseArgs, "--dry-run")
					}
					output := requireMigrationSuccess(t, app, phaseArgs...)
					if !strings.Contains(output, `"wrote":false`) {
						t.Errorf("%s claimed writes: %s", phase, output)
					}
					after := loadRemigrationState(t, root)
					if !reflect.DeepEqual(state.Project, after.Project) || !reflect.DeepEqual(state.Lock, after.Lock) {
						t.Errorf("%s changed state: before=%#v after=%#v", phase, state.Project.Dependencies, after.Project.Dependencies)
					}
					if !mapsEqual(before, hashTreeWithModes(t, root)) {
						t.Errorf("%s changed project bytes or modes", phase)
					}
					assertRetained(t, retained, root)
				}
			})
		}
	}
}

func loadRemigrationState(t *testing.T, root string) dependency.State {
	t.Helper()
	state, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func requireMigrationSuccess(t *testing.T, app cli.Application, args ...string) string {
	t.Helper()
	stdout, stderr, exit := runCLI(t, app, args...)
	if exit != cli.ExitSuccess || stderr != "" {
		t.Fatalf("%v: exit=%d stdout=%s stderr=%s", args, exit, stdout, stderr)
	}
	return stdout
}

func remigrationRemote(t *testing.T) *dependencytest.Remote {
	t.Helper()
	remote := dependencytest.NewRemote()
	for name, archive := range retainedRemote(t).archives {
		source := "github:" + name
		remote.Latest[source] = dependency.Release{ID: 1, Tag: "v1.0.0"}
		remote.Commits[source+"@v1.0.0"] = strings.Repeat("a", 40)
		remote.Commits[source+"@"+strings.Repeat("a", 40)] = strings.Repeat("a", 40)
		remote.Archives[source+"@"+strings.Repeat("a", 40)] = archive
	}
	return remote
}
