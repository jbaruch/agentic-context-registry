package dependency

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// probeTraceVariable names the file each scratch command appends its own name
// to. The trace is what establishes which probes ran, in what order, and that
// a lower-priority probe was never reached.
const probeTraceVariable = "ACR_CREDENTIAL_PROBE_TRACE"

// withCredentialProbeTimeout decides the bound a credential probe runs under
// for one test and restores the shipped default afterwards. The behaviour
// cases run unbounded, so no assertion about which command answers depends on
// a forked shell winning a wall-clock deadline under whatever load the rest of
// the suite is applying; the expiration cases run with a deadline that is
// already past, so the failure mode is reached without waiting for one. Every
// test that calls this runs sequentially, because credential discovery also
// reads process-wide environment through t.Setenv.
func withCredentialProbeTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	previous := credentialProbeTimeout
	credentialProbeTimeout = timeout
	t.Cleanup(func() { credentialProbeTimeout = previous })
}

// fakeCommandShell is the interpreter the scratch commands run under. The
// scratch PATH deliberately holds nothing but the fakes, so the shebang names a
// resolved absolute path rather than relying on a lookup that would fail.
func fakeCommandShell(t *testing.T) string {
	t.Helper()
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("resolve bash for the scratch commands: %v", err)
	}
	return shell
}

