package migrateapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

// releaselessGitHub is the production GitHub client's transport for these
// tests: it answers api.github.com by path from a fixed table and records
// every request with its Authorization header, so a test asserts the exact
// probe sequence the shipped client sends and that it stays authenticated.
type releaselessGitHub struct {
	statuses map[string]int
	bodies   map[string]string
	failPath string
	requests []string
	auth     map[string]bool
}

func (transport *releaselessGitHub) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.requests = append(transport.requests, request.Method+" "+request.URL.Scheme+"://"+request.URL.Host+request.URL.Path)
	if transport.auth == nil {
		transport.auth = map[string]bool{}
	}
	transport.auth[request.Header.Get("Authorization")] = true
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

// productionMigrateApplication wires the shipped GitHub client, with only its
// transport replaced, into the shipped application. Credential discovery
// reads the environment first, so the fixture token keeps the run off the
// gh and git credential helpers.
func productionMigrateApplication(t *testing.T, transport *releaselessGitHub) *Application {
	t.Helper()
	t.Setenv("GH_TOKEN", "fixture-token")
	return NewApplication(dependency.NewGitHubClient(dependency.WithHTTPClient(&http.Client{Transport: transport})), "test")
}

// seedMappedConsumer is seedConsumer with example/alpha installed at the
// given Tessl version, which is what decides whether a tagless --map takes
// the pinned or the latest resolution path.
func seedMappedConsumer(t *testing.T, tesslVersion string) string {
	t.Helper()
	root := seedConsumer(t)
	writeJSON(t, root, "tessl.json", map[string]any{
		"name": "consumer", "mode": "vendored",
		"dependencies": map[string]any{"example/alpha": map[string]string{"version": tesslVersion}},
	})
	return root
}

