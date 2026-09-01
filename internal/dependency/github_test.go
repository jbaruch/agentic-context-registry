package dependency

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseSource(t *testing.T) {
	t.Parallel()

	repository, err := ParseSource("github:owner/plugin")
	if err != nil || repository.FullName() != "owner/plugin" {
		t.Fatalf("ParseSource() = %#v, %v", repository, err)
	}
	for _, source := range []string{"owner/plugin", "github:Owner/plugin", "github:owner", "github:owner/plugin/extra"} {
		if _, err := ParseSource(source); err == nil || !strings.Contains(err.Error(), "github:owner/repository") {
			t.Errorf("ParseSource(%q) error = %v, want canonical guidance", source, err)
		}
	}
}

func TestGitHubClientReleaseAndCommitContracts(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/plugin/releases/latest":
			writeTestResponse(t, writer, `{"id":42,"tag_name":"v1.2.3","draft":false,"prerelease":false}`)
		case "/repos/owner/plugin/releases/tags/v1.2.3":
			writeTestResponse(t, writer, `{"id":42,"tag_name":"v1.2.3","draft":false,"prerelease":false}`)
		case "/repos/owner/plugin/commits/v1.2.3":
			writeTestResponse(t, writer, fmt.Sprintf(`{"sha":%q}`, commit))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := newGitHubClient(server.URL, server.Client())
	client.tokenOnce.Do(func() {})
	repository := Repository{Owner: "owner", Name: "plugin"}

	latest, err := client.LatestRelease(context.Background(), repository)
	if err != nil || latest.ID != 42 || latest.Tag != "v1.2.3" {
		t.Fatalf("LatestRelease() = %#v, %v", latest, err)
	}
	exact, err := client.ReleaseByTag(context.Background(), repository, "v1.2.3")
	if err != nil || exact != latest {
		t.Fatalf("ReleaseByTag() = %#v, %v, want %#v", exact, err, latest)
	}
	gotCommit, err := client.ResolveCommit(context.Background(), repository, latest.Tag)
	if err != nil || gotCommit != commit {
		t.Fatalf("ResolveCommit() = %q, %v", gotCommit, err)
	}
}

func TestGitHubClientRejectsPrereleaseTag(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, writer, `{"id":7,"tag_name":"v2.0.0-rc.1","draft":false,"prerelease":true}`)
	}))
	defer server.Close()
	client := newGitHubClient(server.URL, server.Client())
	client.tokenOnce.Do(func() {})

	_, err := client.ReleaseByTag(context.Background(), Repository{Owner: "owner", Name: "plugin"}, "v2.0.0-rc.1")
	if err == nil || !strings.Contains(err.Error(), "choose a stable release tag") {
		t.Fatalf("ReleaseByTag() error = %v, want stable-release guidance", err)
	}
}

func TestGitHubClientPrivateRepositoryGuidance(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		writeTestResponse(t, writer, `{"message":"Not Found"}`)
	}))
	defer server.Close()
	client := newGitHubClient(server.URL, server.Client())
	client.tokenOnce.Do(func() {})

	_, err := client.LatestRelease(context.Background(), Repository{Owner: "owner", Name: "private"})
	if err == nil || !strings.Contains(err.Error(), "gh auth login") {
		t.Fatalf("LatestRelease() error = %v, want authentication guidance", err)
	}
}

func TestGitHubClientUsesAuthenticationHeader(t *testing.T) {
	t.Parallel()

	const token = "placeholder"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, writer, `{"id":9,"tag_name":"v1.0.0","draft":false,"prerelease":false}`)
	}))
	defer server.Close()
	client := newGitHubClient(server.URL, server.Client())
	client.token = token
	client.tokenOnce.Do(func() {})

	if _, err := client.LatestRelease(context.Background(), Repository{Owner: "owner", Name: "private"}); err != nil {
		t.Fatalf("LatestRelease() error = %v", err)
	}
}

func writeTestResponse(t *testing.T, writer io.Writer, response string) {
	t.Helper()
	if _, err := io.WriteString(writer, response); err != nil {
		t.Errorf("write response: %v", err)
	}
}
