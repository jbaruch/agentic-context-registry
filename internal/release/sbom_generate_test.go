package release

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const cyclonedxGomodPin = "github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.12.0"

func TestCycloneDXGomodRecordsPerTargetBuildConstraints(t *testing.T) {
	generator := installCycloneDXGomod(t)
	moduleDir := cloneReleaseModule(t)
	const version = "1.2.3"
	documents := make(map[Target][]byte, len(Targets()))
	for _, target := range Targets() {
		raw := generateTargetSBOM(t, generator, moduleDir, target)
		if err := ValidateSBOM(raw, version, target); err == nil {
			t.Fatalf("raw %s SBOM passed before identity rewrite", target.SBOMName())
		}
		rewritten := rewriteSBOMIdentity(t, raw, version)
		if err := ValidateSBOM(rewritten, version, target); err != nil {
			t.Fatalf("ValidateSBOM(%s) generated document: %v", target.SBOMName(), err)
		}
		documents[target] = rewritten
	}
	for _, target := range Targets() {
		for _, other := range Targets() {
			if other == target {
				continue
			}
			if err := ValidateSBOM(documents[other], version, target); err == nil {
				t.Fatalf("generated %s document was accepted as %s", other.SBOMName(), target.SBOMName())
			}
		}
	}
}

func installCycloneDXGomod(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	command := exec.Command("go", "install", cyclonedxGomodPin)
	command.Env = append(os.Environ(), "GOBIN="+binDir, "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install %s: %v\n%s", cyclonedxGomodPin, err, output)
	}
	return filepath.Join(binDir, "cyclonedx-gomod")
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
	if !strings.Contains(string(contents), `"name": "github.com/jbaruch/agentic-context-registry"`) {
		t.Fatalf("generated %s does not identify the module", target.SBOMName())
	}
	return contents
}

func rewriteSBOMIdentity(t *testing.T, contents []byte, version string) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatalf("decode generated SBOM: %v", err)
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
