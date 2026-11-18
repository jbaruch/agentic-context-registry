package migrateapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
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
	const retry = "; retry the command, then report the failure at https://github.com/jbaruch/agentic-context-registry/issues if it persists"
	const selection = "select an available release explicitly with --map example/alpha=github:example/alpha@TAG"
	const authentication = "verify the repository source; if it is private, authenticate with gh auth login, then retry acr migrate tessl with the same options"
	const publication = "complete producer stage 0: run 'acr migrate tessl-plugin' in the producer repository, publish a stable release with 'acr publish', then re-run 'acr migrate tessl'"
	const unpublished = "github:example/alpha has no published stable release: the repository is readable with the credentials in use, but GitHub reports no non-draft, non-prerelease release"
	const missingTags = "neither 1.0.0 nor v1.0.0 is a release tag for github:example/alpha"
	const missingLatest = "resolve latest release for github:example/alpha: requested GitHub resource was not found or is inaccessible; verify the source or run 'gh auth login' for private repositories"
	const missingExplicit = "resolve release tag \"v1.0.0\" for github:example/alpha: requested GitHub resource was not found or is inaccessible; verify the source or run 'gh auth login' for private repositories"
	const denied = "GitHub access denied: fixture; run 'gh auth login' or configure Git HTTPS credentials and retry"
	const server = "GitHub API returned Bad Gateway: fixture; retry the request"
	const network = "request GitHub API: Get \"https://api.github.com/repos/example/alpha/releases/latest\": dial tcp: connection refused; check network access and retry"
	tests := []struct {
		name, version, mapping            string
		statuses                          map[string]int
		bodies                            map[string]string
		failPath                          string
		wantCode, wantMessage, wantRemedy string
		wantRequests                      []string
	}{
		{name: "pinned unpublished", version: "1.0.0", mapping: "",
			statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 404, alphaRepo: 200}, bodies: map[string]string{alphaRepo: alphaReadable},
			wantCode: cli.CodeSourceNotPublished, wantMessage: unpublished + ", so Tessl version 1.0.0 (tags 1.0.0 and v1.0.0) cannot resolve; " + publication, wantRemedy: publication,
			wantRequests: []string{alphaPlainTag, alphaVTag, alphaLatest, alphaRepo}},
		{name: "pinned other stable", version: "1.0.0", mapping: "",
			statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 200}, bodies: map[string]string{alphaLatest: alphaStable},
			wantCode: cli.CodeTesslVersionUnavailable, wantMessage: missingTags + "; " + selection, wantRemedy: selection,
			wantRequests: []string{alphaPlainTag, alphaVTag, alphaLatest}},
		{name: "pinned both tags", version: "1.0.0", mapping: "",
			statuses: map[string]int{alphaPlainTag: 200, alphaVTag: 200}, bodies: map[string]string{alphaPlainTag: `{"id":1,"tag_name":"1.0.0"}`, alphaVTag: `{"id":2,"tag_name":"v1.0.0"}`},
			wantCode: cli.CodeAmbiguousTesslVersion, wantMessage: "both 1.0.0 and v1.0.0 are release tags for github:example/alpha; " + selection, wantRemedy: selection,
			wantRequests: []string{alphaPlainTag, alphaVTag}},
		{name: "pinned missing or private", version: "1.0.0", mapping: "",
			statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 404, alphaRepo: 404},
			wantCode: cli.CodeTesslVersionUnavailable, wantMessage: missingTags + "; " + authentication, wantRemedy: authentication,
			wantRequests: []string{alphaPlainTag, alphaVTag, alphaLatest, alphaRepo}},
		{name: "latest unpublished", version: "latest", mapping: "",
			statuses: map[string]int{alphaLatest: 404, alphaRepo: 200}, bodies: map[string]string{alphaRepo: alphaReadable},
			wantCode: cli.CodeSourceNotPublished, wantMessage: unpublished + "; " + publication, wantRemedy: publication,
			wantRequests: []string{alphaLatest, alphaRepo}},
		{name: "latest missing or private", version: "latest", mapping: "",
			statuses: map[string]int{alphaLatest: 404, alphaRepo: 404},
			wantCode: "migrate_failed", wantMessage: missingLatest + "; " + authentication, wantRemedy: authentication,
			wantRequests: []string{alphaLatest, alphaRepo}},
		{name: "explicit unpublished", version: "1.0.0", mapping: "example/alpha=github:example/alpha@v1.0.0",
			statuses: map[string]int{alphaVTag: 404, alphaLatest: 404, alphaRepo: 200}, bodies: map[string]string{alphaRepo: alphaReadable},
			wantCode: cli.CodeSourceNotPublished, wantMessage: unpublished + ", so requested tag \"v1.0.0\" cannot resolve; " + publication, wantRemedy: publication,
			wantRequests: []string{alphaVTag, alphaLatest, alphaRepo}},
		{name: "explicit other stable", version: "1.0.0", mapping: "example/alpha=github:example/alpha@v1.0.0",
			statuses: map[string]int{alphaVTag: 404, alphaLatest: 200}, bodies: map[string]string{alphaLatest: alphaStable},
			wantCode: "migrate_failed", wantMessage: missingExplicit + "; " + selection, wantRemedy: selection,
			wantRequests: []string{alphaVTag, alphaLatest}},
		{name: "explicit missing or private", version: "1.0.0", mapping: "example/alpha=github:example/alpha@v1.0.0",
			statuses: map[string]int{alphaVTag: 404, alphaLatest: 404, alphaRepo: 404},
			wantCode: "migrate_failed", wantMessage: missingExplicit + "; " + authentication, wantRemedy: authentication,
			wantRequests: []string{alphaVTag, alphaLatest, alphaRepo}},
		{name: "pinned inspection 403", version: "1.0.0", mapping: "",
			statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 403},
			wantCode: cli.CodeTesslVersionUnavailable, wantMessage: missingTags + "; release availability could not be inspected: inspect releases for github:example/alpha: " + denied + retry, wantRemedy: "",
			wantRequests: []string{alphaPlainTag, alphaVTag, alphaLatest}},
		{name: "pinned inspection 502", version: "1.0.0", mapping: "",
			statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 502},
			wantCode: cli.CodeTesslVersionUnavailable, wantMessage: missingTags + "; release availability could not be inspected: inspect releases for github:example/alpha: " + server + retry, wantRemedy: "",
			wantRequests: []string{alphaPlainTag, alphaVTag, alphaLatest}},
		{name: "pinned inspection network", version: "1.0.0", mapping: "",
			statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404}, failPath: alphaLatest,
			wantCode: cli.CodeTesslVersionUnavailable, wantMessage: missingTags + "; release availability could not be inspected: inspect releases for github:example/alpha: " + network + retry, wantRemedy: "",
			wantRequests: []string{alphaPlainTag, alphaVTag, alphaLatest}},
		{name: "pinned repository inspection 403", version: "1.0.0", mapping: "",
			statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 404, alphaRepo: 403},
			wantCode: cli.CodeTesslVersionUnavailable, wantMessage: missingTags + "; release availability could not be inspected: inspect repository github:example/alpha: " + denied + retry, wantRemedy: "",
			wantRequests: []string{alphaPlainTag, alphaVTag, alphaLatest, alphaRepo}},
		{name: "pinned repository inspection 500", version: "1.0.0", mapping: "",
			statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 404, alphaRepo: 500},
			wantCode: cli.CodeTesslVersionUnavailable, wantMessage: missingTags + "; release availability could not be inspected: inspect repository github:example/alpha: " + "GitHub API returned Internal Server Error: fixture; retry the request" + retry, wantRemedy: "",
			wantRequests: []string{alphaPlainTag, alphaVTag, alphaLatest, alphaRepo}},
		{name: "explicit inspection 403", version: "1.0.0", mapping: "example/alpha=github:example/alpha@v1.0.0",
			statuses: map[string]int{alphaVTag: 404, alphaLatest: 403},
			wantCode: "migrate_failed", wantMessage: missingExplicit + "; release availability could not be inspected: inspect releases for github:example/alpha: " + denied + retry, wantRemedy: "",
			wantRequests: []string{alphaVTag, alphaLatest}},
		{name: "explicit inspection 502", version: "1.0.0", mapping: "example/alpha=github:example/alpha@v1.0.0",
			statuses: map[string]int{alphaVTag: 404, alphaLatest: 502},
			wantCode: "migrate_failed", wantMessage: missingExplicit + "; release availability could not be inspected: inspect releases for github:example/alpha: " + server + retry, wantRemedy: "",
			wantRequests: []string{alphaVTag, alphaLatest}},
		{name: "explicit inspection network", version: "1.0.0", mapping: "example/alpha=github:example/alpha@v1.0.0",
			statuses: map[string]int{alphaVTag: 404}, failPath: alphaLatest,
			wantCode: "migrate_failed", wantMessage: missingExplicit + "; release availability could not be inspected: inspect releases for github:example/alpha: " + network + retry, wantRemedy: "",
			wantRequests: []string{alphaVTag, alphaLatest}},
		{name: "explicit repository inspection 403", version: "1.0.0", mapping: "example/alpha=github:example/alpha@v1.0.0",
			statuses: map[string]int{alphaVTag: 404, alphaLatest: 404, alphaRepo: 403},
			wantCode: "migrate_failed", wantMessage: missingExplicit + "; release availability could not be inspected: inspect repository github:example/alpha: " + denied + retry, wantRemedy: "",
			wantRequests: []string{alphaVTag, alphaLatest, alphaRepo}},
		{name: "explicit repository inspection 500", version: "1.0.0", mapping: "example/alpha=github:example/alpha@v1.0.0",
			statuses: map[string]int{alphaVTag: 404, alphaLatest: 404, alphaRepo: 500},
			wantCode: "migrate_failed", wantMessage: missingExplicit + "; release availability could not be inspected: inspect repository github:example/alpha: " + "GitHub API returned Internal Server Error: fixture; retry the request" + retry, wantRemedy: "",
			wantRequests: []string{alphaVTag, alphaLatest, alphaRepo}},
		{name: "latest repository inspection 403", version: "latest", mapping: "",
			statuses: map[string]int{alphaLatest: 404, alphaRepo: 403},
			wantCode: "migrate_failed", wantMessage: missingLatest + "; release availability could not be inspected: inspect repository github:example/alpha: " + denied + retry, wantRemedy: "",
			wantRequests: []string{alphaLatest, alphaRepo}},
		{name: "latest repository inspection 500", version: "latest", mapping: "",
			statuses: map[string]int{alphaLatest: 404, alphaRepo: 500},
			wantCode: "migrate_failed", wantMessage: missingLatest + "; release availability could not be inspected: inspect repository github:example/alpha: " + "GitHub API returned Internal Server Error: fixture; retry the request" + retry, wantRemedy: "",
			wantRequests: []string{alphaLatest, alphaRepo}},
		{name: "latest lookup 401", version: "latest", mapping: "",
			statuses: map[string]int{alphaLatest: 401},
			wantCode: "migrate_failed", wantMessage: "resolve latest release for github:example/alpha: " + denied + retry, wantRemedy: "",
			wantRequests: []string{alphaLatest}},
		{name: "latest lookup 403", version: "latest", mapping: "",
			statuses: map[string]int{alphaLatest: 403},
			wantCode: "migrate_failed", wantMessage: "resolve latest release for github:example/alpha: " + denied + retry, wantRemedy: "",
			wantRequests: []string{alphaLatest}},
		{name: "latest lookup 502", version: "latest", mapping: "",
			statuses: map[string]int{alphaLatest: 502},
			wantCode: "migrate_failed", wantMessage: "resolve latest release for github:example/alpha: " + server + retry, wantRemedy: "",
			wantRequests: []string{alphaLatest}},
		{name: "latest lookup network", version: "latest", mapping: "",
			statuses: map[string]int{}, failPath: alphaLatest,
			wantCode: "migrate_failed", wantMessage: "resolve latest release for github:example/alpha: " + network + retry, wantRemedy: "",
			wantRequests: []string{alphaLatest}},
		{name: "pinned invalid latest", version: "1.0.0", statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 200}, bodies: map[string]string{alphaLatest: `null`},
			wantCode: cli.CodeTesslVersionUnavailable, wantMessage: missingTags + "; release availability could not be inspected: GitHub returned an invalid latest release for github:example/alpha; retry or report the repository response" + retry,
			wantRequests: []string{alphaPlainTag, alphaVTag, alphaLatest}},
		{name: "explicit invalid latest", version: "1.0.0", mapping: "example/alpha=github:example/alpha@v1.0.0", statuses: map[string]int{alphaVTag: 404, alphaLatest: 200}, bodies: map[string]string{alphaLatest: `{}`},
			wantCode: "migrate_failed", wantMessage: missingExplicit + "; release availability could not be inspected: GitHub returned an invalid latest release for github:example/alpha; retry or report the repository response" + retry,
			wantRequests: []string{alphaVTag, alphaLatest}},
		{name: "latest invalid response", version: "latest", statuses: map[string]int{alphaLatest: 200}, bodies: map[string]string{alphaLatest: `{"id":0,"tag_name":"v2.0.0"}`},
			wantCode: "migrate_failed", wantMessage: "GitHub returned a non-stable latest release for github:example/alpha; publish a non-draft, non-prerelease release and retry" + retry,
			wantRequests: []string{alphaLatest}},
		{name: "pinned invalid latest tag", version: "1.0.0", statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 200}, bodies: map[string]string{alphaLatest: `{"id":42}`},
			wantCode: cli.CodeTesslVersionUnavailable, wantMessage: missingTags + "; release availability could not be inspected: GitHub returned an invalid latest release for github:example/alpha; retry or report the repository response" + retry,
			wantRequests: []string{alphaPlainTag, alphaVTag, alphaLatest}},
		{name: "explicit mapped upstream stable", version: "1.0.0", mapping: "example/alpha=github:upstream/orbit@v1.0.0",
			statuses: map[string]int{"/repos/upstream/orbit/releases/tags/v1.0.0": 404, "/repos/upstream/orbit/releases/latest": 200}, bodies: map[string]string{"/repos/upstream/orbit/releases/latest": alphaStable},
			wantCode: "migrate_failed", wantMessage: "resolve release tag \"v1.0.0\" for github:upstream/orbit: requested GitHub resource was not found or is inaccessible; verify the source or run 'gh auth login' for private repositories; select an available release explicitly with --map example/alpha=github:upstream/orbit@TAG",
			wantRemedy: "select an available release explicitly with --map example/alpha=github:upstream/orbit@TAG", wantRequests: []string{"/repos/upstream/orbit/releases/tags/v1.0.0", "/repos/upstream/orbit/releases/latest"}},
	}
	for _, test := range tests {
		for _, dryRun := range []bool{true, false} {
			for _, jsonOutput := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s dry=%t json=%t", test.name, dryRun, jsonOutput), func(t *testing.T) {
					mapping := test.mapping
					if mapping == "" {
						mapping = "example/alpha=github:example/alpha"
					}
					transport := &releaselessGitHub{statuses: test.statuses, bodies: test.bodies, failPath: test.failPath}
					application := productionMigrateApplication(t, transport)
					root := seedMappedConsumer(t, test.version)
					before := hashTreeWithModes(t, root)
					args := []string{"migrate", "tessl", "--project", root, "--map", mapping}
					if dryRun {
						args = append(args, "--dry-run")
					}
					want := "acr migrate: " + test.wantMessage + "\n"
					if jsonOutput {
						args = append(args, "--json")
						remedy := ""
						if test.wantRemedy != "" {
							remedy = fmt.Sprintf(",\"remedy\":%q", test.wantRemedy)
						}
						want = fmt.Sprintf("{\"ok\":false,\"command\":\"migrate\",\"error\":{\"code\":%q,\"message\":%q%s}}\n", test.wantCode, test.wantMessage, remedy)
					}
					stdout, stderr, exit := runCLI(t, application, args...)
					if exit != cli.ExitOperational || stdout != "" || stderr != want {
						t.Fatalf("exit=%d stdout=%q\nstderr=%q\nwant=%q", exit, stdout, stderr, want)
					}
					wantRequests := make([]string, 0, len(test.wantRequests))
					for _, path := range test.wantRequests {
						wantRequests = append(wantRequests, "GET https://api.github.com"+path)
					}
					if strings.Join(transport.requests, "\n") != strings.Join(wantRequests, "\n") {
						t.Errorf("requests=%v want=%v", transport.requests, wantRequests)
					}
					if len(transport.auth) != 1 || !transport.auth["Bearer fixture-token"] {
						t.Errorf("Authorization headers=%v", transport.auth)
					}
					if !mapsEqual(before, hashTreeWithModes(t, root)) {
						t.Fatal("refusal changed project paths, bytes or modes")
					}
				})
			}
		}
	}
}

