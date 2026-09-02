package freshness

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)

func TestStateRoundTripAndUnknownVersionRewrite(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	store := Store{BaseDirectory: t.TempDir()}
	if err := store.Write(project, fixedNow, PolicyOutdated, OutcomeOK); err != nil {
		t.Fatal(err)
	}
	state, usable, err := store.Read(project)
	if err != nil || !usable {
		t.Fatalf("Read() = %#v, %t, %v", state, usable, err)
	}
	if state.SchemaVersion != 1 || state.LastCheckedAt != fixedNow || state.LastPolicy != PolicyOutdated || state.LastOutcome != OutcomeOK {
		t.Fatalf("state = %#v", state)
	}
	statePath, _, err := store.Paths(project)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(content), `{"schemaVersion":1,"project":"sha256:`) || !strings.Contains(string(content), `"lastCheckedAt":"2026-09-01T12:00:00Z"`) {
		t.Fatalf("state JSON = %s", content)
	}
	newer := strings.Replace(string(content), `"schemaVersion":1`, `"schemaVersion":99`, 1)
	if err := os.WriteFile(statePath, []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, usable, err := store.Read(project); err != nil || usable {
		t.Fatalf("newer Read() usable = %t, error = %v", usable, err)
	}
	if err := store.Write(project, fixedNow.Add(time.Hour), PolicyInstall, OutcomeFailed); err != nil {
		t.Fatal(err)
	}
	rewritten, usable, err := store.Read(project)
	if err != nil || !usable || rewritten.SchemaVersion != 1 || rewritten.LastPolicy != PolicyInstall {
		t.Fatalf("rewritten = %#v, %t, %v", rewritten, usable, err)
	}
}

func TestCorruptStateIsNoUsablePriorState(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	store := Store{BaseDirectory: t.TempDir()}
	statePath, _, err := store.Paths(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(`{"schemaVersion":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, usable, err := store.Read(project); err != nil || usable {
		t.Fatalf("Read() usable = %t, error = %v", usable, err)
	}
}

func TestThrottleBoundariesAndPolicySwitch(t *testing.T) {
	t.Parallel()

	state := State{LastCheckedAt: fixedNow, LastPolicy: PolicyOutdated}
	tests := []struct {
		name   string
		now    time.Time
		policy Policy
		want   bool
	}{
		{name: "minus one second", now: fixedNow.Add(Window - time.Second), policy: PolicyOutdated, want: true},
		{name: "exactly twenty four hours", now: fixedNow.Add(Window), policy: PolicyOutdated},
		{name: "plus one second", now: fixedNow.Add(Window + time.Second), policy: PolicyOutdated},
		{name: "policy switch", now: fixedNow.Add(time.Second), policy: PolicyInstall},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := Throttled(state, test.policy, test.now); got != test.want {
				t.Fatalf("Throttled() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestCanonicalProjectIdentity(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	realRoot := filepath.Join(parent, "project")
	if err := os.Mkdir(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	realKey, realIdentity, err := ProjectIdentity(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	aliasKey, aliasIdentity, err := ProjectIdentity(alias)
	if err != nil {
		t.Fatal(err)
	}
	if realKey != aliasKey || realIdentity != aliasIdentity {
		t.Fatalf("canonical identities differ: %q/%q and %q/%q", realKey, realIdentity, aliasKey, aliasIdentity)
	}
	otherKey, _, err := ProjectIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if otherKey == realKey {
		t.Fatal("distinct projects share a freshness key")
	}
}

func TestProjectLockIsNonBlocking(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	store := Store{BaseDirectory: t.TempDir()}
	first, err := store.TryLock(project)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := store.TryLock(project)
	if second != nil {
		second.Close()
		t.Fatal("second TryLock() acquired an already-held lock")
	}
	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("second TryLock() error = %v, want ErrLockBusy", err)
	}
}
