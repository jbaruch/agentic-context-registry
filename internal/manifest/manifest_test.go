package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
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

	schema := compileManifestSchema(t)

	for _, example := range []string{"minimal", "complete"} {
		example := example
		t.Run(example, func(t *testing.T) {
			t.Parallel()

			packageRoot := filepath.Join(repositoryRoot(t), "examples", example)
			value, err := Load(packageRoot)
			if err != nil {
				t.Fatalf("Load(%s): %v", example, err)
			}

			if err := validateManifestSchema(t, schema, value); err != nil {
				t.Fatalf("validate %s against JSON Schema: %v", example, err)
			}
		})
	}
}

func TestValidateArtifactsAllowsSynthesizedIdentity(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, root, "rules/project-guidance.md", "# Guidance\n")
	value := Manifest{
		SchemaVersion: CurrentSchemaVersion,
		Name:          "example/orphan",
		Version:       "not-semver",
		Artifacts: Artifacts{Rules: []RuleArtifact{{
			ID: "project-guidance", Path: "rules/project-guidance.md",
			Activation: RuleActivation{Mode: ActivationAlways},
		}}},
	}
	if err := ValidateArtifacts(os.DirFS(root), value); err != nil {
		t.Fatalf("ValidateArtifacts() rejected synthesized manifest: %v", err)
	}
	if err := Validate(root, value); err == nil {
		t.Fatal("Validate() accepted non-publishable synthesized identity")
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

func TestLoadRejectsSymlinkedManifestBeforeReadingTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	targetRoot := t.TempDir()
	target := filepath.Join(targetRoot, "outside.yaml")
	if err := os.WriteFile(target, []byte("not: [valid YAML"), 0o644); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, Filename)); err != nil {
		t.Fatalf("create manifest symlink: %v", err)
	}

	_, err := Load(root)
	if err == nil {
		t.Fatal("Load() error = nil, want symlink failure")
	}
	var validationErrors *ValidationErrors
	if !errors.As(err, &validationErrors) {
		t.Fatalf("Load() error = %T %v, want *ValidationErrors", err, err)
	}
	if len(validationErrors.Issues) != 1 || validationErrors.Issues[0].Code != CodeInvalidArtifactType {
		t.Fatalf("Load() errors = %v, want one %q failure", validationErrors.Issues, CodeInvalidArtifactType)
	}
}

func TestManifestReadRejectsSymlinkSwapBeforeOpen(t *testing.T) {
	t.Parallel()

	root := writeTestPackage(t, validManifest)
	targetRoot := t.TempDir()
	target := filepath.Join(targetRoot, "outside.yaml")
	if err := os.WriteFile(target, []byte("outside package content"), 0o644); err != nil {
		t.Fatalf("write replacement target: %v", err)
	}

	packageRoot, err := os.OpenRoot(root)
	if err != nil {
		t.Fatalf("open package root: %v", err)
	}
	t.Cleanup(func() {
		if err := packageRoot.Close(); err != nil {
			t.Errorf("close package root: %v", err)
		}
	})

	replacingRoot := &manifestReplacingRoot{
		Root: packageRoot,
		beforeOpen: func() error {
			manifestPath := filepath.Join(root, Filename)
			if err := os.Remove(manifestPath); err != nil {
				return fmt.Errorf("remove manifest: %w", err)
			}
			if err := os.Symlink(target, manifestPath); err != nil {
				return fmt.Errorf("replace manifest with symlink: %w", err)
			}
			return nil
		},
	}

	contents, err := readManifestFromRoot(replacingRoot, root)
	if err == nil {
		t.Fatalf("readManifestFromRoot() = %q, nil; want symlink failure", contents)
	}
	var validationErrors *ValidationErrors
	if !errors.As(err, &validationErrors) || !validationErrors.Has(CodeInvalidArtifactType) {
		t.Fatalf("readManifestFromRoot() error = %v, want %q", err, CodeInvalidArtifactType)
	}
}

func TestArtifactPathValidationMatchesJSONSchema(t *testing.T) {
	t.Parallel()

	schema := compileManifestSchema(t)
	root := writeTestPackage(t, validManifest)
	base, err := Load(root)
	if err != nil {
		t.Fatalf("Load(valid manifest): %v", err)
	}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "package relative", path: "rules/project-guidance.md", want: true},
		{name: "drive relative", path: "C:rules/project-guidance.md", want: false},
		{name: "drive absolute", path: "C:/rules/project-guidance.md", want: false},
		{name: "lowercase drive", path: "c:/rules/project-guidance.md", want: false},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			value := base
			rule := value.Artifacts.Rules[0]
			rule.Path = test.path
			value.Artifacts.Rules = []RuleArtifact{rule}

			assertManifestValidity(t, schema, root, value, test.path, test.want)
		})
	}
}

func TestRuleActivationValidationMatchesJSONSchema(t *testing.T) {
	t.Parallel()

	schema := compileManifestSchema(t)
	root := writeTestPackage(t, validManifest)
	base, err := Load(root)
	if err != nil {
		t.Fatalf("Load(valid manifest): %v", err)
	}

	tests := []struct {
		name    string
		pattern string
		want    bool
	}{
		{name: "recursive POSIX glob", pattern: "rules/**/*.md", want: true},
		{name: "root POSIX glob", pattern: "*.md", want: true},
		{name: "leading backslash", pattern: `\rules/*.md`, want: false},
		{name: "middle backslash", pattern: `rules\*.md`, want: false},
		{name: "nested backslash", pattern: `rules/**\*.md`, want: false},
		{name: "parent traversal", pattern: "../rules/*.md", want: false},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			value := base
			rule := value.Artifacts.Rules[0]
			rule.Activation = RuleActivation{Mode: ActivationPaths, Paths: []string{test.pattern}}
			value.Artifacts.Rules = []RuleArtifact{rule}

			assertManifestValidity(t, schema, root, value, test.pattern, test.want)
		})
	}
}

