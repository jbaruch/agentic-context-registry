package manifest

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestCheckedInExamplesValidate(t *testing.T) {
	t.Parallel()

	schemaPath := filepath.Join(repositoryRoot(t), "schemas", "agent-plugin.schema.json")
	compiler := jsonschema.NewCompiler()
	schema, err := compiler.Compile(schemaPath)
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}

	for _, example := range []string{"minimal", "complete"} {
		example := example
		t.Run(example, func(t *testing.T) {
			t.Parallel()

			packageRoot := filepath.Join(repositoryRoot(t), "examples", example)
			value, err := Load(packageRoot)
			if err != nil {
				t.Fatalf("Load(%s): %v", example, err)
			}

			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("marshal %s manifest: %v", example, err)
			}
			var instance any
			if err := json.Unmarshal(encoded, &instance); err != nil {
				t.Fatalf("decode %s JSON instance: %v", example, err)
			}
			if err := schema.Validate(instance); err != nil {
				t.Fatalf("validate %s against JSON Schema: %v", example, err)
			}
		})
	}
}

func TestCompleteExamplePackageFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join(repositoryRoot(t), "examples", "complete")
	value, err := Load(root)
	if err != nil {
		t.Fatalf("Load(complete): %v", err)
	}
	files, err := PackageFiles(root, value)
	if err != nil {
		t.Fatalf("PackageFiles(complete): %v", err)
	}

	want := []string{
		"agent-plugin.yaml",
		"hooks/session-start.sh",
		"hooks/stop.sh",
		"rules/go-guidance.md",
		"scripts/format-go.sh",
		"skills/review-change/SKILL.md",
		"skills/review-change/check.sh",
		"skills/review-change/references/review-guide.md",
	}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("PackageFiles(complete) = %#v, want %#v", files, want)
	}
}

func TestLoadRejectsSemanticFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		manifestYAML string
		code         ErrorCode
	}{
		{
			name:         "unsupported schema version",
			manifestYAML: strings.Replace(validManifest, "schemaVersion: 1", "schemaVersion: 2", 1),
			code:         CodeUnsupportedSchema,
		},
		{
			name: "duplicate IDs across classes",
			manifestYAML: validManifest + `  scripts:
    - id: project-guidance
      path: scripts/tool.sh
`,
			code: CodeDuplicateArtifactID,
		},
		{
			name:         "missing file",
			manifestYAML: strings.Replace(validManifest, "rules/project-guidance.md", "rules/missing.md", 1),
			code:         CodePathNotFound,
		},
		{
			name:         "parent traversal",
			manifestYAML: strings.Replace(validManifest, "rules/project-guidance.md", "../outside.md", 1),
			code:         CodeInvalidPath,
		},
		{
			name: "skill path is a file",
			manifestYAML: `schemaVersion: 1
name: example/test-plugin
version: 1.0.0
source:
  repository: https://github.com/example/test-plugin
artifacts:
  skills:
    - id: review-change
      path: rules/project-guidance.md
`,
			code: CodeInvalidArtifactType,
		},
		{
			name: "unsupported hook event",
			manifestYAML: validManifest + `  hooks:
    - id: before-model
      event: before-model
      path: hooks/check.sh
`,
			code: CodeUnsupportedHookEvent,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := writeTestPackage(t, test.manifestYAML)
			_, err := Load(root)
			if err == nil {
				t.Fatal("Load() error = nil, want validation failure")
			}
			var validationErrors *ValidationErrors
			if !errors.As(err, &validationErrors) {
				t.Fatalf("Load() error = %T %v, want *ValidationErrors", err, err)
			}
			if !validationErrors.Has(test.code) {
				t.Fatalf("Load() errors = %v, want code %q", validationErrors.Issues, test.code)
			}
		})
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	root := writeTestPackage(t, validManifest+"unexpected: true\n")
	_, err := Load(root)
	if err == nil {
		t.Fatal("Load() error = nil, want unknown-field failure")
	}
	if !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("Load() error = %q, want unknown-field diagnostic", err)
	}
}

