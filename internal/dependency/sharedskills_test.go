package dependency

import (
	"strings"
	"testing"
)

// TestGradedSchemaKeepsFeatureFreeProjectsReadable is D30: adding sharedSkills
// to the vocabulary never upgrades a project that does not declare it.
func TestGradedSchemaKeepsFeatureFreeProjectsReadable(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		project string
		want    int
	}{
		{name: "baseline", project: "schemaVersion: 2\n", want: BaselineSchemaVersion},
		{
			name:    "vendor",
			project: "schemaVersion: 3\ndependencies:\n  - source: vendor:example/pkg\n    requested: vendored\n",
			want:    VendorSchemaVersion,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeStateFixture(t, root, testCase.project, "schemaVersion: 2\n")
			state, err := LoadState(root)
			if err != nil {
				t.Fatal(err)
			}
			if state.Project.SchemaVersion != testCase.want {
				t.Fatalf("project schemaVersion = %d, want %d", state.Project.SchemaVersion, testCase.want)
			}
			if state.Project.SharedSkills {
				t.Fatal("sharedSkills was invented for a project that does not declare it")
			}
			projectData, _, err := MarshalState(state)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(projectData), "sharedSkills") {
				t.Fatalf("agents.yaml gained a sharedSkills field:\n%s", projectData)
			}
		})
	}
}

// TestSharedSkillsRequiresItsOwnSchemaVersion is D31: a project that declares
// the surface under an older stamp is refused loudly, so no older ACR reads it
// as a file it understands and silently drops the declaration.
func TestSharedSkillsRequiresItsOwnSchemaVersion(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeStateFixture(t, root, "schemaVersion: 3\nsharedSkills: true\n", "schemaVersion: 2\n")
	_, err := LoadState(root)
	if err == nil || !strings.Contains(err.Error(), "schemaVersion 4") {
		t.Fatalf("LoadState() error = %v, want the required schema version named", err)
	}

	writeStateFixture(t, root, "schemaVersion: 4\nsharedSkills: true\n", "schemaVersion: 2\n")
	state, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Project.SharedSkills || state.Project.SchemaVersion != SharedSkillsSchemaVersion {
		t.Fatalf("state = %+v", state.Project)
	}
	projectData, _, err := MarshalState(state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(projectData), "sharedSkills: true") {
		t.Fatalf("agents.yaml lost the declaration:\n%s", projectData)
	}
}

// TestAnUnknownSchemaVersionRefusesLoudly is D31 from the reader's side. An
// ACR that predates a version reports the same refusal this one gives a
// version it does not know, so a v4 project reaching a v3 binary is a loud
// upgrade instruction, never a silently discarded declaration.
func TestAnUnknownSchemaVersionRefusesLoudly(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeStateFixture(t, root, "schemaVersion: 5\n", "schemaVersion: 2\n")
	_, err := LoadState(root)
	if err == nil || !strings.Contains(err.Error(), "upgrade acr") {
		t.Fatalf("LoadState() error = %v, want an upgrade instruction", err)
	}
}
