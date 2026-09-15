package versioncheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
)

const (
	// EnvSwitch is the environment variable that turns the notice and the
	// refresh off together. Only the value off disables them; the check is on
	// by default and on for every other value. It is deliberately separate
	// from the dependency freshness policy in agents.yaml: whether acr watches
	// a project's packages and whether it watches itself are different
	// questions, and this one is answered per machine.
	EnvSwitch = "ACR_VERSION_CHECK"
	// DefaultTimeout bounds one refresh from the moment its fetch starts.
	DefaultTimeout = 2 * time.Second
	// Window is the throttle window between refresh attempts on one machine.
	Window = freshness.Window
)

// Repository is the release source the check watches: acr's own releases.
var Repository = dependency.Repository{Owner: "jbaruch", Name: "agentic-context-registry"}

// attemptPolicy is the policy both sides of the throttle comparison carry.
// freshness.Throttled takes a policy so a per-project record can run again
// when its policy changes; the version cache has exactly one policy, so the
// same sentinel is stored and compared, and the function contributes what it
// was reused for: the window, and the rule that a timestamp in the future is
// due rather than throttled.
const attemptPolicy = freshness.PolicyOutdated

// Source resolves the latest stable release. dependency.GitHub satisfies it,
// so the shipped binary refreshes through the client the rest of acr already
// trusts, and a test supplies a fake that counts its calls.
type Source interface {
	LatestRelease(context.Context, dependency.Repository) (dependency.Release, error)
}

// Reason explains why Notice did or did not produce a line.
type Reason string

const (
	// ReasonNewer means the cache names a release newer than the running build.
	ReasonNewer Reason = "newer"
	// ReasonCurrent means the running build is at or past the cached release.
	ReasonCurrent Reason = "current"
	// ReasonNoCache means no usable cache exists yet, as on a fresh install.
	ReasonNoCache Reason = "no-cache"
	// ReasonUnreadableCache means a cache file exists but could not be read.
	ReasonUnreadableCache Reason = "unreadable-cache"
	// ReasonUnknownRunning means the running build has no comparable version,
	// as a development build does.
	ReasonUnknownRunning Reason = "unknown-running"
)

// Kind classifies one Refresh.
type Kind string

const (
	// KindRefreshed means the cache now holds the release the source returned.
	KindRefreshed Kind = "refreshed"
	// KindThrottled means an attempt inside the window already happened.
	KindThrottled Kind = "throttled"
	// KindBusy means another process holds the lock, so it owns this attempt.
	KindBusy Kind = "busy"
	// KindFailed means the attempt was made and did not produce a release: the
	// source failed, timed out, or answered with something that is not a
	// stable release, or a record could not be written. The cache is untouched.
	KindFailed Kind = "failed"
)

// Outcome is the result of one Refresh. Err carries the failure behind a
// KindFailed or KindBusy outcome, and a lock-release failure joined onto any
// kind. Nothing here reaches the operator; the caller decides what to do with
// it, and the shipped binary does nothing, by design.
type Outcome struct {
	Kind Kind
	Err  error
}

// Checker decides whether to print the notice and performs the refresh.
type Checker struct {
	enabled     bool
	store       Store
	storeErr    error
	clock       freshness.Clock
	source      Source
	timeout     time.Duration
	environment func() Environment
	terminal    func(io.Writer) bool
}

// Option adjusts one construction detail of the shipped checker. Without
// options, New composes exactly what the binary runs, except for the terminal
// probe, which the composition root supplies.
type Option func(*Checker)

// WithStore replaces the machine-local store and clears any resolution error.
func WithStore(store Store) Option {
	return func(checker *Checker) {
		checker.store, checker.storeErr = store, nil
	}
}

// WithClock replaces the clock the throttle window and the written timestamps
// come from. A nil clock is ignored so a caller cannot accidentally stop time.
func WithClock(clock freshness.Clock) Option {
	return func(checker *Checker) {
		if clock != nil {
			checker.clock = clock
		}
	}
}

// WithSource replaces the release source. A nil source is ignored.
func WithSource(source Source) Option {
	return func(checker *Checker) {
		if source != nil {
			checker.source = source
		}
	}
}

// WithTimeout replaces the refresh timeout. A non-positive value is ignored so
// a caller cannot accidentally make the refresh unbounded or instant.
func WithTimeout(timeout time.Duration) Option {
	return func(checker *Checker) {
		if timeout > 0 {
			checker.timeout = timeout
		}
	}
}

// WithEnvironment replaces what install detection reads with fixed values.
func WithEnvironment(env Environment) Option {
	return func(checker *Checker) {
		checker.environment = func() Environment { return env }
	}
}

// WithTerminalProbe supplies the predicate that decides whether an output
// stream is a real terminal. The composition root passes the binary's own
// probe; without one, no stream is a terminal and the checker stays silent.
func WithTerminalProbe(probe func(io.Writer) bool) Option {
	return func(checker *Checker) {
		if probe != nil {
			checker.terminal = probe
		}
	}
}