func TestSourceRepositoryValidationMatchesJSONSchema(t *testing.T) {
	t.Parallel()

	schema := compileManifestSchema(t)
	root := writeTestPackage(t, validManifest)
	base, err := Load(root)
	if err != nil {
		t.Fatalf("Load(valid manifest): %v", err)
	}

	tests := []struct {
		name       string
		repository string
		want       bool
	}{
		{name: "canonical", repository: "https://github.com/example/test-plugin", want: true},
		{name: "wrong scheme", repository: "http://github.com/example/test-plugin", want: false},
		{name: "wrong host", repository: "https://example.com/example/test-plugin", want: false},
		{name: "trailing slash", repository: "https://github.com/example/test-plugin/", want: false},
		{name: "extra path segment", repository: "https://github.com/example/test-plugin/extra", want: false},
		{name: "missing repository segment", repository: "https://github.com/example", want: false},
		{name: "uppercase owner", repository: "https://github.com/Example/test-plugin", want: false},
		{name: "uppercase repository", repository: "https://github.com/example/Test-Plugin", want: false},
		{name: "query", repository: "https://github.com/example/test-plugin?ref=main", want: false},
		{name: "fragment", repository: "https://github.com/example/test-plugin#readme", want: false},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			value := base
			value.Source.Repository = test.repository

			assertManifestValidity(t, schema, root, value, test.repository, test.want)
		})
	}
}

func TestInvalidPackageNameSuppressesRepositoryMismatch(t *testing.T) {
	t.Parallel()

	root := writeTestPackage(t, strings.Replace(validManifest, "name: example/test-plugin", "name: Example/Test-Plugin", 1))
	_, err := Load(root)
	if err == nil {
		t.Fatal("Load() error = nil, want invalid package name")
	}
	var validationErrors *ValidationErrors
	if !errors.As(err, &validationErrors) {
		t.Fatalf("Load() error = %T %v, want *ValidationErrors", err, err)
	}
	if len(validationErrors.Issues) != 1 || validationErrors.Issues[0].Code != CodeInvalidPackageName {
		t.Fatalf("Load() errors = %v, want one %q failure", validationErrors.Issues, CodeInvalidPackageName)
	}
}

func TestInvalidPackageNameDoesNotSuppressInvalidSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		repository string
	}{
		{name: "wrong scheme and host", repository: "http://example.com/test-plugin"},
		{name: "trailing slash", repository: "https://github.com/example/test-plugin/"},
		{name: "extra path segment", repository: "https://github.com/example/test-plugin/extra"},
		{name: "missing repository segment", repository: "https://github.com/example"},
		{name: "uppercase owner", repository: "https://github.com/Example/test-plugin"},
		{name: "uppercase repository", repository: "https://github.com/example/Test-Plugin"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			manifestYAML := strings.Replace(validManifest, "name: example/test-plugin", "name: Example/Test-Plugin", 1)
			manifestYAML = strings.Replace(manifestYAML, "https://github.com/example/test-plugin", test.repository, 1)
			root := writeTestPackage(t, manifestYAML)
			_, err := Load(root)
			if err == nil {
				t.Fatal("Load() error = nil, want invalid package name and source")
			}
			var validationErrors *ValidationErrors
			if !errors.As(err, &validationErrors) {
				t.Fatalf("Load() error = %T %v, want *ValidationErrors", err, err)
			}
			if len(validationErrors.Issues) != 2 || !validationErrors.Has(CodeInvalidPackageName) || !validationErrors.Has(CodeInvalidSource) {
				t.Fatalf("Load() errors = %v, want %q and %q failures", validationErrors.Issues, CodeInvalidPackageName, CodeInvalidSource)
			}
		})
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

func compileManifestSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	schemaPath := filepath.Join(repositoryRoot(t), "schemas", "agent-plugin.schema.json")
	compiler := jsonschema.NewCompiler()
	schema, err := compiler.Compile(schemaPath)
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return schema
}

func validateManifestSchema(t *testing.T, schema *jsonschema.Schema, value Manifest) error {
	t.Helper()

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	var instance any
	if err := json.Unmarshal(encoded, &instance); err != nil {
		t.Fatalf("decode JSON instance: %v", err)
	}
	return schema.Validate(instance)
}

func assertManifestValidity(t *testing.T, schema *jsonschema.Schema, root string, value Manifest, subject string, want bool) {
	t.Helper()

	goValid := Validate(root, value) == nil
	schemaValid := validateManifestSchema(t, schema, value) == nil
	if goValid != want {
		t.Errorf("Validate() accepted %q = %t, want %t", subject, goValid, want)
	}
	if schemaValid != want {
		t.Errorf("JSON Schema accepted %q = %t, want %t", subject, schemaValid, want)
	}
}

type manifestReplacingRoot struct {
	*os.Root
	beforeOpen func() error
}

func (r *manifestReplacingRoot) Open(name string) (*os.File, error) {
	if err := r.beforeOpen(); err != nil {
		return nil, err
	}
	return r.Root.Open(name)
}
