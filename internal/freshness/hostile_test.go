package freshness

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostileThrottleBoundariesAndPolicySwitch(t *testing.T) {
	t.Parallel()

	state := State{LastCheckedAt: fixedNow, LastPolicy: PolicyOutdated}
	tests := []struct {
		name   string
		now    time.Time
		policy Policy
		want   bool
	}{
		{name: "minusOneSecond", now: fixedNow.Add(Window - time.Second), policy: PolicyOutdated, want: true},
		{name: "exactlyTwentyFourHours", now: fixedNow.Add(Window), policy: PolicyOutdated},
		{name: "plusOneSecond", now: fixedNow.Add(Window + time.Second), policy: PolicyOutdated},
		{name: "policySwitchInsideWindow", now: fixedNow.Add(Window - time.Second), policy: PolicyInstall},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := Throttled(state, test.policy, test.now); got != test.want {
				t.Fatalf("Throttled(%s, %s) = %t, want %t", test.now.Format(time.RFC3339), test.policy, got, test.want)
			}
		})
	}
}

func TestHostileFutureLastCheckedAtIsNotThrottled(t *testing.T) {
	t.Parallel()

	state := State{LastCheckedAt: fixedNow.Add(time.Hour), LastPolicy: PolicyOutdated}
	if Throttled(state, PolicyOutdated, fixedNow) {
		t.Fatal("Throttled() treated a future lastCheckedAt as inside the window; clock skew would suppress checks indefinitely")
	}
}

func TestHostileCorruptAndNewerStateAreUnusable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{name: "truncated", content: `{"schemaVersion":`},
		{name: "invalidJSON", content: `{not json}`},
		{name: "missingSchemaVersion", content: `{"project":"sha256:abc","lastCheckedAt":"2026-09-01T12:00:00Z","lastPolicy":"outdated","lastOutcome":"ok"}` + "\n"},
		{name: "newerSchema", content: `{"schemaVersion":99,"project":"sha256:abc","lastCheckedAt":"2026-09-01T12:00:00Z","lastPolicy":"outdated","lastOutcome":"ok"}` + "\n"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
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
			if err := os.WriteFile(statePath, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, usable, err := store.Read(project); err != nil || usable {
				t.Fatalf("Read() usable = %t, error = %v; corrupt or newer state must be no prior attempt", usable, err)
			}
		})
	}
}

func TestHostileCanonicalIdentitySymlinkVersusTwoProjects(t *testing.T) {
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
	realKey, _, err := ProjectIdentity(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	aliasKey, _, err := ProjectIdentity(alias)
	if err != nil {
		t.Fatal(err)
	}
	if realKey != aliasKey {
		t.Fatalf("symlink identity %q != real identity %q", aliasKey, realKey)
	}
	otherKey, _, err := ProjectIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if otherKey == realKey {
		t.Fatal("two TempDir projects shared a freshness identity")
	}
}
