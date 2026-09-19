package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/producerconvert"
	"go.yaml.in/yaml/v3"
)

// codexWorkflowSteps is the step shape the Codex lane contracts read: what
// runs, under which condition, with which environment, through which action.
type codexWorkflowSteps struct {
	Name     string            `yaml:"name"`
	Run      string            `yaml:"run"`
	If       string            `yaml:"if"`
	Uses     string            `yaml:"uses"`
	Env      map[string]string `yaml:"env"`
	With     map[string]string `yaml:"with"`
	Continue bool              `yaml:"continue-on-error"`
}

type codexWorkflow struct {
	On          map[string]any `yaml:"on"`
	Permissions map[string]any `yaml:"permissions"`
	Jobs        map[string]struct {
		Needs    any    `yaml:"needs"`
		RunsOn   string `yaml:"runs-on"`
		Strategy struct {
			Matrix map[string]any `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []codexWorkflowSteps `yaml:"steps"`
	} `yaml:"jobs"`
}

func parseCodexWorkflow(t *testing.T, name string) (codexWorkflow, string) {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var document codexWorkflow
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return document, string(contents)
}

func codexStringList(t *testing.T, value any) []string {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("%#v is not a list", value)
	}
	var list []string
	for _, item := range items {
		list = append(list, fmt.Sprint(item))
	}
	return list
}

// TestCICodexRuntimeJob holds the runtime-contract lane to the verified
// release table: every verified release runs on both platforms against the
// real binary and the real boundary, mandatory, with evidence uploaded even
// on failure.
func TestCICodexRuntimeJob(t *testing.T) {
	t.Parallel()
	workflow, source := parseCodexWorkflow(t, "ci.yml")
	job, exists := workflow.Jobs["codex-runtime"]
	if !exists {
		t.Fatal("ci.yml has no codex-runtime job")
	}
	if job.Needs != "quality" {
		t.Fatalf("codex-runtime needs = %#v, want quality", job.Needs)
	}
	var want []string
	for _, release := range producerconvert.CodexVerifiedReleases {
		want = append(want, release.Version)
	}
	if got := codexStringList(t, job.Strategy.Matrix["codex"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("codex matrix = %v, want every verified release %v", got, want)
	}
	if got := codexStringList(t, job.Strategy.Matrix["os"]); !reflect.DeepEqual(got, []string{"ubuntu-24.04", "macos-latest"}) {
		t.Fatalf("os matrix = %v", got)
	}
	var install, probe, upload, boundary bool
	for _, step := range job.Steps {
		if step.Continue {
			t.Fatalf("step %q continues on error", step.Name)
		}
		switch {
		case strings.Contains(step.Run, ".github/scripts/install-codex.sh"):
			install = strings.Contains(step.Run, "${{ matrix.codex }}")
		case strings.Contains(step.Run, "go test"):
			probe = step.Env["ACR_CODEX_RELEASE_REQUIRED"] == "1" && step.Env["ACR_CODEX_RELEASE_EVIDENCE"] != "" && strings.Contains(step.Run, "TestCodexRuntimeRealRelease") && strings.Contains(step.Run, "TestCodexReadBoundaryDeniesInheritedInstructionsNatively") && strings.Contains(step.Run, "-race")
		case strings.HasPrefix(step.Uses, "actions/upload-artifact@"):
			upload = step.If == "always()" && step.With["if-no-files-found"] == "error" && step.With["path"] != ""
		case strings.Contains(step.Run, "bubblewrap"):
			boundary = step.If == "runner.os == 'Linux'"
		}
	}
	if !install || !probe || !upload || !boundary {
		t.Fatalf("codex-runtime job lacks install=%t probe=%t upload=%t boundary=%t", install, probe, upload, boundary)
	}
	if strings.Contains(source, "secrets.CODEX_API_KEY") {
		t.Fatal("ci.yml must never reach the Codex credential; only the dispatch-only live workflow may")
	}
	assertWorkflowActionsPinned(t, source)
	ciBoundaryInstall(t, workflow, "codex-runtime")
}

// TestCITestJobInstallsLinuxBoundary keeps the ordinary test matrix able to
// run the real bubblewrap boundary test on its Linux leg.
func TestCITestJobInstallsLinuxBoundary(t *testing.T) {
	t.Parallel()
	workflow, _ := parseCodexWorkflow(t, "ci.yml")
	ciBoundaryInstall(t, workflow, "test")
}

func ciBoundaryInstall(t *testing.T, workflow codexWorkflow, name string) codexWorkflowSteps {
	t.Helper()
	job := workflow.Jobs[name]
	if job.RunsOn != "${{ matrix.os }}" || !reflect.DeepEqual(codexStringList(t, job.Strategy.Matrix["os"]), []string{"ubuntu-24.04", "macos-latest"}) {
		t.Fatalf("%s must bind the native Linux package to Noble/amd64 and retain macOS", name)
	}
	var found *codexWorkflowSteps
	for _, step := range job.Steps {
		if strings.Contains(step.Run, "go test") && found == nil {
			t.Fatalf("%s reaches tests before native package verification", name)
		}
		if !strings.Contains(step.Run, "apt-get install") || !strings.Contains(step.Run, "bubblewrap") {
			continue
		}
		if found != nil || step.If != "runner.os == 'Linux'" || step.Continue {
			t.Fatalf("unexpected %s native package install: %#v", name, step)
		}
		for _, required := range []string{"set -euo pipefail", "Review monthly", "Ubuntu bubblewrap security updates", "Noble amd64", "repeat native boundary proof"} {
			if !strings.Contains(step.Run, required) {
				t.Fatalf("%s package install omits %q", name, required)
			}
		}
		copy := step
		found = &copy
	}
	if found == nil {
		t.Fatalf("%s has no native package install", name)
	}
	return *found
}

// Execute each actual install block with isolated command fixtures. The fixtures
// never reach host apt, sudo or Codex; their log observes effective arguments and
// ordering rather than accepting a pin that appears only in comments.
func TestCILinuxBoundaryInstallMetadata(t *testing.T) {
	t.Parallel()
	workflow, _ := parseCodexWorkflow(t, "ci.yml")
	for _, job := range []string{"test", "codex-runtime"} {
		step := ciBoundaryInstall(t, workflow, job)
		for _, failure := range []string{"success", "update-failure", "install-failure", "query-failure", "wrong-version", "wrong-architecture", "wrong-status", "extra-package"} {
			t.Run(job+"/"+failure, func(t *testing.T) {
				if err := checkCIBoundaryInstall(t, step.Run, failure); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func checkCIBoundaryInstall(t *testing.T, script, failure string) error {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	writeWorkflowTestCommand(t, bin, "sudo", "#!/bin/sh\n[ \"$1\" = apt-get ] || exit 81\nexec \"$@\"\n")
	writeWorkflowTestCommand(t, bin, "apt-get", `#!/bin/bash
set -euo pipefail
printf 'apt-get %s\n' "$*" >> "$TEST_LOG"
case "$*" in
  update) [[ "$TEST_FAILURE" != update-failure ]] ;;
  'install -y --no-install-recommends bubblewrap=0.9.0-1ubuntu0.3') [[ "$TEST_FAILURE" != install-failure ]] ;;
  'install -y --no-install-recommends bubblewrap') exit 0 ;;
  *) exit 81 ;;
