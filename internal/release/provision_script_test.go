package release

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type provisionResult struct {
	Action           string `json:"action"`
	DeployKeyTitle   string `json:"deployKeyTitle"`
	Secret           string `json:"secret"`
	SourceRepository string `json:"sourceRepository"`
	TapRepository    string `json:"tapRepository"`
}

func TestProvisionTapDeployKeyIsIdempotent(t *testing.T) {
	t.Parallel()

	result, stderr, log, secret := runProvisionScript(t, "existing")
	if result.Action != "unchanged" || result.DeployKeyTitle != "acr-release-formula" ||
		result.Secret != "HOMEBREW_TAP_DEPLOY_KEY" ||
		result.SourceRepository != "jbaruch/agentic-context-registry" ||
		result.TapRepository != "jbaruch/homebrew-acr" {
		t.Fatalf("result = %#v", result)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if strings.Contains(log, "ssh-keygen") || strings.Contains(log, "git ") || strings.Contains(log, "secret set") || strings.Contains(log, "--method") {
		t.Fatalf("idempotent run changed state:\n%s", log)
	}
	if !strings.Contains(log, "gh secret list") {
		t.Fatalf("idempotent run did not verify the paired secret:\n%s", log)
	}
	if secret != "" {
		t.Fatal("idempotent run replaced the secret")
	}
}

func TestProvisionTapDeployKeyHelpDescribesIncompleteProvisioningRepair(t *testing.T) {
	t.Parallel()

	stdout, stderr, _, _, err := invokeProvisionScript(t, "unused", "--help")
	if err != nil {
		t.Fatalf("help failed: %v", err)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "deploy key without HOMEBREW_TAP_DEPLOY_KEY is repaired by rotation") {
		t.Fatalf("help omits incomplete-provisioning repair: %q", stdout)
	}
}

func TestProvisionTapDeployKeyRepairsKeyWithoutSecret(t *testing.T) {
	t.Parallel()

	result, stderr, log, secret := runProvisionScript(t, "key-without-secret")
	if result.Action != "repaired" {
		t.Fatalf("action = %q, want repaired", result.Action)
	}
	if !strings.Contains(stderr, "Provisioning is incomplete") || !strings.Contains(stderr, "rotating the unrecoverable key") {
		t.Fatalf("stderr = %q, want incomplete-provisioning explanation", stderr)
	}
	if secret != "private-material\n" {
		t.Fatalf("stored secret = %q, want generated private key", secret)
	}
	sequence := []string{
		"gh api post",
		"git ls-remote",
		"gh secret set",
		"gh api delete 41",
	}
	position := 0
	for _, line := range strings.Split(log, "\n") {
		if position < len(sequence) && strings.HasPrefix(line, sequence[position]) {
			position++
		}
	}
	if position != len(sequence) {
		t.Fatalf("repair order =\n%s\nwant sequence %q", log, sequence)
	}
}

func TestProvisionTapDeployKeyRotatesCredentialSafely(t *testing.T) {
	t.Parallel()

	result, stderr, log, secret := runProvisionScript(t, "existing", "--rotate")
	if result.Action != "rotated" {
		t.Fatalf("action = %q, want rotated", result.Action)
	}
	if secret != "private-material\n" {
		t.Fatalf("stored secret did not come from generated private-key file")
	}
	sequence := []string{
		"gh api post",
		"git ls-remote",
		"gh secret set",
		"gh api delete 41",
	}
	position := 0
	for _, line := range strings.Split(log, "\n") {
		if position < len(sequence) && strings.HasPrefix(line, sequence[position]) {
			position++
		}
	}
	if position != len(sequence) {
		t.Fatalf("rotation order =\n%s\nwant sequence %q", log, sequence)
	}
	if strings.Contains(stderr, "private-material") || strings.Contains(log, "private-material") {
		t.Fatal("private key appeared in diagnostics")
	}
	if !strings.Contains(log, "IdentitiesOnly=yes") || !strings.Contains(log, "git@github.com:jbaruch/homebrew-acr.git") {
		t.Fatalf("authentication was not pinned to the generated key:\n%s", log)
	}
}

func TestProvisionTapDeployKeyRefusesUnauthenticatedGitHubCLI(t *testing.T) {
	t.Parallel()

	_, stderr, log, secret, err := invokeProvisionScript(t, "unauthenticated")
	if err == nil {
		t.Fatal("unauthenticated run succeeded")
	}
	if !strings.Contains(stderr, "run gh auth login --hostname github.com and retry") {
		t.Fatalf("stderr = %q, want actionable authentication guidance", stderr)
	}
	if strings.Contains(log, "api") || strings.Contains(log, "ssh-keygen") || strings.Contains(log, "git ") || strings.Contains(log, "secret set") {
		t.Fatalf("unauthenticated run continued after preflight:\n%s", log)
	}
	if secret != "" {
		t.Fatal("unauthenticated run wrote secret material")
	}
}

func TestProvisionTapDeployKeyRollsBackStagedKeyWhenSecretUpdateFails(t *testing.T) {
	t.Parallel()

	_, stderr, log, _, err := invokeProvisionScript(t, "secret-failure", "--rotate")
	if err == nil {
		t.Fatal("secret update failure succeeded")
	}
	if !strings.Contains(stderr, "preserving the previous credential") {
		t.Fatalf("stderr = %q, want credential-preservation guidance", stderr)
	}
	if !strings.Contains(log, "gh api delete 42") {
		t.Fatalf("staged deploy key was not rolled back:\n%s", log)
	}
	if strings.Contains(log, "gh api delete 41") {
		t.Fatalf("previous deploy key was removed after secret failure:\n%s", log)
	}
	if strings.Contains(stderr, "private-material") || strings.Contains(log, "private-material") {
		t.Fatal("private key appeared in failure diagnostics")
	}
}

func TestProvisionTapDeployKeyCleansUpStagedKeyWhenInterrupted(t *testing.T) {
	t.Parallel()

	_, _, log, _, err := invokeProvisionScript(t, "interrupt-after-registration")
	if err == nil {
		t.Fatal("interrupted run succeeded")
	}
	if !strings.Contains(log, "gh api delete 42") {
		t.Fatalf("staged deploy key was not removed after interruption:\n%s", log)
	}
	var keyPath string
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "ssh-keygen path=") {
			keyPath = strings.TrimPrefix(line, "ssh-keygen path=")
			break
		}
	}
	if keyPath == "" {
		t.Fatalf("generated private-key path was not recorded:\n%s", log)
	}
	if _, statErr := os.Stat(filepath.Dir(keyPath)); !os.IsNotExist(statErr) {
		t.Fatalf("temporary credential directory remains after interruption: %v", statErr)
	}
}