// writeFakeCommand puts an executable on a scratch PATH. Credential discovery
// shells out, and an injected token provider would prove nothing about the
// order those commands are actually tried in.
//
// Each fake runs under strict mode, so a fake that cannot do what it was
// written to do exits non-zero instead of continuing to the answer the test
// expects.
func writeFakeCommand(t *testing.T, shell, directory, name, script string) {
	t.Helper()
	contents := "#!" + shell + "\nset -euo pipefail\n" + script
	if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

// ghProbeContract is the prologue in front of every gh answer. Discovery is
// only allowed to ask gh for a token, so any other argv exits non-zero and the
// case fails rather than being answered by a command that was asked the wrong
// question.
const ghProbeContract = `[ "$#" = 2 ] && [ "$1" = auth ] && [ "$2" = token ] || {
	printf 'gh received argv: %s\n' "$*" >&2
	exit 41
}
printf 'gh\n' >> "${ACR_CREDENTIAL_PROBE_TRACE}"
`

// gitProbeContract is the prologue in front of every git answer. It pins the
// subcommand, the credential request compared whole against its exact bytes,
// and the non-interactive override, all read with shell builtins because
// nothing else is on the scratch PATH. Each check answers a different way the
// production probe could go wrong, so a fake that exits here names which one.
const gitProbeContract = `[ "$#" = 2 ] && [ "$1" = credential ] && [ "$2" = fill ] || {
	printf 'git received argv: %s\n' "$*" >&2
	exit 42
}
printf 'git\n' >> "${ACR_CREDENTIAL_PROBE_TRACE}"
request=""
# Read to end of input without losing a byte. The delimiter is NUL, which a
# credential request never contains, so read consumes everything and reports
# non-zero at EOF with the exact bytes in the variable. Folding each newline
# into a separator instead would make a line break indistinguishable from that
# separator, and would drop an unterminated final field entirely.
if IFS= read -r -d '' request; then
	printf 'git received a credential request containing NUL\n' >&2
	exit 43
fi
[ "${request}" = $'protocol=https\nhost=github.com\n\n' ] || {
	printf 'git received credential request bytes: %q\n' "${request}" >&2
	exit 43
}
[ "${GIT_TERMINAL_PROMPT:-unset}" = 0 ] || {
	printf 'git ran with GIT_TERMINAL_PROMPT=%s\n' "${GIT_TERMINAL_PROMPT:-unset}" >&2
	exit 44
}
`

// The answers both fakes give when they are meant to succeed. Each is padded,
// so discovery that stopped trimming its command output fails the case instead
// of passing it invisibly.
const (
	ghAnswers  = "printf '  from-gh-cli \\n'\n"
	gitAnswers = "printf 'username=fixture\\npassword=  from-git  \\n\\n'\n"
)

// credentialFixture builds a scratch PATH holding only the named fakes and
// returns the trace file they append to. GIT_TERMINAL_PROMPT is inherited as
// 1, so production's own override is what makes the git fake answer; dropping
// it turns every git case red instead of passing on a parent that already had
// prompting disabled. BASH_ENV is cleared so a developer's startup file cannot
// run inside a fake.
func credentialFixture(t *testing.T, shell, gh, git string) string {
	t.Helper()
	directory := t.TempDir()
	trace := filepath.Join(directory, "trace")
	t.Setenv(probeTraceVariable, trace)
	t.Setenv("PATH", directory)
	t.Setenv("BASH_ENV", "")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	if gh != "" {
		writeFakeCommand(t, shell, directory, "gh", ghProbeContract+gh)
	}
	if git != "" {
		writeFakeCommand(t, shell, directory, "git", gitProbeContract+git)
	}
	return trace
}

// assertProbeTrace holds discovery to the probes it was supposed to run, in
// order. An absent file and an empty expectation are the same outcome: no
// command was started.
func assertProbeTrace(t *testing.T, trace, want string) {
	t.Helper()
	contents, err := os.ReadFile(trace)
	if errors.Is(err, os.ErrNotExist) {
		contents = nil
	} else if err != nil {
		t.Fatalf("read the probe trace: %v", err)
	}
	if string(contents) != want {
		t.Fatalf("probe trace = %q, want %q", contents, want)
	}
}

func TestDiscoverGitHubTokenPrefersEnvironmentThenCommands(t *testing.T) {
	shell := fakeCommandShell(t)
	// Unbounded: the outcome of every case below is decided by which command
	// discovery runs and what it answers, never by whether the shell that
	// answers started before a production deadline expired.
	withCredentialProbeTimeout(t, 0)
	for _, test := range []struct {
		name         string
		ghToken      string
		githubToken  string
		gh           string
		git          string
		unexecutable bool
		want         string
		trace        string
	}{
		{name: "GH_TOKEN wins before any command runs", ghToken: "  from-gh-token \t", githubToken: "from-github-token", gh: ghAnswers, git: gitAnswers, want: "from-gh-token"},
		{name: "a blank GH_TOKEN is not an answer", ghToken: " \n", githubToken: "  from-github-token  ", gh: ghAnswers, git: gitAnswers, want: "from-github-token"},
		{name: "gh follows the environment", gh: ghAnswers, git: gitAnswers, want: "from-gh-cli", trace: "gh\n"},
		{name: "a failing gh has its output ignored", gh: "printf 'unreachable\\n'\nexit 7\n", git: gitAnswers, want: "from-git", trace: "gh\ngit\n"},
		{name: "an empty gh answer falls through", gh: "printf '  \\n'\n", git: gitAnswers, want: "from-git", trace: "gh\ngit\n"},
		{name: "a missing gh falls through", git: gitAnswers, want: "from-git", trace: "git\n"},
		{name: "an unexecutable gh falls through", gh: ghAnswers, git: gitAnswers, unexecutable: true, want: "from-git", trace: "git\n"},
		{name: "git credential fill is the last resort", gh: "exit 1\n", git: gitAnswers, want: "from-git", trace: "gh\ngit\n"},
		{name: "a failing git answer is not a credential", gh: "exit 1\n", git: "printf 'password=unreachable\\n'\nexit 8\n", want: "", trace: "gh\ngit\n"},
		{name: "an empty git password is not a credential", gh: "exit 1\n", git: "printf 'password=  \\n'\n", want: "", trace: "gh\ngit\n"},
		{name: "a git answer carrying no password is not a credential", gh: "exit 1\n", git: "printf 'username=fixture\\n'\n", want: "", trace: "gh\ngit\n"},
		{name: "no credential is a public client", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			trace := credentialFixture(t, shell, test.gh, test.git)
			t.Setenv("GH_TOKEN", test.ghToken)
			t.Setenv("GITHUB_TOKEN", test.githubToken)
			if test.unexecutable {
				if err := os.Chmod(filepath.Join(filepath.Dir(trace), "gh"), 0o600); err != nil {
					t.Fatalf("make the gh fake unexecutable: %v", err)
				}
			}

			if got := discoverGitHubToken(context.Background()); got != test.want {
				t.Fatalf("discoverGitHubToken() = %q, want %q", got, test.want)
			}
			assertProbeTrace(t, trace, test.trace)
		})
	}
}