// New constructs the checker. It reads the off switch and resolves the store
// now; it reads no file, spawns nothing, and touches no network.
func New(source Source, options ...Option) *Checker {
	checker := &Checker{
		enabled:     os.Getenv(EnvSwitch) != "off",
		clock:       time.Now,
		source:      source,
		timeout:     DefaultTimeout,
		environment: ProcessEnvironment,
		terminal:    func(io.Writer) bool { return false },
	}
	checker.store, checker.storeErr = DefaultStore()
	for _, option := range options {
		// A nil option is ignored so a caller assembling options
		// conditionally does not have to filter them.
		if option != nil {
			option(checker)
		}
	}
	return checker
}

// Eligible reports whether this invocation may print the notice and refresh
// the cache: the switch is on, the store resolved, the arguments select
// neither --json nor --non-interactive, and both stdout and stderr are real
// terminals. One predicate gates both halves, so a machine-facing run never
// prints and never touches the network either.
func (checker *Checker) Eligible(args []string, stdout, stderr io.Writer) bool {
	return checker.enabled && checker.storeErr == nil && !machineFacing(args) && checker.terminal(stdout) && checker.terminal(stderr)
}

// machineFacing reports whether the arguments ask for output a program will
// read. The scan stops at "--", as the CLI's own --json detection does.
func machineFacing(args []string) bool {
	for _, argument := range args {
		if argument == "--" {
			return false
		}
		if argument == "--json" || argument == "--non-interactive" {
			return true
		}
	}
	return false
}

// Notice returns the one-line, newline-terminated notice for a build running
// the given version, or "" with the reason there is none. It reads the cache
// and nothing else: no lock, no write, no network, and install detection runs
// only once a notice is due.
func (checker *Checker) Notice(running string) (string, Reason) {
	if !comparable(running) {
		return "", ReasonUnknownRunning
	}
	cache, usable, err := checker.store.ReadCache()
	if err != nil {
		return "", ReasonUnreadableCache
	}
	if !usable {
		return "", ReasonNoCache
	}
	if !Newer(running, cache.LatestVersion) {
		return "", ReasonCurrent
	}
	return Render(running, cache.LatestVersion, Classify(checker.environment())), ReasonNewer
}

// Refresh brings the cache up to date at most once per Window. It takes the
// store's advisory lock so two concurrent processes cannot tear the records;
// the one that loses the lock reports busy and leaves the attempt to the
// winner. The fetch runs under a hard timeout, and every attempt is recorded
// before its result is judged, so a failing network costs one timeout per
// window rather than one per command. Only a stable release rewrites the
// cache; a failure leaves the published bytes untouched.
func (checker *Checker) Refresh(ctx context.Context) (outcome Outcome) {
	lock, err := freshness.TryLockFile(checker.store.LockPath())
	if errors.Is(err, freshness.ErrLockBusy) {
		return Outcome{Kind: KindBusy, Err: err}
	}
	if err != nil {
		return Outcome{Kind: KindFailed, Err: err}
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			outcome.Err = errors.Join(outcome.Err, closeErr)
		}
	}()

	now := checker.clock().UTC()
	// An unreadable attempt record is no prior attempt: the refresh is due,
	// and the write that follows reports the store failure if it persists.
	if last, usable, _ := checker.store.ReadAttempt(); usable && freshness.Throttled(attemptState(last), attemptPolicy, now) {
		return Outcome{Kind: KindThrottled}
	}
	release, fetchErr := checker.fetch(ctx)
	if err := checker.store.WriteAttempt(now); err != nil {
		return Outcome{Kind: KindFailed, Err: errors.Join(err, fetchErr)}
	}
	if fetchErr != nil {
		return Outcome{Kind: KindFailed, Err: fetchErr}
	}
	if !stable(release.Tag) {
		return Outcome{Kind: KindFailed, Err: fmt.Errorf("latest release tag %q is not a stable semantic version", release.Tag)}
	}
	if err := checker.store.WriteCache(now, release.Tag); err != nil {
		return Outcome{Kind: KindFailed, Err: err}
	}
	return Outcome{Kind: KindRefreshed}
}

// fetch asks the source for the latest release and waits at most the timeout,
// whether or not the source honours its context. A source that ignores
// cancellation is abandoned in its goroutine; the caller returns, and the
// process exits behind it, so a hung network can delay a command by the
// timeout and never longer.
func (checker *Checker) fetch(ctx context.Context) (dependency.Release, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, checker.timeout)
	defer cancel()
	type answer struct {
		release dependency.Release
		err     error
	}
	done := make(chan answer, 1)
	go func() {
		release, err := checker.source.LatestRelease(fetchCtx, Repository)
		done <- answer{release: release, err: err}
	}()
	select {
	case result := <-done:
		return result.release, result.err
	case <-fetchCtx.Done():
		return dependency.Release{}, fetchCtx.Err()
	}
}

func attemptState(attempt Attempt) freshness.State {
	return freshness.State{LastCheckedAt: attempt.AttemptedAt, LastPolicy: attemptPolicy}
}
