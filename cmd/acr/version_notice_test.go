package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/dependencytest"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/versioncheck"
)

// noticeSource is the call-counting release source the notice tests inject
// in place of GitHub. block, when set, holds a call until its context ends,
// which is how a slow network is reproduced without a clock.
type noticeSource struct {
	mutex   sync.Mutex
	calls   int
	release dependency.Release
	err     error
	block   bool
}

func (source *noticeSource) LatestRelease(ctx context.Context, _ dependency.Repository) (dependency.Release, error) {
	source.mutex.Lock()
	source.calls++
	source.mutex.Unlock()
	if source.block {
		<-ctx.Done()
		return dependency.Release{}, ctx.Err()
	}
	return source.release, source.err
}

func (source *noticeSource) count() int {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	return source.calls
}

// noticeHarness isolates every process-wide input the notice reads — the
// state directory, HOME, the credential, the build identity — and replaces
// the clock, the source, the install environment and the terminal probe.
// A nil probe leaves the binary's own probe in place.
type noticeHarness struct {
	t      *testing.T
	store  versioncheck.Store
	clock  *journeyClock
	source *noticeSource
	remote *dependencytest.Remote
	probe  func(io.Writer) bool
}

const (
	noticeRunningVersion = "1.2.3"
	noticeRunningCommit  = "abc123"
	noticeLatest         = "v9.9.9"
	noticeLine           = "acr 9.9.9 is available (you have 1.2.3). Upgrade: brew upgrade jbaruch/agentic-context-registry/acr\n"
	noticeVersionOutput  = "1.2.3 (abc123)\n"
)

var noticeEnvironment = versioncheck.Environment{Executable: "/opt/homebrew/Cellar/acr/1.2.3/bin/acr"}

