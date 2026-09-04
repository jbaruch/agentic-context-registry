package dependency

import (
	"context"
	"strings"
	"testing"
)

// TestOutdatedMessageDistinguishesNothingToCheck covers issue #85: a project
// with nothing to check reported `All latest dependencies are current.`, which
// reads as a confirmation the command never made — in the dogfood it appeared
// right after a failed install.
func TestOutdatedMessageDistinguishesNothingToCheck(t *testing.T) {
	t.Parallel()

	const source = "github:owner/sibling"
	const pinnedTag = "v2.0.0"
	latest := Release{ID: 5, Tag: pinnedTag}
	commit := strings.Repeat("1", 40)

	tests := []struct {
		name     string
		state    State
		want     string
		unwanted string
	}{
		{
			name: "no declarations",
			state: State{
				Project: Project{SchemaVersion: CurrentSchemaVersion},
				Lock:    Lockfile{SchemaVersion: CurrentSchemaVersion},
			},
			want:     "No dependencies declared; nothing to check.",
			unwanted: "current",
		},
		{
			name: "every declaration pinned",
			state: State{
				Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
					{Source: source, Requested: pinnedTag},
				}},
				Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{
					{Source: source, Requested: pinnedTag, Kind: ResolutionRelease, ReleaseID: 5, Tag: pinnedTag,
						Commit: commit, PackageVersion: "2.0.0", ContentHash: "sha256:" + strings.Repeat("3", 64)},
				}},
			},
			want:     "No dependencies track latest; nothing to check.",
			unwanted: "current",
		},
		{
			name: "latest checked and current",
			state: State{
				Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
					{Source: source, Requested: "latest"},
				}},
				Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{
					{Source: source, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 5, Tag: pinnedTag,
						Commit: commit, PackageVersion: "2.0.0", ContentHash: "sha256:" + strings.Repeat("3", 64)},
				}},
			},
			want:     "All latest dependencies are current.",
			unwanted: "nothing to check",
		},
		{
			name: "latest checked and behind",
			state: State{
				Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
					{Source: source, Requested: "latest"},
				}},
				Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{
					{Source: source, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 4, Tag: "v1.0.0",
						Commit: strings.Repeat("9", 40), PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("3", 64)},
				}},
			},
			want:     "1 latest dependencies are outdated.",
			unwanted: "nothing to check",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if err := WriteState(root, test.state); err != nil {
				t.Fatal(err)
			}
			remote := &perSourceGitHub{
				latest:  map[string]Release{source: latest},
				commits: map[string]string{source + "@" + pinnedTag: commit},
			}
			report, err := NewService(NewResolver(remote)).OutdatedReport(context.Background(), root)
			if err != nil {
				t.Fatalf("OutdatedReport() error = %v", err)
			}
			message := outdatedMessage(report)
			if !strings.Contains(message, test.want) {
				t.Errorf("message = %q, want it to contain %q", message, test.want)
			}
			if strings.Contains(message, test.unwanted) {
				t.Errorf("message = %q, want it to omit %q", message, test.unwanted)
			}
		})
	}
}

// TestOutdatedDoesNotLookUpLatestForPinnedDeclarations keeps the pinned-only
// report from paying for a remote call it has no reason to make.
func TestOutdatedDoesNotLookUpLatestForPinnedDeclarations(t *testing.T) {
	t.Parallel()

	const source = "github:owner/sibling"
	root := t.TempDir()
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
			{Source: source, Requested: "v2.0.0"},
		}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	remote := &perSourceGitHub{}
	report, err := NewService(NewResolver(remote)).OutdatedReport(context.Background(), root)
	if err != nil {
		t.Fatalf("OutdatedReport() error = %v", err)
	}
	if remote.latestCalls != 0 {
		t.Errorf("latest lookups = %d, want none for a pinned declaration", remote.latestCalls)
	}
	if report.Declared != 1 || report.LatestTracked != 0 || len(report.Dependencies) != 0 {
		t.Errorf("report = %#v, want one declaration, none tracking latest, no rows", report)
	}
}
