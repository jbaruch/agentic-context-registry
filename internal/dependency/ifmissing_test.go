package dependency

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestInstallIfMissingPreservesExistingPinLatestAndHold(t *testing.T) {
	for _, policy := range []string{"pin", "latest", "hold"} {
		t.Run(policy, func(t *testing.T) {
			root := heldProject(t, strings.Repeat("a", 40))
			state, err := LoadState(root)
			if err != nil {
				t.Fatal(err)
			}
			if policy != "hold" {
				state.Project.Dependencies[0].Hold = nil
				state.Lock.Dependencies[0].Hold = nil
			}
			if policy == "pin" {
				state.Project.Dependencies[0].Requested = heldTag
				state.Lock.Dependencies[0].Requested = heldTag
			}
			state.Project.Extra = map[string]any{"foreign-setting": "unchanged"}
			if err := WriteState(root, state); err != nil {
				t.Fatal(err)
			}
			beforeProject, beforeLock := readStateFiles(t, root)
			remote := &fakeGitHub{err: errors.New("remote should not resolve an existing lock")}
			result, err := NewService(NewResolver(remote)).InstallIfMissing(context.Background(), root, heldSource, "v9.9.9", false)
			if err != nil || result.Changed {
				t.Fatalf("preserve %s: %v %+v", policy, err, result)
			}
			afterProject, afterLock := readStateFiles(t, root)
			if beforeProject != afterProject || beforeLock != afterLock {
				t.Fatal("existing policy/foreign state changed")
			}
			if remote.downloadCalls != 0 || remote.resolveCalls != 0 || remote.releaseCalls != 0 {
				t.Fatalf("remote calls: %+v", remote)
			}
		})
	}
}
func TestInstallIfMissingResolvesMissingLockUsingExistingRequest(t *testing.T) {
	root := t.TempDir()
	state := State{Project: Project{SchemaVersion: 2, Dependencies: []Declaration{{Source: heldSource, Requested: "v1.0.0"}, {Source: "github:foreign/unresolved", Requested: "latest"}}}, Lock: Lockfile{SchemaVersion: 2}}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	remote := &fakeGitHub{releases: map[string]Release{"v1.0.0": {ID: 1, Tag: "v1.0.0"}}, commits: map[string]string{"v1.0.0": commit}, archives: map[string][]byte{commit: packageArchive(t, "1.0.0", "preserved pin\n")}}
	result, err := NewService(NewResolver(remote)).InstallIfMissing(context.Background(), root, heldSource, "v9.9.9", false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || len(result.Dependencies) != 1 || result.Dependencies[0].Tag != "v1.0.0" {
		t.Fatalf("wrong fallback resolution: %+v", result)
	}
}