// TestCredentialProbeContextBoundsEveryProductionProbe pins the deadline the
// shipped binary puts on one credential probe. The instant it is measured from
// is supplied, and the five seconds it expects is written here rather than read
// back out of the production constant, so the assertion is an independent
// oracle and needs no clock to run down.
func TestCredentialProbeContextBoundsEveryProductionProbe(t *testing.T) {
	const wantProductionTimeout = 5 * time.Second
	if credentialProbeTimeout != wantProductionTimeout {
		t.Fatalf("production probe timeout = %v, want %v", credentialProbeTimeout, wantProductionTimeout)
	}
	// A fixed past instant makes the expected deadline independent of the run date.
	frozen := time.Date(2001, time.February, 2, 2, 2, 2, 0, time.UTC)
	bounded, cancelBounded := credentialProbeContext(context.Background(), frozen)
	defer cancelBounded()

	deadline, ok := bounded.Deadline()
	if !ok {
		t.Fatal("the production probe context carries no deadline; a hung credential helper would stall resolution")
	}
	if !deadline.Equal(frozen.Add(wantProductionTimeout)) {
		t.Fatalf("probe deadline = %v, want %v after the supplied instant", deadline, wantProductionTimeout)
	}

	// The same construction with an instant already behind us is expired on
	// arrival, which is how the expiration cases below reach the failure mode
	// without waiting for a deadline.
	past := time.Date(1999, time.September, 9, 9, 9, 9, 0, time.UTC)
	expired, cancelExpired := credentialProbeContext(context.Background(), past)
	defer cancelExpired()
	if !errors.Is(expired.Err(), context.DeadlineExceeded) {
		t.Fatalf("probe context from a past instant error = %v, want an exceeded deadline", expired.Err())
	}

	// The test-only unbounded setting installs no deadline at all, which is
	// what keeps the behaviour fixtures above from racing anything.
	withCredentialProbeTimeout(t, 0)
	unbounded, cancelUnbounded := credentialProbeContext(context.Background(), frozen)
	defer cancelUnbounded()
	if _, ok := unbounded.Deadline(); ok {
		t.Fatal("a zero probe timeout installed a deadline; the behaviour fixtures would still race it")
	}
}

// TestDiscoverGitHubTokenFallsBackWhenEveryProbeDeadlineIsExceeded covers the
// failure mode that made the precedence suite flaky: a probe whose deadline is
// gone starts no command and contributes nothing, and discovery moves on. The
// controls run the same fakes unbounded first, so the expired run is compared
// against commands that demonstrably answer rather than against nothing.
func TestDiscoverGitHubTokenFallsBackWhenEveryProbeDeadlineIsExceeded(t *testing.T) {
	shell := fakeCommandShell(t)
	trace := credentialFixture(t, shell, ghAnswers, gitAnswers)

	withCredentialProbeTimeout(t, 0)
	if got := commandToken(context.Background(), "gh", []string{"auth", "token"}, nil); got != "from-gh-cli" {
		t.Fatalf("control commandToken(gh) = %q, want the fake's answer", got)
	}
	if got := discoverGitHubToken(context.Background()); got != "from-gh-cli" {
		t.Fatalf("control discoverGitHubToken() = %q, want the fake's answer", got)
	}
	assertProbeTrace(t, trace, "gh\ngh\n")

	expiredTrace := credentialFixture(t, shell, ghAnswers, gitAnswers)
	withCredentialProbeTimeout(t, -time.Second)
	if got := commandToken(context.Background(), "gh", []string{"auth", "token"}, nil); got != "" {
		t.Fatalf("expired commandToken(gh) = %q, want no token", got)
	}
	if got := discoverGitHubToken(context.Background()); got != "" {
		t.Fatalf("expired discoverGitHubToken() = %q, want the anonymous client", got)
	}
	assertProbeTrace(t, expiredTrace, "")
}

