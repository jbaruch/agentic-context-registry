package dependency

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestStateRoundTripIsDeterministicAndPreservesExtensions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	state := State{
		Project: Project{
			SchemaVersion: CurrentSchemaVersion,
			Dependencies: []Declaration{
				{Source: "github:zeta/plugin", Requested: "latest"},
				{Source: "github:alpha/plugin", Requested: "v1.2.3"},
			},
			Extra: map[string]any{"freshness": "outdated"},
		},
		Lock: Lockfile{
			SchemaVersion: CurrentSchemaVersion,
			Dependencies: []LockedDependency{
				{Source: "github:zeta/plugin", Requested: "latest", Kind: ResolutionRelease, ReleaseID: 2, Tag: "v2.0.0", Commit: strings.Repeat("b", 40), PackageVersion: "2.0.0", ContentHash: "sha256:" + strings.Repeat("b", 64)},
				{Source: "github:alpha/plugin", Requested: "v1.2.3", Kind: ResolutionRelease, ReleaseID: 1, Tag: "v1.2.3", Commit: strings.Repeat("a", 40), PackageVersion: "1.2.3", ContentHash: "sha256:" + strings.Repeat("a", 64)},
			},
			Extra: map[string]any{"ownership": map[string]any{"version": 1}},
		},
	}

	if err := WriteState(root, state); err != nil {
		t.Fatalf("WriteState() error = %v", err)
	}
	firstProject := readTestFile(t, filepath.Join(root, ProjectFilename))
	firstLock := readTestFile(t, filepath.Join(root, filepath.FromSlash(LockFilename)))
	if strings.Index(firstProject, "github:alpha/plugin") > strings.Index(firstProject, "github:zeta/plugin") {
		t.Fatalf("agents.yaml dependencies are not sorted:\n%s", firstProject)
	}

	loaded, err := LoadState(root)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if got := loaded.Project.Extra["freshness"]; got != "outdated" {
		t.Fatalf("project extension = %#v, want outdated", got)
	}
	if _, ok := loaded.Lock.Extra["ownership"]; !ok {
		t.Fatalf("lock extensions = %#v, want ownership", loaded.Lock.Extra)
	}
	if err := WriteState(root, loaded); err != nil {
		t.Fatalf("second WriteState() error = %v", err)
	}
	if got := readTestFile(t, filepath.Join(root, ProjectFilename)); got != firstProject {
		t.Fatalf("agents.yaml changed across round trip:\nfirst:\n%s\nsecond:\n%s", firstProject, got)
	}
	if got := readTestFile(t, filepath.Join(root, filepath.FromSlash(LockFilename))); got != firstLock {
		t.Fatalf("lockfile changed across round trip:\nfirst:\n%s\nsecond:\n%s", firstLock, got)
	}
}

func TestLoadStateRejectsDuplicateSources(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	contents := "schemaVersion: 1\ndependencies:\n  - source: github:owner/plugin\n    requested: latest\n  - source: github:owner/plugin\n    requested: v1.0.0\n"
	if err := os.WriteFile(filepath.Join(root, ProjectFilename), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadState(root)
	if err == nil || !strings.Contains(err.Error(), "declared more than once") {
		t.Fatalf("LoadState() error = %v, want duplicate guidance", err)
	}
}

func TestLoadStateRejectsSymlinkedState(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "target.yaml")
	if err := os.WriteFile(target, []byte("schemaVersion: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, ProjectFilename)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := LoadState(root)
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("LoadState() error = %v, want regular-file guidance", err)
	}
}

func TestDependencySchemasMatchGoContracts(t *testing.T) {
	t.Parallel()

	projectSchema := compileDependencySchema(t, "agents.schema.json")
	lockSchema := compileDependencySchema(t, "registry-lock.schema.json")
	project := Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{Source: "github:owner/plugin", Requested: "latest"}}}
	lock := Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
		Source: "github:owner/plugin", Requested: "latest", Kind: ResolutionRelease,
		ReleaseID: 1, Tag: "v1.0.0", Commit: strings.Repeat("a", 40), PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("b", 64),
	}}}

	if err := validateDependencySchema(t, projectSchema, project); err != nil {
		t.Fatalf("agents schema rejected valid project: %v", err)
	}
	if err := validateDependencySchema(t, lockSchema, lock); err != nil {
		t.Fatalf("lock schema rejected valid lock: %v", err)
	}
	lock.Dependencies[0].Commit = "short"
	if err := validateState(project, lock); err == nil {
		t.Fatal("validateState() accepted short lock commit")
	}
	if err := validateDependencySchema(t, lockSchema, lock); err == nil {
		t.Fatal("lock schema accepted short commit")
	}
}

func readTestFile(t *testing.T, filename string) string {
	t.Helper()
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func compileDependencySchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return a filename")
	}
	schemaPath := filepath.Join(filepath.Dir(filename), "..", "..", "schemas", name)
	schema, err := jsonschema.NewCompiler().Compile(schemaPath)
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return schema
}

func validateDependencySchema(t *testing.T, schema *jsonschema.Schema, value any) error {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var instance any
	if err := json.Unmarshal(encoded, &instance); err != nil {
		t.Fatal(err)
	}
	return schema.Validate(instance)
}
