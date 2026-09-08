package migrate

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveGitignoreBlockKeepsOneBlankLine(t *testing.T) {
	content := []byte("# user policy\n/build/\n\n# === Tessl-generated artifacts (managed by tessl) ===\n.tessl/cache/\n# === end Tessl-generated artifacts ===\n\n*.tmp\n")
	want := []byte("# user policy\n/build/\n\n*.tmp\n")

	got, removed := removeGitignoreBlock(content)
	if !removed || !bytes.Equal(got, want) {
		t.Fatalf("removeGitignoreBlock() = %q, %t, want %q, true", got, removed, want)
	}
}

// planNativeFixture writes one installed Tessl package, one per-agent native
// link with the stored target under test, and returns the plan planning
// builds from the bytes it reads.
func planNativeFixture(t *testing.T, target string, extra func(t *testing.T, root string)) FinalizePlan {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "tessl.json", []byte("{}\n"), 0o644)
	writeFile(t, root, ".tessl/plugins/example/orphan/skills/review/SKILL.md", []byte("# Review\n"), 0o644)
	if extra != nil {
		extra(t, root)
	}
	if err := os.MkdirAll(filepath.Join(root, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, ".claude", "skills", "tessl__review")); err != nil {
		t.Fatal(err)
	}
	inventory := Report{Packages: []PackageReport{{
		Name:          "example/orphan",
		TesslIdentity: "example/orphan",
		Artifacts: []ArtifactReport{{
			ID: "review", Kind: kindSkill, Classification: classMigratable,
			Natives: []string{".claude/skills/tessl__review"},
		}},
	}}}
	plan, err := PlanFinalization(openSnapshot(t, root), inventory)
	if err != nil {
		t.Fatalf("plan finalization: %v", err)
	}
	return plan
}

func plannedEdit(plan FinalizePlan, filename string) (FinalizeEdit, bool) {
	for _, edit := range plan.Edits {
		if edit.Path == filename {
			return edit, true
		}
	}
	return FinalizeEdit{}, false
}

// TestPlanningRetiresOnlyAProvenNativeDestination is the pure half of N2. The
// matching basename is the name Tessl writes; the destination the link
// actually stores is the evidence, and planning reads it from the same bytes
// the transaction accepts as its before-image.
func TestPlanningRetiresOnlyAProvenNativeDestination(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name   string
		target string
		extra  func(t *testing.T, root string)
		owned  bool
	}{
		{
			name:   "the canonical link into this package's plugin tree",
			target: "../../.tessl/plugins/example/orphan/skills/review",
			owned:  true,
		},
		{
			name:   "a relative repoint at a user tree",
			target: "../../team/skills/review",
			extra: func(t *testing.T, root string) {
				writeFile(t, root, "team/skills/review/SKILL.md", []byte("# Team survives\n"), 0o644)
			},
		},
		{
			name:   "an absolute destination the pure layer cannot place",
			target: "/tmp/somewhere/skills/review",
		},
		{
			name:   "a destination inside another package's plugin tree",
			target: "../../.tessl/plugins/example/other/skills/review",
			extra: func(t *testing.T, root string) {
				writeFile(t, root, ".tessl/plugins/example/other/skills/review/SKILL.md", []byte("# Other\n"), 0o644)
			},
		},
		{
			name:   "a stored shortcut/.. the kernel follows somewhere else",
			target: "../../shortcut/../.tessl/plugins/example/orphan/skills/review",
			extra: func(t *testing.T, root string) {
				writeFile(t, root, "team/stage/keep.md", []byte("staged\n"), 0o644)
				if err := os.Symlink("team/stage", filepath.Join(root, "shortcut")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			plan := planNativeFixture(t, testCase.target, testCase.extra)
			edit, planned := plannedEdit(plan, ".claude/skills/tessl__review")
			if testCase.owned {
				if !planned || edit.Operation != "delete" || edit.LinkTarget != testCase.target {
					t.Fatalf("edit = %+v, planned=%t; want a delete bound to %q", edit, planned, testCase.target)
				}
				if len(plan.Unowned) != 0 {
					t.Fatalf("a proven destination was recorded as unowned: %#v", plan.Unowned)
				}
				return
			}
			if planned {
				t.Fatalf("an unproven destination was scheduled for deletion: %+v", edit)
			}
			if len(plan.Unowned) != 1 || plan.Unowned[0].Path != ".claude/skills/tessl__review" ||
				plan.Unowned[0].Target != testCase.target || plan.Unowned[0].Reason != reasonNativeUnowned {
				t.Fatalf("unowned = %#v", plan.Unowned)
			}
			retained := false
			for _, record := range plan.Retained {
				if record.Path == ".claude/skills/tessl__review" && record.Reason == reasonNativeUnowned {
					retained = true
				}
			}
			if !retained {
				t.Fatalf("retained = %#v", plan.Retained)
			}
		})
	}
}

// TestPlanningRebindsANativeToTheTargetItReads is the changed-target half.
// Two plans over the same inventory disagree exactly when the stored bytes
// changed between them, so a link repointed after it was inventoried is
// retained instead of deleted under evidence that no longer describes it.
func TestPlanningRebindsANativeToTheTargetItReads(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "tessl.json", []byte("{}\n"), 0o644)
	writeFile(t, root, ".tessl/plugins/example/orphan/skills/review/SKILL.md", []byte("# Review\n"), 0o644)
	writeFile(t, root, "team/skills/review/SKILL.md", []byte("# Team survives\n"), 0o644)
	native := filepath.Join(root, ".claude", "skills", "tessl__review")
	if err := os.MkdirAll(filepath.Dir(native), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../.tessl/plugins/example/orphan/skills/review", native); err != nil {
		t.Fatal(err)
	}
	inventory := Report{Packages: []PackageReport{{
		Name:          "example/orphan",
		TesslIdentity: "example/orphan",
		Artifacts: []ArtifactReport{{
			ID: "review", Kind: kindSkill, Classification: classMigratable,
			Natives: []string{".claude/skills/tessl__review"},
		}},
	}}}

	before, err := PlanFinalization(openSnapshot(t, root), inventory)
	if err != nil {
		t.Fatal(err)
	}
	if _, planned := plannedEdit(before, ".claude/skills/tessl__review"); !planned {
		t.Fatalf("the canonical link was not planned: %#v", before.Edits)
	}

	if err := os.Remove(native); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../team/skills/review", native); err != nil {
		t.Fatal(err)
	}

	after, err := PlanFinalization(openSnapshot(t, root), inventory)
	if err != nil {
		t.Fatal(err)
	}
	if edit, planned := plannedEdit(after, ".claude/skills/tessl__review"); planned {
		t.Fatalf("the repointed link was still scheduled for deletion: %+v", edit)
	}
	if len(after.Unowned) != 1 || after.Unowned[0].Target != "../../team/skills/review" {
		t.Fatalf("unowned = %#v", after.Unowned)
	}
}
