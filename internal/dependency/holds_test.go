package dependency

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeHoldPolicy struct {
	calls    int
	decision HoldDecision
	err      error
}

func (policy *fakeHoldPolicy) Resolve(_ context.Context, _ Declaration, _ *LockedDependency, _ Release) (HoldDecision, error) {
	policy.calls++
	return policy.decision, policy.err
}

func TestHoldPolicySkipPreservesLatestLockWithoutArchive(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	current := strings.Repeat("a", 40)
	candidate := strings.Repeat("b", 40)
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{Source: "github:owner/plugin", Requested: "latest"}}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
			Source: "github:owner/plugin", Requested: "latest", Kind: ResolutionRelease,
			ReleaseID: 1, Tag: "v1.0.0", Commit: current, PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("a", 64),
		}}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(LockFilename)))
	if err != nil {
		t.Fatal(err)
	}
	remote := &fakeGitHub{latest: Release{ID: 2, Tag: "v2.0.0"}, commits: map[string]string{"v2.0.0": candidate}}
	holds := &fakeHoldPolicy{decision: HoldDecision{Skip: true, Notice: "Held at v1.0.0."}}
	result, err := NewServiceWithHoldPolicy(NewResolver(remote), holds).Reconcile(context.Background(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(LockFilename)))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || result.Changed || remote.downloadCalls != 0 || holds.calls != 1 {
		t.Fatalf("result = %#v, remote = %#v, hold calls = %d", result, remote, holds.calls)
	}
	if len(result.Notices) != 1 || result.Notices[0] != "Held at v1.0.0." {
		t.Fatalf("notices = %#v", result.Notices)
	}
}

func TestOutdatedConsultsHoldPolicyAndSurfacesNotice(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	current := strings.Repeat("c", 40)
	candidate := strings.Repeat("d", 40)
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{Source: "github:owner/plugin", Requested: "latest"}}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
			Source: "github:owner/plugin", Requested: "latest", Kind: ResolutionRelease,
			ReleaseID: 1, Tag: "v1.0.0", Commit: current, PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("c", 64),
		}}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	remote := &fakeGitHub{latest: Release{ID: 2, Tag: "v2.0.0"}, commits: map[string]string{"v2.0.0": candidate}}
	holds := &fakeHoldPolicy{decision: HoldDecision{Skip: true, Notice: "A newer candidate exists beyond the rollback barrier."}}
	outdated, err := NewServiceWithHoldPolicy(NewResolver(remote), holds).Outdated(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if holds.calls != 1 || remote.downloadCalls != 0 || len(outdated) != 1 || outdated[0].Notice != holds.decision.Notice {
		t.Fatalf("Outdated() = %#v, remote = %#v, hold calls = %d", outdated, remote, holds.calls)
	}
}
