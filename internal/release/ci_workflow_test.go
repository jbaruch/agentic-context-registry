package release

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// ciWorkflow returns the repository's own CI workflow, the counterpart to
// releaseWorkflow for .github/workflows/ci.yml.
func ciWorkflow(t *testing.T) []byte {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

// workflowStepSpec is one step as these contracts read it: what the runner
// executes, and whether the step may be skipped or allowed to fail.
type workflowStepSpec struct {
	Name     string `yaml:"name"`
	Run      string `yaml:"run"`
	Shell    string `yaml:"shell"`
	If       string `yaml:"if"`
	Continue bool   `yaml:"continue-on-error"`
}

// workflowJobs is the subset of a workflow document these contracts read: the
// job dependency edges and each job's steps.
type workflowJobs struct {
	Jobs map[string]struct {
		Needs any                `yaml:"needs"`
		Steps []workflowStepSpec `yaml:"steps"`
	} `yaml:"jobs"`
}

func parseWorkflowJobs(t *testing.T, contents []byte, source string) workflowJobs {
	t.Helper()
	var document workflowJobs
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatalf("parse %s: %v", source, err)
	}
	return document
}

// TestCIQualityJobRunsPythonDiagnostics executes the quality job's own steps
// against command fixtures and decides the contract on what ran, not on how the
// YAML is spelled. A step that merely echoed the pyright command line would
// leave the fixtures untouched and fail here.
func TestCIQualityJobRunsPythonDiagnostics(t *testing.T) {
	for _, failure := range []string{"success", "venv-failure", "install-failure", "checker-failure"} {
		t.Run(failure, func(t *testing.T) { runCIQualityJob(t, failure) })
	}
}

