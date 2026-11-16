package realizeapp

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

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
