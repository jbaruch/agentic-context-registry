package realizeapp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

// Exercise the actual no-agent Uninstall caller. Its state writer is the
// deterministic scheduling boundary: removal has observed the grant/store,
// but has not yet persisted its already-derived prune.
func TestLocalRemovalAbsentAuthorization(t *testing.T) {
	for _, parent := range []bool{false, true} {
		for _, interleave := range []bool{false, true} {
			name := "no-parent"
			if parent {
				name = "existing-parent"
			}
			if interleave {
				name += "/install-before-prune"
			} else {
				name += "/offline"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				project, source, store := t.TempDir(), t.TempDir(), t.TempDir()
				t.Setenv("ACR_STATE_HOME", store)
				manifest := "schemaVersion: 1\nname: owner/plugin\nversion: 1.0.0\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  rules:\n    - id: guidance\n      path: guidance.md\n      activation:\n        mode: always\n"
				for path, body := range map[string]string{"agent-plugin.yaml": manifest, "guidance.md": "original\n"} {
					if err := os.WriteFile(filepath.Join(source, path), []byte(body), 0644); err != nil {
						t.Fatal(err)
					}
				}
				resolver := dependency.NewResolver(nil)
				installer := dependency.NewService(resolver)
				if _, err := installer.InstallLocal(ctx, project, source, false); err != nil {
					t.Fatal(err)
				}
				before, err := dependency.LoadState(project)
				if err != nil {
					t.Fatal(err)
				}
				if parent {
					err = filepath.WalkDir(store, func(path string, d os.DirEntry, err error) error {
						if err != nil {
							return err
						}
						if strings.HasSuffix(path, ".json") {
							return os.Remove(path)
						}
						return nil
					})
				} else {
					err = os.RemoveAll(store)
				}
				if err != nil {
					t.Fatal(err)
				}
				if !interleave {
					if err := os.RemoveAll(source); err != nil {
						t.Fatal(err)
					}
				}
				service := NewService(resolver)
				installed, pruned := false, false
				service.writeState = func(root string, state dependency.State) error {
					if interleave {
						if _, err := installer.InstallLocal(ctx, project, source, false); err != nil {
							return err
						}
						installed = true
						// Prove the install finished before the stale prune, not afterward.
						pkg, cleanup, err := resolver.MaterializeLockedAt(ctx, project, before.Lock.Dependencies[0])
						_ = pkg
						if err != nil {
							return err
						}
						if err := cleanup(); err != nil {
							return err
						}
					}
					if err := dependency.WriteState(root, state); err != nil {
						return err
					}
					pruned = true
					return nil
				}
				_, removeErr := service.Uninstall(ctx, project, "github:owner/plugin", false)
				after, err := dependency.LoadState(project)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("parent=%v installCompleted=%v stalePruneCompleted=%v removal=%v", parent, installed, pruned, removeErr)
				if !interleave {
					if removeErr != nil || !pruned || len(after.Project.Dependencies) != 0 {
						t.Fatalf("offline removal: %+v %v", after, removeErr)
					}
					if !parent {
						if _, err := os.Lstat(store); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("offline removal created store: %v", err)
						}
					}
					return
				}
				if parent {
					if removeErr == nil || !strings.Contains(removeErr.Error(), "busy") || installed || pruned {
						t.Fatalf("uncoordinated removal: %v", removeErr)
					}
					if !reflect.DeepEqual(before, after) {
						t.Fatal("busy refusal changed project")
					}
				} else {
					if !installed || !pruned || len(after.Project.Dependencies) != 0 {
						t.Fatal("required install-before-stale-prune ordering missing")
					}
					if removeErr == nil || !strings.Contains(removeErr.Error(), "project operation completed") || !strings.Contains(removeErr.Error(), "concurrently") {
						t.Fatalf("missing truthful partial-state conflict: %v", removeErr)
					}
					_, cleanup, err := resolver.MaterializeLockedAt(ctx, project, before.Lock.Dependencies[0])
					if cleanup != nil {
						if e := cleanup(); e != nil {
							t.Fatal(e)
						}
					}
					if err != nil {
						t.Fatalf("concurrent writer's grant was destroyed: %v", err)
					}
				}
			})
		}
	}
}
