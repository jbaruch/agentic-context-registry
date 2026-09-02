package migrateapp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
)

type migrationGitHub struct {
	releases map[string]dependency.Release
	calls    []string
}

func (github *migrationGitHub) LatestRelease(context.Context, dependency.Repository) (dependency.Release, error) {
	return dependency.Release{}, errors.New("unexpected latest release call")
}

func (github *migrationGitHub) ReleaseByTag(_ context.Context, _ dependency.Repository, tag string) (dependency.Release, error) {
	github.calls = append(github.calls, tag)
	if release, ok := github.releases[tag]; ok {
		return release, nil
	}
	return dependency.Release{}, &dependency.RemoteError{StatusCode: 404, Err: fmt.Errorf("tag %s not found", tag)}
}

func (github *migrationGitHub) ResolveCommit(context.Context, dependency.Repository, string) (string, error) {
	return "", errors.New("unexpected resolve call")
}

func (github *migrationGitHub) DownloadArchive(context.Context, dependency.Repository, string) ([]byte, error) {
	return nil, errors.New("unexpected download call")
}

func (github *migrationGitHub) DownloadReleaseAsset(context.Context, dependency.Repository, dependency.ReleaseAsset) ([]byte, error) {
	return nil, errors.New("unexpected asset call")
}

func TestTesslPinResolvesToTag(t *testing.T) {
	for _, test := range []struct {
		name     string
		releases map[string]dependency.Release
		want     string
		code     string
	}{
		{name: "plain", releases: map[string]dependency.Release{"1.2.3": {ID: 1, Tag: "1.2.3"}}, want: "1.2.3"},
		{name: "v-prefixed", releases: map[string]dependency.Release{"v1.2.3": {ID: 2, Tag: "v1.2.3"}}, want: "v1.2.3"},
		{name: "absent", releases: map[string]dependency.Release{}, code: "tessl_version_unavailable"},
		{name: "ambiguous", releases: map[string]dependency.Release{"1.2.3": {ID: 1, Tag: "1.2.3"}, "v1.2.3": {ID: 2, Tag: "v1.2.3"}}, code: "ambiguous_tessl_version"},
	} {
		t.Run(test.name, func(t *testing.T) {
			github := &migrationGitHub{releases: test.releases}
			service := newService(github)
			got, _, _, err := service.resolveMapping(context.Background(), dependency.State{}, migrate.Mapping{
				From: "example/pkg", Source: "github:example/pkg", Requested: "1.2.3", TesslVersion: "1.2.3",
			})
			if test.code == "" {
				if err != nil || got != test.want {
					t.Fatalf("resolveMapping() = %q, %v, want %q", got, err, test.want)
				}
			} else {
				var migrationErr *Error
				if !errors.As(err, &migrationErr) || migrationErr.Code != test.code {
					t.Fatalf("error = %#v, want code %s", err, test.code)
				}
			}
			if len(github.calls) != 2 || github.calls[0] != "1.2.3" || github.calls[1] != "v1.2.3" {
				t.Fatalf("tag calls = %#v", github.calls)
			}
		})
	}
}

func TestCompatibleProjectStateRejectsDisagreement(t *testing.T) {
	existing := dependency.State{
		Project: dependency.Project{Agents: []string{"codex"}, Dependencies: []dependency.Declaration{{Source: "github:one/pkg", Requested: "latest"}}},
		Lock:    dependency.Lockfile{Dependencies: []dependency.LockedDependency{{Source: "github:one/pkg", Requested: "latest"}}},
	}
	desired := dependency.State{
		Project: dependency.Project{Agents: []string{"cursor"}, Dependencies: []dependency.Declaration{{Source: "github:two/pkg", Requested: "latest"}}},
	}
	var migrationErr *Error
	if err := compatibleProjectState(existing, desired); !errors.As(err, &migrationErr) || migrationErr.Code != "project_state_conflict" {
		t.Fatalf("error = %#v", err)
	}
}
