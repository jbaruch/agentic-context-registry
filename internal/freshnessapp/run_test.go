package freshnessapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
)

var runnerNow = time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)

type fakeOutdatedChecker struct {
	calls    int
	outdated []dependency.OutdatedDependency
	err      error
}

type failingLock struct {
	err error
}

func (lock failingLock) Close() error { return lock.err }

func (checker *fakeOutdatedChecker) Outdated(context.Context, string) ([]dependency.OutdatedDependency, error) {
	checker.calls++
	return append([]dependency.OutdatedDependency(nil), checker.outdated...), checker.err
}

func TestOutdatedModeIsReadOnlyOnProjectAndPackages(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	writeProjectFile(t, project, "agents.yaml", "schemaVersion: 1\nfreshness: outdated\n")
	writeProjectFile(t, project, ".agents/registry.lock", "schemaVersion: 1\n")
	writeProjectFile(t, project, ".agents/packages/immutable.txt", "package bytes\n")
	writeProjectFile(t, project, ".codex/config.toml", "model = \"gpt-5\"\n")
	before := hashProjectTree(t, project)
	checker := &fakeOutdatedChecker{outdated: []dependency.OutdatedDependency{{
		Source: "github:example/plugin", CurrentTag: "v1.0.0", CurrentCommit: strings.Repeat("a", 40),
		LatestTag: "v1.1.0", LatestCommit: strings.Repeat("b", 40),
	}}}
	runner := NewRunner(freshness.Store{BaseDirectory: t.TempDir()}, func() time.Time { return runnerNow }, checker)
	result, err := runner.Run(context.Background(), project, freshness.PolicyOutdated)
	if err != nil {
		t.Fatal(err)
	}
	if checker.calls != 1 || len(result.Outdated) != 1 || len(result.Notices) != 1 || result.Notices[0].Code != CodeOutdated {
		t.Fatalf("result = %#v, calls = %d", result, checker.calls)
	}
	if after := hashProjectTree(t, project); after != before {
		t.Fatalf("project tree changed: before %s, after %s", before, after)
	}
}

func TestRunnerThrottleBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		now       time.Time
		wantCalls int
		throttled bool
	}{
		{name: "minus one second", now: runnerNow.Add(freshness.Window - time.Second), throttled: true},
		{name: "exactly twenty four hours", now: runnerNow.Add(freshness.Window), wantCalls: 1},
		{name: "plus one second", now: runnerNow.Add(freshness.Window + time.Second), wantCalls: 1},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			project := t.TempDir()
			store := freshness.Store{BaseDirectory: t.TempDir()}
			if err := store.Write(project, runnerNow, freshness.PolicyOutdated, freshness.OutcomeOK); err != nil {
				t.Fatal(err)
			}
			checker := &fakeOutdatedChecker{}
			runner := NewRunner(store, func() time.Time { return test.now }, checker)
			result, err := runner.Run(context.Background(), project, freshness.PolicyOutdated)
			if err != nil {
				t.Fatal(err)
			}
			if checker.calls != test.wantCalls || result.Throttled != test.throttled {
				t.Fatalf("calls = %d, throttled = %t; want %d, %t", checker.calls, result.Throttled, test.wantCalls, test.throttled)
			}
		})
	}
}

func TestRunnerPolicySwitchBypassesThrottle(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	store := freshness.Store{BaseDirectory: t.TempDir()}
	if err := store.Write(project, runnerNow, freshness.PolicyInstall, freshness.OutcomeOK); err != nil {
		t.Fatal(err)
	}
	checker := &fakeOutdatedChecker{}
	runner := NewRunner(store, func() time.Time { return runnerNow.Add(time.Second) }, checker)
	result, err := runner.Run(context.Background(), project, freshness.PolicyOutdated)
	if err != nil || result.Throttled || checker.calls != 1 {
		t.Fatalf("Run() = %#v, %v, calls = %d", result, err, checker.calls)
	}
}

