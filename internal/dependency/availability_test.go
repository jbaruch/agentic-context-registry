package dependency

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// availabilityTransport answers production-host requests by path from a
// fixed table and records each request, so a test asserts both the
// classification and the exact probe sequence that produced it.
type availabilityTransport struct {
	statuses  map[string]int
	bodies    map[string]string
	failPath  string
	requests  []string
	authValue string
}

func (transport *availabilityTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.requests = append(transport.requests, request.Method+" "+request.URL.Scheme+"://"+request.URL.Host+request.URL.Path)
	transport.authValue = request.Header.Get("Authorization")
	if request.URL.Path == transport.failPath {
		return nil, errors.New("dial tcp: connection refused")
	}
	status, ok := transport.statuses[request.URL.Path]
	if !ok {
		status = http.StatusTeapot
	}
	body := transport.bodies[request.URL.Path]
	if body == "" {
		body = `{"message":"fixture"}`
	}
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Request: request,
		Body:   io.NopCloser(strings.NewReader(body)),
		Header: http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func TestInspectReleaseAvailabilityClassifiesGitHubEvidence(t *testing.T) {
	t.Parallel()

	const latest = "/repos/owner/plugin/releases/latest"
	const repository = "/repos/owner/plugin"
	stable := `{"id":42,"tag_name":"v1.0.0","draft":false,"prerelease":false}`
	tests := []struct {
		name         string
		statuses     map[string]int
		bodies       map[string]string
		failPath     string
		want         ReleaseAvailability
		wantErr      string
		wantStatus   int
		plainError   bool
		wantRequests []string
	}{
		{name: "stable release", statuses: map[string]int{latest: 200}, bodies: map[string]string{latest: stable},
			want: ReleaseAvailability{Accessible: true, Stable: true}, wantRequests: []string{latest}},
		{name: "non-stable latest is not stable", statuses: map[string]int{latest: 200},
			bodies: map[string]string{latest: `{"id":42,"tag_name":"v1.0.0-rc1","prerelease":true}`},
			want:   ReleaseAvailability{Accessible: true}, wantRequests: []string{latest}},
		{name: "null latest release is not evidence", statuses: map[string]int{latest: 200}, bodies: map[string]string{latest: `null`},
			wantErr: "invalid latest release", plainError: true, wantRequests: []string{latest}},
		{name: "empty latest release is not evidence", statuses: map[string]int{latest: 200}, bodies: map[string]string{latest: `{}`},
			wantErr: "invalid latest release", plainError: true, wantRequests: []string{latest}},
		{name: "latest release without a positive id is not evidence", statuses: map[string]int{latest: 200}, bodies: map[string]string{latest: `{"id":0,"tag_name":"v2.0.0"}`},
			wantErr: "invalid latest release", plainError: true, wantRequests: []string{latest}},
		{name: "latest release with a string id is not evidence", statuses: map[string]int{latest: 200}, bodies: map[string]string{latest: `{"id":"42","tag_name":"v2.0.0"}`},
			wantErr: "decode GitHub response", plainError: true, wantRequests: []string{latest}},
		{name: "latest release without a tag is not evidence", statuses: map[string]int{latest: 200}, bodies: map[string]string{latest: `{"id":42}`},
			wantErr: "invalid latest release", plainError: true, wantRequests: []string{latest}},
		{name: "readable repository without a stable release", statuses: map[string]int{latest: 404, repository: 200},
			bodies: map[string]string{repository: `{"id":7,"full_name":"owner/plugin"}`},
			want:   ReleaseAvailability{Accessible: true}, wantRequests: []string{latest, repository}},
		{name: "missing or private repository stays ambiguous", statuses: map[string]int{latest: 404, repository: 404},
			want: ReleaseAvailability{}, wantRequests: []string{latest, repository}},
		{name: "invalid repository resource is not evidence", statuses: map[string]int{latest: 404, repository: 200},
			bodies: map[string]string{repository: `{}`}, wantErr: "invalid repository", plainError: true, wantRequests: []string{latest, repository}},
		{name: "unauthorized release lookup", statuses: map[string]int{latest: 401},
			wantErr: "gh auth login", wantStatus: 401, wantRequests: []string{latest}},
		{name: "forbidden or rate-limited release lookup", statuses: map[string]int{latest: 403},
			wantErr: "GitHub access denied", wantStatus: 403, wantRequests: []string{latest}},
		{name: "server error on release lookup", statuses: map[string]int{latest: 503},
			wantErr: "retry the request", wantStatus: 503, wantRequests: []string{latest}},
		{name: "forbidden repository probe", statuses: map[string]int{latest: 404, repository: 403},
			wantErr: "GitHub access denied", wantStatus: 403, wantRequests: []string{latest, repository}},
		{name: "server error on repository probe", statuses: map[string]int{latest: 404, repository: 500},
			wantErr: "retry the request", wantStatus: 500, wantRequests: []string{latest, repository}},
		{name: "network failure on release lookup", failPath: latest,
			wantErr: "check network access", wantRequests: []string{latest}},
		{name: "network failure on repository probe", statuses: map[string]int{latest: 404}, failPath: repository,
			wantErr: "check network access", wantRequests: []string{latest, repository}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transport := &availabilityTransport{statuses: test.statuses, bodies: test.bodies, failPath: test.failPath}
			client := NewGitHubClient(WithHTTPClient(&http.Client{Transport: transport}))
			client.tokenOnce.Do(func() { client.token = "fixture-token" })

			got, err := client.InspectReleaseAvailability(context.Background(), Repository{Owner: "owner", Name: "plugin"})
			if test.wantErr == "" {
				if err != nil || got != test.want {
					t.Fatalf("InspectReleaseAvailability() = %+v, %v, want %+v", got, err, test.want)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) || got != (ReleaseAvailability{}) {
					t.Fatalf("InspectReleaseAvailability() = %+v, %v, want error containing %q", got, err, test.wantErr)
				}
				var remote *RemoteError
				if test.plainError && errors.As(err, &remote) {
					t.Fatalf("error = %v, want a plain error without RemoteError", err)
				}
				if !test.plainError && (!errors.As(err, &remote) || remote.StatusCode != test.wantStatus) {
					t.Fatalf("error = %v, want RemoteError with status %d", err, test.wantStatus)
				}
			}
			want := make([]string, 0, len(test.wantRequests))
			for _, path := range test.wantRequests {
				want = append(want, "GET https://api.github.com"+path)
			}
			if strings.Join(transport.requests, "\n") != strings.Join(want, "\n") {
				t.Fatalf("requests = %v, want %v", transport.requests, want)
			}
			if transport.authValue != "Bearer fixture-token" {
				t.Fatalf("Authorization = %q, want the discovered token on every probe", transport.authValue)
			}
		})
	}
}

