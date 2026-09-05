package dependency

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

const maxArchiveBytes = 100 << 20

var (
	sourcePattern       = regexp.MustCompile(`^github:([a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?)/([a-z0-9](?:[a-z0-9._-]*[a-z0-9])?)$`)
	vendorSourcePattern = regexp.MustCompile(`^vendor:([a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?)/([a-z0-9](?:[a-z0-9._-]*[a-z0-9])?)$`)
	commitPattern       = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
	fullCommitPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	contentHashPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Scheme identifies the transport encoded in a dependency source.
type Scheme string

const (
	SchemeGitHub Scheme = "github"
	SchemeVendor Scheme = "vendor"
)

// Repository is a parsed canonical github:owner/name source.
type Repository struct {
	Owner string
	Name  string
}

// String returns the canonical dependency source.
func (repository Repository) String() string {
	return "github:" + repository.Owner + "/" + repository.Name
}

// FullName returns owner/name for GitHub APIs and package identity checks.
func (repository Repository) FullName() string {
	return repository.Owner + "/" + repository.Name
}

// ParseSource validates and parses a canonical GitHub dependency source.
func ParseSource(source string) (Repository, error) {
	matches := sourcePattern.FindStringSubmatch(source)
	if matches == nil {
		return Repository{}, fmt.Errorf("invalid source %q; use github:owner/repository with lowercase canonical names", source)
	}
	return Repository{Owner: matches[1], Name: matches[2]}, nil
}

// VendorIdentity is a canonical Tessl workspace/package identity.
type VendorIdentity struct {
	Workspace string
	Package   string
}

// String returns the canonical vendored dependency source.
func (identity VendorIdentity) String() string {
	return "vendor:" + identity.Workspace + "/" + identity.Package
}

// FullName returns the Tessl workspace/package identity.
func (identity VendorIdentity) FullName() string {
	return identity.Workspace + "/" + identity.Package
}

// SourceScheme validates a dependency source and reports its scheme without
// widening ParseSource beyond GitHub API callers.
func SourceScheme(source string) (Scheme, error) {
	if sourcePattern.MatchString(source) {
		return SchemeGitHub, nil
	}
	if vendorSourcePattern.MatchString(source) {
		return SchemeVendor, nil
	}
	return "", fmt.Errorf("invalid source %q; use github:owner/repository or vendor:workspace/package with lowercase canonical names", source)
}

// ParseVendorSource validates and parses a canonical vendored source.
func ParseVendorSource(source string) (VendorIdentity, error) {
	matches := vendorSourcePattern.FindStringSubmatch(source)
	if matches == nil {
		return VendorIdentity{}, fmt.Errorf("invalid vendor source %q; use vendor:workspace/package with lowercase canonical names", source)
	}
	return VendorIdentity{Workspace: matches[1], Package: matches[2]}, nil
}

// Release is the stable GitHub release metadata needed for a lock.
type Release struct {
	ID         int64
	Tag        string
	Draft      bool
	Prerelease bool
	Target     string
	HTMLURL    string
	Assets     []ReleaseAsset
}

// ReleaseAsset is one GitHub Release asset returned by the API.
type ReleaseAsset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// GitHub accesses release, commit, and archive data without exposing auth.
type GitHub interface {
	LatestRelease(context.Context, Repository) (Release, error)
	ReleaseByTag(context.Context, Repository, string) (Release, error)
	ResolveCommit(context.Context, Repository, string) (string, error)
	DownloadArchive(context.Context, Repository, string) ([]byte, error)
	DownloadReleaseAsset(context.Context, Repository, ReleaseAsset) ([]byte, error)
}

// Remote is the complete GitHub surface used by the shipped application.
// It combines dependency resolution with immutable release publication so the
// command entry point can accept one in-process test double.
type Remote interface {
	GitHub
	LookupRelease(context.Context, Repository, string) (Release, bool, error)
	TagCommit(context.Context, Repository, string) (string, bool, error)
	CreateRelease(context.Context, Repository, string, string) (Release, error)
	UploadAsset(context.Context, Repository, int64, string, string, []byte) (ReleaseAsset, []byte, error)
	PublishRelease(context.Context, Repository, int64) (Release, error)
	DeleteRelease(context.Context, Repository, int64) error
}

// RemoteError preserves whether a GitHub failure had an HTTP status. A zero
// status identifies DNS, connection, timeout, and other transport failures.
type RemoteError struct {
	StatusCode int
	Err        error
}

func (err *RemoteError) Error() string { return err.Err.Error() }

func (err *RemoteError) Unwrap() error { return err.Err }

// GitHubClient uses the GitHub REST API and reuses environment, gh CLI, or Git
// credentials. An empty token remains valid for public repositories.
type GitHubClient struct {
	baseURL                    string
	httpClient                 *http.Client
	trustedArchiveOrigins      map[string]struct{}
	trustedReleaseAssetOrigins map[string]struct{}
	token                      string
	tokenOnce                  sync.Once
	tokenProvider              func(context.Context) string
}

// ClientOption adjusts one construction detail of the production GitHub
// client. Every option leaves the production API host, upload host, and
// trusted redirect origins in place, so an acceptance test can route those
// hostnames at the transport while the client still builds, authenticates,
// and validates exactly the requests it builds in production.
type ClientOption func(*GitHubClient)

// WithHTTPClient replaces the HTTP client the GitHub client sends through. A
// nil client is ignored so a caller cannot accidentally disable transport
// timeouts.
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(client *GitHubClient) {
		if httpClient != nil {
			client.httpClient = httpClient
		}
	}
}

