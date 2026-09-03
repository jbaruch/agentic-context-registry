package freshnessapp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
)

// TestVerify8SessionStartHookExcludesVendoredAndKeepsUpstream mixes a vendored
// dependency with an ordinary GitHub dependency that is genuinely behind, and
// requires the session-start hook to surface exactly one of them.
//
// The vendored row has no upstream to advance to, so a notice about it is pure
// noise a user cannot act on -- but suppressing it must not suppress the real
// one that shares the run. The existing coverage checks the vendored row's
// Actionable() in isolation, which cannot catch an over-broad filter that drops
// the whole batch.
func TestVerify8SessionStartHookExcludesVendoredAndKeepsUpstream(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	writeProjectFile(t, project, "agents.yaml", "schemaVersion: 3\nfreshness: outdated\n")
	before := hashProjectTree(t, project)
	checker := &fakeOutdatedChecker{outdated: []dependency.OutdatedDependency{
		{Source: "vendor:example/orphan", Status: dependency.OutdatedVendored, CurrentContentHash: "sha256:" + strings.Repeat("c", 64)},
		{
			Source: "github:example/plugin", CurrentTag: "v1.0.0", CurrentCommit: strings.Repeat("a", 40),
			LatestTag: "v1.1.0", LatestCommit: strings.Repeat("b", 40),
		},
	}}
	runner := NewRunner(freshness.Store{BaseDirectory: t.TempDir()}, func() time.Time { return runnerNow }, checker)
	result, err := runner.Run(context.Background(), project, freshness.PolicyOutdated)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Outdated) != 1 || result.Outdated[0].Source != "github:example/plugin" {
		t.Fatalf("hook reported %#v, want only the upstream dependency", result.Outdated)
	}
	if len(result.Notices) != 1 || result.Notices[0].Code != CodeOutdated {
		t.Fatalf("notices = %#v, want exactly the upstream outdated notice", result.Notices)
	}
	if strings.Contains(result.Notices[0].Message, "vendor:") {
		t.Fatalf("the vendored dependency leaked into the hook notice: %q", result.Notices[0].Message)
	}
	if after := hashProjectTree(t, project); after != before {
		t.Fatalf("the session-start hook changed the project tree: before %s, after %s", before, after)
	}
}

// TestVerify8SessionStartHookIsSilentForAVendorOnlyProject is the boundary case
// of the row above: a project whose every dependency is vendored has nothing to
// report at session start, and must produce no notice rather than an empty
// "outdated" banner.
func TestVerify8SessionStartHookIsSilentForAVendorOnlyProject(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	writeProjectFile(t, project, "agents.yaml", "schemaVersion: 3\nfreshness: outdated\n")
	checker := &fakeOutdatedChecker{outdated: []dependency.OutdatedDependency{
		{Source: "vendor:example/alpha", Status: dependency.OutdatedVendored},
		{Source: "vendor:example/orphan", Status: dependency.OutdatedVendored},
	}}
	runner := NewRunner(freshness.Store{BaseDirectory: t.TempDir()}, func() time.Time { return runnerNow }, checker)
	result, err := runner.Run(context.Background(), project, freshness.PolicyOutdated)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Outdated) != 0 || len(result.Notices) != 0 {
		t.Fatalf("vendor-only project produced session-start output: %#v", result)
	}
}
