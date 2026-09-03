package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerify8Round3ValidateArtifactsRefusesEveryLinkShapeInDirFS pins NEW-2 past
// the leaf case: an intermediate directory link, a link that escapes the DirFS
// root, and a skill directory reached through a link must all refuse, because
// each is a different branch of validateFilesystemPathFS and only the leaf
// branch was covered.
func TestVerify8Round3ValidateArtifactsRefusesEveryLinkShapeInDirFS(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name  string
		build func(t *testing.T, root, outside string)
		value Manifest
	}{
		{
			name: "intermediate directory link",
			build: func(t *testing.T, root, _ string) {
				writeTestFile(t, root, "real/guide.md", "# Guide\n")
				if err := os.Symlink("real", filepath.Join(root, "rules")); err != nil {
					t.Fatal(err)
				}
			},
			value: Manifest{
				SchemaVersion: CurrentSchemaVersion, Name: "example/intermediate", Version: "1.0.0",
				Artifacts: Artifacts{Rules: []RuleArtifact{{
					ID: "guide", Path: "rules/guide.md",
					Activation: RuleActivation{Mode: ActivationAlways},
				}}},
			},
		},
		{
			name: "leaf link escaping the FS root",
			build: func(t *testing.T, root, outside string) {
				if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("# Secret\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(root, "rules"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "rules", "escape.md")); err != nil {
					t.Fatal(err)
				}
			},
			value: Manifest{
				SchemaVersion: CurrentSchemaVersion, Name: "example/escape", Version: "1.0.0",
				Artifacts: Artifacts{Rules: []RuleArtifact{{
					ID: "escape", Path: "rules/escape.md",
					Activation: RuleActivation{Mode: ActivationAlways},
				}}},
			},
		},
		{
			name: "skill directory reached through a link",
			build: func(t *testing.T, root, _ string) {
				writeTestFile(t, root, "real/review/SKILL.md", "# Review\n")
				if err := os.MkdirAll(filepath.Join(root, "skills"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join("..", "real", "review"), filepath.Join(root, "skills", "review")); err != nil {
					t.Fatal(err)
				}
			},
			value: Manifest{
				SchemaVersion: CurrentSchemaVersion, Name: "example/skilllink", Version: "1.0.0",
				Artifacts: Artifacts{Skills: []SkillArtifact{{ID: "review", Path: "skills/review"}}},
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			outside := t.TempDir()
			testCase.build(t, root, outside)

			err := ValidateArtifacts(os.DirFS(root), testCase.value)
			var problems *ValidationErrors
			if !errors.As(err, &problems) || !problems.Has(CodeInvalidArtifactType) {
				t.Fatalf("ValidateArtifacts() = %v, want %s", err, CodeInvalidArtifactType)
			}
			if !strings.Contains(err.Error(), "contains a symbolic link") {
				t.Fatalf("ValidateArtifacts() = %q, want the symbolic-link refusal", err)
			}
		})
	}
}

// TestVerify8Round3ValidateArtifactsStillAcceptsARealTree keeps the refusal
// above from passing vacuously: the same shapes without links must validate.
func TestVerify8Round3ValidateArtifactsStillAcceptsARealTree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, root, "rules/guide.md", "# Guide\n")
	writeTestFile(t, root, "skills/review/SKILL.md", "# Review\n")
	value := Manifest{
		SchemaVersion: CurrentSchemaVersion, Name: "example/real", Version: "1.0.0",
		Artifacts: Artifacts{
			Rules:  []RuleArtifact{{ID: "guide", Path: "rules/guide.md", Activation: RuleActivation{Mode: ActivationAlways}}},
			Skills: []SkillArtifact{{ID: "review", Path: "skills/review"}},
		},
	}

	if err := ValidateArtifacts(os.DirFS(root), value); err != nil {
		t.Fatalf("ValidateArtifacts() on a link-free tree = %v", err)
	}
}