func TestLoadReportsNewerSchemaBeforeUnknownFields(t *testing.T) {
	t.Parallel()

	manifestYAML := strings.Replace(validManifest, "schemaVersion: 1", "schemaVersion: 2\nfutureField: true", 1)
	root := writeTestPackage(t, manifestYAML)
	_, err := Load(root)
	if err == nil {
		t.Fatal("Load() error = nil, want unsupported-schema failure")
	}
	var validationErrors *ValidationErrors
	if !errors.As(err, &validationErrors) || !validationErrors.Has(CodeUnsupportedSchema) {
		t.Fatalf("Load() error = %v, want %q", err, CodeUnsupportedSchema)
	}
}

func TestLoadRequiresSchemaVersion(t *testing.T) {
	t.Parallel()

	root := writeTestPackage(t, strings.Replace(validManifest, "schemaVersion: 1\n", "", 1))
	_, err := Load(root)
	if err == nil {
		t.Fatal("Load() error = nil, want required-field failure")
	}
	var validationErrors *ValidationErrors
	if !errors.As(err, &validationErrors) || !validationErrors.Has(CodeRequired) {
		t.Fatalf("Load() error = %v, want %q", err, CodeRequired)
	}
}

func TestPackageFilesExcludesUndeclaredFiles(t *testing.T) {
	t.Parallel()

	manifestYAML := `schemaVersion: 1
name: example/test-plugin
version: 1.0.0
source:
  repository: https://github.com/example/test-plugin
artifacts:
  skills:
    - id: review-change
      path: skills/review-change
`
	root := writeTestPackage(t, manifestYAML)
	writeTestFile(t, root, "skills/review-change/SKILL.md", "# Review Change\n")
	writeTestFile(t, root, "skills/review-change/references/guide.md", "# Guide\n")
	writeTestFile(t, root, "notes.txt", "not published\n")

	value, err := Load(root)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	files, err := PackageFiles(root, value)
	if err != nil {
		t.Fatalf("PackageFiles(): %v", err)
	}

	want := []string{
		"agent-plugin.yaml",
		"skills/review-change/SKILL.md",
		"skills/review-change/references/guide.md",
	}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("PackageFiles() = %#v, want %#v", files, want)
	}
}

func TestLoadRejectsSymlinkedArtifact(t *testing.T) {
	t.Parallel()

	root := writeTestPackage(t, strings.Replace(validManifest, "rules/project-guidance.md", "rules/link.md", 1))
	if err := os.Symlink("project-guidance.md", filepath.Join(root, "rules", "link.md")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	_, err := Load(root)
	if err == nil {
		t.Fatal("Load() error = nil, want symlink failure")
	}
	var validationErrors *ValidationErrors
	if !errors.As(err, &validationErrors) || !validationErrors.Has(CodeInvalidArtifactType) {
		t.Fatalf("Load() error = %v, want %q", err, CodeInvalidArtifactType)
	}
}

func TestSemanticVersions(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"0.0.0", "1.2.3", "1.2.3-rc.1", "1.2.3+build.7", "1.2.3-rc.1+build.7", "123456789012345678901234567890.2.3"} {
		if !isSemver(version) {
			t.Errorf("isSemver(%q) = false, want true", version)
		}
	}
	for _, version := range []string{"", "1", "1.2", "01.2.3", "1.2.3-01", "1.2.3+", "1.2.3+bad!"} {
		if isSemver(version) {
			t.Errorf("isSemver(%q) = true, want false", version)
		}
	}
}

const validManifest = `schemaVersion: 1
name: example/test-plugin
version: 1.0.0
source:
  repository: https://github.com/example/test-plugin
artifacts:
  rules:
    - id: project-guidance
      path: rules/project-guidance.md
      activation:
        mode: always
`

func writeTestPackage(t *testing.T, manifestYAML string) string {
	t.Helper()

	root := t.TempDir()
	writeTestFile(t, root, Filename, manifestYAML)
	writeTestFile(t, root, "rules/project-guidance.md", "# Project Guidance\n")
	writeTestFile(t, root, "scripts/tool.sh", "#!/usr/bin/env bash\n")
	writeTestFile(t, root, "hooks/check.sh", "#!/usr/bin/env bash\n")
	return root
}

func writeTestFile(t *testing.T, root, relative, content string) {
	t.Helper()

	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatalf("create parent directory for %s: %v", relative, err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", relative, err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) did not return a filename")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}