type migrateFailureEnvelope struct {
	OK    bool `json:"ok"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Remedy  string `json:"remedy"`
	} `json:"error"`
}

const (
	alphaPlainTag = "/repos/example/alpha/releases/tags/1.0.0"
	alphaVTag     = "/repos/example/alpha/releases/tags/v1.0.0"
	alphaLatest   = "/repos/example/alpha/releases/latest"
	alphaRepo     = "/repos/example/alpha"
	alphaReadable = `{"id":7,"full_name":"example/alpha"}`
	alphaStable   = `{"id":42,"tag_name":"v2.0.0","draft":false,"prerelease":false}`
	notPublished  = "has no published stable release"
	inspectFailed = "release availability could not be inspected"
)

func TestMigrateClassifiesAReleaselessMappedSource(t *testing.T) {
	tests := []struct {
		name         string
		version      string
		mapping      string
		statuses     map[string]int
		bodies       map[string]string
		failPath     string
		wantCode     string
		wantText     []string
		wantNot      []string
		wantRequests []string
	}{
		{name: "pinned tagless mapping to a readable repository with no stable release", version: "1.0.0",
			statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 404, alphaRepo: 200}, bodies: map[string]string{alphaRepo: alphaReadable},
			wantCode: cli.CodeSourceNotPublished, wantText: []string{notPublished, "Tessl version 1.0.0 (tags 1.0.0 and v1.0.0) cannot resolve"}, wantNot: []string{"gh auth login"},
			wantRequests: []string{alphaPlainTag, alphaVTag, alphaLatest, alphaRepo}},
		{name: "pinned tagless mapping when another stable release exists", version: "1.0.0",
			statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 200}, bodies: map[string]string{alphaLatest: alphaStable},
			wantCode: cli.CodeTesslVersionUnavailable, wantText: []string{"neither 1.0.0 nor v1.0.0 is a release tag"}, wantNot: []string{notPublished, inspectFailed},
			wantRequests: []string{alphaPlainTag, alphaVTag, alphaLatest}},
		{name: "pinned tagless mapping with ambiguous tags", version: "1.0.0",
			statuses: map[string]int{alphaPlainTag: 200, alphaVTag: 200}, bodies: map[string]string{alphaPlainTag: `{"id":1,"tag_name":"1.0.0"}`, alphaVTag: `{"id":2,"tag_name":"v1.0.0"}`},
			wantCode: cli.CodeAmbiguousTesslVersion, wantRequests: []string{alphaPlainTag, alphaVTag}},
		{name: "pinned tagless mapping to a missing or private repository", version: "1.0.0",
			statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 404, alphaRepo: 404},
			wantCode: cli.CodeTesslVersionUnavailable, wantNot: []string{notPublished, inspectFailed}, wantRequests: []string{alphaPlainTag, alphaVTag, alphaLatest, alphaRepo}},
		{name: "pinned tagless mapping whose probe is rate limited", version: "1.0.0",
			statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 403},
			wantCode: cli.CodeTesslVersionUnavailable, wantText: []string{inspectFailed, "GitHub access denied"}, wantNot: []string{notPublished},
			wantRequests: []string{alphaPlainTag, alphaVTag, alphaLatest}},
		{name: "latest mapping to a readable repository with no stable release", version: "latest",
			statuses: map[string]int{alphaLatest: 404, alphaRepo: 200}, bodies: map[string]string{alphaRepo: alphaReadable},
			wantCode: cli.CodeSourceNotPublished, wantText: []string{"github:example/alpha " + notPublished}, wantNot: []string{"gh auth login", "requested tag", "cannot resolve"},
			wantRequests: []string{alphaLatest, alphaRepo}},
		{name: "latest mapping to a missing or private repository", version: "latest",
			statuses: map[string]int{alphaLatest: 404, alphaRepo: 404},
			wantCode: "migrate_failed", wantText: []string{"gh auth login"}, wantNot: []string{notPublished, inspectFailed}, wantRequests: []string{alphaLatest, alphaRepo}},
		{name: "latest mapping refused authentication", version: "latest", statuses: map[string]int{alphaLatest: 401},
			wantCode: "migrate_failed", wantText: []string{"gh auth login"}, wantNot: []string{notPublished}, wantRequests: []string{alphaLatest}},
		{name: "latest mapping rate limited", version: "latest", statuses: map[string]int{alphaLatest: 403},
			wantCode: "migrate_failed", wantText: []string{"GitHub access denied"}, wantNot: []string{notPublished}, wantRequests: []string{alphaLatest}},
		{name: "latest mapping hitting a server error", version: "latest", statuses: map[string]int{alphaLatest: 502},
			wantCode: "migrate_failed", wantText: []string{"retry the request"}, wantNot: []string{notPublished}, wantRequests: []string{alphaLatest}},
		{name: "latest mapping without network", version: "latest", failPath: alphaLatest,
			wantCode: "migrate_failed", wantText: []string{"check network access"}, wantNot: []string{notPublished}, wantRequests: []string{alphaLatest}},
		{name: "latest mapping whose repository probe fails", version: "latest", statuses: map[string]int{alphaLatest: 404, alphaRepo: 500},
			wantCode: "migrate_failed", wantText: []string{"gh auth login", inspectFailed}, wantNot: []string{notPublished}, wantRequests: []string{alphaLatest, alphaRepo}},
		{name: "explicit tag on a readable repository with no stable release", version: "1.0.0", mapping: "example/alpha=github:example/alpha@v1.0.0",
			statuses: map[string]int{alphaVTag: 404, alphaLatest: 404, alphaRepo: 200}, bodies: map[string]string{alphaRepo: alphaReadable},
			wantCode: cli.CodeSourceNotPublished, wantText: []string{notPublished, `requested tag "v1.0.0" cannot resolve`}, wantNot: []string{"gh auth login"},
			wantRequests: []string{alphaVTag, alphaLatest, alphaRepo}},
		{name: "explicit tag when another stable release exists", version: "1.0.0", mapping: "example/alpha=github:example/alpha@v1.0.0",
			statuses: map[string]int{alphaVTag: 404, alphaLatest: 200}, bodies: map[string]string{alphaLatest: alphaStable},
			wantCode: "migrate_failed", wantText: []string{`release tag "v1.0.0"`}, wantNot: []string{notPublished}, wantRequests: []string{alphaVTag, alphaLatest}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mapping := test.mapping
			if mapping == "" {
				mapping = "example/alpha=github:example/alpha"
			}
			transport := &releaselessGitHub{statuses: test.statuses, bodies: test.bodies, failPath: test.failPath}
			application := productionMigrateApplication(t, transport)
			root := seedMappedConsumer(t, test.version)
			before := hashTree(t, root)

			stdout, stderr, exitCode := runCLI(t, application, "migrate", "tessl", "--dry-run", "--json", "--project", root, "--map", mapping)
			if exitCode != cli.ExitOperational || stdout != "" {
				t.Fatalf("exit = %d stdout = %q stderr = %q, want exit %d and no stdout", exitCode, stdout, stderr, cli.ExitOperational)
			}
			var envelope migrateFailureEnvelope
			if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
				t.Fatalf("stderr %q is not one JSON envelope: %v", stderr, err)
			}
			if envelope.OK || envelope.Error.Code != test.wantCode {
				t.Fatalf("envelope = %+v, want code %s", envelope, test.wantCode)
			}
			for _, want := range test.wantText {
				if !strings.Contains(envelope.Error.Message, want) {
					t.Errorf("message %q does not contain %q", envelope.Error.Message, want)
				}
			}
			for _, unwanted := range test.wantNot {
				if strings.Contains(envelope.Error.Message, unwanted) {
					t.Errorf("message %q must not contain %q", envelope.Error.Message, unwanted)
				}
			}
			if wantRemedy := test.wantCode == cli.CodeSourceNotPublished; wantRemedy != strings.Contains(envelope.Error.Remedy, "acr publish") || wantRemedy != strings.Contains(envelope.Error.Remedy, "acr migrate tessl-plugin") {
				t.Errorf("remedy = %q, want a producer conversion and publish remedy exactly for %s", envelope.Error.Remedy, cli.CodeSourceNotPublished)
			}
			want := make([]string, 0, len(test.wantRequests))
			for _, path := range test.wantRequests {
				want = append(want, "GET https://api.github.com"+path)
			}
			if strings.Join(transport.requests, "\n") != strings.Join(want, "\n") {
				t.Errorf("requests = %v, want %v", transport.requests, want)
			}
			if len(transport.auth) != 1 || !transport.auth["Bearer fixture-token"] {
				t.Errorf("Authorization headers = %v, want only the discovered token", transport.auth)
			}
			if after := hashTree(t, root); !mapsEqual(before, after) {
				t.Fatalf("classification mutated the project\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

// The text diagnostic carries the remedy in its one line, since text mode
// prints no separate remedy field, and an apply run refuses before any write.
func TestMigrateReleaselessSourceTextDiagnosticAndApplyWriteNothing(t *testing.T) {
	for _, test := range []struct {
		name    string
		version string
		dryRun  bool
		detail  string
	}{
		{name: "latest dry run", version: "latest", dryRun: true},
		{name: "pinned apply", version: "1.0.0", detail: "Tessl version 1.0.0 (tags 1.0.0 and v1.0.0) cannot resolve"},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := &releaselessGitHub{
				statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 404, alphaRepo: 200},
				bodies:   map[string]string{alphaRepo: alphaReadable},
			}
			application := productionMigrateApplication(t, transport)
			root := seedMappedConsumer(t, test.version)
			before := hashTree(t, root)
			args := []string{"migrate", "tessl", "--project", root, "--map", "example/alpha=github:example/alpha"}
			if test.dryRun {
				args = append(args, "--dry-run")
			}

			stdout, stderr, exitCode := runCLI(t, application, args...)
			if exitCode != cli.ExitOperational || stdout != "" {
				t.Fatalf("exit = %d stdout = %q stderr = %q", exitCode, stdout, stderr)
			}
			prefix := "acr migrate: github:example/alpha " + notPublished
			if !strings.HasPrefix(stderr, prefix) || strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, test.detail) {
				t.Fatalf("stderr = %q, want one line starting %q naming %q", stderr, prefix, test.detail)
			}
			for _, want := range []string{"acr migrate tessl-plugin", "acr publish", "producer stage 0"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q does not carry the remedy %q", stderr, want)
				}
			}
			if strings.Contains(stderr, "gh auth login") {
				t.Errorf("stderr %q recommends authentication for a readable repository", stderr)
			}
			if after := hashTree(t, root); !mapsEqual(before, after) {
				t.Fatalf("refusal mutated the project\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

// A 404 without the production client's evidence claims nothing: a GitHub
// double that cannot be asked keeps the classification every caller had.
func TestUnpublishedSourceNeedsProductionEvidence(t *testing.T) {
	t.Parallel()
	service := newService(&migrationGitHub{})
	notFound := &dependency.RemoteError{StatusCode: 404, Err: errors.New("not found")}
	if unpublished, err := service.unpublishedSource(context.Background(), "github:example/pkg", "latest", notFound); unpublished || err != nil {
		t.Fatalf("unpublishedSource() = %v, %v, want false without a probe", unpublished, err)
	}
	var migrationErr *Error
	if err := service.classifyCandidateError(context.Background(), "github:example/pkg", "latest", notFound); errors.As(err, &migrationErr) || !errors.Is(err, notFound) {
		t.Fatalf("classifyCandidateError() = %#v, want the original 404 unchanged", err)
	}
}

// A 200 whose body is not a release is an inspection failure, never evidence:
// the explicit-tag and pinned refusals keep their codes, say why the evidence
// could not be read, carry no producer remedy, and never ask for the
// repository.
func TestMigrateInvalidLatestReleaseIsNeverPublicationEvidence(t *testing.T) {
	forms := []struct {
		name, mapping, wantCode string
		statuses                map[string]int
		wantRequests            []string
	}{
		{name: "explicit tag", mapping: "example/alpha=github:example/alpha@v1.0.0", wantCode: "migrate_failed",
			statuses: map[string]int{alphaVTag: 404, alphaLatest: 200}, wantRequests: []string{alphaVTag, alphaLatest}},
		{name: "pinned tagless", mapping: "example/alpha=github:example/alpha", wantCode: cli.CodeTesslVersionUnavailable,
			statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 200}, wantRequests: []string{alphaPlainTag, alphaVTag, alphaLatest}},
	}
	for _, body := range []string{`null`, `{}`, `{"id":0,"tag_name":"v2.0.0"}`, `{"id":42}`} {
		for _, form := range forms {
			t.Run(form.name+" "+body, func(t *testing.T) {
				transport := &releaselessGitHub{statuses: form.statuses, bodies: map[string]string{alphaLatest: body}}
				application := productionMigrateApplication(t, transport)
				root := seedMappedConsumer(t, "1.0.0")
				before := hashTree(t, root)

				stdout, stderr, exitCode := runCLI(t, application, "migrate", "tessl", "--dry-run", "--json", "--project", root, "--map", form.mapping)
				var envelope migrateFailureEnvelope
				if err := json.Unmarshal([]byte(stderr), &envelope); err != nil || exitCode != cli.ExitOperational || stdout != "" {
					t.Fatalf("exit = %d stdout = %q stderr = %q (%v)", exitCode, stdout, stderr, err)
				}
				if envelope.Error.Code != form.wantCode || !strings.Contains(envelope.Error.Message, inspectFailed) || !strings.Contains(envelope.Error.Message, "invalid latest release") || strings.Contains(envelope.Error.Message, notPublished) || envelope.Error.Remedy != "" {
					t.Fatalf("envelope = %+v, want %s naming the inspection failure and no producer claim", envelope, form.wantCode)
				}
				want := make([]string, 0, len(form.wantRequests))
				for _, path := range form.wantRequests {
					want = append(want, "GET https://api.github.com"+path)
				}
				if strings.Join(transport.requests, "\n") != strings.Join(want, "\n") {
					t.Errorf("requests = %v, want %v with the repository never asked", transport.requests, want)
				}
				if after := hashTree(t, root); !mapsEqual(before, after) {
					t.Fatalf("refusal mutated the project\nbefore=%v\nafter=%v", before, after)
				}
			})
		}
	}
}
