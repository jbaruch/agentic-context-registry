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
		Needs    any `yaml:"needs"`
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
	if got := codexStringList(t, job.Strategy.Matrix["os"]); !reflect.DeepEqual(got, []string{"ubuntu-latest", "macos-latest"}) {
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
}

// TestCITestJobInstallsLinuxBoundary keeps the ordinary test matrix able to
// run the real bubblewrap boundary test on its Linux leg.
func TestCITestJobInstallsLinuxBoundary(t *testing.T) {
	t.Parallel()
	workflow, _ := parseCodexWorkflow(t, "ci.yml")
	for _, step := range workflow.Jobs["test"].Steps {
		if strings.Contains(step.Run, "apt-get install") && strings.Contains(step.Run, "bubblewrap") {
			if step.If != "runner.os == 'Linux'" || step.Continue {
				t.Fatalf("bubblewrap install step = %#v", step)
			}
			return
		}
	}
	t.Fatal("the test job does not install bubblewrap on Linux")
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

// TestCodexLiveWorkflowContract keeps the credential-bearing lane explicitly
// triggered, guarded first, and unable to leak: the secret enters only through
// step environments, every required-lane flag is set, evidence uploads on any
// outcome, and no pull request or push can start it.
func TestCodexLiveWorkflowContract(t *testing.T) {
	t.Parallel()
	workflow, source := parseCodexWorkflow(t, "codex-live.yml")
	if len(workflow.On) != 1 {
		t.Fatalf("triggers = %#v, want workflow_dispatch only", workflow.On)
	}
	if _, ok := workflow.On["workflow_dispatch"]; !ok {
		t.Fatalf("triggers = %#v, want workflow_dispatch", workflow.On)
	}
	if workflow.Permissions["contents"] != "read" || len(workflow.Permissions) != 1 {
		t.Fatalf("permissions = %#v, want contents: read only", workflow.Permissions)
	}
	job, exists := workflow.Jobs["live"]
	if !exists || len(job.Steps) == 0 {
		t.Fatal("codex-live.yml has no live job")
	}
	guard := job.Steps[0]
	if guard.Env["CODEX_API_KEY"] != "${{ secrets.CODEX_API_KEY }}" || !strings.Contains(guard.Run, `[[ -z "${CODEX_API_KEY}" ]]`) || !strings.Contains(guard.Run, "https://github.com/jbaruch/agentic-context-registry/settings/secrets/actions") || !strings.Contains(guard.Run, ".env.example") {
		t.Fatalf("first step = %#v, want the credential guard", guard)
	}
	var convert, consume, upload, install, fixtures bool
	for _, step := range job.Steps {
		if step.Continue {
			t.Fatalf("step %q continues on error", step.Name)
		}
		if strings.Contains(step.Run, "secrets.") || strings.Contains(step.Run, "CODEX_API_KEY=") {
			t.Fatalf("step %q carries the credential in its script", step.Name)
		}
		for name, value := range step.With {
			if strings.Contains(value, "secrets.") {
				t.Fatalf("step %q passes the credential through with.%s", step.Name, name)
			}
		}
		switch {
		case strings.Contains(step.Run, "install-codex.sh"):
			install = strings.Contains(step.Run, "${{ inputs.codex-version }}")
		case strings.Contains(step.Run, "git clone"):
			fixtures = strings.Contains(step.Run, "f21fda887815af815979a4fea43a66eb5174ee3e") && strings.Contains(step.Run, "142babbb1e2bebc798eb42128ac2466f21b5131d")
		case strings.Contains(step.Run, "TestCodexLiveUpstreamConversion"):
			// Every fixture is either a checkout the lane converts or an explicit
			// `skip` whose reason is recorded beside it in the workflow.
			fixturesConfigured := true
			for _, key := range []string{"GOC", "FFA"} {
				value := step.Env["ACR_CODEX_LIVE_"+key]
				if value == "" || (value == "skip" && !strings.Contains(source, "ACR_CODEX_LIVE_"+key+": skip")) {
					fixturesConfigured = false
				}
			}
			convert = fixturesConfigured && step.Env["CODEX_API_KEY"] == "${{ secrets.CODEX_API_KEY }}" && step.Env["ACR_CODEX_LIVE"] == "1" && step.Env["ACR_CODEX_LIVE_REQUIRED"] == "1" && step.Env["ACR_CODEX_LIVE_EVIDENCE"] != "" && step.Env["ACR_CODEX_LIVE_GOC"] != "skip" && step.Env["ACR_CODEX_LIVE_GOC_SHA"] == "f21fda887815af815979a4fea43a66eb5174ee3e" && step.Env["ACR_CODEX_LIVE_FFA_SHA"] == "142babbb1e2bebc798eb42128ac2466f21b5131d" && strings.Contains(step.Run, "TestCodexLiveSemanticConversion")
		case strings.Contains(step.Run, "TestSemanticLiveGeneratedPublication"):
			consume = step.Env["ACR_SEMANTIC_ACCEPTANCE_REQUIRED"] == "1" && strings.Contains(step.Run, "ACR_SEMANTIC_ACCEPTANCE_COMMIT=") && strings.Contains(step.Run, "converted-root.txt") && strings.Contains(step.Run, "exit 1")
		case strings.HasPrefix(step.Uses, "actions/upload-artifact@"):
			upload = step.If == "always()" && step.With["if-no-files-found"] == "error"
		}
	}
	if !install || !fixtures || !convert || !consume || !upload {
		t.Fatalf("live job lacks install=%t fixtures=%t convert=%t consume=%t upload=%t", install, fixtures, convert, consume, upload)
	}
	for _, forbidden := range []string{"pull_request", "push:", "continue-on-error", "workflow_call"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("codex-live.yml contains forbidden %q", forbidden)
		}
	}
	assertWorkflowActionsPinned(t, source)

	// The guard is executed, not read: an empty secret stops the job with the
	// remedy, a present one lets it continue.
	for _, secret := range []string{"", "fixture-credential"} {
		script := filepath.Join(t.TempDir(), "guard.sh")
		if err := os.WriteFile(script, []byte(guard.Run), 0o600); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("bash", "-e", script)
		command.Env = []string{"PATH=/usr/bin:/bin", "CODEX_API_KEY=" + secret}
		output, err := command.CombinedOutput()
		if (err == nil) != (secret != "") {
			t.Fatalf("guard with secret=%q: %v\n%s", secret, err, output)
		}
		if secret == "" && !strings.Contains(string(output), "settings/secrets/actions") {
			t.Fatalf("guard refusal lacks the remedy: %s", output)
		}
		if strings.Contains(string(output), "fixture-credential") {
			t.Fatal("guard echoed the credential")
		}
	}
}
