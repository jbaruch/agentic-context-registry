package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func validationFixture(t *testing.T) (*journeyProject, string) {
	t.Helper()
	project := newJourneyProject(t, nil)
	root := t.TempDir()
	pkg := newJourneyPackage(t, "independent/validation", "2.3.4")
	for _, f := range pkg.files {
		mode := os.FileMode(0o644)
		if f.executable {
			mode = 0o755
		}
		reverify2Put(t, root, f.path, f.body, mode)
	}
	return project, root
}

func journeyValidateSuccess(t *testing.T) int {
	binary := journeyBuiltBinary(t)
	project, root := validationFixture(t)
	before := snapshotProjectTree(t, root)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", t.TempDir())
	result := project.runBinary(binary, 0, "validate", root, "--json")
	body := journeyResult(t, result.stdout)
	if body["valid"] != true || body["name"] != "independent/validation" || body["version"] != "2.3.4" {
		t.Fatalf("validation result: %#v", body)
	}
	var files []string
	for _, name := range body["files"].([]any) {
		files = append(files, name.(string))
	}
	want := []string{"agent-plugin.yaml", "hooks/session-start.sh", "rules/boundaries.md", "rules/scoped.md", "skills/advocate/SKILL.md", "skills/advocate/references/guide.md", "skills/advocate/scripts/check.sh"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("distribution inventory: %v", files)
	}
	assertTreeUnchanged(t, before, root, "authored validate without Git or credentials")
	assertStateHomeEmpty(t, project)
	return 3
}

func journeyValidateRefusals(t *testing.T) int {
	project, root := validationFixture(t)
	if err := os.Remove(filepath.Join(root, "rules/boundaries.md")); err != nil {
		t.Fatal(err)
	}
	before := snapshotProjectTree(t, root)
	result := project.runOnPath(root, 1, "validate", "--json")
	if journeyError(t, result.stderr)["code"] != "path_not_found" {
		t.Fatalf("missing artifact: %s", result.stderr)
	}
	assertTreeUnchanged(t, before, root, "refused validation")
	reverify2Put(t, root, "rules/boundaries.md", "# restored\n", 0o644)
	if err := os.Symlink("../../../../outside", filepath.Join(root, "skills/advocate/escape")); err != nil {
		t.Fatal(err)
	}
	result = project.runOnPath(root, 1, "validate", "--json")
	if journeyError(t, result.stderr)["code"] != "invalid_skill_tree" {
		t.Fatalf("escaped support: %s", result.stderr)
	}
	result = project.runOnPath(t.TempDir(), 1, "validate", "--json")
	if journeyError(t, result.stderr)["code"] != "operation_failed" {
		t.Fatalf("missing manifest: %s", result.stderr)
	}
	project.runOnPath(root, 2, "validate", "--dry-run", "--json")
	return 4
}

func TestValidateCommand(t *testing.T) {
	t.Run("success", func(t *testing.T) { journeyValidateSuccess(t) })
	t.Run("refusals", func(t *testing.T) { journeyValidateRefusals(t) })
}
