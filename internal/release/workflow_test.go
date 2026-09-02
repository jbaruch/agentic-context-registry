package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestReleaseWorkflowContract(t *testing.T) {
	t.Parallel()

	contents := releaseWorkflow(t)
	var workflow struct {
		Name        string         `yaml:"name"`
		On          map[string]any `yaml:"on"`
		Permissions map[string]any `yaml:"permissions"`
		Concurrency struct {
			Group            string `yaml:"group"`
			CancelInProgress bool   `yaml:"cancel-in-progress"`
		} `yaml:"concurrency"`
		Jobs map[string]struct {
			Needs       any            `yaml:"needs"`
			Permissions map[string]any `yaml:"permissions"`
			RunsOn      any            `yaml:"runs-on"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}
	if workflow.Name != "Release acr CLI" || len(workflow.Permissions) != 0 || workflow.Concurrency.CancelInProgress || !strings.Contains(workflow.Concurrency.Group, "release-cli-") {
		t.Fatalf("workflow root contract = %#v", workflow)
	}
	push, ok := workflow.On["push"].(map[string]any)
	if !ok {
		t.Fatalf("workflow trigger = %#v, want push map", workflow.On)
	}
	tags, ok := push["tags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != "v*" || len(workflow.On) != 1 {
		t.Fatalf("workflow trigger = %#v, want only v* tags", workflow.On)
	}
	wantJobs := []string{"guard", "build", "verify", "release", "formula", "brew", "tap"}
	for _, name := range wantJobs {
		if _, exists := workflow.Jobs[name]; !exists {
			t.Errorf("workflow omits %q job", name)
		}
	}
	releaseJob := workflow.Jobs["release"]
	if releaseJob.Permissions["contents"] != "write" || releaseJob.Permissions["id-token"] != "write" || releaseJob.Permissions["attestations"] != "write" {
		t.Fatalf("release permissions = %#v", releaseJob.Permissions)
	}
	for _, name := range []string{"guard", "build", "verify", "formula", "brew", "tap"} {
		permissions := workflow.Jobs[name].Permissions
		if permissions["contents"] != "read" || len(permissions) != 1 {
			t.Errorf("%s permissions = %#v, want contents:read only", name, permissions)
		}
	}

	source := string(contents)
	for _, required := range []string{
		"needs: [guard, build, verify]",
		"cancel-in-progress: false",
		"cosign sign-blob --yes",
		"cosign verify-blob",
		"actions/attest-build-provenance@4d101475d8b20a2381f78447822ac1eab6504dd8",
		"gh attestation verify",
		"brew install --formula",
		"brew test acr",
		"macos-latest, ubuntu-latest",
		"go build -trimpath -ldflags",
		`.metadata.component.name | test("agentic-context-registry")`,
		"checksums.txt.sigstore.json",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("workflow omits required contract %q", required)
		}
	}
	for _, forbidden := range []string{"pull_request_target", "workflow_call", "acr publish", "acr-package.json", "-buildid="} {
		if strings.Contains(source, forbidden) {
			t.Errorf("workflow contains forbidden %q", forbidden)
		}
	}
	assertWorkflowActionsPinned(t, source)
}

func TestReleaseWorkflowDistinctFromPackagePublish(t *testing.T) {
	t.Parallel()

	cliWorkflow := string(releaseWorkflow(t))
	packagePath := filepath.Join("..", "..", ".github", "workflows", "publish-package.yml")
	packageWorkflow, err := os.ReadFile(packagePath)
	if err != nil {
		t.Fatal(err)
	}
	packageSource := string(packageWorkflow)
	if strings.Contains(cliWorkflow, "workflow_call") || strings.Contains(cliWorkflow, "acr publish") || strings.Contains(cliWorkflow, "acr-package.json") {
		t.Fatal("CLI release workflow contains package-publishing behavior")
	}
	if !strings.Contains(packageSource, "workflow_call") || strings.Contains(packageSource, "acr-darwin-amd64.tar.gz") || strings.Contains(packageSource, "release-cli") {
		t.Fatal("package workflow contains CLI-release behavior")
	}
}

func assertWorkflowActionsPinned(t *testing.T, source string) {
	t.Helper()
	for _, line := range strings.Split(source, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "uses:") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			t.Fatalf("malformed action line %q", line)
		}
		parts := strings.Split(fields[1], "@")
		if len(parts) != 2 || len(parts[1]) != 40 {
			t.Errorf("action is not pinned to a full commit: %q", line)
		}
	}
}

func releaseWorkflow(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("..", "..", ".github", "workflows", "release-cli.yml")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}
