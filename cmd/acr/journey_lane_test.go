package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshnessapp"
)

// journeySubprocessEndpoint hands the fixture address to the
// composed-subprocess lane. It is read by a test entry point in this test
// binary, never by the shipped executable, which has no test-only switch.
const journeySubprocessEndpoint = "ACR_JOURNEY_FIXTURE_ENDPOINT"

// journeyToken is the credential the fixture expects. Setting it keeps
// discovery on its first branch instead of shelling out to gh or git.
const journeyToken = "journey-fixture-token"

// journeyRun is one command's complete process-visible result.
type journeyRun struct {
	args   []string
	stdout string
	stderr string
	exit   int
}

// output is stdout and stderr together, for a check that does not care which
// stream carried the sentence.
func (run journeyRun) output() string { return run.stdout + run.stderr }

// journeyProject is one fixture project: its directory, its isolated machine
// state, the GitHub fixture its commands reach, and any construction options
// the journey replaces. Without options the composition is the shipped one.
type journeyProject struct {
	t         *testing.T
	root      string
	stateHome string
	github    *journeyGitHub
	freshness []freshnessapp.Option
}

// journeyClock is a clock a journey holds still. Time moves only when the
// journey moves it, so a throttle window is crossed on purpose rather than by
// however long the suite happened to take getting here.
type journeyClock struct {
	mutex sync.Mutex
	now   time.Time
}

// newJourneyClock starts at a fixed historical instant. Nothing derives it from
// the machine's clock, so the same run happens on every machine and every day.
func newJourneyClock() *journeyClock {
	return &journeyClock{now: time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)}
}

