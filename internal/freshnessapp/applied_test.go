package freshnessapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"github.com/jbaruch/agentic-context-registry/internal/realizeapp"
)

func TestFreshnessConvergedAppliedEvidenceMatrix(t *testing.T) {
	for _, evidence := range []string{"missing", "stale", "invalid", "missing-ignore", "wrong-mode"} {
		t.Run(evidence, func(t *testing.T) {
			project := t.TempDir()
			state := dependency.State{
				Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion, Agents: []string{"codex"}, Freshness: "none"},
				Lock:    dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion},
			}
			if err := dependency.WriteState(project, state); err != nil {
				t.Fatal(err)
			}
			realizer := realizeapp.NewService(nil)
			if _, err := realizer.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
				t.Fatal(err)
			}
			filename := filepath.Join(project, ".agents/.acr-applied.json")
			if err := os.Chmod(filename, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(project, ".agents/.gitignore"), 0o640); err != nil {
				t.Fatal(err)
			}
			switch evidence {
			case "missing-ignore":
				if err := os.Remove(filepath.Join(project, ".agents/.gitignore")); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(filename); err != nil {
					t.Fatal(err)
				}
			case "stale":
				if err := os.WriteFile(filename, []byte(`{"schema_version":1,"ledger":{"schemaVersion":2}}`), 0o644); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				if err := os.WriteFile(filename, []byte(`{"schema_version":99}`), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			readState := func() map[string]realize.StateFile {
				files := map[string]realize.StateFile{}
				for _, path := range []string{dependency.ProjectFilename, dependency.LockFilename, ".agents/.gitignore", ".agents/.acr-applied.json"} {
					file, exists, err := realize.ReadStateFile(project, path)
					if err != nil {
						t.Fatal(err)
					}
					if exists {
						files[path] = file
					}
				}
				return files
			}
			before := readState()

			for _, policy := range []freshness.Policy{freshness.PolicyNone, freshness.PolicyOutdated} {
				runner := NewRunner(freshness.Store{BaseDirectory: t.TempDir()}, func() time.Time { return runnerNow }, &fakeOutdatedChecker{})
				result, err := runner.Run(context.Background(), project, policy)
				if err != nil || len(result.Notices) != 0 || result.RestartRequired || !reflect.DeepEqual(before, readState()) {
					t.Fatalf("read-only freshness %s reported or wrote evidence drift: %v, %#v", policy, err, result)
				}
			}
			for _, mode := range []realize.Mode{realize.ModeCheck, realize.ModeDryRun} {
				result, err := realizer.Run(context.Background(), project, nil, mode)
				if err != nil || result.Plan.HasChanges() || !reflect.DeepEqual(before, readState()) {
					t.Fatalf("%s reported drift or wrote auxiliary state: %v, %#v", mode, err, result)
				}
			}
			// Install-mode freshness invokes a successful apply. Its auxiliary
			// maintenance is permitted, but it must never trigger a restart.
			runner := NewRunner(freshness.Store{BaseDirectory: t.TempDir()}, func() time.Time { return runnerNow }, &fakeOutdatedChecker{}).WithInstall(&fakeReconciler{}, realizer)
			result, err := runner.Run(context.Background(), project, freshness.PolicyInstall)
			if err != nil || result.RestartRequired || len(result.RestartAgents) != 0 || len(result.Notices) != 0 {
				t.Fatalf("evidence-only successful apply requested a restart: %v, %#v", err, result)
			}
			after := readState()
			var receipt struct {
				SchemaVersion int `json:"schema_version"`
			}
			if err := json.Unmarshal(after[".agents/.acr-applied.json"].Content, &receipt); err != nil || receipt.SchemaVersion != 2 {
				t.Fatalf("install did not repair receipt: %v, %#v", err, receipt)
			}
			ignore, exists := after[".agents/.gitignore"]
			wantMode := uint32(0o640)
			if evidence == "missing-ignore" {
				wantMode = 0o644
			}
			if !exists || !strings.HasSuffix(string(ignore.Content), "/.acr-applied.json\n") || ignore.Mode != wantMode {
				t.Fatalf("install did not maintain ignore rule and permissions: %#v", ignore)
			}
			delete(before, ".agents/.gitignore")
			delete(after, ".agents/.gitignore")
			delete(before, ".agents/.acr-applied.json")
			delete(after, ".agents/.acr-applied.json")
			if !reflect.DeepEqual(before, after) {
				t.Fatal("evidence-only freshness apply changed product state")
			}
			converged := readState()
			identities := map[string]os.FileInfo{}
			for _, path := range []string{".agents/.acr-applied.json", ".agents/.gitignore"} {
				info, err := os.Stat(filepath.Join(project, path))
				if err != nil {
					t.Fatal(err)
				}
				identities[path] = info
			}
			for rerun := 0; rerun < 2; rerun++ {
				// A fresh timer store exercises apply again instead of taking the
				// matching-policy throttle shortcut.
				reconciler := &fakeReconciler{}
				runner := NewRunner(freshness.Store{BaseDirectory: t.TempDir()}, func() time.Time { return runnerNow }, &fakeOutdatedChecker{}).WithInstall(reconciler, realizer)
				result, err := runner.Run(context.Background(), project, freshness.PolicyInstall)
				if err != nil || result.Throttled || reconciler.calls != 1 || result.RestartRequired || len(result.RestartAgents) != 0 || len(result.Notices) != 0 {
					t.Fatalf("converged install rerun = %v, %#v, reconcile calls=%d", err, result, reconciler.calls)
				}
				if !reflect.DeepEqual(converged, readState()) {
					t.Fatal("converged install changed state bytes or modes")
				}
				for path, before := range identities {
					after, err := os.Stat(filepath.Join(project, path))
					if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
						t.Fatalf("converged install replaced or changed auxiliary %s: %v", path, err)
					}
				}
			}
		})
	}
}