// TestDiscoverGitHubTokenReturnsNoTokenWhenTheCallerIsCancelled keeps discovery
// from answering out of a dead context under the production bound, and from
// starting a command it cannot use. The environment sources still answer,
// because they read no process.
func TestDiscoverGitHubTokenReturnsNoTokenWhenTheCallerIsCancelled(t *testing.T) {
	shell := fakeCommandShell(t)
	trace := credentialFixture(t, shell, ghAnswers, gitAnswers)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := discoverGitHubToken(cancelled); got != "" {
		t.Fatalf("discoverGitHubToken(cancelled) = %q, want no token", got)
	}
	assertProbeTrace(t, trace, "")

	t.Setenv("GH_TOKEN", "  from-gh-token  ")
	if got := discoverGitHubToken(cancelled); got != "from-gh-token" {
		t.Fatalf("discoverGitHubToken(cancelled) = %q, want the environment token", got)
	}
	assertProbeTrace(t, trace, "")
}

// TestGitHubClientCachesTheFirstDiscoveryThroughTheRealProbes runs the shipped
// client over the shipped discovery path and then changes every credential it
// could read. A later request has to carry the first answer and start no
// second probe, so removing the cache is a failing assertion rather than two
// identical headers from a provider that answers the same way twice. The
// anonymous case is the one that fails silently everywhere else: an empty
// header looks the same whether it was cached or rediscovered.
func TestGitHubClientCachesTheFirstDiscoveryThroughTheRealProbes(t *testing.T) {
	shell := fakeCommandShell(t)
	for _, test := range []struct {
		name  string
		gh    string
		git   string
		want  string
		trace string
	}{
		{name: "an authenticated first answer", gh: ghAnswers, git: gitAnswers, want: "Bearer from-gh-cli", trace: "gh\n"},
		{name: "an anonymous first answer", gh: "exit 1\n", git: "exit 1\n", want: "", trace: "gh\ngit\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			trace := credentialFixture(t, shell, test.gh, test.git)
			withCredentialProbeTimeout(t, 0)
			client := NewGitHubClient()

			// The first request is cancelled: discovery must still run, and the
			// request must still carry its own cancellation.
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			first, err := client.newRequest(cancelled, http.MethodGet, "https://example.invalid/first", nil)
			if err != nil {
				t.Fatalf("first newRequest() error = %v", err)
			}
			if !errors.Is(first.Context().Err(), context.Canceled) {
				t.Fatal("the first request lost its own cancellation")
			}
			if got := first.Header.Get("Authorization"); got != test.want {
				t.Fatalf("first Authorization = %q, want %q", got, test.want)
			}
			assertProbeTrace(t, trace, test.trace)

			// Every source a second discovery could read now answers
			// differently. Nothing the client sends may change.
			t.Setenv("GH_TOKEN", "rediscovered")
			writeFakeCommand(t, shell, filepath.Dir(trace), "gh", ghProbeContract+"printf 'rediscovered\\n'\n")
			second, err := client.newRequest(context.Background(), http.MethodGet, "https://example.invalid/second", nil)
			if err != nil {
				t.Fatalf("second newRequest() error = %v", err)
			}
			if second.Context().Err() != nil {
				t.Fatalf("the second request inherited cancellation: %v", second.Context().Err())
			}
			if got := second.Header.Get("Authorization"); got != test.want {
				t.Fatalf("second Authorization = %q, want the cached %q", got, test.want)
			}
			assertProbeTrace(t, trace, test.trace)
		})
	}
}