func newNoticeHarness(t *testing.T) *noticeHarness {
	t.Helper()
	stateHome := t.TempDir()
	t.Setenv("ACR_STATE_HOME", stateHome)
	t.Setenv("ACR_VERSION_CHECK", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GH_TOKEN", journeyToken)
	t.Setenv("GITHUB_TOKEN", "")
	originalVersion, originalCommit := version, commit
	version, commit = noticeRunningVersion, noticeRunningCommit
	t.Cleanup(func() { version, commit = originalVersion, originalCommit })
	return &noticeHarness{
		t:      t,
		store:  versioncheck.Store{BaseDirectory: stateHome},
		clock:  newJourneyClock(),
		source: &noticeSource{release: dependency.Release{ID: 99, Tag: noticeLatest}},
		remote: dependencytest.NewRemote(),
		probe:  func(io.Writer) bool { return true },
	}
}

func (harness *noticeHarness) options() []versioncheck.Option {
	options := []versioncheck.Option{
		versioncheck.WithClock(harness.clock.Now),
		versioncheck.WithSource(harness.source),
		versioncheck.WithEnvironment(noticeEnvironment),
		versioncheck.WithTimeout(20 * time.Millisecond),
	}
	if harness.probe != nil {
		options = append(options, versioncheck.WithTerminalProbe(harness.probe))
	}
	return options
}

// run executes one command through the shipped composition with the
// supplied output streams and returns the exit code.
func (harness *noticeHarness) run(stdout, stderr io.Writer, args ...string) int {
	harness.t.Helper()
	return runComposed(harness.remote, strings.NewReader(""), stdout, stderr, args, nil, harness.options())
}

// capture runs a command with separate buffers and returns both streams.
func (harness *noticeHarness) capture(args ...string) (string, string, int) {
	harness.t.Helper()
	var stdout, stderr bytes.Buffer
	exit := harness.run(&stdout, &stderr, args...)
	return stdout.String(), stderr.String(), exit
}

func (harness *noticeHarness) seedCache(latest string) {
	harness.t.Helper()
	if err := harness.store.WriteCache(harness.clock.Now().Add(-time.Hour), latest); err != nil {
		harness.t.Fatal(err)
	}
}

func (harness *noticeHarness) cacheBytes() []byte {
	harness.t.Helper()
	content, err := os.ReadFile(harness.store.CachePath())
	if err != nil {
		harness.t.Fatal(err)
	}
	return content
}

func (harness *noticeHarness) assertNoAttempt() {
	harness.t.Helper()
	if _, err := os.Lstat(harness.store.AttemptPath()); !os.IsNotExist(err) {
		harness.t.Fatalf("a refresh attempt was recorded: %v", err)
	}
}

// TestReleaseNoticePrecedesCommandOutputOnATerminal is the first row of the
// design: one line, on the diagnostic stream, before the command's first
// byte. Both streams share one buffer so the order is the order.
func TestReleaseNoticePrecedesCommandOutputOnATerminal(t *testing.T) {
	harness := newNoticeHarness(t)
	harness.seedCache(noticeLatest)

	var combined bytes.Buffer
	if exit := harness.run(&combined, &combined, "version"); exit != 0 {
		t.Fatalf("acr version exit = %d\n%s", exit, combined.String())
	}
	if got, want := combined.String(), noticeLine+noticeVersionOutput; got != want {
		t.Fatalf("terminal stream = %q, want %q", got, want)
	}

	combined.Reset()
	project := t.TempDir()
	if exit := harness.run(&combined, &combined, "list", "--project", project); exit != 0 {
		t.Fatalf("acr list exit = %d\n%s", exit, combined.String())
	}
	if got, want := combined.String(), noticeLine+"No dependencies declared.\n"; got != want {
		t.Fatalf("terminal stream = %q, want %q", got, want)
	}
	if count := strings.Count(combined.String(), "acr 9.9.9 is available"); count != 1 {
		t.Fatalf("the notice appeared %d times in one run, want exactly once", count)
	}

	stdout, stderr, exit := harness.capture("version")
	if exit != 0 || stdout != noticeVersionOutput || stderr != noticeLine {
		t.Fatalf("acr version = %q / %q / %d; the notice belongs on stderr alone", stdout, stderr, exit)
	}
}

// TestReleaseNoticeMachineFacingRunsAreByteIdenticalToTheOffSwitch is the
// second row: --json, --non-interactive and a non-terminal stream each print
// nothing extra, and stdout is the same bytes the run produces with the check
// switched off. None of them reaches the release source.
func TestReleaseNoticeMachineFacingRunsAreByteIdenticalToTheOffSwitch(t *testing.T) {
	harness := newNoticeHarness(t)
	harness.seedCache(noticeLatest)
	project := t.TempDir()
	never := func(io.Writer) bool { return false }
	var stdout, stderr bytes.Buffer
	stdoutOnly := func(writer io.Writer) bool { return writer == &stdout }
	stderrOnly := func(writer io.Writer) bool { return writer == &stderr }

	shapes := []struct {
		name  string
		args  []string
		probe func(io.Writer) bool
	}{
		{name: "json", args: []string{"version", "--json"}},
		{name: "json domain command", args: []string{"list", "--project", project, "--json"}},
		{name: "non-interactive", args: []string{"init", "--agent", "codex", "--freshness", "none", "--non-interactive", "--dry-run", "--project", project}},
		{name: "piped stdout", args: []string{"version"}, probe: stderrOnly},
		{name: "piped stderr", args: []string{"version"}, probe: stdoutOnly},
		{name: "both piped", args: []string{"version"}, probe: never},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			harness.probe = shape.probe
			if harness.probe == nil {
				harness.probe = func(io.Writer) bool { return true }
			}
			t.Setenv("ACR_VERSION_CHECK", "")
			stdout.Reset()
			stderr.Reset()
			exit := harness.run(&stdout, &stderr, shape.args...)
			checked, checkedErr := stdout.String(), stderr.String()

			t.Setenv("ACR_VERSION_CHECK", "off")
			stdout.Reset()
			stderr.Reset()
			offExit := harness.run(&stdout, &stderr, shape.args...)
			off, offErr := stdout.String(), stderr.String()

			if exit != offExit || exit != 0 {
				t.Fatalf("exit = %d with the check, %d without", exit, offExit)
			}
			if checked != off {
				t.Fatalf("stdout differs with the check on:\n on:  %q\n off: %q", checked, off)
			}
			if checkedErr != "" || offErr != "" {
				t.Fatalf("stderr = %q with the check, %q without; want both empty", checkedErr, offErr)
			}
			if checked == "" {
				t.Fatal("the shape produced no output at all, so parity proves nothing")
			}
		})
	}
	if harness.source.count() != 0 {
		t.Fatalf("machine-facing runs reached the release source %d times", harness.source.count())
	}
	harness.assertNoAttempt()
}