// Clients without inspection capability must not invent availability evidence.
func TestReleaseFailureWithoutProber(t *testing.T) {
	for _, test := range []struct {
		name, version, mapping, code, diagnostic string
		requests                                 []string
	}{
		{name: "pinned", version: "1.0.0", mapping: "example/alpha=github:example/alpha", code: "tessl_version_unavailable", diagnostic: "neither 1.0.0 nor v1.0.0 is a release tag for github:example/alpha", requests: []string{alphaPlainTag, alphaVTag}},
		{name: "explicit", version: "1.0.0", mapping: "example/alpha=github:example/alpha@v1.0.0", code: "migrate_failed", diagnostic: "resolve release tag \"v1.0.0\" for github:example/alpha: requested GitHub resource was not found or is inaccessible; verify the source or run 'gh auth login' for private repositories", requests: []string{alphaVTag}},
		{name: "latest", version: "latest", mapping: "example/alpha=github:example/alpha", code: "migrate_failed", diagnostic: "resolve latest release for github:example/alpha: requested GitHub resource was not found or is inaccessible; verify the source or run 'gh auth login' for private repositories", requests: []string{alphaLatest}},
	} {
		for _, dryRun := range []bool{false, true} {
			for _, jsonOutput := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s dry=%t json=%t", test.name, dryRun, jsonOutput), func(t *testing.T) {
					root := seedMappedConsumer(t, test.version)
					before := hashTreeWithModes(t, root)
					message := test.diagnostic + "; retry the command, then report the failure at https://github.com/jbaruch/agentic-context-registry/issues if it persists"
					args := []string{"migrate", "tessl", "--project", root, "--map", test.mapping}
					if dryRun {
						args = append(args, "--dry-run")
					}
					want := "acr migrate: " + message + "\n"
					if jsonOutput {
						args = append(args, "--json")
						want = fmt.Sprintf("{\"ok\":false,\"command\":\"migrate\",\"error\":{\"code\":%q,\"message\":%q}}\n", test.code, message)
					}
					transport := &releaselessGitHub{statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404, alphaLatest: 404}}
					app := productionMigrateApplication(t, transport)
					// Keep the real client's lookups, exposing only the required API.
					app.service = newService(struct{ dependency.GitHub }{app.service.github})
					stdout, stderr, exit := runCLI(t, app, args...)
					if exit != cli.ExitOperational || stdout != "" || stderr != want {
						t.Fatalf("exit=%d stdout=%q stderr=%q want=%q", exit, stdout, stderr, want)
					}
					var wantRequests []string
					for _, path := range test.requests {
						wantRequests = append(wantRequests, "GET https://api.github.com"+path)
					}
					if strings.Join(transport.requests, "\n") != strings.Join(wantRequests, "\n") {
						t.Fatalf("requests=%v want=%v", transport.requests, wantRequests)
					}
					if !mapsEqual(before, hashTreeWithModes(t, root)) {
						t.Fatal("refusal changed project")
					}
				})
			}
		}
	}
}