// blockingCredentialFake blocks with shell builtins alone, because the
// scratch PATH holds nothing but the fakes and an external command would
// simply fail to resolve — which an empty result cannot be told apart from,
// and which is how an earlier version of this fixture passed a cancellation
// assertion without ever blocking.
//
// It consumes the credential request, records that it reached the FIFO
// blocking boundary, then opens the FIFO for reading. That open does not return until a
// writer appears, and nothing ever writes, so the process sits in the kernel
// until it is killed.
const blockingCredentialFake = `while IFS= read -r line; do
	:
done
: > "${CREDENTIAL_FAKE_STARTED}"
read -r answer < "${CREDENTIAL_FAKE_FIFO}"
printf 'password=unreachable\n'
`

// blockedCredentialCommand puts the blocking fake on a scratch PATH and
// returns the marker the fake writes immediately before opening the FIFO,
// so a caller can require that the process reached that blocking boundary.
func blockedCredentialCommand(t *testing.T, name string) string {
	t.Helper()
	shell := fakeCommandShell(t)
	directory := t.TempDir()
	writeFakeCommand(t, shell, directory, name, blockingCredentialFake)

	fifo := filepath.Join(t.TempDir(), "credential.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("create the blocking fixture's fifo: %v", err)
	}
	started := filepath.Join(t.TempDir(), "started")

	t.Setenv("PATH", directory)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("BASH_ENV", "")
	t.Setenv("CREDENTIAL_FAKE_FIFO", fifo)
	t.Setenv("CREDENTIAL_FAKE_STARTED", started)
	return started
}

// assertReachedTheBlockedState fails unless the fake wrote its marker, which
// is what separates "the command was abandoned while blocked" from "the
// command never ran".
func assertReachedTheBlockedState(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the blocking fake never reached its blocked state: %v", err)
	}
}

// credentialHarnessContext reserves cleanup time before the test runner's
// deadline. This is only a failing-fixture safety bound; reaching it fails the
// test and never counts as the credential probe's controlled expiry.
func credentialHarnessContext(t *testing.T) context.Context {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("credential subprocess tests require a finite go test -timeout for failure cleanup")
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline.Add(-time.Second))
	t.Cleanup(cancel)
	return ctx
}

type credentialOperation struct {
	done  chan struct{}
	token string // Read only after done closes.
}

func startCredentialOperation(t *testing.T, abort func(), run func() string) *credentialOperation {
	t.Helper()
	operation := &credentialOperation{done: make(chan struct{})}
	// Register before launch, and join before restoring environment or seams.
	t.Cleanup(func() {
		abort()
		<-operation.done
	})
	go func() {
		defer close(operation.done)
		operation.token = run()
	}()
	return operation
}

func (operation *credentialOperation) result(t *testing.T, ctx context.Context) string {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("credential operation exceeded the test harness bound: %v", ctx.Err())
	case <-operation.done:
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("credential operation reached the test harness bound: %v", err)
	}
	return operation.token
}

var errCredentialCompletedBeforeStartup = errors.New("credential operation completed before startup was observed")

// observeCredentialStartup returns every failure to its caller. The retry
// events only yield between inspections; no polling duration decides success.
func observeCredentialStartup(ctx context.Context, done <-chan struct{}, inspect func() error, retry <-chan time.Time) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for credential startup: %w", ctx.Err())
		case <-done:
			return errCredentialCompletedBeforeStartup
		default:
		}
		if err := inspect(); err == nil {
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspect the blocking fixture's marker: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for credential startup: %w", ctx.Err())
		case <-done:
			return errCredentialCompletedBeforeStartup
		case <-retry:
		}
	}
}

func waitForTheBlockedState(ctx context.Context, marker string, done <-chan struct{}) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	return observeCredentialStartup(ctx, done, func() error {
		_, err := os.Stat(marker)
		return err
	}, ticker.C)
}