// NewGitHubClient constructs the production GitHub client. Without options it
// is the client the shipped binary uses.
func NewGitHubClient(options ...ClientOption) *GitHubClient {
	client := newGitHubClient("https://api.github.com", &http.Client{Timeout: 2 * time.Minute})
	for _, option := range options {
		// A nil option is ignored rather than fatal: options are variadic and
		// a caller assembling them conditionally should not have to filter.
		if option != nil {
			option(client)
		}
	}
	return client
}

func newGitHubClient(baseURL string, httpClient *http.Client) *GitHubClient {
	return &GitHubClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
		trustedArchiveOrigins: map[string]struct{}{
			"https://codeload.github.com": {},
		},
		trustedReleaseAssetOrigins: map[string]struct{}{
			"https://objects.githubusercontent.com":        {},
			"https://release-assets.githubusercontent.com": {},
		},
		tokenProvider: discoverGitHubToken,
	}
}

// LatestRelease returns GitHub's newest non-draft, non-prerelease release.
func (client *GitHubClient) LatestRelease(ctx context.Context, repository Repository) (Release, error) {
	var response releaseResponse
	endpoint := fmt.Sprintf("/repos/%s/%s/releases/latest", url.PathEscape(repository.Owner), url.PathEscape(repository.Name))
	if err := client.getJSON(ctx, endpoint, &response); err != nil {
		return Release{}, fmt.Errorf("resolve latest release for %s: %w", repository.String(), err)
	}
	release := response.release()
	if release.ID <= 0 || release.Draft || release.Prerelease || release.Tag == "" {
		return Release{}, fmt.Errorf("GitHub returned a non-stable latest release for %s; publish a non-draft, non-prerelease release and retry", repository.String())
	}
	return release, nil
}

// ReleaseByTag returns an exact stable GitHub release.
func (client *GitHubClient) ReleaseByTag(ctx context.Context, repository Repository, tag string) (Release, error) {
	var response releaseResponse
	endpoint := fmt.Sprintf("/repos/%s/%s/releases/tags/%s", url.PathEscape(repository.Owner), url.PathEscape(repository.Name), url.PathEscape(tag))
	if err := client.getJSON(ctx, endpoint, &response); err != nil {
		return Release{}, fmt.Errorf("resolve release tag %q for %s: %w", tag, repository.String(), err)
	}
	release := response.release()
	if release.ID <= 0 {
		return Release{}, fmt.Errorf("GitHub returned an invalid release ID for tag %q; retry or report the repository response", tag)
	}
	if release.Draft || release.Prerelease {
		return Release{}, fmt.Errorf("release tag %q for %s is a draft or prerelease; choose a stable release tag", tag, repository.String())
	}
	if release.Tag != tag {
		return Release{}, fmt.Errorf("GitHub returned tag %q for requested tag %q; retry with the canonical release tag", release.Tag, tag)
	}
	return release, nil
}

