package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/producerconvert"
)

// The live lane runs the shipped binary against the real Codex CLI with the
// operator's configured account or CODEX_API_KEY. It is opt-in through
// ACR_CODEX_LIVE=1; ACR_CODEX_LIVE_REQUIRED=1 turns every skip into a failure
// so a CI lane cannot go green by skipping. Every process result is written
// under ACR_CODEX_LIVE_EVIDENCE before any assertion reads it, and a retry
// never reuses an attempt directory.

type codexLiveLane struct {
	t        *testing.T
	evidence string
	version  string
	secrets  []string
}

func codexLive(t *testing.T, name string) *codexLiveLane {
	t.Helper()
	if os.Getenv("ACR_CODEX_LIVE") != "1" {
		if os.Getenv("ACR_CODEX_LIVE_REQUIRED") == "1" {
			t.Fatal("ACR_CODEX_LIVE_REQUIRED=1 but ACR_CODEX_LIVE is not 1")
		}
		t.Skip("live Codex conversion is opt-in through ACR_CODEX_LIVE=1")
	}
	base := os.Getenv("ACR_CODEX_LIVE_EVIDENCE")
	if base == "" {
		t.Fatal("ACR_CODEX_LIVE=1 requires ACR_CODEX_LIVE_EVIDENCE, the directory every process result is written to")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Fatalf("no verified Codex read boundary on %s", runtime.GOOS)
	}
	// The journey harness moves HOME to a temporary directory; the child
	// binary must still find the configured Codex home, so it is pinned from
	// the environment this process started with.
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		codexHome = filepath.Join(home, ".codex")
	}
	t.Setenv("CODEX_HOME", codexHome)
	lane := &codexLiveLane{t: t}
	if key := os.Getenv("CODEX_API_KEY"); key != "" {
		lane.secrets = append(lane.secrets, key)
	}
	platform := filepath.Join(base, runtime.GOOS+"-"+runtime.GOARCH, name)
	for attempt := 1; ; attempt++ {
		candidate := filepath.Join(platform, "attempt-"+strconv.Itoa(attempt))
		if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(candidate, 0o755); err == nil {
			lane.evidence = candidate
			break
		} else if !errors.Is(err, os.ErrExist) {
			t.Fatal(err)
		}
	}
	t.Logf("evidence: %s", lane.evidence)
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Fatalf("live lane needs codex on PATH: %v", err)
	}
	version, err := exec.Command(codex, "--version").Output()
	if err != nil {
		t.Fatalf("codex --version: %v", err)
	}
	lane.version = strings.TrimSpace(strings.SplitN(string(version), "\n", 2)[0])
	lane.record("codex-version.txt", lane.version+"\n")
	// `codex login status` reports the auth.json route only; the CODEX_API_KEY
	// route reports "Not logged in" and still authenticates the run, so only
	// the exit code is logged and nothing depends on it.
	status := exec.Command(codex, "login", "status")
	status.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	statusOutput, statusErr := status.CombinedOutput()
	lane.record("login-status.txt", lane.redact(string(statusOutput)))
	t.Logf("codex login status exit error: %v", statusErr)
	return lane
}

func (lane *codexLiveLane) redact(text string) string {
	for _, secret := range lane.secrets {
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	return text
}

func (lane *codexLiveLane) record(name, body string) {
	lane.t.Helper()
	if err := os.WriteFile(filepath.Join(lane.evidence, name), []byte(lane.redact(body)), 0o644); err != nil {
		lane.t.Fatal(err)
	}
}

// run executes the shipped binary and files its complete result before the
// caller sees it, so a failing assertion never loses the evidence.
func (lane *codexLiveLane) run(binary, name, stateHome string, args ...string) journeyRun {
	lane.t.Helper()
	stdout, stderr, exit := hostileRunBinary(lane.t, binary, stateHome, strings.NewReader(""), args...)
	lane.record(name+".stdout", stdout)
	lane.record(name+".stderr", stderr)
	lane.record(name+".exit", strconv.Itoa(exit)+"\n")
	lane.record(name+".argv", strings.Join(args, "\n")+"\n")
	for _, secret := range lane.secrets {
		if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
			lane.t.Fatalf("%s: credential value reached a process stream", name)
		}
	}
	return journeyRun{args: args, stdout: stdout, stderr: stderr, exit: exit}
}

// codexRuns decodes the agentRuns of a report envelope.
func codexRuns(t *testing.T, result map[string]any) []map[string]any {
	t.Helper()
	raw, ok := result["agentRuns"].([]any)
	if !ok {
		return nil
	}
	var runs []map[string]any
	for _, entry := range raw {
		run, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("agentRuns entry = %#v", entry)
		}
		runs = append(runs, run)
	}
	return runs
}