func requireCredentialStartup(t *testing.T, ctx context.Context, marker string, operation *credentialOperation) {
	t.Helper()
	if err := waitForTheBlockedState(ctx, marker, operation.done); err != nil {
		t.Fatalf("credential helper did not reach the FIFO blocking boundary: %v", err)
	}
	select {
	case <-operation.done:
		t.Fatal("credential operation completed before explicit expiry or cancellation")
	default:
	}
	assertReachedTheBlockedState(t, marker)
}

// controlledProbeExpiry distinguishes an injected deadline from a parent's
// cancellation cause. WithCancelCause supplies synchronized Done and cleanup
// without a timer or a test-owned propagation goroutine.
var controlledProbeExpiry = errors.New("controlled credential deadline expiry")

type controlledProbeContext struct {
	context.Context
	deadline time.Time
	expire   context.CancelCauseFunc
}

func (ctx *controlledProbeContext) Deadline() (time.Time, bool) { return ctx.deadline, true }

func (ctx *controlledProbeContext) Err() error {
	err := ctx.Context.Err()
	if err != nil && context.Cause(ctx.Context) == controlledProbeExpiry {
		return context.DeadlineExceeded
	}
	return err
}

type controlledCredentialDeadlines struct {
	mu      sync.Mutex
	probes  []*controlledProbeContext
	stopped bool
}

func controlCredentialDeadlines(t *testing.T) *controlledCredentialDeadlines {
	t.Helper()
	control := &controlledCredentialDeadlines{}
	previous := credentialProbeDeadline
	t.Cleanup(func() {
		control.stop()
		credentialProbeDeadline = previous
	})
	credentialProbeDeadline = func(parent context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancelCause(parent)
		if earlier, ok := parent.Deadline(); ok && earlier.Before(deadline) {
			deadline = earlier
		}
		probe := &controlledProbeContext{Context: ctx, deadline: deadline, expire: cancel}
		control.mu.Lock()
		defer control.mu.Unlock()
		control.probes = append(control.probes, probe)
		if control.stopped {
			cancel(context.Canceled)
		}
		return probe, func() { cancel(context.Canceled) }
	}
	return control
}

func (control *controlledCredentialDeadlines) stop() {
	control.mu.Lock()
	defer control.mu.Unlock()
	control.stopped = true
	for _, probe := range control.probes {
		probe.expire(context.Canceled)
	}
}

func (control *controlledCredentialDeadlines) expireRunningProbe(t *testing.T) {
	t.Helper()
	control.mu.Lock()
	defer control.mu.Unlock()
	var running *controlledProbeContext
	for _, probe := range control.probes {
		if probe.Err() == nil {
			if running != nil {
				t.Fatal("more than one credential probe is running")
			}
			running = probe
		}
	}
	if running == nil {
		t.Fatal("no live credential probe to expire after helper startup")
	}
	running.expire(controlledProbeExpiry)
	select {
	case <-running.Done():
	default:
		t.Fatal("controlled deadline did not close Done")
	}
	if running.Err() != context.DeadlineExceeded {
		t.Fatalf("expired probe error = %v, want DeadlineExceeded", running.Err())
	}
}

// These cancellation outcomes disable the probe deadline entirely. Startup
// observation happens on the main goroutine before cancelling the caller.
func TestCommandTokenHonoursCallerCancellation(t *testing.T) {
	withCredentialProbeTimeout(t, 0)
	marker := blockedCredentialCommand(t, "git")
	harness := credentialHarnessContext(t)
	ctx, cancel := context.WithCancel(harness)
	operation := startCredentialOperation(t, cancel, func() string {
		return commandToken(ctx, "git", []string{"credential", "fill"}, []byte("protocol=https\nhost=github.com\n\n"))
	})
	requireCredentialStartup(t, harness, marker, operation)
	cancel()
	if got := operation.result(t, harness); got != "" {
		t.Fatalf("commandToken() = %q, want no token after the caller cancelled", got)
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("caller error = %v, want Canceled", ctx.Err())
	}
}