// Run the quality job the way the runner does: every step in its own `bash -e`,
// stopping at the first non-zero exit, carrying whatever a step appended to
// GITHUB_PATH into the steps that follow.
//
// The engine is a fixture on purpose. Executing the pinned Pyright would bind
// the assertion to whichever version the machine resolves, and the test job
// installs none; the quality job's own pyright step is where the real engine's
// verdict is established. What this test owns is the plumbing around it —
// that the install runs before the check, that the check is reached only
// through the interpreter the install provisioned, and that either one failing
// stops the job.
func runCIQualityJob(t *testing.T, failure string) {
	t.Helper()

	quality, exists := parseWorkflowJobs(t, ciWorkflow(t), "ci.yml").Jobs["quality"]
	if !exists {
		t.Fatal("ci.yml has no quality job")
	}

	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	runner := filepath.Join(root, "runner")
	for _, dir := range []string{bin, runner, filepath.Join(root, "internal", "producerconvert")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"requirements-dev.txt", "pyrightconfig.json", "internal/producerconvert/check-tests.py"} {
		body, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Every argument guard branches and exits explicitly. bash 3.2, still
	// /bin/bash on macOS runners, does not apply `set -e` to a failing `[[ ]]`,
	// so a bare guard would silently accept whatever the workflow passed.
	writeWorkflowTestCommand(t, bin, "gofmt", `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" != '-l .' ]]; then echo "unexpected gofmt arguments: $*" >&2; exit 1; fi
printf 'format\n' >> "$TEST_LOG"
`)
	writeWorkflowTestCommand(t, bin, "go", `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
 'vet ./...') printf 'vet\n' >> "$TEST_LOG" ;;
 *) echo "unexpected Go command: $*" >&2; exit 1 ;;
esac
`)
	writeWorkflowTestCommand(t, bin, "python3", `#!/usr/bin/env bash
set -euo pipefail
if [[ "$#" != 3 || "$1" != -m || "$2" != venv || "$3" != "$RUNNER_TEMP/acr-python-diagnostics" ]]; then
  echo "unexpected python3 arguments: $*" >&2
  exit 1
fi
printf 'venv\n' >> "$TEST_LOG"
if [[ "$TEST_FAILURE" == venv-failure ]]; then exit 21; fi
mkdir -p "$3/bin"
cp "$TEST_BIN/pip-python" "$3/bin/python"
`)
	writeWorkflowTestCommand(t, bin, "pip-python", `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" != '-m pip install -r requirements-dev.txt' ]]; then
  echo "unexpected pip arguments: $*" >&2
  exit 1
fi
cmp requirements-dev.txt "$TEST_REQUIREMENTS"
printf 'install\n' >> "$TEST_LOG"
if [[ "$TEST_FAILURE" == install-failure ]]; then exit 22; fi
cp "$TEST_BIN/checker" "$RUNNER_TEMP/acr-python-diagnostics/bin/pyright"
`)
	// The checker asserts its own arguments, so dropping --warnings turns the
	// zero-findings gate into a warning-tolerant one and fails the success case.
	writeWorkflowTestCommand(t, bin, "checker", `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" != '--project pyrightconfig.json --warnings' ]]; then
  echo "unexpected pyright arguments: $*" >&2
  exit 1
fi
printf 'checker\n' >> "$TEST_LOG"
if [[ "$TEST_FAILURE" == checker-failure ]]; then exit 23; fi
printf '0 errors, 0 warnings, 0 informations\n'
`)

	requirementPath, err := filepath.Abs(filepath.Join("..", "..", "requirements-dev.txt"))
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "sequence.log")
	githubPath := filepath.Join(root, "github-path")
	if err := os.WriteFile(githubPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	executed, failedStep, output := runWorkflowJobSteps(t, quality.Steps, jobEnvironment{
		root:        root,
		bin:         bin,
		runner:      runner,
		githubPath:  githubPath,
		logPath:     logPath,
		failure:     failure,
		requirement: requirementPath,
	})

	wantSuccess := failure == "success"
	if (failedStep == "") != wantSuccess {
		t.Fatalf("quality job success=%t want=%t (failed at %q)\n%s", failedStep == "", wantSuccess, failedStep, output)
	}

	// The fixtures' own log is the evidence. Absence of a later token is what
	// proves a failing step stopped the job rather than being stepped over.
	expected := []string{"format", "vet", "venv"}
	if failure != "venv-failure" {
		expected = append(expected, "install")
	}
	if failure != "venv-failure" && failure != "install-failure" {
		expected = append(expected, "checker")
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(body))
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("executed order=%q want=%q\n%s", got, expected, output)
	}
	scripted := 0
	for _, step := range quality.Steps {
		if strings.TrimSpace(step.Run) != "" {
			scripted++
		}
	}
	if wantSuccess && executed != scripted {
		t.Fatalf("quality job ran %d of its %d scripted steps", executed, scripted)
	}
	t.Logf("%s: sequence=%s failed=%q output=%s", failure, fmt.Sprint(got), failedStep, output)
}

// jobEnvironment is the sandbox one simulated job runs inside.
type jobEnvironment struct {
	root        string
	bin         string
	runner      string
	githubPath  string
	logPath     string
	failure     string
	requirement string
}

