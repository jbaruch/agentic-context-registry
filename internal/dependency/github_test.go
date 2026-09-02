package dependency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
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
	if err != nil || !reflect.DeepEqual(exact, latest) {
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

func TestGitHubClientReleasePublishingContracts(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("b", 40)
	created := false
	assets := make(map[string][]byte)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/plugin/releases/tags/v1.2.3":
			if !created {
				http.NotFound(writer, request)
				return
			}
			writeTestResponse(t, writer, `{"id":77,"tag_name":"v1.2.3","target_commitish":"`+commit+`","draft":true,"prerelease":false,"assets":[]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/plugin/git/ref/tags/v1.2.3":
			writeTestResponse(t, writer, `{"object":{"sha":"`+commit+`"}}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/plugin/commits/v1.2.3":
			writeTestResponse(t, writer, `{"sha":"`+commit+`"}`)
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/plugin/releases":
			var payload struct {
				Tag        string `json:"tag_name"`
				Target     string `json:"target_commitish"`
				Draft      bool   `json:"draft"`
				Prerelease bool   `json:"prerelease"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Errorf("decode create payload: %v", err)
			}
			if payload.Tag != "v1.2.3" || payload.Target != commit || !payload.Draft || payload.Prerelease {
				t.Errorf("create payload = %#v", payload)
			}
			created = true
			writer.WriteHeader(http.StatusCreated)
			writeTestResponse(t, writer, `{"id":77,"tag_name":"v1.2.3","target_commitish":"`+commit+`","draft":true,"prerelease":false}`)
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/plugin/releases/77/assets":
			name := request.URL.Query().Get("name")
			contents, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read uploaded asset: %v", err)
			}
			assets[name] = contents
			writer.WriteHeader(http.StatusCreated)
			writeTestResponse(t, writer, `{"id":91,"name":`+fmt.Sprintf("%q", name)+`,"url":`+fmt.Sprintf("%q", server.URL+"/assets/91")+`}`)
		case request.Method == http.MethodGet && request.URL.Path == "/assets/91":
			writer.Header().Set("Content-Type", "application/octet-stream")
			writeTestResponse(t, writer, string(assets["checksums.txt"]))
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/plugin/releases/77":
			writeTestResponse(t, writer, `{"id":77,"tag_name":"v1.2.3","target_commitish":"`+commit+`","draft":false,"prerelease":false,"html_url":"https://github.com/owner/plugin/releases/tag/v1.2.3"}`)
		case request.Method == http.MethodDelete && request.URL.Path == "/repos/owner/plugin/releases/77":
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := newGitHubClient(server.URL, server.Client())
	client.tokenOnce.Do(func() {})
	repository := Repository{Owner: "owner", Name: "plugin"}

	if _, exists, err := client.LookupRelease(context.Background(), repository, "v1.2.3"); err != nil || exists {
		t.Fatalf("LookupRelease(absent) exists = %t, error = %v", exists, err)
	}
	gotCommit, exists, err := client.TagCommit(context.Background(), repository, "v1.2.3")
	if err != nil || !exists || gotCommit != commit {
		t.Fatalf("TagCommit() = %q, %t, %v", gotCommit, exists, err)
	}
	draft, err := client.CreateRelease(context.Background(), repository, "v1.2.3", commit)
	if err != nil || draft.ID != 77 || !draft.Draft {
		t.Fatalf("CreateRelease() = %#v, %v", draft, err)
	}
	if _, exists, err := client.LookupRelease(context.Background(), repository, "v1.2.3"); err != nil || !exists {
		t.Fatalf("LookupRelease(draft) exists = %t, error = %v", exists, err)
	}
	asset := []byte("digest  archive.tar.gz\n")
	_, verified, err := client.UploadAsset(context.Background(), repository, draft.ID, "checksums.txt", "text/plain", asset)
	if err != nil || string(verified) != string(asset) {
		t.Fatalf("UploadAsset() verified = %q, error = %v", verified, err)
	}
	published, err := client.PublishRelease(context.Background(), repository, draft.ID)
	if err != nil || published.Draft || published.Prerelease || published.HTMLURL == "" {
		t.Fatalf("PublishRelease() = %#v, %v", published, err)
	}
	if err := client.DeleteRelease(context.Background(), repository, draft.ID); err != nil {
		t.Fatalf("DeleteRelease() error = %v", err)
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
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusNotFound {
		t.Fatalf("LatestRelease() error = %v, want RemoteError with status 404", err)
	}
	if !IsGitHubStatus(err, http.StatusNotFound) {
		t.Fatalf("LatestRelease() error = %v, want nested GitHubAPIError with status 404", err)
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

func TestGitHubClientCancellationDoesNotPoisonTokenCache(t *testing.T) {
	t.Parallel()

	client := newGitHubClient("https://example.invalid", http.DefaultClient)
	client.tokenProvider = func(ctx context.Context) string {
		if err := ctx.Err(); err != nil {
			t.Fatalf("token provider context error = %v, want cancellation detached", err)
		}
		return "placeholder"
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	request, err := client.request(cancelled, "/repos/owner/plugin/releases/latest")
	if err != nil {
		t.Fatalf("request() error = %v", err)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer placeholder" {
		t.Fatalf("Authorization = %q, want cached bearer token", got)
	}

	retry, err := client.request(context.Background(), "/repos/owner/plugin/releases/latest")
	if err != nil {
		t.Fatalf("retry request() error = %v", err)
	}
	if got := retry.Header.Get("Authorization"); got != "Bearer placeholder" {
		t.Fatalf("retry Authorization = %q, want cached bearer token", got)
	}
}

func TestGitHubClientForwardsAuthenticationToTrustedArchiveRedirect(t *testing.T) {
	t.Parallel()

	const token = "placeholder"
	archiveAuthorization := make(chan string, 1)
	archiveServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		archiveAuthorization <- request.Header.Get("Authorization")
		writeTestResponse(t, writer, "private archive")
	}))
	defer archiveServer.Close()
	archiveOrigin := strings.Replace(archiveServer.URL, "127.0.0.1", "localhost", 1)
	apiServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, archiveOrigin+"/owner/plugin.tar.gz", http.StatusFound)
	}))
	defer apiServer.Close()
	client := newGitHubClient(apiServer.URL, apiServer.Client())
	client.trustedDownloadOrigins[archiveOrigin] = struct{}{}
	client.token = token
	client.tokenOnce.Do(func() {})

	contents, err := client.DownloadArchive(context.Background(), Repository{Owner: "owner", Name: "plugin"}, strings.Repeat("a", 40))
	if err != nil {
		t.Fatalf("DownloadArchive() error = %v", err)
	}
	if string(contents) != "private archive" {
		t.Fatalf("DownloadArchive() = %q, want private archive", contents)
	}
	if authorization := <-archiveAuthorization; authorization != "Bearer "+token {
		t.Fatalf("redirect Authorization = %q, want bearer token", authorization)
	}
}

func TestGitHubClientRejectsUntrustedArchiveRedirect(t *testing.T) {
	t.Parallel()

	archiveRequest := make(chan struct{}, 1)
	archiveServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		archiveRequest <- struct{}{}
		writeTestResponse(t, writer, "untrusted archive")
	}))
	defer archiveServer.Close()
	archiveOrigin := strings.Replace(archiveServer.URL, "127.0.0.1", "localhost", 1)
	apiServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, archiveOrigin+"/owner/plugin.tar.gz", http.StatusFound)
	}))
	defer apiServer.Close()
	client := newGitHubClient(apiServer.URL, apiServer.Client())
	client.token = "placeholder"
	client.tokenOnce.Do(func() {})

	_, err := client.DownloadArchive(context.Background(), Repository{Owner: "owner", Name: "plugin"}, strings.Repeat("a", 40))
	if err == nil || !strings.Contains(err.Error(), "untrusted origin") {
		t.Fatalf("DownloadArchive() error = %v, want untrusted-origin rejection", err)
	}
	select {
	case <-archiveRequest:
		t.Fatal("untrusted archive server received redirected request")
	default:
	}
}

func TestGitHubClientReleaseAssetRedirectPolicy(t *testing.T) {
	t.Parallel()

	t.Run("trusted allowlisted origin", func(t *testing.T) {
		const token = "placeholder"
		assetAuthorization := make(chan string, 1)
		assetServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assetAuthorization <- request.Header.Get("Authorization")
			writeTestResponse(t, writer, "release asset")
		}))
		defer assetServer.Close()
		apiServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			http.Redirect(writer, request, assetServer.URL+"/asset", http.StatusFound)
		}))
		defer apiServer.Close()
		client := newGitHubClient(apiServer.URL, apiServer.Client())
		client.trustedDownloadOrigins[urlOrigin(mustParseURL(t, assetServer.URL))] = struct{}{}
		client.token = token
		client.tokenOnce.Do(func() {})

		contents, err := client.DownloadReleaseAsset(context.Background(), Repository{Owner: "owner", Name: "plugin"}, ReleaseAsset{ID: 1, Name: "acr-package.json", URL: apiServer.URL + "/assets/1"})
		if err != nil {
			t.Fatalf("DownloadReleaseAsset() error = %v", err)
		}
		if string(contents) != "release asset" {
			t.Fatalf("DownloadReleaseAsset() = %q, want release asset", contents)
		}
		if authorization := <-assetAuthorization; authorization != "Bearer "+token {
			t.Fatalf("redirect Authorization = %q, want bearer token", authorization)
		}
	})

	t.Run("untrusted origin", func(t *testing.T) {
		assetRequest := make(chan struct{}, 1)
		assetServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			assetRequest <- struct{}{}
			writeTestResponse(t, writer, "untrusted asset")
		}))
		defer assetServer.Close()
		apiServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			http.Redirect(writer, request, assetServer.URL+"/asset", http.StatusFound)
		}))
		defer apiServer.Close()
		client := newGitHubClient(apiServer.URL, apiServer.Client())
		client.tokenOnce.Do(func() {})

		_, err := client.DownloadReleaseAsset(context.Background(), Repository{Owner: "owner", Name: "plugin"}, ReleaseAsset{ID: 1, Name: "acr-package.json", URL: apiServer.URL + "/assets/1"})
		if err == nil || !strings.Contains(err.Error(), "untrusted origin") {
			t.Fatalf("DownloadReleaseAsset() error = %v, want untrusted-origin rejection", err)
		}
		select {
		case <-assetRequest:
			t.Fatal("untrusted asset server received redirected request")
		default:
		}
	})

	t.Run("bounded chain", func(t *testing.T) {
		var apiServer *httptest.Server
		apiServer = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			step, err := strconv.Atoi(strings.TrimPrefix(request.URL.Path, "/assets/"))
			if err != nil {
				t.Errorf("parse redirect step: %v", err)
				http.Error(writer, "invalid step", http.StatusBadRequest)
				return
			}
			http.Redirect(writer, request, apiServer.URL+"/assets/"+strconv.Itoa(step+1), http.StatusFound)
		}))
		defer apiServer.Close()
		client := newGitHubClient(apiServer.URL, apiServer.Client())
		client.tokenOnce.Do(func() {})

		_, err := client.DownloadReleaseAsset(context.Background(), Repository{Owner: "owner", Name: "plugin"}, ReleaseAsset{ID: 1, Name: "acr-package.json", URL: apiServer.URL + "/assets/0"})
		if err == nil || !strings.Contains(err.Error(), "stop after 10 GitHub release asset redirects") {
			t.Fatalf("DownloadReleaseAsset() error = %v, want bounded-redirect rejection", err)
		}
	})
}

func mustParseURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse URL %q: %v", rawURL, err)
	}
	return parsed
}

func writeTestResponse(t *testing.T, writer io.Writer, response string) {
	t.Helper()
	if _, err := io.WriteString(writer, response); err != nil {
		t.Errorf("write response: %v", err)
	}
}
