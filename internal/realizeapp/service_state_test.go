package realizeapp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestRunStateUsesCallerSuppliedState(t *testing.T) {
	project := t.TempDir()
	state := dependency.State{
		Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion, Agents: []string{"codex"}},
		Lock:    dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion},
	}
	result, err := NewService(noPackageLoader{}).RunState(context.Background(), project, state, nil, realize.ModeDryRun)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := result.Agents, []string{"codex"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("agents = %#v, want %#v", got, want)
	}
	if _, err := os.Stat(filepath.Join(project, dependency.ProjectFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run wrote caller state: %v", err)
	}
}

func TestEmptyPlanStillWritesState(t *testing.T) {
	project := t.TempDir()
	state := dependency.State{
		Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion, Agents: []string{"codex"}, Freshness: "none"},
		Lock:    dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion},
	}
	result, err := NewService(noPackageLoader{}).RunState(context.Background(), project, state, nil, realize.ModeApply)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Plan.HasChanges() || result.Plan.LedgerChanged {
		t.Fatalf("state-only plan = %#v", result.Plan)
	}
	loaded, err := dependency.LoadState(project)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Project.Agents, []string{"codex"}) || loaded.Project.Freshness != "none" {
		t.Fatalf("persisted project = %#v", loaded.Project)
	}
}
