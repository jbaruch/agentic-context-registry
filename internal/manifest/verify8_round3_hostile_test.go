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

func TestListedValidationDoesNotWalkExcludedEntriesAndPropagatesListingFailure(t *testing.T) {
	root := t.TempDir()
	value := Manifest{SchemaVersion: 1, Name: "example/listed", Version: "1.0.0", Source: Source{Repository: "https://github.com/example/listed"}, Artifacts: Artifacts{Skills: []SkillArtifact{{ID: "worker", Path: "skills/worker"}}}}
	writeTestFile(t, root, Filename, "schemaVersion: 1\nname: example/listed\nversion: 1.0.0\nsource:\n  repository: https://github.com/example/listed\nartifacts:\n  skills:\n    - id: worker\n      path: skills/worker\n")
	writeTestFile(t, root, "skills/worker/SKILL.md", "# Worker")
	if err := os.Symlink("absent", filepath.Join(root, "skills/worker/ignored")); err != nil {
		t.Fatal(err)
	}
	list := func(relative string, directory bool) ([]string, error) {
		if directory {
			return []string{"skills/worker/SKILL.md"}, nil
		}
		return []string{relative}, nil
	}
	if _, _, err := LoadListed(root, list); err != nil {
		t.Fatalf("excluded link inspected: %v", err)
	}
	cause := errors.New("provider unavailable")
	failed := func(relative string, directory bool) ([]string, error) {
		if directory {
			return nil, cause
		}
		return []string{relative}, nil
	}
	if _, _, err := LoadListed(root, failed); !errors.Is(err, cause) {
		t.Fatalf("operational error rewritten: %v", err)
	}
	if err := Validate(root, value); err == nil {
		t.Fatal("legacy full walker stopped checking unselected links")
	}
}

func TestListedValidationRejectsSelectedAncestorLinks(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, Filename, "schemaVersion: 1\nname: example/listed\nversion: 1.0.0\nsource:\n  repository: https://github.com/example/listed\nartifacts:\n  skills:\n    - id: worker\n      path: skills/worker\n")
	writeTestFile(t, root, "skills/worker/SKILL.md", "# Worker")
	writeTestFile(t, root, "real/support.md", "support")
	if err := os.Symlink("../../real", filepath.Join(root, "skills/worker/linked")); err != nil {
		t.Fatal(err)
	}
	list := func(relative string, directory bool) ([]string, error) {
		if directory {
			return []string{"skills/worker/SKILL.md", "skills/worker/linked/support.md"}, nil
		}
		return []string{relative}, nil
	}
	_, _, err := LoadListed(root, list)
	var invalid *ValidationErrors
	if !errors.As(err, &invalid) || !invalid.Has(CodeInvalidSkillTree) || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("selected ancestor = %v", err)
	}
}
