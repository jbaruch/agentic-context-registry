package release

import (
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// The generator is the release build's pinned cyclonedx-gomod. The suite
	// never installs it: an install reaches the module proxy and the checksum
	// database, and a dropped connection there reddened unrelated pull
	// requests (issue #129). The CI and release workflows provision it before
	// the suite runs, and the generation test consumes it from PATH.
	cyclonedxGomodPackage = "github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod"
	cyclonedxGomodVersion = "v1.12.0"
	cyclonedxGomodPin     = cyclonedxGomodPackage + "@" + cyclonedxGomodVersion
	cyclonedxGomodBinary  = "cyclonedx-gomod"
	generatedModuleName   = "github.com/jbaruch/agentic-context-registry"
	generationTestVersion = "1.2.3"
)

func TestCycloneDXGomodRecordsPerTargetBuildConstraints(t *testing.T) {
	generator := requirePinnedCycloneDXGomod(t)
	moduleDir := cloneReleaseModule(t)
	documents := make(map[Target][]byte, len(Targets()))
	for _, target := range Targets() {
		raw := generateTargetSBOM(t, generator, moduleDir, target)
		if err := ValidateSBOM(raw, generationTestVersion, target); err == nil {
			t.Fatalf("raw %s SBOM passed before identity rewrite", target.SBOMName())
		}
		rewritten := rewriteSBOMIdentity(t, raw, generationTestVersion)
		if err := ValidateSBOM(rewritten, generationTestVersion, target); err != nil {
			t.Fatalf("ValidateSBOM(%s) generated document: %v", target.SBOMName(), err)
		}
		documents[target] = rewritten
	}
	for _, target := range Targets() {
		for _, other := range Targets() {
			if other == target {
				continue
			}
			if err := ValidateSBOM(documents[other], generationTestVersion, target); err == nil {
				t.Fatalf("generated %s document was accepted as %s", other.SBOMName(), target.SBOMName())
			}
		}
	}
}

// requirePinnedCycloneDXGomod fails the test with an installation instruction
// when the provisioned generator is missing or is not the pinned build, the
// way requireWorkflowTool does for jq. It never skips: a silent skip would
// look like a pass in the release workflow's tagged-source gate.
func requirePinnedCycloneDXGomod(t *testing.T) string {
	t.Helper()
	generator, err := pinnedCycloneDXGomod(exec.LookPath)
	if err != nil {
		t.Fatal(err)
	}
	return generator
}

// pinnedCycloneDXGomod resolves the generator through lookPath and holds it to
// the pin by the build information embedded in the binary, so the test runs
// against exactly the release the workflows install and nothing else that
// happens to be on PATH.
func pinnedCycloneDXGomod(lookPath func(string) (string, error)) (string, error) {
	generator, err := lookPath(cyclonedxGomodBinary)
	if err != nil {
		return "", fmt.Errorf("SBOM generation test requires %s: %w; install the pinned generator with `go install %s` and put it on PATH (CONTRIBUTING.md lists the prerequisites)", cyclonedxGomodBinary, err, cyclonedxGomodPin)
	}
	info, err := buildinfo.ReadFile(generator)
	if err != nil {
		return "", fmt.Errorf("SBOM generation test found %s but cannot read its Go build information: %w; install the pinned generator with `go install %s`", generator, err, cyclonedxGomodPin)
	}
	if info.Path != cyclonedxGomodPackage || info.Main.Version != cyclonedxGomodVersion {
		return "", fmt.Errorf("SBOM generation test found %s built from %s@%s, want %s; install the pinned generator with `go install %s`", generator, info.Path, info.Main.Version, cyclonedxGomodPin, cyclonedxGomodPin)
	}
	return generator, nil
}