func (lane *codexLiveLane) assertCodexRuns(result map[string]any) {
	lane.t.Helper()
	runs := codexRuns(lane.t, result)
	if len(runs) == 0 {
		lane.t.Fatalf("no agentRuns in %#v", result)
	}
	for index, run := range runs {
		if run["provider"] != "codex" || run["runtimeVersion"] != lane.version || run["isolation"] == "" || run["isolation"] == nil {
			lane.t.Fatalf("agentRuns[%d] = %#v, want provider codex at %s with isolation evidence", index, run, lane.version)
		}
		if failure, _ := run["failure"].(string); failure != "" {
			lane.t.Fatalf("agentRuns[%d] failed: %s", index, failure)
		}
	}
}

// TestCodexLiveSemanticConversion converts the clean producer fixture through
// the shipped binary with a real Codex release: preview leaves the tree
// untouched, apply rewrites the helper the deterministic converter refuses,
// and the rerun is inert without a provider call.
func TestCodexLiveSemanticConversion(t *testing.T) {
	lane := codexLive(t, "semantic")
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	root := cleanProducerFixture(t)
	name := "packages/compass/skills/answer/info.py"
	reverify2Put(t, root, name, "print('tessl install old/compass')\n", 0o644)
	args := []string{"migrate", "tessl-plugin", filepath.Join(root, "packages/compass"), "--acr-only", "--repository", "https://github.com/example/semantic-compass", "--agent", "codex", "--json"}
	before := snapshotProjectTree(t, root)
	preview := lane.run(binary, "dry-run", project.stateHome, append(append([]string{}, args...), "--dry-run")...)
	if preview.exit != 0 {
		t.Fatalf("dry-run exit %d\n%s\n%s", preview.exit, preview.stdout, preview.stderr)
	}
	result := journeyResult(t, preview.stdout)
	lane.assertCodexRuns(result)
	if result["wrote"] != false || result["current"] != false {
		t.Fatalf("preview = %#v", result)
	}
	assertTreeUnchanged(t, before, root, "live Codex dry-run")
	applied := lane.run(binary, "apply", project.stateHome, args...)
	if applied.exit != 0 {
		t.Fatalf("apply exit %d\n%s\n%s", applied.exit, applied.stdout, applied.stderr)
	}
	result = journeyResult(t, applied.stdout)
	lane.assertCodexRuns(result)
	if result["wrote"] != true {
		t.Fatalf("apply = %#v", result)
	}
	body, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "tessl install") {
		t.Fatalf("helper still carries the Tessl operation: %s", body)
	}
	if _, err := os.Stat(filepath.Join(root, producerconvert.ReceiptPath)); err != nil {
		t.Fatal(err)
	}
	rerun := lane.run(binary, "rerun", project.stateHome, args...)
	if rerun.exit != 0 {
		t.Fatalf("rerun exit %d\n%s\n%s", rerun.exit, rerun.stdout, rerun.stderr)
	}
	result = journeyResult(t, rerun.stdout)
	if result["current"] != true || len(codexRuns(t, result)) != 0 {
		t.Fatalf("rerun = %#v, want current without a provider call", result)
	}
}

// codexLiveFixture is one untouched upstream plugin the live lane converts.
// The original test invocations come from the upstream repository's own CI
// definition at the pinned commit, never from ACR.
type codexLiveFixture struct {
	key        string
	pkg        string
	repository string
	tests      [][]string
	setup      func(t *testing.T, root string) []string
}

