package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

const (
	testerReverifyClosedStdin = "ACR_TESTER_REVERIFY_CLOSED_STDIN"
	testerReverifySource      = "github:tester/nonterminal"
)

func TestTesterReverifyBuiltBinaryJSONUsesStdoutOnly(t *testing.T) {
	binary := reverifyBuildACR(t)
	var networkCalls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		networkCalls.Add(1)
		http.Error(writer, "network is forbidden in this deterministic test", http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")

	t.Run("init --json", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# detected\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, exitCode := reverifyRunACR(t, binary, t.TempDir(), "init", "--project", root, "--json")
		assertTesterReverifyJSONStdout(t, "init", stdout, stderr, exitCode)
	})

	t.Run("install SOURCE --json", func(t *testing.T) {
		root := testerReverifyPinnedProject(t)
		stdout, stderr, exitCode := reverifyRunACR(t, binary, t.TempDir(),
			"install", testerReverifySource+"@v1.0.0", "--project", root, "--json")
		assertTesterReverifyJSONStdout(t, "install", stdout, stderr, exitCode)
	})
	if calls := networkCalls.Load(); calls != 0 {
		t.Fatalf("built-binary JSON rows made %d network call(s)", calls)
	}
}

func assertTesterReverifyJSONStdout(t *testing.T, command, stdout, stderr string, exitCode int) {
	t.Helper()
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("%s exit = %d, stdout = %q, stderr = %q", command, exitCode, stdout, stderr)
	}
	decoder := json.NewDecoder(strings.NewReader(stdout))
	var envelope struct {
		OK      bool   `json:"ok"`
		Command string `json:"command"`
	}
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("decode %s stdout %q: %v", command, stdout, err)
	}
	if !envelope.OK || envelope.Command != command {
		t.Fatalf("%s envelope = %#v", command, envelope)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("%s stdout contains more than one envelope: %q (second decode: %v)", command, stdout, err)
	}
	if strings.Count(stdout, "\n") != 1 {
		t.Fatalf("%s stdout is not exactly one envelope line: %q", command, stdout)
	}
	if containsTesterReverifyQuestion(stdout) || containsTesterReverifyQuestion(stderr) {
		t.Fatalf("%s JSON output contains question text: stdout=%q stderr=%q", command, stdout, stderr)
	}
}

func TestTesterReverifyBuiltBinaryRejectsClosedAndDevNullStdin(t *testing.T) {
	if os.Getenv(testerReverifyClosedStdin) == "1" {
		separator := -1
		for index, argument := range os.Args {
			if argument == "--" {
				separator = index
				break
			}
		}
		if separator < 0 || separator+1 >= len(os.Args) {
			t.Fatal("closed-stdin helper has no target command")
		}
		if err := syscall.Close(0); err != nil {
			t.Fatalf("close stdin: %v", err)
		}
		if err := syscall.Exec(os.Args[separator+1], os.Args[separator+1:], os.Environ()); err != nil {
			t.Fatalf("exec target with closed stdin: %v", err)
		}
	}

	binary := reverifyBuildACR(t)
	tests := []struct {
		name string
		run  func(*testing.T, string, string, []string) (string, string, int)
	}{
		{name: "fd 0 closed", run: testerReverifyRunClosedStdin},
		{name: "fd 0 opened as dev null", run: testerReverifyRunDevNullStdin},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := testerReverifyLatestProject(t)
			projectBefore := testerReverifyStateBytes(t, root)
			args := []string{"install", testerReverifySource + "@v0.5.0", "--project", root}
			stdout, stderr, exitCode := test.run(t, binary, t.TempDir(), args)

			if exitCode != cli.ExitUsage || stdout != "" {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
			}
			if containsTesterReverifyQuestion(stdout) || containsTesterReverifyQuestion(stderr) {
				t.Fatalf("non-terminal stdin printed a question: stdout=%q stderr=%q", stdout, stderr)
			}
			if !strings.Contains(stderr, "rollback from the locked v1.0.0") ||
				!strings.Contains(stderr, "--hold") || !strings.Contains(stderr, "--pin") {
				t.Fatalf("typed downgrade refusal = %q", stderr)
			}
			if after := testerReverifyStateBytes(t, root); !bytes.Equal(after, projectBefore) {
				t.Fatalf("typed refusal changed dependency state:\n before %q\n after  %q", projectBefore, after)
			}
		})
	}
}

func testerReverifyRunClosedStdin(t *testing.T, binary, stateHome string, args []string) (string, string, int) {
	t.Helper()
	commandArgs := append([]string{"-test.run=^TestTesterReverifyBuiltBinaryRejectsClosedAndDevNullStdin$", "--", binary}, args...)
	command := exec.Command(os.Args[0], commandArgs...)
	command.Env = append(environmentWith("ACR_STATE_HOME", stateHome), testerReverifyClosedStdin+"=1")
	return hostileRunCommand(t, command)
}

func testerReverifyRunDevNullStdin(t *testing.T, binary, stateHome string, args []string) (string, string, int) {
	t.Helper()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	command := exec.Command(binary, args...)
	command.Stdin = devNull
	command.Env = environmentWith("ACR_STATE_HOME", stateHome)
	return hostileRunCommand(t, command)
}

func testerReverifyPinnedProject(t *testing.T) string {
	t.Helper()
	return testerReverifyProject(t, "v1.0.0")
}

func testerReverifyLatestProject(t *testing.T) string {
	t.Helper()
	return testerReverifyProject(t, "latest")
}

func testerReverifyProject(t *testing.T, requested string) string {
	t.Helper()
	root := t.TempDir()
	state := dependency.State{
		Project: dependency.Project{
			SchemaVersion: dependency.CurrentSchemaVersion,
			Agents:        []string{"codex"},
			Freshness:     "outdated",
			Dependencies:  []dependency.Declaration{{Source: testerReverifySource, Requested: requested}},
		},
		Lock: dependency.Lockfile{
			SchemaVersion: dependency.CurrentSchemaVersion,
			Dependencies: []dependency.LockedDependency{{
				Source: testerReverifySource, Requested: requested, Kind: dependency.ResolutionRelease,
				ReleaseID: 1, Tag: "v1.0.0", Commit: strings.Repeat("a", 40), PackageVersion: "1.0.0",
				ContentHash: "sha256:" + strings.Repeat("b", 64),
			}},
		},
	}
	if err := dependency.WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	return root
}

func testerReverifyStateBytes(t *testing.T, root string) []byte {
	t.Helper()
	var result []byte
	for _, path := range []string{dependency.ProjectFilename, dependency.LockFilename} {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, content...)
	}
	return result
}

func containsTesterReverifyQuestion(output string) bool {
	for _, text := range []string{
		"Which agents should this project realize context for?",
		"What should ACR do at session start?",
		"Record it as:",
		"Answer with a number or a name:",
	} {
		if strings.Contains(output, text) {
			return true
		}
	}
	return false
}