func TestPinnedGeneratorDiagnosticsNameTheInstallCommand(t *testing.T) {
	t.Parallel()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), cyclonedxGomodBinary)
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\nset -euo pipefail\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	install := "go install " + cyclonedxGomodPin
	for _, test := range []struct {
		name     string
		lookPath func(string) (string, error)
		want     []string
	}{
		{
			name: "absent from PATH",
			lookPath: func(name string) (string, error) {
				return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
			},
			want: []string{install, "CONTRIBUTING.md"},
		},
		{
			name:     "not a Go build",
			lookPath: func(string) (string, error) { return script, nil },
			want:     []string{script, install},
		},
		{
			name:     "another Go program",
			lookPath: func(string) (string, error) { return self, nil },
			want:     []string{self, install},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			generator, err := pinnedCycloneDXGomod(test.lookPath)
			if err == nil {
				t.Fatalf("pinnedCycloneDXGomod accepted %q", generator)
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("diagnostic %q does not name %q", err, want)
				}
			}
		})
	}
}

func cloneReleaseModule(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..")
	dir := t.TempDir()
	command := exec.Command("git", "clone", "--local", "--no-hardlinks", ".", dir)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("clone module for SBOM generation: %v\n%s", err, output)
	}
	return dir
}

func generateTargetSBOM(t *testing.T, generator, moduleDir string, target Target) []byte {
	t.Helper()
	output := filepath.Join(t.TempDir(), target.SBOMName())
	command := exec.Command(generator, "app", "-json", "-licenses", "-noserial", "-notimestamp", "-main", "cmd/acr", "-output", output, ".")
	command.Dir = moduleDir
	command.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS="+target.GOOS,
		"GOARCH="+target.GOARCH,
	)
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cyclonedx-gomod app for %s/%s: %v\n%s", target.GOOS, target.GOARCH, err, combined)
	}
	contents, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func TestGeneratedModuleNameAcceptsPrettyAndCompactJSON(t *testing.T) {
	t.Parallel()

	const compact = `{"metadata":{"component":{"name":"github.com/jbaruch/agentic-context-registry"}}}`
	pretty := "{\n  \"metadata\": {\n    \"component\": {\n      \"name\" : \"github.com/jbaruch/agentic-context-registry\"\n    }\n  }\n}"
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "compact", raw: compact},
		{name: "pretty", raw: pretty},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := requireGeneratedModuleName([]byte(test.raw)); err != nil {
				t.Fatalf("requireGeneratedModuleName(%s) = %v", test.name, err)
			}
		})
	}
}

func TestGeneratedModuleNameRejectsWrongRootWithMatchingDependency(t *testing.T) {
	t.Parallel()

	raw := `{"metadata":{"component":{"name":"wrong-module"}},"components":[{"name":"github.com/jbaruch/agentic-context-registry"}]}`
	if err := requireGeneratedModuleName([]byte(raw)); err == nil {
		t.Fatal("requireGeneratedModuleName accepted a wrong root component because a dependency used the module name")
	}
}

func rewriteSBOMIdentity(t *testing.T, contents []byte, version string) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatalf("decode generated SBOM: %v", err)
	}
	name, err := componentName(document)
	if err != nil {
		t.Fatal(err)
	}
	if name != generatedModuleName {
		t.Fatalf("generated SBOM metadata.component.name = %q, want %s", name, generatedModuleName)
	}
	metadata, ok := document["metadata"].(map[string]any)
	if !ok {
		t.Fatal("generated SBOM has no metadata object")
	}
	component, ok := metadata["component"].(map[string]any)
	if !ok {
		t.Fatal("generated SBOM has no metadata.component object")
	}
	component["name"] = "acr"
	component["version"] = version
	rewritten, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode rewritten SBOM: %v", err)
	}
	return rewritten
}

func requireGeneratedModuleName(contents []byte) error {
	name, err := metadataComponentName(contents)
	if err != nil {
		return err
	}
	if name != generatedModuleName {
		return fmt.Errorf("metadata.component.name is %q, expected %s", name, generatedModuleName)
	}
	return nil
}

func metadataComponentName(contents []byte) (string, error) {
	var document map[string]any
	if err := json.Unmarshal(contents, &document); err != nil {
		return "", fmt.Errorf("decode generated SBOM: %w", err)
	}
	return componentName(document)
}

func componentName(document map[string]any) (string, error) {
	metadata, ok := document["metadata"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("generated SBOM has no metadata object")
	}
	component, ok := metadata["component"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("generated SBOM has no metadata.component object")
	}
	name, ok := component["name"].(string)
	if !ok || name == "" {
		return "", fmt.Errorf("generated SBOM metadata.component.name is not a string")
	}
	return name, nil
}