// ResolveCommit resolves a tag, abbreviated SHA, or full SHA to a full commit.
func (client *GitHubClient) ResolveCommit(ctx context.Context, repository Repository, reference string) (string, error) {
	var response struct {
		SHA string `json:"sha"`
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/commits/%s", url.PathEscape(repository.Owner), url.PathEscape(repository.Name), url.PathEscape(reference))
	if err := client.getJSON(ctx, endpoint, &response); err != nil {
		return "", fmt.Errorf("resolve commit %q for %s: %w", reference, repository.String(), err)
	}
	if len(response.SHA) != 40 || !commitPattern.MatchString(response.SHA) {
		return "", fmt.Errorf("GitHub returned invalid commit %q for %s; retry or report the repository response", response.SHA, repository.String())
	}
	return strings.ToLower(response.SHA), nil
}

// DownloadArchive downloads one immutable commit archive with a fixed limit.
func (client *GitHubClient) DownloadArchive(ctx context.Context, repository Repository, commit string) ([]byte, error) {
	endpoint := fmt.Sprintf("/repos/%s/%s/tarball/%s", url.PathEscape(repository.Owner), url.PathEscape(repository.Name), url.PathEscape(commit))
	request, err := client.request(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	response, err := client.archiveHTTPClient().Do(request)
	if err != nil {
		return nil, &RemoteError{Err: fmt.Errorf("download %s at %s: %w; check network access and retry", repository.String(), commit, err)}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, client.responseError(response, repository)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxArchiveBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read archive for %s at %s: %w; retry the download", repository.String(), commit, err)
	}
	if len(contents) > maxArchiveBytes {
		return nil, fmt.Errorf("archive for %s exceeds %d MiB; reduce package size and retry", repository.String(), maxArchiveBytes>>20)
	}
	return contents, nil
}

func (client *GitHubClient) archiveHTTPClient() *http.Client {
	return client.redirectHTTPClient("archive", client.trustedArchiveOrigins, true)
}

func (client *GitHubClient) releaseAssetHTTPClient() *http.Client {
	return client.redirectHTTPClient("release asset", client.trustedReleaseAssetOrigins, false)
}

func (client *GitHubClient) redirectHTTPClient(resource string, trustedOrigins map[string]struct{}, forwardAuthorization bool) *http.Client {
	downloadClient := *client.httpClient
	configuredRedirect := downloadClient.CheckRedirect
	downloadClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return fmt.Errorf("refuse GitHub %s redirect without an originating request", resource)
		}
		if len(via) >= 10 {
			return fmt.Errorf("stop after 10 GitHub %s redirects", resource)
		}
		origin := urlOrigin(request.URL)
		previousOrigin := urlOrigin(via[len(via)-1].URL)
		if origin != previousOrigin {
			if _, trusted := trustedOrigins[origin]; !trusted {
				return fmt.Errorf("refuse GitHub %s redirect from %s to untrusted origin %s", resource, previousOrigin, origin)
			}
		}
		if configuredRedirect != nil {
			if err := configuredRedirect(request, via); err != nil {
				return err
			}
		}
		if forwardAuthorization {
			if authorization := via[0].Header.Get("Authorization"); authorization != "" {
				request.Header.Set("Authorization", authorization)
			}
		} else {
			request.Header.Del("Authorization")
		}
		return nil
	}
	return &downloadClient
}

func urlOrigin(location *url.URL) string {
	return strings.ToLower(location.Scheme) + "://" + strings.ToLower(location.Host)
}