func TestRepositoryReadableReportsOnlyAShownRepository(t *testing.T) {
	t.Parallel()

	const repository = "/repos/owner/plugin"
	tests := []struct {
		name    string
		status  int
		body    string
		want    bool
		wantErr string
	}{
		{name: "shown", status: 200, body: `{"id":7,"full_name":"owner/plugin"}`, want: true},
		{name: "missing or private", status: 404},
		{name: "invalid resource", status: 200, body: `{"id":0}`, wantErr: "invalid repository"},
		{name: "null resource", status: 200, body: `null`, wantErr: "invalid repository"},
		{name: "forbidden", status: 403, wantErr: "GitHub access denied"},
		{name: "server error", status: 500, wantErr: "retry the request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transport := &availabilityTransport{statuses: map[string]int{repository: test.status}, bodies: map[string]string{repository: test.body}}
			client := NewGitHubClient(WithHTTPClient(&http.Client{Transport: transport}))
			client.tokenOnce.Do(func() { client.token = "fixture-token" })

			got, err := client.RepositoryReadable(context.Background(), Repository{Owner: "owner", Name: "plugin"})
			if got != test.want || (test.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("RepositoryReadable() = %v, %v, want %v with error %q", got, err, test.want, test.wantErr)
			}
			if len(transport.requests) != 1 || transport.requests[0] != "GET https://api.github.com"+repository {
				t.Fatalf("requests = %v, want one repository request", transport.requests)
			}
		})
	}
}