func TestProvisionTapDeployKeyRefusesMismatchedGitHubHostKey(t *testing.T) {
	t.Parallel()

	_, stderr, log, secret, err := invokeProvisionScript(t, "host-key-mismatch")
	if err == nil {
		t.Fatal("host-key mismatch succeeded")
	}
	if !strings.Contains(stderr, "could not authenticate") {
		t.Fatalf("stderr = %q, want authentication refusal", stderr)
	}
	if !strings.Contains(log, "StrictHostKeyChecking=yes") || strings.Contains(log, "StrictHostKeyChecking=accept-new") {
		t.Fatalf("SSH host-key verification was not strict:\n%s", log)
	}
	if !strings.Contains(log, "known-hosts github.com ssh-ed25519 github-host-key") {
		t.Fatalf("GitHub published host keys were not supplied to SSH:\n%s", log)
	}
	if strings.Contains(log, "gh secret set") || secret != "" {
		t.Fatalf("host-key mismatch reached secret update:\n%s", log)
	}
	if !strings.Contains(log, "gh api delete 42") {
		t.Fatalf("staged deploy key was not rolled back after host-key mismatch:\n%s", log)
	}
}

func runProvisionScript(t *testing.T, scenario string, args ...string) (provisionResult, string, string, string) {
	t.Helper()
	stdout, stderr, log, secret, err := invokeProvisionScript(t, scenario, args...)
	if err != nil {
		t.Fatalf("provision script failed: %v\nstderr:\n%s\nlog:\n%s", err, stderr, log)
	}
	var result provisionResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nstdout: %s", err, stdout)
	}
	return result, stderr, log, secret
}