// The production entry still launches the executable helper and abandons it
// at its FIFO boundary. Only deadline scheduling is injected: the default
// five-second duration is independently checked against the real constructor.
func TestDiscoverGitHubTokenAbandonsABlockedCommand(t *testing.T) {
	marker := blockedCredentialCommand(t, "git")
	harness := credentialHarnessContext(t)
	deadlines := controlCredentialDeadlines(t)
	caller := context.Background()
	operation := startCredentialOperation(t, deadlines.stop, func() string {
		return discoverGitHubToken(caller)
	})
	requireCredentialStartup(t, harness, marker, operation)
	deadlines.expireRunningProbe(t)
	if got := operation.result(t, harness); got != "" {
		t.Fatalf("discoverGitHubToken() = %q, want no token from a blocked helper", got)
	}
	if caller.Err() != nil {
		t.Fatalf("probe expiry cancelled its caller: %v", caller.Err())
	}
}

func TestDiscoverGitHubTokenHonoursCallerCancellation(t *testing.T) {
	withCredentialProbeTimeout(t, 0)
	marker := blockedCredentialCommand(t, "git")
	harness := credentialHarnessContext(t)
	ctx, cancel := context.WithCancel(harness)
	operation := startCredentialOperation(t, cancel, func() string { return discoverGitHubToken(ctx) })
	requireCredentialStartup(t, harness, marker, operation)
	cancel()
	if got := operation.result(t, harness); got != "" {
		t.Fatalf("discoverGitHubToken() = %q, want no token after the caller cancelled", got)
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("caller error = %v, want Canceled", ctx.Err())
	}
}

func TestDiscoverGitHubTokenFallsBackAfterRunningProbeExpires(t *testing.T) {
	shell := fakeCommandShell(t)
	marker := blockedCredentialCommand(t, "gh")
	directory := os.Getenv("PATH")
	trace := filepath.Join(directory, "trace")
	t.Setenv(probeTraceVariable, trace)
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	writeFakeCommand(t, shell, directory, "gh", ghProbeContract+blockingCredentialFake)
	writeFakeCommand(t, shell, directory, "git", gitProbeContract+gitAnswers)
	harness := credentialHarnessContext(t)
	deadlines := controlCredentialDeadlines(t)
	operation := startCredentialOperation(t, deadlines.stop, func() string {
		return discoverGitHubToken(context.Background())
	})
	requireCredentialStartup(t, harness, marker, operation)
	deadlines.expireRunningProbe(t)
	if got := operation.result(t, harness); got != "from-git" {
		t.Fatalf("discovery after gh expiry = %q, want the next probe's credential", got)
	}
	assertProbeTrace(t, trace, "gh\ngit\n")
}

func TestControlledCredentialDeadlinePreservesCallerCancellation(t *testing.T) {
	controlCredentialDeadlines(t)
	parent, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	probe, stop := credentialProbeContext(parent, time.Date(2001, time.February, 2, 2, 2, 2, 0, time.UTC))
	defer stop()
	// Even a parent cancelled with a deadline-shaped cause remains Canceled.
	cancel(context.DeadlineExceeded)
	select {
	case <-probe.Done():
	default:
		t.Fatal("the controlled probe swallowed parent cancellation")
	}
	if probe.Err() != context.Canceled {
		t.Fatalf("controlled probe error = %v, want caller Canceled", probe.Err())
	}
}

