package versioncheck

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
)

// fakeSource is a call-counting release source. block, when set, holds every
// call until the test closes it; a source that honours its context returns
// the context's error instead when the context ends first, and one that does
// not stays blocked exactly as a hung network would.
type fakeSource struct {
	t             *testing.T
	mutex         sync.Mutex
	calls         int
	release       dependency.Release
	err           error
	block         chan struct{}
	honourContext bool
	entered       chan struct{}
}

func (source *fakeSource) LatestRelease(ctx context.Context, repository dependency.Repository) (dependency.Release, error) {
	if repository != Repository {
		source.t.Errorf("LatestRelease(%v), want %v", repository, Repository)
	}
	source.mutex.Lock()
	source.calls++
	source.mutex.Unlock()
	if source.entered != nil {
		source.entered <- struct{}{}
	}
	if source.block != nil {
		if source.honourContext {
			select {
			case <-source.block:
			case <-ctx.Done():
				return dependency.Release{}, ctx.Err()
			}
		} else {
			<-source.block
		}
	}
	return source.release, source.err
}

func (source *fakeSource) count() int {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	return source.calls
}

// stepClock is a clock the test moves. Nothing reads time.Now.
type stepClock struct {
	mutex sync.Mutex
	now   time.Time
}

func (clock *stepClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *stepClock) Advance(interval time.Duration) {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	clock.now = clock.now.Add(interval)
}

func alwaysTerminal(io.Writer) bool { return true }

var homebrewEnvironment = Environment{Executable: "/opt/homebrew/Cellar/acr/0.1.0/bin/acr"}