var codexLiveFixtures = []codexLiveFixture{
	{
		// tesslio/good-oss-citizen .github/workflows/test.yml at f21fda88: the two
		// offline suites always; the GitHub envelope sweep only with a token.
		key: "GOC", pkg: "plugins/good-oss-citizen", repository: "https://github.com/tesslio/good-oss-citizen",
		tests: [][]string{{"python3", "tests/test_contribution_declaration.py"}, {"python3", "tests/test_install_gate_scaffold.py"}},
	},
	{
		// jbaruch/frequent-flyer-advocate .github/scripts/pre-publish-gate.sh at
		// 142babbb: pinned pyright plus three suites, run in a throwaway venv.
		key: "FFA", pkg: ".", repository: "https://github.com/jbaruch/frequent-flyer-advocate",
		tests: [][]string{{"bash", ".github/scripts/pre-publish-gate.sh"}},
		setup: func(t *testing.T, root string) []string {
			t.Helper()
			venv := filepath.Join(t.TempDir(), "venv")
			command := exec.Command("python3", "-m", "venv", venv)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("python3 -m venv: %v\n%s", err, output)
			}
			return []string{"PATH=" + filepath.Join(venv, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"), "VIRTUAL_ENV=" + venv}
		},
	},
}

// TestCodexLiveUpstreamConversion starts from an untouched checkout of each
// upstream fixture supplied through ACR_CODEX_LIVE_<KEY>: the original tests
// pass before conversion, the deterministic converter refuses, a real Codex
// release converts, nothing outside the reported delta moves, the converted
// commit is recorded, the rerun is inert, and the original tests still pass
// on the converted tree. Optional ACR_CODEX_LIVE_<KEY>_SHA pins the checkout
// and ACR_CODEX_LIVE_<KEY>_REPOSITORY overrides the conversion target. The
// literal value `skip` is an explicit, caller-declared exclusion for a fixture
// ACR cannot convert yet; it is honored under ACR_CODEX_LIVE_REQUIRED=1 and
// logged, so a required lane never skips a fixture silently.
func TestCodexLiveUpstreamConversion(t *testing.T) {
	for _, fixture := range codexLiveFixtures {
		t.Run(fixture.key, func(t *testing.T) {
			root := os.Getenv("ACR_CODEX_LIVE_" + fixture.key)
			if root == "skip" {
				t.Skipf("ACR_CODEX_LIVE_%s=skip: the caller explicitly excluded this fixture; the exclusion and its reason are recorded where the variable is set", fixture.key)
			}
			if root == "" {
				if os.Getenv("ACR_CODEX_LIVE_REQUIRED") == "1" && os.Getenv("ACR_CODEX_LIVE") == "1" {
					t.Fatalf("ACR_CODEX_LIVE_REQUIRED=1 but ACR_CODEX_LIVE_%s is unset; supply the checkout or set it to skip with a recorded reason", fixture.key)
				}
				t.Skipf("upstream fixture is supplied through ACR_CODEX_LIVE_%s", fixture.key)
			}
			lane := codexLive(t, "upstream-"+strings.ToLower(fixture.key))
			binary := journeyBuiltBinary(t)
			project := newJourneyProject(t, nil)
			if journeyGit(t, root, "status", "--porcelain") != "" {
				t.Fatalf("%s is not an untouched checkout", root)
			}
			head := journeyGit(t, root, "rev-parse", "HEAD")
			if want := os.Getenv("ACR_CODEX_LIVE_" + fixture.key + "_SHA"); want != "" && head != want {
				t.Fatalf("%s is at %s, want %s", root, head, want)
			}
			lane.record("source-commit.txt", head+"\n")
			repository := fixture.repository
			if override := os.Getenv("ACR_CODEX_LIVE_" + fixture.key + "_REPOSITORY"); override != "" {
				repository = override
			}
			var env []string
			if fixture.setup != nil {
				env = fixture.setup(t, root)
			}
			// The baseline must leave the checkout exactly as it found it; a
			// suite that writes into the tree would otherwise hand the converter
			// artifacts the upstream never published.
			untouched := snapshotProjectTree(t, root)
			baseline := lane.originalTests(root, fixture, env, "baseline")
			assertTreeUnchanged(t, untouched, root, "original tests in the untouched tree")
			args := []string{"migrate", "tessl-plugin", filepath.Join(root, fixture.pkg), "--acr-only", "--repository", repository, "--json"}
			// The deterministic converter must refuse: a fixture it converts
			// on its own would not exercise Codex.
			refused := lane.run(binary, "deterministic-dry-run", project.stateHome, append(append([]string{}, args...), "--dry-run")...)
			if refused.exit != 1 || journeyError(t, refused.stderr)["code"] != "unsupported_semantic_conversion" {
				t.Fatalf("deterministic preview did not refuse with unsupported_semantic_conversion: exit %d\n%s", refused.exit, refused.stderr)
			}
			codexArgs := append(append([]string{}, args...), "--agent", "codex")
			before := snapshotProjectTree(t, root)
			preview := lane.run(binary, "codex-dry-run", project.stateHome, append(append([]string{}, codexArgs...), "--dry-run")...)
			if preview.exit != 0 {
				t.Fatalf("codex dry-run exit %d\n%s\n%s", preview.exit, preview.stdout, preview.stderr)
			}
			result := journeyResult(t, preview.stdout)
			lane.assertCodexRuns(result)
			if result["wrote"] != false {
				t.Fatalf("preview wrote: %#v", result)
			}
			assertTreeUnchanged(t, before, root, "live upstream dry-run")
			applied := lane.run(binary, "codex-apply", project.stateHome, codexArgs...)
			if applied.exit != 0 {
				t.Fatalf("codex apply exit %d\n%s\n%s", applied.exit, applied.stdout, applied.stderr)
			}
			result = journeyResult(t, applied.stdout)
			lane.assertCodexRuns(result)
			if result["wrote"] != true {
				t.Fatalf("apply wrote nothing: %#v", result)
			}
			// No hand-porting: the paths Git sees changed are exactly the
			// reported delta plus the receipt.
			reported := map[string]bool{producerconvert.ReceiptPath: true}
			if changes, ok := result["changes"].([]any); ok {
				for _, entry := range changes {
					change := entry.(map[string]any)
					reported[change["path"].(string)] = true
				}
			}
			moved := map[string]bool{}
			for _, line := range strings.Split(journeyGit(t, root, "status", "--porcelain", "--untracked-files=all"), "\n") {
				if len(line) > 3 {
					moved[strings.TrimSpace(line[3:])] = true
				}
			}
			if !reflect.DeepEqual(moved, reported) {
				t.Fatalf("changed paths %v differ from the reported delta %v", sortedKeys(moved), sortedKeys(reported))
			}
			lane.record("changed-paths.txt", strings.Join(sortedKeys(moved), "\n")+"\n")
			journeyGit(t, root, "add", "-A")
			journeyGit(t, root, "commit", "-qm", "Convert "+fixture.key+" through acr migrate tessl-plugin --agent codex ("+lane.version+")")
			converted := journeyGit(t, root, "rev-parse", "HEAD")
			lane.record("converted-commit.txt", converted+"\n")
			lane.record("converted-root.txt", root+"\n")
			archive := exec.Command("git", "-C", root, "archive", "--format=tar.gz", "-o", filepath.Join(lane.evidence, "converted-tree.tar.gz"), "HEAD")
			if output, err := archive.CombinedOutput(); err != nil {
				t.Fatalf("git archive: %v\n%s", err, output)
			}
			rerun := lane.run(binary, "rerun", project.stateHome, append(append([]string{}, args...), "--dry-run")...)
			if rerun.exit != 0 || journeyResult(t, rerun.stdout)["current"] != true {
				t.Fatalf("converted tree is not current without a provider: exit %d\n%s\n%s", rerun.exit, rerun.stdout, rerun.stderr)
			}
			after := lane.originalTests(root, fixture, env, "converted")
			for index := range fixture.tests {
				if after[index] < baseline[index] {
					t.Fatalf("original test %v reported %d passes on the converted tree, %d before conversion", fixture.tests[index], after[index], baseline[index])
				}
			}
			t.Logf("%s converted at %s from %s with %s; original tests pass counts %v -> %v", fixture.key, converted, head, lane.version, baseline, after)
		})
	}
}

// originalTests runs each upstream test command in the checkout with its
// original invocation, files the logs, requires exit 0, and returns the
// number of reported passes per command so the converted tree is held to at
// least the baseline.
func (lane *codexLiveLane) originalTests(root string, fixture codexLiveFixture, env []string, phase string) []int {
	lane.t.Helper()
	var counts []int
	for index, argv := range fixture.tests {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		command := exec.CommandContext(ctx, argv[0], argv[1:]...)
		command.Dir = root
		// Python must not litter the checkout with bytecode caches: the
		// converter refuses __pycache__ inside a skill tree as unpublishable.
		command.Env = append(append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1"), env...)
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		err := command.Run()
		cancel()
		name := fmt.Sprintf("%s-test-%d", phase, index+1)
		lane.record(name+".log", output.String())
		if err != nil {
			lane.t.Fatalf("%s: original test %v failed in the %s tree: %v\n%s", fixture.key, argv, phase, err, output.String())
		}
		passes := 0
		for _, line := range strings.Split(output.String(), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "PASS") {
				passes++
			}
		}
		lane.record(name+".passes", strconv.Itoa(passes)+"\n")
		counts = append(counts, passes)
	}
	return counts
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// TestCodexLiveEvidenceIsMachineReadable keeps the evidence layout stable for
// the CI upload and the report: a JSON index of every attempt directory.
func TestCodexLiveEvidenceIsMachineReadable(t *testing.T) {
	if os.Getenv("ACR_CODEX_LIVE") != "1" {
		t.Skip("live Codex conversion is opt-in through ACR_CODEX_LIVE=1")
	}
	base := os.Getenv("ACR_CODEX_LIVE_EVIDENCE")
	if base == "" {
		t.Fatal("ACR_CODEX_LIVE_EVIDENCE is unset")
	}
	index := map[string]map[string]string{}
	err := filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".exit") {
			return nil
		}
		relative, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		attempt := filepath.Dir(relative)
		if index[attempt] == nil {
			index[attempt] = map[string]string{}
		}
		index[attempt][strings.TrimSuffix(filepath.Base(relative), ".exit")] = strings.TrimSpace(string(body))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "index.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