// runWorkflowJobSteps executes each scripted step in order and returns how many
// ran, the name of the step that failed (empty when all passed), and that
// step's output.
//
// PATH deliberately starts as the fixture directory plus a bare system path:
// omitting the developer's own PATH means the only pyright reachable is the one
// the install step provisions, so losing the GITHUB_PATH export surfaces as a
// failure instead of silently resolving a locally installed engine.
func runWorkflowJobSteps(t *testing.T, steps []workflowStepSpec, env jobEnvironment) (int, string, []byte) {
	t.Helper()

	const systemPath = "/usr/bin:/bin"
	exported := []string(nil)
	executed := 0
	for index, step := range steps {
		if strings.TrimSpace(step.Run) == "" {
			continue
		}
		if step.If != "" || step.Continue {
			t.Fatalf("step %q is conditional or continues on error; the gate must fail closed", step.Name)
		}
		script := filepath.Join(env.root, fmt.Sprintf("step-%02d.sh", index))
		if err := os.WriteFile(script, []byte(step.Run), 0o600); err != nil {
			t.Fatal(err)
		}
		search := append([]string{env.bin}, exported...)
		// `bash -e <script>` is the runner's own default invocation for a `run:`
		// step on Linux, so the step meets exactly the shell CI gives it.
		command := exec.Command("bash", "-e", script)
		command.Dir = env.root
		command.Env = []string{
			"PATH=" + strings.Join(append(search, systemPath), string(os.PathListSeparator)),
			"HOME=" + env.root,
			"RUNNER_TEMP=" + env.runner,
			"GITHUB_PATH=" + env.githubPath,
			"TEST_BIN=" + env.bin,
			"TEST_LOG=" + env.logPath,
			"TEST_FAILURE=" + env.failure,
			"TEST_REQUIREMENTS=" + env.requirement,
		}
		output, err := command.CombinedOutput()
		executed++
		if err != nil {
			return executed, step.Name, output
		}
		// The runner folds whatever the step appended to GITHUB_PATH into the
		// search path of every step after it.
		appended, err := os.ReadFile(env.githubPath)
		if err != nil {
			t.Fatal(err)
		}
		exported = nil
		for _, line := range strings.Split(string(appended), "\n") {
			if entry := strings.TrimSpace(line); entry != "" {
				exported = append([]string{entry}, exported...)
			}
		}
	}
	return executed, "", nil
}

// TestCITestJobWaitsForQualityGate keeps the cheap diagnostics job ahead of the
// expensive test matrix, per rules/code-formatting.md CI Integration.
func TestCITestJobWaitsForQualityGate(t *testing.T) {
	t.Parallel()

	test, exists := parseWorkflowJobs(t, ciWorkflow(t), "ci.yml").Jobs["test"]
	if !exists {
		t.Fatal("ci.yml has no test job")
	}
	var needs []string
	switch declared := test.Needs.(type) {
	case string:
		needs = []string{declared}
	case []any:
		for _, entry := range declared {
			name, ok := entry.(string)
			if !ok {
				t.Fatalf("test job needs entry = %#v, want a job name", entry)
			}
			needs = append(needs, name)
		}
	default:
		t.Fatalf("test job needs = %#v, want quality", test.Needs)
	}
	for _, name := range needs {
		if name == "quality" {
			return
		}
	}
	t.Fatalf("test job needs = %q, want the quality gate among them", needs)
}

// TestWorkflowShellBlocksFailFast requires every multi-line shell block in every
// workflow to opt into all three failure modes itself. GitHub's default step
// invocation enables neither `-u` nor `pipefail`, so an unset variable or a
// failing pipeline stage otherwise passes the step silently
// (rules/error-handling.md Shell Error Handling).
func TestWorkflowShellBlocksFailFast(t *testing.T) {
	t.Parallel()

	paths, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no workflows found to inspect")
	}
	inspected := 0
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(path)
		for job, definition := range parseWorkflowJobs(t, contents, name).Jobs {
			for _, step := range definition.Steps {
				body := strings.TrimSpace(step.Run)
				// Single-line steps carry no continuation to guard.
				if body == "" || !strings.Contains(body, "\n") {
					continue
				}
				// A step that selects a non-bash shell needs a different prologue.
				if step.Shell != "" && step.Shell != "bash" && step.Shell != "sh" {
					continue
				}
				inspected++
				if first, _, _ := strings.Cut(body, "\n"); strings.TrimSpace(first) != "set -euo pipefail" {
					t.Errorf("%s job %q step %q begins with %q, want `set -euo pipefail`", name, job, step.Name, strings.TrimSpace(first))
				}
			}
		}
	}
	if inspected == 0 {
		t.Fatal("inspected no multi-line shell blocks; the contract would pass vacuously")
	}
}