func TestCredentialStartupObserverOutcomes(t *testing.T) {
	for _, name := range []string{"readiness", "completion", "cancellation", "inspection error"} {
		t.Run(name, func(t *testing.T) {
			harness := credentialHarnessContext(t)
			ctx, cancel := context.WithCancel(harness)
			done := make(chan struct{})
			retry := make(chan time.Time)
			inspected := make(chan struct{}, 1)
			finished := make(chan struct{})
			var result error
			inspectionError := error(fs.ErrNotExist)
			t.Cleanup(func() { cancel(); <-finished })
			go func() {
				defer close(finished)
				result = observeCredentialStartup(ctx, done, func() error {
					err := inspectionError
					select {
					case inspected <- struct{}{}:
					default:
					}
					return err
				}, retry)
			}()
			select {
			case <-inspected:
			case <-ctx.Done():
				t.Fatalf("observer never inspected the marker: %v", ctx.Err())
			}
			var want error
			switch name {
			case "readiness":
				inspectionError = nil
			case "inspection error":
				inspectionError = fs.ErrPermission
				want = fs.ErrPermission
			case "completion":
				close(done)
				want = errCredentialCompletedBeforeStartup
			case "cancellation":
				cancel()
				want = context.Canceled
			}
			if name == "readiness" || name == "inspection error" {
				select {
				case retry <- time.Time{}:
				case <-ctx.Done():
					t.Fatalf("observer did not accept inspection event: %v", ctx.Err())
				}
			}
			select {
			case <-finished:
			case <-harness.Done():
				t.Fatalf("observer failed to terminate: %v", harness.Err())
			}
			if !errors.Is(result, want) {
				t.Fatalf("observer result = %v, want %v", result, want)
			}
		})
	}
}

func TestCredentialStartupRejectsFailedHelpers(t *testing.T) {
	for _, failure := range []string{"exit before marker", "missing executable", "unexecutable helper"} {
		t.Run(failure, func(t *testing.T) {
			withCredentialProbeTimeout(t, 0)
			shell := fakeCommandShell(t)
			marker := blockedCredentialCommand(t, "git")
			directory := os.Getenv("PATH")
			switch failure {
			case "exit before marker":
				writeFakeCommand(t, shell, directory, "git", "exit 7\n")
			case "missing executable":
				if err := os.Remove(filepath.Join(directory, "git")); err != nil {
					t.Fatal(err)
				}
			case "unexecutable helper":
				if err := os.Chmod(filepath.Join(directory, "git"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			harness := credentialHarnessContext(t)
			ctx, cancel := context.WithCancel(harness)
			operation := startCredentialOperation(t, cancel, func() string { return discoverGitHubToken(ctx) })
			if err := waitForTheBlockedState(harness, marker, operation.done); !errors.Is(err, errCredentialCompletedBeforeStartup) {
				t.Fatalf("startup error = %v, want completion before startup", err)
			}
			if got := operation.result(t, harness); got != "" {
				t.Fatalf("failed helper returned %q", got)
			}
			if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("failed helper marker error = %v, want missing marker", err)
			}
		})
	}
}

// Returning from a failed startup observation must cancel and join even a real
// helper that cannot finish normally. Subtest cleanup is the explicit abort.
func TestCredentialStartupAbortCleansUpRunningHelper(t *testing.T) {
	for _, failure := range []string{"inspection error", "cancellation"} {
		var operation *credentialOperation
		var caller context.Context
		t.Run(failure, func(t *testing.T) {
			withCredentialProbeTimeout(t, 0)
			marker := blockedCredentialCommand(t, "git")
			harness := credentialHarnessContext(t)
			ctx, cancel := context.WithCancel(harness)
			caller = ctx
			operation = startCredentialOperation(t, cancel, func() string { return discoverGitHubToken(ctx) })
			requireCredentialStartup(t, harness, marker, operation)
			observer, abort := context.WithCancel(harness)
			defer abort()
			var want error = syscall.ENOTDIR
			if failure == "cancellation" {
				abort()
				want = context.Canceled
			}
			if err := waitForTheBlockedState(observer, filepath.Join(marker, "invalid-child"), operation.done); !errors.Is(err, want) {
				t.Fatalf("startup abort = %v, want %v", err, want)
			}
		})
		if operation == nil {
			t.Fatal("cleanup control never launched its helper")
		}
		select {
		case <-operation.done:
		default:
			t.Fatal("startup failure cleanup left the credential operation running")
		}
		if operation.token != "" || caller.Err() != context.Canceled {
			t.Fatalf("cleanup token = %q, caller error = %v; want no token and Canceled", operation.token, caller.Err())
		}
	}
}
