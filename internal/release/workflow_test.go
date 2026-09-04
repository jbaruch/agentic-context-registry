package release

import (
	"os"
	"os/exec"
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
			Steps       []struct {
				Name string            `yaml:"name"`
				Env  map[string]string `yaml:"env"`
				Run  string            `yaml:"run"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
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
	guardSteps := workflow.Jobs["guard"].Steps
	if len(guardSteps) == 0 || guardSteps[0].Env["HOMEBREW_TAP_DEPLOY_KEY"] != "${{ secrets.HOMEBREW_TAP_DEPLOY_KEY }}" ||
		!strings.Contains(guardSteps[0].Run, `[[ -z "${HOMEBREW_TAP_DEPLOY_KEY}" ]]`) {
		t.Fatalf("guard first step = %#v, want Homebrew deploy-key preflight", guardSteps)
	}
	var taggedSourceGate string
	for _, step := range guardSteps {
		if step.Name == "Verify tagged source" {
			taggedSourceGate = step.Run
			break
		}
	}
	for _, required := range []string{
		`test -z "$(gofmt -l .)"`,
		"go vet ./...",
		"go test -race ./...",
		"go build ./cmd/acr",
		"go mod verify",
	} {
		if !strings.Contains(taggedSourceGate, required) {
			t.Errorf("tagged-source gate omits %q", required)
		}
	}

	source := string(contents)
	for _, required := range []string{
		"needs: [guard, build, verify]",
		"cancel-in-progress: false",
		"cosign sign-blob --yes",
		"cosign verify-blob",
		"# Pinned release; review monthly beside the GitHub Actions pins.\n          cosign-release: v3.0.6",
		"actions/attest-build-provenance@4d101475d8b20a2381f78447822ac1eab6504dd8",
		"gh attestation verify",
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
	tapSteps := workflow.Jobs["tap"].Steps
	var tapSSHKey string
	var tapRepository string
	for _, step := range tapSteps {
		if step.Name == "Check out Homebrew tap" {
			tapSSHKey = step.With["ssh-key"]
			tapRepository = step.With["repository"]
			break
		}
	}
	if tapSSHKey != "${{ secrets.HOMEBREW_TAP_DEPLOY_KEY }}" {
		t.Errorf("tap checkout ssh-key = %q, want Homebrew deploy key secret", tapSSHKey)
	}
	if tapRepository != "jbaruch/homebrew-agentic-context-registry" {
		t.Errorf("tap checkout repository = %q, want renamed Homebrew tap repository", tapRepository)
	}
	for _, forbidden := range []string{"pull_request_target", "workflow_call", "acr publish", "acr-package.json", "-buildid="} {
		if strings.Contains(source, forbidden) {
			t.Errorf("workflow contains forbidden %q", forbidden)
		}
	}
	assertWorkflowActionsPinned(t, source)
}

func TestHomebrewGateInstallsCandidateThroughTap(t *testing.T) {
	t.Parallel()

	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(releaseWorkflow(t), &workflow); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}
	var brewGate string
	for _, step := range workflow.Jobs["brew"].Steps {
		if step.Name == "Install and test the published release" {
			brewGate = step.Run
			break
		}
	}
	if brewGate == "" {
		t.Fatal("Homebrew install-and-test step is missing")
	}

	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	tapDir := filepath.Join(root, "tap")
	binDir := filepath.Join(root, "bin")
	runnerTemp := filepath.Join(root, "runner")
	for _, path := range []string{filepath.Join(workspace, "formula"), binDir, runnerTemp} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	formula := []byte("class Acr < Formula\nend\n")
	if err := os.WriteFile(filepath.Join(workspace, "formula", "acr.rb"), formula, 0o644); err != nil {
		t.Fatal(err)
	}
	writeWorkflowTestCommand(t, binDir, "brew", `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  tap-new)
    [[ "$2" == "--no-git" && "$3" == */* ]]
    printf '%s\n' "$3" > "${TEST_LOG}.tap"
    mkdir -p "${TEST_TAP_DIR}/Formula"
    ;;
  --repository)
    [[ "$2" == "$(< "${TEST_LOG}.tap")" ]]
    printf '%s\n' "${TEST_TAP_DIR}"
    ;;
  install)
    [[ "$2" == "$(< "${TEST_LOG}.tap")/acr" ]]
    cmp "${GITHUB_WORKSPACE}/formula/acr.rb" "${TEST_TAP_DIR}/Formula/acr.rb"
    printf 'install tap-qualified\n' >> "${TEST_LOG}"
    ;;
  test)
    [[ "$2" == "acr" ]]
    printf 'test %s\n' "$2" >> "${TEST_LOG}"
    ;;
  *)
    echo "unexpected brew command: $*" >&2
    exit 1
    ;;
esac
`)
	writeWorkflowTestCommand(t, binDir, "acr", `#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == "version" && "$2" == "--json" ]]
printf '{"result":{"version":"%s","commit":"%s"}}\n' "${VERSION}" "${COMMIT}"
`)
	writeWorkflowTestCommand(t, binDir, "jq", `#!/usr/bin/env bash
set -euo pipefail
case "$2" in
  .result.version) printf '%s\n' "${VERSION}" ;;
  .result.commit) printf '%s\n' "${COMMIT}" ;;
  *) echo "unexpected jq expression: $2" >&2; exit 1 ;;
esac
`)

	logPath := filepath.Join(root, "brew.log")
	cmd := exec.Command("bash", "-c", brewGate)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GITHUB_WORKSPACE="+workspace,
		"RUNNER_TEMP="+runnerTemp,
		"COMMIT=0123456789abcdef0123456789abcdef01234567",
		"VERSION=1.2.3",
		"TEST_TAP_DIR="+tapDir,
		"TEST_LOG="+logPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run Homebrew gate: %v\n%s", err, output)
	}
	copied, err := os.ReadFile(filepath.Join(tapDir, "Formula", "acr.rb"))
	if err != nil {
		t.Fatal(err)
	}
	if string(copied) != string(formula) {
		t.Fatalf("tap formula = %q, want downloaded candidate %q", copied, formula)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(log), "install tap-qualified\ntest acr\n"; got != want {
		t.Fatalf("Homebrew operations = %q, want %q", got, want)
	}
}

func writeWorkflowTestCommand(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
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