esac
`)
	writeWorkflowTestCommand(t, bin, "dpkg-query", `#!/bin/bash
set -euo pipefail
[[ "$*" == '-W -f=${Version} ${Architecture} ${db:Status-Status}\n bubblewrap' ]] || exit 81
printf 'dpkg-query\n' >> "$TEST_LOG"
case "$TEST_FAILURE" in
  query-failure) exit 1 ;;
  wrong-version) printf '0.9.0-1ubuntu0.2 amd64 installed\n' ;;
  wrong-architecture) printf '0.9.0-1ubuntu0.3 arm64 installed\n' ;;
  wrong-status) printf '0.9.0-1ubuntu0.3 amd64 unpacked\n' ;;
  extra-package) printf '0.9.0-1ubuntu0.3 amd64 installed\n0.9.0-1ubuntu0.3 arm64 installed\n' ;;
  *) printf '0.9.0-1ubuntu0.3 amd64 installed\n' ;;
esac
`)
	log := filepath.Join(root, "commands.log")
	if err := os.WriteFile(log, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// The final marker represents the next native test step, reached only after
	// the install step succeeds under the runner's fail-fast Bash invocation.
	command := exec.Command("bash", "-e", "-o", "pipefail", "-c", script+"\nprintf 'native-tests\\n' >> \"$TEST_LOG\"\n")
	command.Dir = root
	command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + root, "TEST_LOG=" + log, "TEST_FAILURE=" + failure}
	output, runErr := command.CombinedOutput()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "apt-get update\n"
	if failure != "update-failure" {
		want += "apt-get install -y --no-install-recommends bubblewrap=0.9.0-1ubuntu0.3\n"
		if failure != "install-failure" {
			want += "dpkg-query\n"
		}
	}
	if failure == "success" {
		want += "native-tests\n"
	}
	if string(data) != want || (runErr == nil) != (failure == "success") {
		return fmt.Errorf("package boundary %s: error=%v commands=%q want=%q output=%s", failure, runErr, data, want, output)
	}
	return nil
}

func TestCILinuxBoundaryInstallControlsDetectFloatingFallback(t *testing.T) {
	t.Parallel()
	workflow, _ := parseCodexWorkflow(t, "ci.yml")
	for _, job := range []string{"test", "codex-runtime"} {
		step := ciBoundaryInstall(t, workflow, job)
		pinned := "sudo apt-get install -y --no-install-recommends bubblewrap=0.9.0-1ubuntu0.3"
		for _, mutation := range []struct{ name, replacement, failure string }{
			{"floating", "sudo apt-get install -y --no-install-recommends bubblewrap", "success"},
			{"fallback", pinned + " || sudo apt-get install -y --no-install-recommends bubblewrap", "install-failure"},
		} {
			t.Run(job+"/"+mutation.name, func(t *testing.T) {
				changed := strings.Replace(step.Run, pinned, mutation.replacement, 1)
				if changed == step.Run {
					t.Fatal("mutation did not change the actual install operand")
				}
				if err := checkCIBoundaryInstall(t, changed, mutation.failure); err == nil {
					t.Fatal("unsafe install mutation was not detected")
				}
			})
		}
	}
}

// TestCodexInstallScriptPinsEveryVerifiedRelease requires a digest row for
// every verified release on every runner platform the lanes use.
func TestCodexInstallScriptPinsEveryVerifiedRelease(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile(filepath.Join("..", "..", ".github", "scripts", "install-codex.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(script), "#!/usr/bin/env bash\n") || !strings.Contains(string(script), "set -euo pipefail") {
		t.Fatal("install-codex.sh must be a fail-fast bash script")
	}
	for _, release := range producerconvert.CodexVerifiedReleases {
		for _, asset := range []string{"codex-x86_64-unknown-linux-musl", "codex-aarch64-unknown-linux-musl", "codex-aarch64-apple-darwin", "codex-x86_64-apple-darwin"} {
			row := release.Version + "/" + asset + ") sha256="
			index := strings.Index(string(script), row)
			if index < 0 {
				t.Errorf("install-codex.sh has no digest for %s %s", release.Version, asset)
				continue
			}
			digest := string(script)[index+len(row):]
			digest = digest[:strings.IndexAny(digest, " \n")]
			if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
				t.Errorf("install-codex.sh digest for %s %s is not a sha256: %q", release.Version, asset, digest)
			}
		}
	}
}

// TestCodexInstallScriptFailsClosed executes install-codex.sh against command
// fixtures: the download, the digest check and the extraction happen in that
// order, the environment export happens only after all three, and every
// failure stops the script before anything downstream can run.
func TestCodexInstallScriptFailsClosed(t *testing.T) {
	t.Parallel()
	scriptPath, err := filepath.Abs(filepath.Join("..", "..", ".github", "scripts", "install-codex.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"success", "unknown-version", "unsupported-runner", "curl-failure", "digest-mismatch", "asset-missing"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			runner := filepath.Join(root, "runner")
			for _, dir := range []string{bin, runner} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			asset := "codex-x86_64-unknown-linux-musl"
			if failure == "asset-missing" {
				asset = "codex-not-the-asset"
			}
			archive := codexTestArchive(t, asset)
			archivePath := filepath.Join(root, "fixture.tar.gz")
			if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
				t.Fatal(err)
			}
			// Every fixture guards its arguments and exits explicitly; the log is
			// the evidence of what ran and in which order.
			writeWorkflowTestCommand(t, bin, "curl", `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" != -fsSL || "$2" != --retry || "$4" != -o ]]; then echo "unexpected curl arguments: $*" >&2; exit 1; fi
