package main

import (
	"fmt"
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

func TestValidateEmptyMappingKeys(t *testing.T) {
	binary := journeyBuiltBinary(t)
	const header = "'schemaVersion': 1\nname: independent/validation\nversion: 2.3.4\n"
	const source = "source:\n  \"repository\": https://github.com/independent/validation\n"
	const explicitSource = "source: &metadata\n  ?\n    # repository key\n\n    repository\n  : https://github.com/independent/validation\n"
	const artifacts = "artifacts:\n  rules:\n    - id: boundaries\n      path: rules/boundaries.md\n      activation: {mode: always}\n"
	for _, fixture := range []struct{ name, content, detail string }{
		{"root", header + source + artifacts + ": ignored\n", "yaml: line 10: did not find expected key"},
		{"nested", header + source + "  : ignored\n" + artifacts, "yaml: line 4: did not find expected key"},
		{"anchored nested", header + "source: &metadata\n  repository: https://github.com/independent/validation\n  : ignored\n" + artifacts, "yaml: line 4: did not find expected key"},
		{"anchored multiline explicit", header + explicitSource + "  : ignored\n" + artifacts, "yaml: line 4: did not find expected key"},
		{"flow", header + "source: {: ignored, repository: https://github.com/independent/validation}\n" + artifacts, "yaml: line 3: did not find expected node content"},
		{"quoted", header + source + artifacts, ""},
		{"written multiline explicit", header + explicitSource + artifacts, ""},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			project, root := validationFixture(t)
			path := filepath.Join(root, "agent-plugin.yaml")
			reverify2Put(t, root, "agent-plugin.yaml", fixture.content, 0o644)
			before := snapshotProjectTree(t, root)
			if fixture.detail == "" {
				result := project.runBinary(binary, 0, "validate", root, "--json")
				if journeyResult(t, result.stdout)["valid"] != true || result.stderr != "" {
					t.Fatalf("written keys: stdout=%q stderr=%q", result.stdout, result.stderr)
				}
			} else {
				message := fmt.Sprintf("validate package: decode %s: %s; retry the command, then report the failure at https://github.com/jbaruch/agentic-context-registry/issues if it persists", path, fixture.detail)
				json := project.runBinary(binary, 1, "validate", root, "--json")
				wantJSON := fmt.Sprintf("{\"ok\":false,\"command\":\"validate\",\"error\":{\"code\":\"operation_failed\",\"message\":%q}}\n", message)
				if json.stdout != "" || json.stderr != wantJSON {
					t.Fatalf("JSON refusal: stdout=%q stderr=%q, want %q", json.stdout, json.stderr, wantJSON)
				}
				plain := project.runBinary(binary, 1, "validate", root)
				if plain.stdout != "" || plain.stderr != "acr validate: "+message+"\n" {
					t.Fatalf("text refusal: stdout=%q stderr=%q, want %q", plain.stdout, plain.stderr, message)
				}
			}
			assertTreeUnchanged(t, before, root, "validation")
		})
	}
}