// Now is the clock the composed stack reads.
func (clock *journeyClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

// Advance moves the clock forward by exactly one interval.
func (clock *journeyClock) Advance(interval time.Duration) {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	clock.now = clock.now.Add(interval)
}

// useClock makes every later command in this project read the supplied clock
// instead of time.Now. Only a journey that reasons about time calls it.
func (project *journeyProject) useClock(clock *journeyClock) {
	project.freshness = append(project.freshness, freshnessapp.WithClock(clock.Now))
}

// newJourneyProject isolates every process-wide input a command reads: the
// machine-local freshness directory, the credential, and HOME, so a journey
// can never read or write the operator's own agent configuration.
func newJourneyProject(t *testing.T, github *journeyGitHub) *journeyProject {
	t.Helper()
	stateHome := t.TempDir()
	t.Setenv("ACR_STATE_HOME", stateHome)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GH_TOKEN", journeyToken)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	return &journeyProject{t: t, root: t.TempDir(), stateHome: stateHome, github: github}
}

// composedClient is the production GitHub client with the fixture transport.
// The API host, the upload host, and the trusted redirect origins are the
// production ones; only the dialled address differs.
func (project *journeyProject) composedClient() dependency.Remote {
	if project.github == nil {
		return dependency.NewGitHubClient(dependency.WithHTTPClient(&http.Client{Transport: &journeyOfflineTransport{}}))
	}
	return dependency.NewGitHubClient(dependency.WithHTTPClient(&http.Client{Transport: project.github.Transport()}))
}

// journeyOfflineTransport refuses every request. A journey that promised to
// make no network call uses it so a call becomes a failure rather than a
// silent success against a fixture.
type journeyOfflineTransport struct{}

func (*journeyOfflineTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return nil, errors.New("journey ran offline and the command attempted " + request.URL.String())
}

// run executes one command through the shipped composition — the same
// runWith cmd/acr's main calls — with the production GitHub client.
func (project *journeyProject) run(want int, args ...string) journeyRun {
	project.t.Helper()
	return project.runStdin(strings.NewReader(""), want, args...)
}

func (project *journeyProject) runStdin(stdin io.Reader, want int, args ...string) journeyRun {
	project.t.Helper()
	full := append(append([]string(nil), args...), "--project", project.root)
	var stdout, stderr bytes.Buffer
	exit := runWith(project.composedClient(), stdin, &stdout, &stderr, full, project.freshness...)
	run := journeyRun{args: full, stdout: stdout.String(), stderr: stderr.String(), exit: exit}
	if exit != want {
		project.t.Fatalf("acr %s exit = %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), exit, want, run.stdout, run.stderr)
	}
	if project.github != nil {
		project.github.AssertNoUnknownRequests(project.t)
	}
	return run
}

// runExact executes one command with the argv the caller supplies verbatim,
// for the shapes whose whole point is where the arguments sit.
func (project *journeyProject) runExact(want int, args ...string) journeyRun {
	project.t.Helper()
	var stdout, stderr bytes.Buffer
	exit := runWith(project.composedClient(), strings.NewReader(""), &stdout, &stderr, args, project.freshness...)
	run := journeyRun{args: args, stdout: stdout.String(), stderr: stderr.String(), exit: exit}
	if exit != want {
		project.t.Fatalf("acr %s exit = %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), exit, want, run.stdout, run.stderr)
	}
	if project.github != nil {
		project.github.AssertNoUnknownRequests(project.t)
	}
	return run
}

// runOnPath executes a command whose subject is a package directory rather
// than the current project: publish and the producer migration both take that
// directory as a positional PATH.
func (project *journeyProject) runOnPath(root string, want int, args ...string) journeyRun {
	project.t.Helper()
	full := append(append([]string(nil), args...), root)
	var stdout, stderr bytes.Buffer
	exit := runWith(project.composedClient(), strings.NewReader(""), &stdout, &stderr, full, project.freshness...)
	run := journeyRun{args: full, stdout: stdout.String(), stderr: stderr.String(), exit: exit}
	if exit != want {
		project.t.Fatalf("acr %s exit = %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), exit, want, run.stdout, run.stderr)
	}
	if project.github != nil {
		project.github.AssertNoUnknownRequests(project.t)
	}
	return run
}

// runSubprocess executes one command in a separate process through the
// composed-subprocess lane: the same runWith, the same production client and
// fixture transport, but real file descriptors, a real argv and a real exit
// status. This is deliberately NOT the shipped binary; the shipped binary
// cannot be routed at a local fixture without a test-only production switch,
// so its successful network path stays outside deterministic coverage.
func (project *journeyProject) runSubprocess(want int, args ...string) journeyRun {
	project.t.Helper()
	full := append(append([]string(nil), args...), "--project", project.root)
	command := exec.Command(os.Args[0], append([]string{"-test.run=^TestJourneyComposedSubprocessEntry$", "--"}, full...)...)
	command.Env = append(os.Environ(), journeySubprocessEndpoint+"="+project.github.Endpoint())
	command.Stdin = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	exit := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			project.t.Fatalf("composed subprocess %v: %v", full, err)
		}
		exit = exitError.ExitCode()
	}
	run := journeyRun{args: full, stdout: stdout.String(), stderr: stderr.String(), exit: exit}
	if exit != want {
		project.t.Fatalf("composed-subprocess acr %s exit = %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), exit, want, run.stdout, run.stderr)
	}
	project.github.AssertNoUnknownRequests(project.t)
	return run
}

// runBinary executes one command through the compiled shipped executable.
// It reaches no fixture host, so it covers the local and offline behavior of
// a command rather than its remote path.
func (project *journeyProject) runBinary(binary string, want int, args ...string) journeyRun {
	project.t.Helper()
	full := append(append([]string(nil), args...), "--project", project.root)
	stdout, stderr, exit := hostileRunBinary(project.t, binary, project.stateHome, strings.NewReader(""), full...)
	run := journeyRun{args: full, stdout: stdout, stderr: stderr, exit: exit}
	if exit != want {
		project.t.Fatalf("shipped binary acr %s exit = %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), exit, want, stdout, stderr)
	}
	return run
}

// runBinaryBare executes the shipped executable without a project argument,
// for the meta commands that read no project state and accept no --project.
func (project *journeyProject) runBinaryBare(binary string, want int, args ...string) journeyRun {
	project.t.Helper()
	stdout, stderr, exit := hostileRunBinary(project.t, binary, project.stateHome, strings.NewReader(""), args...)
	run := journeyRun{args: args, stdout: stdout, stderr: stderr, exit: exit}
	if exit != want {
		project.t.Fatalf("shipped binary acr %s exit = %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), exit, want, stdout, stderr)
	}
	return run
}

// snapshot records the project tree the way a promise about it is stated.
func (project *journeyProject) snapshot() map[string]string {
	project.t.Helper()
	return snapshotProjectTree(project.t, project.root)
}

// assertUnchanged fails when a command that promised to write nothing wrote.
func (project *journeyProject) assertUnchanged(before map[string]string, what string) {
	project.t.Helper()
	assertTreeUnchanged(project.t, before, project.root, what)
}

// path joins one project-relative path.
func (project *journeyProject) path(relative string) string {
	return filepath.Join(project.root, filepath.FromSlash(relative))
}

// TestJourneyComposedSubprocessEntry is the composed-subprocess lane's entry
// point rather than a test of its own. Without the fixture endpoint in the
// environment it is inert, so an ordinary suite run never executes it.
func TestJourneyComposedSubprocessEntry(t *testing.T) {
	endpoint := os.Getenv(journeySubprocessEndpoint)
	if endpoint == "" {
		t.Skip("composed-subprocess entry point, driven by the journey harness")
	}
	args := os.Args[1:]
	for index, argument := range os.Args {
		if argument == "--" {
			args = os.Args[index+1:]
			break
		}
	}
	client := dependency.NewGitHubClient(dependency.WithHTTPClient(&http.Client{Transport: &journeyTransport{endpoint: endpoint}}))
	os.Exit(runWith(client, os.Stdin, os.Stdout, os.Stderr, args))
}

var journeyBinaryOnce struct {
	sync.Once
	directory string
	path      string
	err       error
}

// TestMain owns the lifecycle of what the journey lanes build once for the
// whole process. A per-test cleanup cannot remove a binary its siblings still
// run, so the removal happens after every consumer has finished.
func TestMain(m *testing.M) {
	code := m.Run()
	if err := removeJourneyBinary(); err != nil {
		fmt.Fprintf(os.Stderr, "remove the journey binary directory: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// journeyBuiltBinary compiles the shipped executable once per test process.
func journeyBuiltBinary(t *testing.T) string {
	t.Helper()
	journeyBinaryOnce.Do(func() {
		directory, err := os.MkdirTemp("", "acr-journey-binary-*")
		if err != nil {
			journeyBinaryOnce.err = err
			return
		}
		// Recorded before the build, so a failed build leaves a directory
		// TestMain still removes.
		journeyBinaryOnce.directory = directory
		binary := filepath.Join(directory, "acr")
		command := exec.Command("go", "build", "-o", binary, "./cmd/acr")
		command.Dir = filepath.Join("..", "..")
		if output, buildErr := command.CombinedOutput(); buildErr != nil {
			journeyBinaryOnce.err = errors.New("build acr: " + buildErr.Error() + "\n" + string(output))
			return
		}
		journeyBinaryOnce.path = binary
	})
	if journeyBinaryOnce.err != nil {
		t.Fatal(journeyBinaryOnce.err)
	}
	return journeyBinaryOnce.path
}

// removeJourneyBinary deletes the directory journeyBuiltBinary created. A
// process that never built one has nothing to remove.
func removeJourneyBinary() error {
	if journeyBinaryOnce.directory == "" {
		return nil
	}
	return os.RemoveAll(journeyBinaryOnce.directory)
}

// journeyEnvelope decodes exactly one JSON envelope and requires the stream to
// end there, so a command that emitted a second document fails.
func journeyEnvelope(t *testing.T, stream string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(stream))
	var envelope map[string]any
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("decode JSON envelope %q: %v", stream, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		t.Fatalf("JSON stream %q carries more than one document", stream)
	}
	if _, ok := envelope["ok"]; !ok {
		t.Fatalf("JSON envelope %q has no ok field", stream)
	}
	return envelope
}

// journeyResult returns the result object of a successful envelope.
func journeyResult(t *testing.T, stream string) map[string]any {
	t.Helper()
	envelope := journeyEnvelope(t, stream)
	if envelope["ok"] != true {
		t.Fatalf("envelope %q is not a success", stream)
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		t.Fatalf("envelope %q has no result object", stream)
	}
	return result
}

// journeyError returns the error object of a refusal envelope.
func journeyError(t *testing.T, stream string) map[string]any {
	t.Helper()
	envelope := journeyEnvelope(t, stream)
	if envelope["ok"] != false {
		t.Fatalf("envelope %q is not a refusal", stream)
	}
	failure, ok := envelope["error"].(map[string]any)
	if !ok {
		t.Fatalf("envelope %q has no error object", stream)
	}
	return failure
}

// assertNoCredentialLeak keeps the fixture token out of every stream a user or
// a log can see.
func assertNoCredentialLeak(t *testing.T, run journeyRun) {
	t.Helper()
	if strings.Contains(run.output(), journeyToken) {
		t.Fatalf("acr %v leaked the credential: %s", run.args, run.output())
	}
}