// TestReleaseNoticeEmptyCacheIsSilentThenRefreshesForTheNextRun is rows three
// and four: a fresh install says nothing, the refresh after the command fills
// the cache, the next run prints from it, and a second run inside the window
// performs no second fetch.
func TestReleaseNoticeEmptyCacheIsSilentThenRefreshesForTheNextRun(t *testing.T) {
	harness := newNoticeHarness(t)

	stdout, stderr, exit := harness.capture("version")
	if exit != 0 || stdout != noticeVersionOutput || stderr != "" {
		t.Fatalf("first run = %q / %q / %d; an empty cache must be silent", stdout, stderr, exit)
	}
	if harness.source.count() != 1 {
		t.Fatalf("source called %d times after the first run, want 1", harness.source.count())
	}
	cache, usable, err := harness.store.ReadCache()
	if err != nil || !usable || cache.LatestVersion != noticeLatest || cache.CheckedAt != harness.clock.Now() {
		t.Fatalf("cache after the first run = %#v, %t, %v", cache, usable, err)
	}

	stdout, stderr, exit = harness.capture("version")
	if exit != 0 || stdout != noticeVersionOutput || stderr != noticeLine {
		t.Fatalf("second run = %q / %q / %d; want the notice the refresh made possible", stdout, stderr, exit)
	}
	if harness.source.count() != 1 {
		t.Fatalf("source called %d times across two runs in one window, want 1", harness.source.count())
	}

	harness.clock.Advance(versioncheck.Window)
	harness.source.release.Tag = "v10.0.0"
	if _, _, exit := harness.capture("version"); exit != 0 {
		t.Fatalf("third run exit = %d", exit)
	}
	if harness.source.count() != 2 {
		t.Fatalf("source called %d times after the window closed, want 2", harness.source.count())
	}
	if _, stderr, _ := harness.capture("version"); stderr != "acr 10.0.0 is available (you have 1.2.3). Upgrade: brew upgrade jbaruch/agentic-context-registry/acr\n" {
		t.Fatalf("fourth run stderr = %q, want the newly cached release", stderr)
	}
}

func TestReleaseNoticeOffSwitchDisablesBothTheNoticeAndTheRefresh(t *testing.T) {
	harness := newNoticeHarness(t)
	harness.seedCache(noticeLatest)
	t.Setenv("ACR_VERSION_CHECK", "off")

	stdout, stderr, exit := harness.capture("version")
	if exit != 0 || stdout != noticeVersionOutput || stderr != "" {
		t.Fatalf("run with the check off = %q / %q / %d", stdout, stderr, exit)
	}
	if harness.source.count() != 0 {
		t.Fatalf("the switched-off check reached the release source %d times", harness.source.count())
	}
	harness.assertNoAttempt()
}

