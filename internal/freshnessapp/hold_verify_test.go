package freshnessapp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
)

func TestVerifySessionStartInstallUnderAHoldMakesNoReinstall(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	heldCommit := strings.Repeat("c", 40)
	state := dependency.State{
		Project: dependency.Project{
			SchemaVersion: dependency.CurrentSchemaVersion, Freshness: "install",
			Dependencies: []dependency.Declaration{{
				Source: "github:acme/widget", Requested: "latest",
				Hold: &dependency.Hold{Pin: "v2.1.0", Rejected: "v2.2.0"},
			}},
		},
		Lock: dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion, Dependencies: []dependency.LockedDependency{{
			Source: "github:acme/widget", Requested: "latest", Kind: dependency.ResolutionRelease,
			ReleaseID: 21, Tag: "v2.1.0", Commit: heldCommit, PackageVersion: "2.1.0",
			ContentHash: "sha256:" + strings.Repeat("c", 64),
			Hold:        &dependency.LockHold{RejectedTag: "v2.2.0", RejectedReleaseID: 22},
		}}},
	}
	if err := dependency.WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, filepath.FromSlash(dependency.LockFilename))
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	remote := &heldGitHub{candidate: strings.Repeat("d", 40)}
	service := dependency.NewService(dependency.NewResolver(remote))
	realizer := &fakeRealizer{}
	runner := NewRunner(freshness.Store{BaseDirectory: t.TempDir()}, func() time.Time { return runnerNow }, &fakeOutdatedChecker{}).WithInstall(service, realizer)

	result, err := runner.Run(context.Background(), root, freshness.PolicyInstall)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("session-start install rewrote the lock under a hold:\n%s", after)
	}
	if remote.downloadCalls != 0 {
		t.Fatalf("session-start install downloaded the rejected release: calls=%d", remote.downloadCalls)
	}
	if realizer.calls != 1 {
		t.Fatalf("realizer calls = %d, want the install mode to still run realization on the unchanged lock", realizer.calls)
	}
	_ = result
}
