package dependency

import (
	"context"
	"strings"
	"testing"
)

func TestFollowupsHoldSkipKeepsLockDataAndCarriesDeclaredRequested(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)
	hash := "sha256:" + strings.Repeat("b", 64)
	declaration := Declaration{
		Source: heldSource, Requested: "latest",
		Hold: &Hold{Pin: heldTag, Rejected: rejectedTag},
	}
	existing := LockedDependency{
		Source: heldSource, Requested: heldTag, Kind: ResolutionRelease, ReleaseID: 987,
		Tag: heldTag, Commit: commit, PackageVersion: "1.3.2", ContentHash: hash,
		Hold: &LockHold{RejectedTag: rejectedTag},
	}
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{declaration}},
		Lock:    Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{existing}},
	}
	remote := &fakeGitHub{
		latest:  Release{ID: 2048, Tag: rejectedTag},
		commits: map[string]string{rejectedTag: strings.Repeat("9", 40)},
	}
	holds := &fakeHoldPolicy{decision: HoldDecision{Skip: true, Notice: "Held at " + heldTag + "."}}

	got, outcome, err := NewServiceWithHoldPolicy(NewResolver(remote), holds).resolveState(context.Background(), t.TempDir(), state, map[string]bool{heldSource: true})
	if err != nil {
		t.Fatalf("resolveState() error = %v", err)
	}
	if holds.calls != 1 || remote.downloadCalls != 0 || remote.resolveCalls != 0 {
		t.Fatalf("Skip path contacted the archive: holds=%d remote=%#v", holds.calls, remote)
	}
	if len(got.Lock.Dependencies) != 1 {
		t.Fatalf("locks = %#v", got.Lock.Dependencies)
	}
	locked := got.Lock.Dependencies[0]
	if locked.Requested != "latest" {
		t.Fatalf("Skip path kept stale requested %q, want the newly declared latest policy", locked.Requested)
	}
	if locked.Commit != commit || locked.Tag != heldTag || locked.ContentHash != hash || locked.ReleaseID != 987 {
		t.Fatalf("Skip path rewrote resolved lock data: %#v", locked)
	}
	if locked.Hold == nil || locked.Hold.RejectedTag != rejectedTag {
		t.Fatalf("Skip path dropped hold metadata: %#v", locked.Hold)
	}
	if len(outcome.held) != 1 || outcome.held[0] != heldSource {
		t.Fatalf("held = %#v", outcome.held)
	}
}