func TestReleaseNoticeIsSilentForCurrentNewerAndUnknownBuilds(t *testing.T) {
	for _, test := range []struct {
		name    string
		running string
		cache   string
	}{
		{name: "current", running: "9.9.9", cache: noticeLatest},
		{name: "current with module prefix", running: "v9.9.9", cache: noticeLatest},
		{name: "newer", running: "10.0.0", cache: noticeLatest},
		{name: "development build", running: "dev", cache: noticeLatest},
		{name: "malformed cache", running: "1.2.3", cache: "{not a record}"},
		{name: "prerelease cache", running: "1.2.3", cache: `{"schemaVersion":1,"checkedAt":"2001-02-03T04:05:06Z","latestVersion":"v9.9.9-rc1"}` + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNoticeHarness(t)
			version = test.running
			if strings.HasPrefix(test.cache, "v") {
				harness.seedCache(test.cache)
			} else {
				if err := os.MkdirAll(harness.store.BaseDirectory+"/version", 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(harness.store.CachePath(), []byte(test.cache), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			stdout, stderr, exit := harness.capture("version")
			if exit != 0 || stderr != "" || !strings.HasPrefix(stdout, test.running) {
				t.Fatalf("run = %q / %q / %d, want silence", stdout, stderr, exit)
			}
		})
	}
}

// TestReleaseNoticeSourceFailuresPreserveExitCodeAndCache is row six: an
// unreachable, slow, or nonsensical release source changes nothing the
// operator can see — not the exit code of a success, not the exit code of a
// refusal, not the cached bytes — and is not retried inside the window.
func TestReleaseNoticeSourceFailuresPreserveExitCodeAndCache(t *testing.T) {
	failing := &noticeSource{err: context.DeadlineExceeded}
	slow := &noticeSource{block: true}
	garbage := &noticeSource{release: dependency.Release{ID: 5, Tag: "nightly-build"}}
	for _, test := range []struct {
		name   string
		source *noticeSource
	}{
		{name: "unreachable", source: failing},
		{name: "slower than the timeout", source: slow},
		{name: "garbage tag", source: garbage},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNoticeHarness(t)
			harness.source = test.source
			harness.seedCache("v1.0.0")
			published := harness.cacheBytes()

			stdout, stderr, exit := harness.capture("version")
			if exit != 0 || stdout != noticeVersionOutput || stderr != "" {
				t.Fatalf("success run = %q / %q / %d", stdout, stderr, exit)
			}
			if got := harness.cacheBytes(); !bytes.Equal(got, published) {
				t.Fatalf("a failed refresh changed the cache: %q", got)
			}
			if _, usable, err := harness.store.ReadAttempt(); err != nil || !usable {
				t.Fatalf("the failed attempt was not recorded: %t, %v", usable, err)
			}
			calls := test.source.count()
			if calls != 1 {
				t.Fatalf("source called %d times, want 1", calls)
			}

			stdout, stderr, exit = harness.capture("missing")
			if exit != 2 || stdout != "" || !strings.Contains(stderr, `unknown command "missing"`) {
				t.Fatalf("refusal run = %q / %q / %d; the refusal must survive the check unchanged", stdout, stderr, exit)
			}
			if test.source.count() != calls {
				t.Fatalf("a failed attempt was retried inside the window: %d calls", test.source.count())
			}
			if got := harness.cacheBytes(); !bytes.Equal(got, published) {
				t.Fatalf("the cache changed across the refusal run: %q", got)
			}
		})
	}
}

func TestReleaseNoticeFutureAttemptDoesNotSuppressTheRefresh(t *testing.T) {
	harness := newNoticeHarness(t)
	if err := harness.store.WriteAttempt(harness.clock.Now().Add(365 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, stderr, exit := harness.capture("version"); exit != 0 || stderr != "" {
		t.Fatalf("run = %q / %d", stderr, exit)
	}
	if harness.source.count() != 1 {
		t.Fatalf("source called %d times with a future attempt on disk, want 1", harness.source.count())
	}
	if attempt, _, _ := harness.store.ReadAttempt(); attempt.AttemptedAt != harness.clock.Now() {
		t.Fatalf("attempt after the run = %s, want the current instant", attempt.AttemptedAt)
	}
}

func TestReleaseNoticeYieldsToAConcurrentRefreshHoldingTheLock(t *testing.T) {
	harness := newNoticeHarness(t)
	harness.seedCache(noticeLatest)
	holder, err := freshness.TryLockFile(harness.store.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	published := harness.cacheBytes()

	stdout, stderr, exit := harness.capture("version")
	if exit != 0 || stdout != noticeVersionOutput || stderr != noticeLine {
		t.Fatalf("run under a held lock = %q / %q / %d; the notice still serves from the cache", stdout, stderr, exit)
	}
	if harness.source.count() != 0 {
		t.Fatalf("a run that lost the lock reached the release source %d times", harness.source.count())
	}
	if got := harness.cacheBytes(); !bytes.Equal(got, published) {
		t.Fatalf("a run that lost the lock changed the cache: %q", got)
	}
	harness.assertNoAttempt()
}

// TestReleaseNoticeWritesNothingOffATerminal proves an ineligible run leaves
// the machine state directory exactly as empty as it found it: no cache, no
// attempt, no lock, no directory.
func TestReleaseNoticeWritesNothingOffATerminal(t *testing.T) {
	harness := newNoticeHarness(t)
	harness.probe = func(io.Writer) bool { return false }

	if _, stderr, exit := harness.capture("version"); exit != 0 || stderr != "" {
		t.Fatalf("run = %q / %d", stderr, exit)
	}
	entries, err := os.ReadDir(harness.store.BaseDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("an ineligible run wrote machine state: %v", entries)
	}
	if harness.source.count() != 0 {
		t.Fatalf("an ineligible run reached the release source %d times", harness.source.count())
	}
}
