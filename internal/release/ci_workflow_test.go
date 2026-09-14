package release

import (
	"os"
	"path/filepath"
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

// workflowJobs is the subset of a workflow document these contracts read: the
// job dependency edges and, per step, what the runner executes.
type workflowJobs struct {
	Jobs map[string]struct {
		Needs any `yaml:"needs"`
		Steps []struct {
			Name  string `yaml:"name"`
			Run   string `yaml:"run"`
			Shell string `yaml:"shell"`
		} `yaml:"steps"`
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

// TestCIQualityJobRunsPythonDiagnostics holds the Python half of the diagnostics
// gate to what rules/language-diagnostics.md requires: the pinned engine is
// installed and then run over the project configuration at zero findings.
// Deleting either step, or reordering them, must fail the suite rather than
// silently reduce CI to Go-only diagnostics.
func TestCIQualityJobRunsPythonDiagnostics(t *testing.T) {
	t.Parallel()

	quality, exists := parseWorkflowJobs(t, ciWorkflow(t), "ci.yml").Jobs["quality"]
	if !exists {
		t.Fatal("ci.yml has no quality job")
	}
	install, diagnostics := -1, -1
	for index, step := range quality.Steps {
		if strings.Contains(step.Run, "pip install -r requirements-dev.txt") {
			install = index
		}
		if strings.Contains(step.Run, "pyright") {
			diagnostics = index
		}
	}
	if install < 0 {
		t.Fatal("quality job never installs the pinned diagnostics engine from requirements-dev.txt")
	}
	if diagnostics < 0 {
		t.Fatal("quality job never runs pyright")
	}
	if install > diagnostics {
		t.Fatalf("quality job installs the engine at step %d, after running it at step %d", install, diagnostics)
	}
	// --warnings is what makes this a zero-findings gate; without it a warning
	// passes CI and the gate stops enforcing what the rule requires.
	gate := quality.Steps[diagnostics].Run
	for _, required := range []string{"--project pyrightconfig.json", "--warnings"} {
		if !strings.Contains(gate, required) {
			t.Errorf("pyright invocation %q omits %q", strings.TrimSpace(gate), required)
		}
	}
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
