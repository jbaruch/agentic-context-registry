package dependency

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// recordingTransport routes every request to one handler while preserving the
// URL an acceptance fixture needs to assert: scheme, host, path and method.
type recordingTransport struct {
	requests []string
	respond  func(*http.Request) *http.Response
}

func (transport *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.requests = append(transport.requests, request.Method+" "+request.URL.Scheme+"://"+request.URL.Host+request.URL.Path)
	return transport.respond(request), nil
}

func TestNewGitHubClientDefaultsToProductionTransport(t *testing.T) {
	t.Parallel()

	client := NewGitHubClient()
	if client.baseURL != "https://api.github.com" {
		t.Fatalf("baseURL = %q, want the production API host", client.baseURL)
	}
	if client.httpClient == nil || client.httpClient.Timeout != 2*time.Minute {
		t.Fatalf("httpClient = %#v, want the production timeout", client.httpClient)
	}
	if _, trusted := client.trustedArchiveOrigins["https://codeload.github.com"]; !trusted {
		t.Fatal("default client lost its trusted archive origin")
	}
	// A nil option value must not disable the transport timeout.
	unchanged := NewGitHubClient(WithHTTPClient(nil))
	if unchanged.httpClient == nil || unchanged.httpClient.Timeout != 2*time.Minute {
		t.Fatalf("WithHTTPClient(nil) httpClient = %#v", unchanged.httpClient)
	}
}

func TestWithHTTPClientKeepsProductionHostsAndAuthorization(t *testing.T) {
	t.Parallel()

	transport := &recordingTransport{}
	transport.respond = func(request *http.Request) *http.Response {
		body := `{"id":42,"tag_name":"v1.0.0","draft":false,"prerelease":false}`
		status := http.StatusOK
		if request.Method == http.MethodPost {
			body = `{"id":7,"name":"acr-package.json","url":"https://api.github.com/repos/owner/plugin/releases/assets/7"}`
			status = http.StatusCreated
		}
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Request:    request,
		}
	}
	client := NewGitHubClient(WithHTTPClient(&http.Client{Transport: transport}))
	client.tokenOnce.Do(func() { client.token = "fixture-token" })
	repository := Repository{Owner: "owner", Name: "plugin"}

	if _, err := client.LatestRelease(context.Background(), repository); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.UploadAsset(context.Background(), repository, 42, "acr-package.json", "application/json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET https://api.github.com/repos/owner/plugin/releases/latest",
		"POST https://uploads.github.com/repos/owner/plugin/releases/42/assets",
		"GET https://api.github.com/repos/owner/plugin/releases/assets/7",
	}
	if len(transport.requests) != len(want) {
		t.Fatalf("requests = %v, want %v", transport.requests, want)
	}
	for index, request := range want {
		if transport.requests[index] != request {
			t.Errorf("request %d = %q, want %q", index, transport.requests[index], request)
		}
	}
}
