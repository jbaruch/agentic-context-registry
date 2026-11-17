package realizeapp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestFrozenLocalLicenseLockWithRealLoader(t *testing.T) {
	// Same manifest, bytes and frozen pre-upgrade hash as dependency's
	// license_compatibility_test.go, computed by 1894b7d's v1 hasher.
	const frozenHash = "sha256:9ad9979f91a1f15f020e7a47af71f1f4900f8424a3d49dca68f65f603060c351"
	const frozenManifest = "schemaVersion: 1\nname: owner/plugin\nversion: 1.2.3\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  rules:\n    - id: guidance\n      path: rules/guidance.md\n      activation:\n        mode: always\n"
	for _, agent := range []string{"claude-code", "codex", "cursor"} {
		for _, kind := range []string{"symlink", "fifo"} {
			t.Run(agent+"/"+kind, func(t *testing.T) {
				t.Setenv("ACR_STATE_HOME", t.TempDir())
				project, source := t.TempDir(), t.TempDir()
				writeFixture(t, filepath.Join(source, manifest.Filename), []byte(frozenManifest), 0o644)
				writeFixture(t, filepath.Join(source, "rules/guidance.md"), []byte("Use deterministic tests.\n"), 0o644)
				ctx := context.Background()
				resolver := dependency.NewResolver(nil)
				// Bootstrap the machine-local authorization before introducing a
				// notice. No install or refresh occurs in the upgrade exercise.
				if _, err := dependency.NewService(resolver).InstallLocal(ctx, project, source, false); err != nil {
					t.Fatal(err)
				}
				state, err := dependency.LoadState(project)
				if err != nil {
					t.Fatal(err)
				}
				locked := dependency.LockedDependency{Source: "github:owner/plugin", Requested: dependency.RequestedLocal, Kind: dependency.ResolutionLocal, Path: source, PackageVersion: "1.2.3", ContentHash: frozenHash}
				state.Lock.Dependencies = []dependency.LockedDependency{locked}
				if err := dependency.WriteState(project, state); err != nil {
					t.Fatal(err)
				}
				service := NewService(resolver)
				if _, err := service.Run(ctx, project, []string{agent}, realize.ModeApply); err != nil {
					t.Fatal(err)
				}
				baseline, ledger := snapshotClaudeUpgrade(t, project), projectLedger(t, project)
				if len(ledger.Targets) == 0 {
					t.Fatal("baseline has no native targets")
				}
				notice := filepath.Join(source, "LICENSE")
				if kind == "symlink" {
					// Opening the dangling external target would fail.
					err = os.Symlink(filepath.Join(t.TempDir(), "missing"), notice)
				} else {
					// No writer: opening the actual FIFO would block.
					err = syscall.Mkfifo(notice, 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
				pkg, cleanup, err := resolver.MaterializeLockedAt(ctx, project, locked)
				if err != nil {
					t.Fatalf("materialize frozen lock: %v", err)
				}
				if _, err := os.Lstat(filepath.Join(pkg.Root, "LICENSE")); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("implicit notice entered snapshot: %v", err)
				}
				if err := cleanup(); err != nil {
					t.Fatal(err)
				}
				if result, err := service.Run(ctx, project, []string{agent}, realize.ModeApply); err != nil || result.Plan.HasChanges() {
					t.Fatalf("real-loader upgrade realization = %#v, %v", result, err)
				}
				if checked, err := service.Run(ctx, project, []string{agent}, realize.ModeCheck); err != nil || checked.Plan.HasChanges() {
					t.Fatalf("second check = %#v, %v", checked, err)
				}
				after, err := dependency.LoadState(project)
				if err != nil || !reflect.DeepEqual(after.Lock.Dependencies, []dependency.LockedDependency{locked}) {
					t.Fatalf("frozen lock refreshed: %#v, %v", after.Lock.Dependencies, err)
				}
				if !reflect.DeepEqual(baseline, snapshotClaudeUpgrade(t, project)) || !reflect.DeepEqual(ledger, projectLedger(t, project)) {
					t.Fatal("implicit notice changed native bytes, modes, lock bytes or ledger")
				}
			})
		}
	}
}

func TestImplicitRootLicenseLeavesEveryNativeOutputAndLedgerUnchanged(t *testing.T) {
	t.Parallel()
	for _, agent := range []string{"claude-code", "codex", "cursor"} {
		t.Run(agent, func(t *testing.T) {
			_, packageRoot, state, value := realizationFixture(t)
			value.Artifacts.Skills = []manifest.SkillArtifact{{ID: "helper", Path: "skills/helper"}}
			writeFixture(t, filepath.Join(packageRoot, "skills/helper/SKILL.md"), []byte("---\nname: helper\ndescription: Test helper.\n---\n# Helper\n"), 0o644)
			writeFixture(t, filepath.Join(packageRoot, "skills/helper/LICENSE"), []byte("Skill support notice.\n"), 0o644)
			service := NewService(fixtureLoader{root: packageRoot, manifest: value})
			var baseline map[string]struct {
				Content string
				Mode    os.FileMode
			}
			var baselineLedger realize.Ledger
			for _, withNotice := range []bool{false, true} {
				project := t.TempDir()
				if withNotice {
					writeFixture(t, filepath.Join(packageRoot, "LICENSE"), []byte("Implicit root notice.\n"), 0o644)
				}
				if err := dependency.WriteState(project, state); err != nil {
					t.Fatal(err)
				}
				if _, err := service.Run(context.Background(), project, []string{agent}, realize.ModeApply); err != nil {
					t.Fatal(err)
				}
				ledger := projectLedger(t, project)
				snapshot := snapshotClaudeUpgrade(t, project)
				if withNotice {
					if !reflect.DeepEqual(snapshot, baseline) || !reflect.DeepEqual(ledger, baselineLedger) {
						t.Fatal("implicit notice changed native bytes, modes or realization ledger")
					}
				} else {
					baseline, baselineLedger = snapshot, ledger
				}
				foundSupport := false
				for name, file := range snapshot {
					if filepath.Base(name) == "LICENSE" {
						if !strings.Contains(filepath.ToSlash(name), "helper/LICENSE") || file.Content != "Skill support notice.\n" {
							t.Fatalf("unexpected native license %s: %q", name, file.Content)
						}
						foundSupport = true
					}
				}
				if !foundSupport {
					t.Fatal("declared skill license stopped rendering")
				}
				if checked, err := service.Run(context.Background(), project, []string{agent}, realize.ModeCheck); err != nil || checked.Plan.HasChanges() {
					t.Fatalf("second realization check = %#v, %v", checked, err)
				}
				if !reflect.DeepEqual(snapshot, snapshotClaudeUpgrade(t, project)) {
					t.Fatal("check mutated native files or ledger")
				}
			}
		})
	}
}