// An apply run claims the transaction lock before it reads anything, and the
// claim used to leave `.agents/.acr-transactions/.lock` behind whenever
// `.agents` already existed. A refusal has to leave every path, mode and byte
// as it found them: parent present or absent, first run and repeat, text and
// JSON, dry-run and apply.
func TestMigrateRefusalLeavesEveryPathModeAndByteUnchanged(t *testing.T) {
	parents := []struct {
		name string
		seed func(*testing.T, string)
	}{
		{name: "absent .agents", seed: func(*testing.T, string) {}},
		{name: "existing .agents", seed: func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, ".agents"), 0o750); err != nil {
				t.Fatal(err)
			}
			writeFile(t, root, ".agents/keep", []byte("operator file\n"), 0o640)
		}},
	}
	for _, parent := range parents {
		for _, dryRun := range []bool{true, false} {
			for _, jsonOutput := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s dry-run=%t json=%t", parent.name, dryRun, jsonOutput), func(t *testing.T) {
					transport := &releaselessGitHub{statuses: map[string]int{alphaLatest: 404, alphaRepo: 200}, bodies: map[string]string{alphaRepo: alphaReadable}}
					application := productionMigrateApplication(t, transport)
					root := seedMappedConsumer(t, "latest")
					parent.seed(t, root)
					args := []string{"migrate", "tessl", "--project", root, "--map", "example/alpha=github:example/alpha"}
					if dryRun {
						args = append(args, "--dry-run")
					}
					if jsonOutput {
						args = append(args, "--json")
					}
					before := hashTreeWithModes(t, root)
					for _, run := range []string{"first", "repeated"} {
						stdout, stderr, exitCode := runCLI(t, application, args...)
						if exitCode != cli.ExitOperational || stdout != "" || !strings.Contains(stderr, notPublished) {
							t.Fatalf("%s run: exit = %d stdout = %q stderr = %q", run, exitCode, stdout, stderr)
						}
						if after := hashTreeWithModes(t, root); !mapsEqual(before, after) {
							t.Fatalf("%s run changed the project\nbefore=%v\nafter=%v", run, before, after)
						}
					}
				})
			}
		}
	}
}