func TestRunnerCorruptAndNewerStateConverge(t *testing.T) {
	t.Parallel()

	for _, content := range []string{`{"schemaVersion":`, `{"schemaVersion":99}` + "\n"} {
		content := content
		t.Run(content, func(t *testing.T) {
			t.Parallel()
			project := t.TempDir()
			store := freshness.Store{BaseDirectory: t.TempDir()}
			statePath, _, err := store.Paths(project)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(statePath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			checker := &fakeOutdatedChecker{}
			runner := NewRunner(store, func() time.Time { return runnerNow }, checker)
			if _, err := runner.Run(context.Background(), project, freshness.PolicyOutdated); err != nil {
				t.Fatal(err)
			}
			state, usable, err := store.Read(project)
			if err != nil || !usable || state.SchemaVersion != freshness.StateSchemaVersion || checker.calls != 1 {
				t.Fatalf("state = %#v, usable = %t, error = %v, calls = %d", state, usable, err, checker.calls)
			}
		})
	}
}

func TestRunnerClassifiesRemoteFailuresAndAdvancesState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		remote  *dependency.RemoteError
		code    string
		outcome freshness.Outcome
		command string
	}{
		{name: "offline", remote: &dependency.RemoteError{Err: errors.New("network unreachable")}, code: CodeOffline, outcome: freshness.OutcomeOffline},
		{name: "authentication", remote: &dependency.RemoteError{StatusCode: 401, Err: errors.New("access denied")}, code: CodeAuth, outcome: freshness.OutcomeAuth},
		{name: "server error", remote: &dependency.RemoteError{StatusCode: 500, Err: errors.New("server unavailable")}, code: CodeUpdateFailed, outcome: freshness.OutcomeFailed, command: "acr outdated"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			project := t.TempDir()
			store := freshness.Store{BaseDirectory: t.TempDir()}
			checker := &fakeOutdatedChecker{err: test.remote}
			runner := NewRunner(store, func() time.Time { return runnerNow }, checker)
			result, err := runner.Run(context.Background(), project, freshness.PolicyOutdated)
			var runErr *RunError
			if !errors.As(err, &runErr) || runErr.Code != test.code || runErr.ExitCode != 1 {
				t.Fatalf("Run() error = %#v", err)
			}
			if len(result.Notices) != 1 || result.Notices[0].Code != test.code {
				t.Fatalf("notices = %#v", result.Notices)
			}
			if test.command != "" && (!strings.Contains(result.Notices[0].Message, test.command) || strings.Contains(result.Notices[0].Message, "acr install")) {
				t.Fatalf("notice = %q, want %q guidance without acr install", result.Notices[0].Message, test.command)
			}
			state, usable, readErr := store.Read(project)
			if readErr != nil || !usable || state.LastCheckedAt != runnerNow || state.LastOutcome != test.outcome {
				t.Fatalf("state = %#v, usable = %t, error = %v", state, usable, readErr)
			}
		})
	}
}

func TestRunnerLockContentionIsSilentFromRemote(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	store := freshness.Store{BaseDirectory: t.TempDir()}
	held, err := store.TryLock(project)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	checker := &fakeOutdatedChecker{}
	result, err := NewRunner(store, func() time.Time { return runnerNow }, checker).Run(context.Background(), project, freshness.PolicyOutdated)
	if err != nil || checker.calls != 0 || len(result.Notices) != 1 || result.Notices[0].Code != CodeBusy {
		t.Fatalf("Run() = %#v, %v, calls = %d", result, err, checker.calls)
	}
	if _, usable, err := store.Read(project); err != nil || usable {
		t.Fatalf("state usable = %t, error = %v", usable, err)
	}
}

func TestRunnerReportsStateWriteFailure(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	checker := &fakeOutdatedChecker{}
	runner := NewRunner(freshness.Store{BaseDirectory: t.TempDir()}, func() time.Time { return runnerNow }, checker)
	runner.write = func(freshness.Store, string, time.Time, freshness.Policy, freshness.Outcome) error {
		return errors.New("read-only state volume")
	}
	result, err := runner.Run(context.Background(), project, freshness.PolicyOutdated)
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Code != CodeStateUnwritable || checker.calls != 1 {
		t.Fatalf("Run() = %#v, %v, calls = %d", result, err, checker.calls)
	}
	if len(result.Notices) != 1 || result.Notices[0].Code != CodeStateUnwritable {
		t.Fatalf("notices = %#v", result.Notices)
	}
}

func TestRunnerReportsLockReleaseFailure(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	checker := &fakeOutdatedChecker{}
	runner := NewRunner(freshness.Store{BaseDirectory: t.TempDir()}, func() time.Time { return runnerNow }, checker)
	runner.acquire = func(freshness.Store, string) (lockHandle, error) {
		return failingLock{err: errors.New("close failed")}, nil
	}
	result, err := runner.Run(context.Background(), project, freshness.PolicyOutdated)
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Code != CodeLockRelease || runErr.ExitCode != 1 || checker.calls != 1 {
		t.Fatalf("Run() = %#v, %v, calls = %d", result, err, checker.calls)
	}
	if len(result.Notices) != 1 || result.Notices[0].Code != CodeLockRelease || strings.Contains(result.Notices[0].Message, "not writable") {
		t.Fatalf("notices = %#v", result.Notices)
	}
}

func writeProjectFile(t *testing.T, root, path, content string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hashProjectTree(t *testing.T, root string) string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	digest := sha256.New()
	for _, path := range paths {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		digest.Write([]byte(filepath.ToSlash(relative)))
		digest.Write([]byte{0})
		digest.Write([]byte(info.Mode().Perm().String()))
		digest.Write([]byte{0})
		digest.Write(content)
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}