func invokeProvisionScript(t *testing.T, scenario string, args ...string) (string, string, string, string, error) {
	t.Helper()
	temporaryDirectory := t.TempDir()
	binDirectory := filepath.Join(temporaryDirectory, "bin")
	if err := os.Mkdir(binDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(temporaryDirectory, "calls.log")
	secretPath := filepath.Join(temporaryDirectory, "secret-input")
	writeExecutable(t, filepath.Join(binDirectory, "gh"), fakeGH)
	writeExecutable(t, filepath.Join(binDirectory, "git"), fakeGit)
	writeExecutable(t, filepath.Join(binDirectory, "ssh-keygen"), fakeSSHKeygen)
	writeExecutable(t, filepath.Join(binDirectory, "ssh"), "#!/usr/bin/env bash\nset -euo pipefail\nexit 0\n")

	scriptPath := filepath.Join("..", "..", "scripts", "provision-tap-deploy-key.sh")
	command := exec.Command("bash", append([]string{scriptPath}, args...)...)
	command.Env = append(os.Environ(),
		"PATH="+binDirectory+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_GH_SCENARIO="+scenario,
		"PROVISION_TEST_LOG="+logPath,
		"PROVISION_TEST_SECRET="+secretPath,
	)
	var stdout strings.Builder
	var stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	logContents, readErr := os.ReadFile(logPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	secretContents, readErr := os.ReadFile(secretPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return stdout.String(), stderr.String(), string(logContents), string(secretContents), err
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

const fakeGH = `#!/usr/bin/env bash
set -euo pipefail
printf 'gh %s\n' "$*" >> "${PROVISION_TEST_LOG}"
case "$1" in
  auth)
    if [[ "${FAKE_GH_SCENARIO}" == "unauthenticated" ]]; then
      exit 1
    fi
    ;;
  api)
    if [[ "$*" == *"--method POST"* ]]; then
      printf 'gh api post\n' >> "${PROVISION_TEST_LOG}"
      printf '42\n'
    elif [[ "$*" == *"--method DELETE"* ]]; then
      printf 'gh api delete %s\n' "${4##*/}" >> "${PROVISION_TEST_LOG}"
    elif [[ "$*" == *"https://api.github.com/meta"* ]]; then
      printf 'github.com ssh-ed25519 github-host-key\n'
    elif [[ "${FAKE_GH_SCENARIO}" == "existing" || "${FAKE_GH_SCENARIO}" == "secret-failure" || "${FAKE_GH_SCENARIO}" == "key-without-secret" ]]; then
      printf '41\n'
    fi
    ;;
  secret)
    if [[ "$2" == "list" ]]; then
      if [[ "${FAKE_GH_SCENARIO}" == "existing" || "${FAKE_GH_SCENARIO}" == "secret-failure" ]]; then
        printf 'HOMEBREW_TAP_DEPLOY_KEY\n'
      fi
    else
      printf 'gh secret set\n' >> "${PROVISION_TEST_LOG}"
      command cat > "${PROVISION_TEST_SECRET}"
      if [[ "${FAKE_GH_SCENARIO}" == "secret-failure" ]]; then
        exit 1
      fi
    fi
    ;;
  *)
    exit 64
    ;;
esac
`

const fakeGit = `#!/usr/bin/env bash
set -euo pipefail
printf 'git %s ssh=%s\n' "$*" "${GIT_SSH_COMMAND:-}" >> "${PROVISION_TEST_LOG}"
known_hosts_path="${GIT_SSH_COMMAND##*UserKnownHostsFile=}"
printf 'known-hosts %s\n' "$(<"${known_hosts_path}")" >> "${PROVISION_TEST_LOG}"
if [[ "${FAKE_GH_SCENARIO}" == "interrupt-after-registration" ]]; then
  kill -TERM "${PPID}"
fi
if [[ "${FAKE_GH_SCENARIO}" == "host-key-mismatch" ]]; then
  exit 1
fi
printf 'deadbeef\tHEAD\n'
`

const fakeSSHKeygen = `#!/usr/bin/env bash
set -euo pipefail
key_path=''
while [[ "$#" -gt 0 ]]; do
  if [[ "$1" == '-f' ]]; then
    key_path="$2"
    break
  fi
  shift
done
if [[ -z "${key_path}" ]]; then
  exit 64
fi
printf 'ssh-keygen path=%s\n' "${key_path}" >> "${PROVISION_TEST_LOG}"
printf 'private-material\n' > "${key_path}"
printf 'ssh-ed25519 public-material acr-release-formula\n' > "${key_path}.pub"
`