type releaseResponse struct {
	ID         int64          `json:"id"`
	Tag        string         `json:"tag_name"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Target     string         `json:"target_commitish"`
	HTMLURL    string         `json:"html_url"`
	Assets     []ReleaseAsset `json:"assets"`
}

func (response releaseResponse) release() Release {
	return Release{
		ID: response.ID, Tag: response.Tag, Draft: response.Draft, Prerelease: response.Prerelease,
		Target: response.Target, HTMLURL: response.HTMLURL, Assets: append([]ReleaseAsset(nil), response.Assets...),
	}
}

func (client *GitHubClient) getJSON(ctx context.Context, endpoint string, target any) error {
	request, err := client.request(ctx, endpoint)
	if err != nil {
		return err
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return &RemoteError{Err: fmt.Errorf("request GitHub API: %w; check network access and retry", err)}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return client.responseError(response, Repository{})
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode GitHub response: %w; retry or report the malformed response", err)
	}
	return nil
}

func (client *GitHubClient) request(ctx context.Context, endpoint string) (*http.Request, error) {
	return client.newRequest(ctx, http.MethodGet, client.baseURL+endpoint, nil)
}

func (client *GitHubClient) newRequest(ctx context.Context, method, requestURL string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return nil, fmt.Errorf("create GitHub request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "acr")
	client.tokenOnce.Do(func() {
		// Credential discovery has its own bounded command timeouts. Detach it
		// from a single request so cancellation cannot permanently cache an
		// unauthenticated client for later requests.
		client.token = client.tokenProvider(context.WithoutCancel(ctx))
	})
	if client.token != "" {
		request.Header.Set("Authorization", "Bearer "+client.token)
	}
	return request, nil
}

func (client *GitHubClient) responseError(response *http.Response, repository Repository) error {
	contents, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var payload struct {
		Message string `json:"message"`
	}
	if readErr == nil {
		_ = json.Unmarshal(contents, &payload)
	}
	message := strings.TrimSpace(payload.Message)
	if message == "" && readErr == nil {
		message = strings.TrimSpace(strings.ToValidUTF8(string(contents), "�"))
	}
	if message == "" {
		message = response.Status
	}
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		message = fmt.Sprintf("GitHub access denied: %s; run 'gh auth login' or configure Git HTTPS credentials and retry", message)
	case http.StatusNotFound:
		target := "requested GitHub resource"
		if repository.Owner != "" {
			target = repository.String()
		}
		message = fmt.Sprintf("%s was not found or is inaccessible; verify the source or run 'gh auth login' for private repositories", target)
	default:
		switch {
		case response.StatusCode >= http.StatusInternalServerError:
			message = fmt.Sprintf("GitHub API returned %s: %s; retry the request", response.Status, message)
		case response.StatusCode >= http.StatusBadRequest:
			message = fmt.Sprintf("GitHub API returned %s: %s; the request will not succeed unchanged", response.Status, message)
		default:
			message = fmt.Sprintf("GitHub API returned %s: %s", response.Status, message)
		}
	}
	return &RemoteError{
		StatusCode: response.StatusCode,
		Err:        &GitHubAPIError{StatusCode: response.StatusCode, Message: message},
	}
}

// commandTokenBudget is how long one credential command may run before
// discovery abandons it and moves to the next source, so a credential helper
// that blocks cannot hang an install.
const commandTokenBudget = 5 * time.Second

func discoverGitHubToken(ctx context.Context) string {
	return discoverGitHubTokenWithin(ctx, commandTokenBudget)
}

// discoverGitHubTokenWithin is discovery with the per-command budget as an
// argument. A budget of zero leaves ctx as the only bound.
//
// The budget is a parameter because it is a resource guard, not part of the
// order this function establishes, and a test of that order must not race it:
// under a loaded full -race suite two subprocess spawns exceeded the
// production budget, both commands were killed, and the discovery order was
// reported wrong when nothing about the order had changed.
func discoverGitHubTokenWithin(ctx context.Context, budget time.Duration) string {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			return token
		}
	}
	if token := commandToken(ctx, budget, "gh", []string{"auth", "token"}, nil); token != "" {
		return token
	}
	input := []byte("protocol=https\nhost=github.com\n\n")
	return commandToken(ctx, budget, "git", []string{"credential", "fill"}, input)
}

func commandToken(ctx context.Context, budget time.Duration, name string, args []string, input []byte) string {
	if budget > 0 {
		commandContext, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		ctx = commandContext
	}
	command := exec.CommandContext(ctx, name, args...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	output, err := command.Output()
	if err != nil {
		return ""
	}
	if name == "git" {
		for _, line := range strings.Split(string(output), "\n") {
			if value, found := strings.CutPrefix(line, "password="); found {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	return strings.TrimSpace(string(output))
}

func isCommitRequest(requested string) bool {
	return commitPattern.MatchString(requested)
}

func validateRequested(requested string) error {
	if requested == "latest" || isCommitRequest(requested) {
		return nil
	}
	if requested == "" || strings.TrimSpace(requested) != requested || strings.ContainsAny(requested, "~^:?*@[\\") || strings.Contains(requested, "..") || strings.HasPrefix(requested, ".") || strings.HasSuffix(requested, ".") || strings.HasPrefix(requested, "/") || strings.HasSuffix(requested, "/") {
		return errors.New("requested version must be latest, a stable release tag, or a 7-40 character commit SHA")
	}
	return nil
}