func TestPinnedMappingUsesTheOnlyAvailableTag(t *testing.T) {
	for _, tag := range []string{"1.0.0", "v1.0.0"} {
		t.Run(tag, func(t *testing.T) {
			transport := &releaselessGitHub{statuses: map[string]int{alphaPlainTag: 404, alphaVTag: 404}, bodies: map[string]string{}}
			path := alphaPlainTag
			if tag == "v1.0.0" {
				path = alphaVTag
			}
			transport.statuses[path] = 200
			transport.bodies[path] = fmt.Sprintf(`{"id":7,"tag_name":%q}`, tag)
			app := productionMigrateApplication(t, transport)
			requested, _, release, reused, err := app.service.resolveMapping(context.Background(), dependency.State{}, migrate.Mapping{From: "example/alpha", Source: "github:example/alpha", TesslVersion: "1.0.0"})
			if err != nil || reused || requested != tag || release == nil || release.Tag != tag {
				t.Fatalf("requested=%q release=%+v reused=%t err=%v", requested, release, reused, err)
			}
			want := "GET https://api.github.com/repos/example/alpha/releases/tags/1.0.0\nGET https://api.github.com/repos/example/alpha/releases/tags/v1.0.0"
			if strings.Join(transport.requests, "\n") != want {
				t.Fatalf("requests=%v want=%s", transport.requests, want)
			}
		})
	}
}