// newTestChecker composes a checker over an isolated store with every
// process-wide input replaced: the clock, the source, the environment and the
// terminal probe. The off switch is read from the process because that is the
// contract; a parallel test does not set it.
func newTestChecker(t *testing.T, store Store, clock *stepClock, source Source, options ...Option) *Checker {
	t.Helper()
	base := []Option{WithStore(store), WithClock(clock.Now), WithSource(source), WithEnvironment(homebrewEnvironment), WithTerminalProbe(alwaysTerminal)}
	return New(nil, append(base, options...)...)
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func TestNoticeReadsTheCacheAndNothingElse(t *testing.T) {
	t.Parallel()

	store := Store{BaseDirectory: t.TempDir()}
	source := &fakeSource{t: t, release: dependency.Release{ID: 1, Tag: "v9.9.9"}}
	checker := newTestChecker(t, store, &stepClock{now: fixedNow}, source)

	if notice, reason := checker.Notice("0.1.0"); notice != "" || reason != ReasonNoCache {
		t.Fatalf("Notice() on an empty cache = %q, %q", notice, reason)
	}
	if err := store.WriteCache(fixedNow, "v0.2.0"); err != nil {
		t.Fatal(err)
	}
	notice, reason := checker.Notice("0.1.0")
	if want := "acr 0.2.0 is available (you have 0.1.0). Upgrade: brew upgrade jbaruch/agentic-context-registry/acr\n"; notice != want || reason != ReasonNewer {
		t.Fatalf("Notice() = %q, %q, want %q, %q", notice, reason, want, ReasonNewer)
	}
	for _, test := range []struct {
		running string
		reason  Reason
	}{
		{running: "0.2.0", reason: ReasonCurrent},
		{running: "v0.2.0", reason: ReasonCurrent},
		{running: "0.3.0", reason: ReasonCurrent},
		{running: "dev", reason: ReasonUnknownRunning},
		{running: "", reason: ReasonUnknownRunning},
	} {
		if notice, reason := checker.Notice(test.running); notice != "" || reason != test.reason {
			t.Errorf("Notice(%q) = %q, %q, want silence for %q", test.running, notice, reason, test.reason)
		}
	}
	if err := os.WriteFile(store.CachePath(), []byte("{not a record}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if notice, reason := checker.Notice("0.1.0"); notice != "" || reason != ReasonNoCache {
		t.Fatalf("Notice() on a corrupt cache = %q, %q", notice, reason)
	}
	if err := os.Remove(store.CachePath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(store.CachePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if notice, reason := checker.Notice("0.1.0"); notice != "" || reason != ReasonUnreadableCache {
		t.Fatalf("Notice() on an unreadable cache = %q, %q", notice, reason)
	}

	if source.count() != 0 {
		t.Fatalf("Notice() reached the release source %d times; it must never touch the network", source.count())
	}
	for _, path := range []string{store.AttemptPath(), store.LockPath()} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("Notice() created %s: %v", path, err)
		}
	}
}

func TestNoticeNamesTheGoCommandForAGoInstall(t *testing.T) {
	t.Parallel()

	store := Store{BaseDirectory: t.TempDir()}
	if err := store.WriteCache(fixedNow, "v0.2.0"); err != nil {
		t.Fatal(err)
	}
	checker := newTestChecker(t, store, &stepClock{now: fixedNow}, &fakeSource{t: t},
		WithEnvironment(Environment{Executable: "/home/u/go/bin/acr", Home: "/home/u"}))
	notice, _ := checker.Notice("0.1.0")
	if want := "acr 0.2.0 is available (you have 0.1.0). Upgrade: go install github.com/jbaruch/agentic-context-registry/cmd/acr@latest\n"; notice != want {
		t.Fatalf("Notice() = %q, want %q", notice, want)
	}
}

func TestEligibleGatesEveryMachineFacingShape(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ACR_STATE_HOME", base)
	t.Setenv("ACR_VERSION_CHECK", "")
	var stdout, stderr bytes.Buffer
	terminals := map[io.Writer]bool{&stdout: true, &stderr: true}
	probe := func(writer io.Writer) bool { return terminals[writer] }
	checker := New(&fakeSource{t: t}, WithTerminalProbe(probe))
	if checker.store.BaseDirectory != base {
		t.Fatalf("New() store = %q, want ACR_STATE_HOME", checker.store.BaseDirectory)
	}
	if checker.timeout != DefaultTimeout {
		t.Fatalf("New() timeout = %s, want %s", checker.timeout, DefaultTimeout)
	}

	for _, test := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "plain command", args: []string{"list"}, want: true},
		{name: "no arguments", args: nil, want: true},
		{name: "help", args: []string{"help"}, want: true},
		{name: "json", args: []string{"list", "--json"}, want: false},
		{name: "json first", args: []string{"--json", "list"}, want: false},
		{name: "version json", args: []string{"version", "--json"}, want: false},
		{name: "non-interactive", args: []string{"init", "--non-interactive"}, want: false},
		{name: "json after the terminator is an argument", args: []string{"install", "--", "--json"}, want: true},
	} {
		if got := checker.Eligible(test.args, &stdout, &stderr); got != test.want {
			t.Errorf("Eligible(%v) = %t, want %t", test.args, got, test.want)
		}
	}
	var pipe bytes.Buffer
	if checker.Eligible([]string{"list"}, &pipe, &stderr) {
		t.Error("Eligible() with a piped stdout = true, want false")
	}
	if checker.Eligible([]string{"list"}, &stdout, &pipe) {
		t.Error("Eligible() with a piped stderr = true, want false")
	}
	if checker.Eligible([]string{"list"}, &pipe, &pipe) {
		t.Error("Eligible() with neither stream a terminal = true, want false")
	}

	t.Setenv("ACR_VERSION_CHECK", "off")
	if New(&fakeSource{t: t}, WithTerminalProbe(probe)).Eligible([]string{"list"}, &stdout, &stderr) {
		t.Error("Eligible() with ACR_VERSION_CHECK=off = true, want false")
	}
	for _, value := range []string{"OFF", "0", "false", "on"} {
		t.Setenv("ACR_VERSION_CHECK", value)
		if !New(&fakeSource{t: t}, WithTerminalProbe(probe)).Eligible([]string{"list"}, &stdout, &stderr) {
			t.Errorf("Eligible() with ACR_VERSION_CHECK=%q = false; only the value off disables the check", value)
		}
	}

	// Without a resolvable store there is nowhere to read or write, so the
	// check stays off rather than failing the command.
	t.Setenv("ACR_VERSION_CHECK", "")
	t.Setenv("ACR_STATE_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	unresolved := New(&fakeSource{t: t}, WithTerminalProbe(probe))
	if unresolved.storeErr == nil {
		t.Fatal("New() resolved a store with no ACR_STATE_HOME and no HOME")
	}
	if unresolved.Eligible([]string{"list"}, &stdout, &stderr) {
		t.Error("Eligible() without a store = true, want false")
	}
	if New(&fakeSource{t: t}, WithStore(Store{BaseDirectory: base}), WithTerminalProbe(probe)).Eligible([]string{"list"}, &stdout, &stderr) != true {
		t.Error("WithStore() did not clear the resolution error")
	}
	if New(&fakeSource{t: t}).Eligible([]string{"list"}, &stdout, &stderr) {
		t.Error("Eligible() without a terminal probe = true; no stream is a terminal until the composition root says so")
	}
}

func TestRefreshPopulatesAnEmptyCacheThenThrottlesForAWindow(t *testing.T) {
	t.Parallel()

	store := Store{BaseDirectory: t.TempDir()}
	clock := &stepClock{now: fixedNow}
	source := &fakeSource{t: t, release: dependency.Release{ID: 7, Tag: "v0.2.0"}}
	checker := newTestChecker(t, store, clock, source)
	ctx := context.Background()

	if outcome := checker.Refresh(ctx); outcome.Kind != KindRefreshed || outcome.Err != nil {
		t.Fatalf("first Refresh() = %+v", outcome)
	}
	cache, usable, err := store.ReadCache()
	if err != nil || !usable || cache.LatestVersion != "v0.2.0" || cache.CheckedAt != fixedNow {
		t.Fatalf("cache after refresh = %#v, %t, %v", cache, usable, err)
	}
	if notice, reason := checker.Notice("0.1.0"); reason != ReasonNewer || notice == "" {
		t.Fatalf("Notice() after refresh = %q, %q", notice, reason)
	}

	if outcome := checker.Refresh(ctx); outcome.Kind != KindThrottled {
		t.Fatalf("second Refresh() in the window = %+v", outcome)
	}
	clock.Advance(Window - time.Second)
	if outcome := checker.Refresh(ctx); outcome.Kind != KindThrottled {
		t.Fatalf("Refresh() one second before the window closes = %+v", outcome)
	}
	if source.count() != 1 {
		t.Fatalf("source called %d times inside one window, want 1", source.count())
	}
	clock.Advance(time.Second)
	source.release.Tag = "v0.3.0"
	if outcome := checker.Refresh(ctx); outcome.Kind != KindRefreshed {
		t.Fatalf("Refresh() at the window boundary = %+v", outcome)
	}
	if cache, _, _ := store.ReadCache(); cache.LatestVersion != "v0.3.0" || cache.CheckedAt != fixedNow.Add(Window) {
		t.Fatalf("cache after the second window = %#v", cache)
	}
	if source.count() != 2 {
		t.Fatalf("source called %d times across two windows, want 2", source.count())
	}
}

func TestRefreshTreatsAFutureAttemptAsDue(t *testing.T) {
	t.Parallel()

	store := Store{BaseDirectory: t.TempDir()}
	if err := store.WriteAttempt(fixedNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteAttempt(fixedNow.Add(365 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	source := &fakeSource{t: t, release: dependency.Release{ID: 7, Tag: "v0.2.0"}}
	checker := newTestChecker(t, store, &stepClock{now: fixedNow}, source)

	if outcome := checker.Refresh(context.Background()); outcome.Kind != KindRefreshed {
		t.Fatalf("Refresh() with a future attempt = %+v; clock skew would suppress the check indefinitely", outcome)
	}
	if attempt, usable, err := store.ReadAttempt(); err != nil || !usable || attempt.AttemptedAt != fixedNow {
		t.Fatalf("attempt after refresh = %#v, %t, %v; want it rewritten to the current instant", attempt, usable, err)
	}
	if source.count() != 1 {
		t.Fatalf("source called %d times, want 1", source.count())
	}
}

// TestRefreshFailureRecordsTheAttemptAndKeepsThePublishedBytes is the pair the
// two records exist for: the published cache is byte-identical after a failed
// attempt, and the attempt still counts against the window, so an offline day
// costs one timeout rather than one per command.
func TestRefreshFailureRecordsTheAttemptAndKeepsThePublishedBytes(t *testing.T) {
	t.Parallel()

	store := Store{BaseDirectory: t.TempDir()}
	if err := store.WriteCache(fixedNow.Add(-2*Window), "v0.1.5"); err != nil {
		t.Fatal(err)
	}
	published := readBytes(t, store.CachePath())
	failure := errors.New("dial tcp: no route to host")
	source := &fakeSource{t: t, err: failure}
	checker := newTestChecker(t, store, &stepClock{now: fixedNow}, source)

	outcome := checker.Refresh(context.Background())
	if outcome.Kind != KindFailed || !errors.Is(outcome.Err, failure) {
		t.Fatalf("Refresh() against a failing source = %+v", outcome)
	}
	if got := readBytes(t, store.CachePath()); !bytes.Equal(got, published) {
		t.Fatalf("a failed refresh changed the published cache:\n got %q\nwant %q", got, published)
	}
	if attempt, usable, err := store.ReadAttempt(); err != nil || !usable || attempt.AttemptedAt != fixedNow {
		t.Fatalf("attempt after failure = %#v, %t, %v", attempt, usable, err)
	}
	if outcome := checker.Refresh(context.Background()); outcome.Kind != KindThrottled {
		t.Fatalf("Refresh() after a failed attempt in the window = %+v, want throttled", outcome)
	}
	if source.count() != 1 {
		t.Fatalf("source called %d times, want 1: a failed attempt still counts against the window", source.count())
	}
	if notice, reason := checker.Notice("0.1.0"); reason != ReasonNewer || notice == "" {
		t.Fatalf("Notice() after a failed refresh = %q, %q; the previous cache must still serve", notice, reason)
	}
}

func TestRefreshRejectsAnythingButAStableRelease(t *testing.T) {
	t.Parallel()

	for _, tag := range []string{"", "latest", "v0.3.0-rc1", "1.2.3.4", "nightly"} {
		tag := tag
		t.Run(tag, func(t *testing.T) {
			t.Parallel()
			store := Store{BaseDirectory: t.TempDir()}
			if err := store.WriteCache(fixedNow.Add(-2*Window), "v0.1.5"); err != nil {
				t.Fatal(err)
			}
			published := readBytes(t, store.CachePath())
			source := &fakeSource{t: t, release: dependency.Release{ID: 9, Tag: tag}}
			checker := newTestChecker(t, store, &stepClock{now: fixedNow}, source)
			if outcome := checker.Refresh(context.Background()); outcome.Kind != KindFailed || outcome.Err == nil {
				t.Fatalf("Refresh() with tag %q = %+v", tag, outcome)
			}
			if got := readBytes(t, store.CachePath()); !bytes.Equal(got, published) {
				t.Fatalf("garbage tag %q changed the published cache: %q", tag, got)
			}
			if _, usable, _ := store.ReadAttempt(); !usable {
				t.Fatal("the attempt was not recorded")
			}
		})
	}
}

func TestRefreshTimesOutASlowSourceAndAbandonsAHungOne(t *testing.T) {
	t.Parallel()

	const timeout = 20 * time.Millisecond
	t.Run("source that honours its context", func(t *testing.T) {
		t.Parallel()
		store := Store{BaseDirectory: t.TempDir()}
		if err := store.WriteCache(fixedNow.Add(-2*Window), "v0.1.5"); err != nil {
			t.Fatal(err)
		}
		published := readBytes(t, store.CachePath())
		source := &fakeSource{t: t, release: dependency.Release{ID: 9, Tag: "v0.2.0"}, block: make(chan struct{}), honourContext: true}
		t.Cleanup(func() { close(source.block) })
		checker := newTestChecker(t, store, &stepClock{now: fixedNow}, source, WithTimeout(timeout))
		outcome := checker.Refresh(context.Background())
		if outcome.Kind != KindFailed || !errors.Is(outcome.Err, context.DeadlineExceeded) {
			t.Fatalf("Refresh() against a slow source = %+v", outcome)
		}
		if got := readBytes(t, store.CachePath()); !bytes.Equal(got, published) {
			t.Fatalf("a timed-out refresh changed the published cache: %q", got)
		}
		if _, usable, _ := store.ReadAttempt(); !usable {
			t.Fatal("the timed-out attempt was not recorded")
		}
	})
	t.Run("source that ignores its context", func(t *testing.T) {
		t.Parallel()
		store := Store{BaseDirectory: t.TempDir()}
		source := &fakeSource{t: t, release: dependency.Release{ID: 9, Tag: "v0.2.0"}, block: make(chan struct{})}
		t.Cleanup(func() { close(source.block) })
		checker := newTestChecker(t, store, &stepClock{now: fixedNow}, source, WithTimeout(timeout))
		outcome := checker.Refresh(context.Background())
		if outcome.Kind != KindFailed || !errors.Is(outcome.Err, context.DeadlineExceeded) {
			t.Fatalf("Refresh() against a hung source = %+v; the bound must hold whether or not the source cooperates", outcome)
		}
		if _, usable, _ := store.ReadCache(); usable {
			t.Fatal("a hung refresh published a cache")
		}
	})
	t.Run("parent context already cancelled", func(t *testing.T) {
		t.Parallel()
		store := Store{BaseDirectory: t.TempDir()}
		source := &fakeSource{t: t, release: dependency.Release{ID: 9, Tag: "v0.2.0"}, block: make(chan struct{}), honourContext: true}
		t.Cleanup(func() { close(source.block) })
		checker := newTestChecker(t, store, &stepClock{now: fixedNow}, source, WithTimeout(timeout))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		outcome := checker.Refresh(ctx)
		if outcome.Kind != KindFailed || !errors.Is(outcome.Err, context.Canceled) {
			t.Fatalf("Refresh() under a cancelled context = %+v", outcome)
		}
		if _, usable, _ := store.ReadCache(); usable {
			t.Fatal("a cancelled refresh published a cache")
		}
	})
}

func TestRefreshReportsBusyWhileAnotherHolderHasTheLock(t *testing.T) {
	t.Parallel()

	store := Store{BaseDirectory: t.TempDir()}
	holder, err := freshness.TryLockFile(store.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	source := &fakeSource{t: t, release: dependency.Release{ID: 9, Tag: "v0.2.0"}}
	checker := newTestChecker(t, store, &stepClock{now: fixedNow}, source)

	outcome := checker.Refresh(context.Background())
	if outcome.Kind != KindBusy || !errors.Is(outcome.Err, freshness.ErrLockBusy) {
		t.Fatalf("Refresh() under a held lock = %+v", outcome)
	}
	if source.count() != 0 {
		t.Fatalf("a busy refresh reached the source %d times", source.count())
	}
	if _, err := os.Lstat(store.AttemptPath()); !os.IsNotExist(err) {
		t.Fatalf("a busy refresh recorded an attempt: %v", err)
	}
}

// TestConcurrentRefreshesPerformExactlyOneFetch runs one refresh into the
// fetch, holds it there, and starts eight more against the same store: every
// one of them is refused by the lock, the held one completes alone, and the
// next refresh after it is throttled. One window, one fetch, one intact
// record, whatever the process count.
func TestConcurrentRefreshesPerformExactlyOneFetch(t *testing.T) {
	t.Parallel()

	store := Store{BaseDirectory: t.TempDir()}
	clock := &stepClock{now: fixedNow}
	source := &fakeSource{t: t, release: dependency.Release{ID: 9, Tag: "v0.2.0"}, block: make(chan struct{}), entered: make(chan struct{}, 1)}
	ctx := context.Background()

	holder := newTestChecker(t, store, clock, source, WithTimeout(10*time.Second))
	holderDone := make(chan Outcome, 1)
	go func() { holderDone <- holder.Refresh(ctx) }()
	<-source.entered

	const contenders = 8
	outcomes := make(chan Outcome, contenders)
	var group sync.WaitGroup
	for index := 0; index < contenders; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			outcomes <- newTestChecker(t, store, clock, source, WithTimeout(10*time.Second)).Refresh(ctx)
		}()
	}
	group.Wait()
	close(outcomes)
	for outcome := range outcomes {
		if outcome.Kind != KindBusy {
			t.Errorf("contender Refresh() = %+v, want busy while the holder is inside its fetch", outcome)
		}
	}

	close(source.block)
	if outcome := <-holderDone; outcome.Kind != KindRefreshed || outcome.Err != nil {
		t.Fatalf("holder Refresh() = %+v", outcome)
	}
	if outcome := newTestChecker(t, store, clock, source).Refresh(ctx); outcome.Kind != KindThrottled {
		t.Fatalf("Refresh() after the holder finished = %+v, want throttled", outcome)
	}
	if source.count() != 1 {
		t.Fatalf("source called %d times by %d concurrent refreshes, want 1", source.count(), contenders+2)
	}
	cache, usable, err := store.ReadCache()
	if err != nil || !usable || cache.LatestVersion != "v0.2.0" {
		t.Fatalf("cache after concurrent refreshes = %#v, %t, %v", cache, usable, err)
	}
	if entries, err := os.ReadDir(filepath.Dir(store.CachePath())); err != nil || len(entries) != 3 {
		t.Fatalf("record directory holds %v (%v), want the two records and the lock", entries, err)
	}
}