case "$6" in https://github.com/openai/codex/releases/download/rust-v*/codex-*.tar.gz) ;; *) echo "unexpected download URL: $6" >&2; exit 1 ;; esac
printf 'curl %s\n' "$6" >> "$TEST_LOG"
if [[ "$TEST_FAILURE" == curl-failure ]]; then exit 22; fi
cp "$TEST_ARCHIVE" "$5"
`)
			writeWorkflowTestCommand(t, bin, "shasum", `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" != '-a 256 -c -' ]]; then echo "unexpected shasum arguments: $*" >&2; exit 1; fi
read -r digest file
if [[ ! "$digest" =~ ^[0-9a-f]{64}$ || ! -f "$file" ]]; then echo "malformed digest line: $digest $file" >&2; exit 1; fi
printf 'shasum %s\n' "$digest" >> "$TEST_LOG"
if [[ "$TEST_FAILURE" == digest-mismatch ]]; then echo "$file: FAILED" >&2; exit 1; fi
echo "$file: OK"
`)
			githubEnv := filepath.Join(root, "github-env")
			githubPath := filepath.Join(root, "github-path")
			logPath := filepath.Join(root, "sequence.log")
			for _, path := range []string{githubEnv, githubPath, logPath} {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			version := "0.154.0"
			if failure == "unknown-version" {
				version = "9.9.9"
			}
			runnerOS, runnerArch := "Linux", "X64"
			if failure == "unsupported-runner" {
				runnerOS, runnerArch = "Windows", "X64"
			}
			command := exec.Command("bash", scriptPath, version)
			command.Env = []string{
				"PATH=" + bin + string(os.PathListSeparator) + "/usr/bin:/bin",
				"HOME=" + root,
				"RUNNER_OS=" + runnerOS,
				"RUNNER_ARCH=" + runnerArch,
				"RUNNER_TEMP=" + runner,
				"GITHUB_ENV=" + githubEnv,
				"GITHUB_PATH=" + githubPath,
				"TEST_LOG=" + logPath,
				"TEST_FAILURE=" + failure,
				"TEST_ARCHIVE=" + archivePath,
			}
			output, runErr := command.CombinedOutput()
			wantSuccess := failure == "success"
			if (runErr == nil) != wantSuccess {
				t.Fatalf("install success=%t want=%t: %v\n%s", runErr == nil, wantSuccess, runErr, output)
			}
			logBody, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			ran := strings.Fields(string(logBody))
			exported, err := os.ReadFile(githubEnv)
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "unknown-version", "unsupported-runner":
				if len(ran) != 0 {
					t.Fatalf("refused version still ran %v", ran)
				}
			case "curl-failure":
				if len(ran) != 2 || ran[0] != "curl" {
					t.Fatalf("download failure sequence = %v", ran)
				}
			case "digest-mismatch":
				if len(ran) != 4 || ran[2] != "shasum" {
					t.Fatalf("digest failure sequence = %v", ran)
				}
				if entries, err := os.ReadDir(filepath.Join(runner, "codex-"+version)); err != nil || len(entries) != 0 {
					t.Fatalf("archive extracted despite a digest mismatch: %v %v", entries, err)
				}
			case "asset-missing":
				if len(ran) != 4 {
					t.Fatalf("asset failure sequence = %v", ran)
				}
			case "success":
				if len(ran) != 4 || ran[0] != "curl" || ran[2] != "shasum" || ran[3] != "d7e18b2597ae8f242f5f31ee9e90deef48dbc9edd634d9868fb6435d08c07f02" {
					t.Fatalf("success sequence = %v, want the download, then the check of the published digest", ran)
				}
				binary := filepath.Join(runner, "codex-"+version, asset)
				info, err := os.Stat(binary)
				if err != nil || info.Mode().Perm() != 0o755 {
					t.Fatalf("installed binary: %v %v", info, err)
				}
				if !strings.Contains(string(exported), "ACR_CODEX_RELEASE_BIN="+binary+"\n") || !strings.Contains(string(exported), "ACR_CODEX_RELEASE_VERSION="+version+"\n") {
					t.Fatalf("GITHUB_ENV = %q", exported)
				}
				pathBody, err := os.ReadFile(githubPath)
				if err != nil || strings.TrimSpace(string(pathBody)) != filepath.Join(runner, "codex-"+version, "bin") {
					t.Fatalf("GITHUB_PATH = %q %v", pathBody, err)
				}
				if link, err := os.Readlink(filepath.Join(runner, "codex-"+version, "bin", "codex")); err != nil || link != binary {
					t.Fatalf("codex on PATH = %q %v", link, err)
				}
				return
			}
			if len(exported) != 0 {
				t.Fatalf("failure exported %q", exported)
			}
		})
	}
}

// codexTestArchive builds the tar.gz shape the official release publishes:
// one executable at the archive root named after the asset.
func codexTestArchive(t *testing.T, asset string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	zipped := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(zipped)
	body := []byte("#!/bin/sh\necho codex-cli 0.154.0\n")
	if err := archive.WriteHeader(&tar.Header{Name: asset, Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// Authentication lives centrally; the runtime matrix must remain auth-free.
func TestCodexAcceptanceUsesCentralSubscription(t *testing.T) {
	if _, err := os.Stat(filepath.Join("..", "..", ".github", "workflows", "codex-live.yml")); !os.IsNotExist(err) {
		t.Fatalf("retired API-only lane remains: %v", err)
	}
	_, source := parseCodexWorkflow(t, "review-trigger.yml")
	if !strings.Contains(source, "FLEET_DISPATCH_TOKEN") || !strings.Contains(source, "jbaruch/coding-policy") {
		t.Fatal("central dispatch route missing")
	}
}
