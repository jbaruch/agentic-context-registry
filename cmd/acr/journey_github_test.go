package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The production hostnames the journey fixture answers for. Routing happens in
// the transport, so the client keeps building, signing, redirect-checking and
// origin-validating exactly the URLs it builds against GitHub itself.
const (
	journeyAPIHost      = "api.github.com"
	journeyUploadHost   = "uploads.github.com"
	journeyCodeloadHost = "codeload.github.com"
	journeyAssetHost    = "objects.githubusercontent.com"
)

type journeyAsset struct {
	ID          int64
	Name        string
	ContentType string
	Bytes       []byte
}

type journeyRelease struct {
	ID         int64
	Tag        string
	Target     string
	Draft      bool
	Prerelease bool
	Assets     []*journeyAsset
}

// journeyRepository is one repository's remote state: which tags exist, what
// commit each points at, the source tarball each commit serves, and the
// releases published so far. A journey advances this state by running acr, not
// by writing the state a later step reads back.
type journeyRepository struct {
	Tags     map[string]string
	Archives map[string][]byte
	Releases []*journeyRelease
}

// journeyGitHub is a stateful stand-in for the GitHub REST boundary: the
// release, commit, tarball, upload and asset-download endpoints acr uses, each
// answering from one store that every later request observes.
type journeyGitHub struct {
	t        *testing.T
	server   *httptest.Server
	mutex    sync.Mutex
	repos    map[string]*journeyRepository
	requests []string
	failures []string
	nextID   int64
	// untrustedArchiveRedirect points the tarball redirect at an origin the
	// client does not trust, so a journey can prove the allowlist refuses it.
	untrustedArchiveRedirect bool
	tokenSeen                map[string]bool
	assetIndex               map[int64]*journeyAsset
	assetOwner               map[int64]string
}

func newJourneyGitHub(t *testing.T) *journeyGitHub {
	t.Helper()
	fixture := &journeyGitHub{
		t:          t,
		repos:      map[string]*journeyRepository{},
		nextID:     9000,
		tokenSeen:  map[string]bool{},
		assetIndex: map[int64]*journeyAsset{},
		assetOwner: map[int64]string{},
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serve))
	t.Cleanup(fixture.server.Close)
	return fixture
}

// Repository returns the mutable remote state of one owner/name, creating it
// on first use.
func (fixture *journeyGitHub) Repository(fullName string) *journeyRepository {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	return fixture.repositoryLocked(fullName)
}

func (fixture *journeyGitHub) repositoryLocked(fullName string) *journeyRepository {
	repository, ok := fixture.repos[fullName]
	if !ok {
		repository = &journeyRepository{Tags: map[string]string{}, Archives: map[string][]byte{}}
		fixture.repos[fullName] = repository
	}
	return repository
}

// PublishSource makes one tagged commit resolvable and its source tarball
// downloadable, the way pushing a tag does.
func (fixture *journeyGitHub) PublishSource(fullName, tag, commit string, archive []byte) {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	repository := fixture.repositoryLocked(fullName)
	repository.Tags[tag] = commit
	repository.Archives[commit] = append([]byte(nil), archive...)
}

// SeedRelease adds an already-visible release, for journeys whose subject is
// what a consumer does with one rather than how it was created.
func (fixture *journeyGitHub) SeedRelease(fullName, tag, commit string, archive []byte) *journeyRelease {
	fixture.PublishSource(fullName, tag, commit, archive)
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	repository := fixture.repositoryLocked(fullName)
	fixture.nextID++
	release := &journeyRelease{ID: fixture.nextID, Tag: tag, Target: commit}
	repository.Releases = append(repository.Releases, release)
	return release
}

// RedirectArchivesToAnUntrustedOrigin makes the tarball redirect point outside
// the origins the client trusts.
func (fixture *journeyGitHub) RedirectArchivesToAnUntrustedOrigin() {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	fixture.untrustedArchiveRedirect = true
}

// Requests returns every request the fixture answered, as "METHOD scheme://host/path".
func (fixture *journeyGitHub) Requests() []string {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	return append([]string(nil), fixture.requests...)
}

// RequestCount returns how many recorded requests contain substring.
func (fixture *journeyGitHub) RequestCount(substring string) int {
	count := 0
	for _, request := range fixture.Requests() {
		if strings.Contains(request, substring) {
			count++
		}
	}
	return count
}

// ResetRequests clears the log so one step's traversal can be asserted alone.
func (fixture *journeyGitHub) ResetRequests() {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	fixture.requests = nil
}

// AssertNoUnknownRequests fails when acr reached an endpoint this fixture does
// not implement, so an unimplemented boundary can never read as a pass.
func (fixture *journeyGitHub) AssertNoUnknownRequests(t *testing.T) {
	t.Helper()
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	if len(fixture.failures) != 0 {
		t.Fatalf("unimplemented GitHub endpoints reached: %v", fixture.failures)
	}
}

// AuthorizationSeenOn reports whether a request to one host carried a bearer
// token. Archive redirects keep the token; asset redirects must strip it.
func (fixture *journeyGitHub) AuthorizationSeenOn(host string) bool {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	return fixture.tokenSeen[host]
}

// Transport routes the production hostnames to this fixture without changing
// the URLs acr builds, so origin allowlists and Host headers stay real.
func (fixture *journeyGitHub) Transport() http.RoundTripper {
	endpoint, err := url.Parse(fixture.server.URL)
	if err != nil {
		fixture.t.Fatal(err)
	}
	return &journeyTransport{endpoint: endpoint.Host}
}

// Endpoint is the address a composed-subprocess journey routes to.
func (fixture *journeyGitHub) Endpoint() string {
	endpoint, err := url.Parse(fixture.server.URL)
	if err != nil {
		fixture.t.Fatal(err)
	}
	return endpoint.Host
}

type journeyTransport struct{ endpoint string }

func (transport *journeyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	switch request.URL.Host {
	case journeyAPIHost, journeyUploadHost, journeyCodeloadHost, journeyAssetHost:
	default:
		return nil, fmt.Errorf("journey fixture refused a request to %s", request.URL.Host)
	}
	routed := request.Clone(request.Context())
	routed.URL.Scheme = "http"
	routed.URL.Host = transport.endpoint
	// Host carries the production authority the handler dispatches on.
	routed.Host = request.URL.Host
	return http.DefaultTransport.RoundTrip(routed)
}

func (fixture *journeyGitHub) serve(writer http.ResponseWriter, request *http.Request) {
	fixture.mutex.Lock()
	fixture.requests = append(fixture.requests, request.Method+" https://"+request.Host+request.URL.Path)
	if strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") {
		fixture.tokenSeen[request.Host] = true
	}
	fixture.mutex.Unlock()

	handled := false
	switch request.Host {
	case journeyAPIHost:
		handled = fixture.serveAPI(writer, request)
	case journeyUploadHost:
		handled = fixture.serveUpload(writer, request)
	case journeyCodeloadHost:
		handled = fixture.serveCodeload(writer, request)
	case journeyAssetHost:
		handled = fixture.serveAssetBytes(writer, request)
	}
	if handled {
		return
	}
	fixture.mutex.Lock()
	fixture.failures = append(fixture.failures, request.Method+" host="+request.Host+" path="+request.URL.Path)
	fixture.mutex.Unlock()
	http.Error(writer, `{"message":"journey fixture does not implement this endpoint"}`, http.StatusNotImplemented)
}

// segments splits a path such as /repos/owner/name/releases/latest.
func segments(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, "/")
	for index, part := range parts {
		decoded, err := url.PathUnescape(part)
		if err == nil {
			parts[index] = decoded
		}
	}
	return parts
}

func (fixture *journeyGitHub) serveAPI(writer http.ResponseWriter, request *http.Request) bool {
	parts := segments(request.URL.Path)
	if len(parts) < 4 || parts[0] != "repos" {
		return false
	}
	fullName := parts[1] + "/" + parts[2]
	rest := parts[3:]
	if request.Method == http.MethodGet && len(rest) == 3 && rest[0] == "releases" && rest[1] == "assets" {
		id, err := strconv.ParseInt(rest[2], 10, 64)
		if err != nil {
			return false
		}
		return fixture.serveAssetMetadata(writer, request, id)
	}
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	repository := fixture.repositoryLocked(fullName)

	switch {
	case request.Method == http.MethodGet && len(rest) == 2 && rest[0] == "releases" && rest[1] == "latest":
		latest := (*journeyRelease)(nil)
		for _, release := range repository.Releases {
			if release.Draft || release.Prerelease {
				continue
			}
			if latest == nil || release.ID > latest.ID {
				latest = release
			}
		}
		if latest == nil {
			return writeJourneyJSON(writer, http.StatusNotFound, map[string]string{"message": "Not Found"})
		}
		return writeJourneyJSON(writer, http.StatusOK, releasePayload(fullName, latest))
	case request.Method == http.MethodGet && len(rest) == 3 && rest[0] == "releases" && rest[1] == "tags":
		release := findRelease(repository, rest[2])
		if release == nil {
			return writeJourneyJSON(writer, http.StatusNotFound, map[string]string{"message": "Not Found"})
		}
		return writeJourneyJSON(writer, http.StatusOK, releasePayload(fullName, release))
	case request.Method == http.MethodGet && len(rest) == 2 && rest[0] == "commits":
		commit, ok := resolveJourneyReference(repository, rest[1])
		if !ok {
			return writeJourneyJSON(writer, http.StatusNotFound, map[string]string{"message": "No commit found"})
		}
		return writeJourneyJSON(writer, http.StatusOK, map[string]string{"sha": commit})
	case request.Method == http.MethodGet && len(rest) == 4 && rest[0] == "git" && rest[1] == "ref" && rest[2] == "tags":
		commit, ok := repository.Tags[rest[3]]
		if !ok {
			return writeJourneyJSON(writer, http.StatusNotFound, map[string]string{"message": "Not Found"})
		}
		return writeJourneyJSON(writer, http.StatusOK, map[string]any{"object": map[string]string{"sha": commit}})
	case request.Method == http.MethodGet && len(rest) == 2 && rest[0] == "tarball":
		commit, ok := resolveJourneyReference(repository, rest[1])
		if !ok {
			return writeJourneyJSON(writer, http.StatusNotFound, map[string]string{"message": "Not Found"})
		}
		location := "https://" + journeyCodeloadHost + "/" + fullName + "/tar.gz/" + commit
		if fixture.untrustedArchiveRedirect {
			location = "https://archives.example.invalid/" + fullName + "/tar.gz/" + commit
		}
		writer.Header().Set("Location", location)
		writer.WriteHeader(http.StatusFound)
		return true
	case request.Method == http.MethodPost && len(rest) == 1 && rest[0] == "releases":
		var payload struct {
			Tag    string `json:"tag_name"`
			Target string `json:"target_commitish"`
			Draft  bool   `json:"draft"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			return writeJourneyJSON(writer, http.StatusBadRequest, map[string]string{"message": "malformed release request"})
		}
		if findRelease(repository, payload.Tag) != nil {
			return writeJourneyJSON(writer, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed: already_exists"})
		}
		fixture.nextID++
		release := &journeyRelease{ID: fixture.nextID, Tag: payload.Tag, Target: payload.Target, Draft: payload.Draft}
		repository.Releases = append(repository.Releases, release)
		return writeJourneyJSON(writer, http.StatusCreated, releasePayload(fullName, release))
	case len(rest) == 2 && rest[0] == "releases":
		id, err := strconv.ParseInt(rest[1], 10, 64)
		if err != nil {
			return false
		}
		release := findReleaseByID(repository, id)
		if release == nil {
			return writeJourneyJSON(writer, http.StatusNotFound, map[string]string{"message": "Not Found"})
		}
		switch request.Method {
		case http.MethodPatch:
			var payload struct {
				Draft *bool `json:"draft"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				return writeJourneyJSON(writer, http.StatusBadRequest, map[string]string{"message": "malformed release update"})
			}
			if payload.Draft != nil {
				release.Draft = *payload.Draft
			}
			return writeJourneyJSON(writer, http.StatusOK, releasePayload(fullName, release))
		case http.MethodDelete:
			remaining := repository.Releases[:0]
			for _, candidate := range repository.Releases {
				if candidate.ID != id {
					remaining = append(remaining, candidate)
				}
			}
			repository.Releases = remaining
			writer.WriteHeader(http.StatusNoContent)
			return true
		}
	}
	return false
}

// serveAssetBytes answers the signed-download origin an asset redirect points
// at. GitHub never sees a token here, and neither does this fixture.
func (fixture *journeyGitHub) serveAssetBytes(writer http.ResponseWriter, request *http.Request) bool {
	parts := segments(request.URL.Path)
	if request.Method != http.MethodGet || len(parts) != 2 || parts[0] != "assets" {
		return false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	fixture.mutex.Lock()
	asset, ok := fixture.assetIndex[id]
	fixture.mutex.Unlock()
	if !ok {
		return writeJourneyJSON(writer, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
	writer.Header().Set("Content-Type", "application/octet-stream")
	if _, err := writer.Write(asset.Bytes); err != nil {
		fixture.t.Errorf("write asset %d: %v", id, err)
	}
	return true
}

func (fixture *journeyGitHub) serveUpload(writer http.ResponseWriter, request *http.Request) bool {
	parts := segments(request.URL.Path)
	if request.Method != http.MethodPost || len(parts) != 6 || parts[0] != "repos" || parts[3] != "releases" || parts[5] != "assets" {
		return false
	}
	fullName := parts[1] + "/" + parts[2]
	id, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		return false
	}
	name := request.URL.Query().Get("name")
	if name == "" {
		return writeJourneyJSON(writer, http.StatusBadRequest, map[string]string{"message": "missing asset name"})
	}
	contents, err := readAllBody(request)
	if err != nil {
		return writeJourneyJSON(writer, http.StatusBadRequest, map[string]string{"message": err.Error()})
	}
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	repository := fixture.repositoryLocked(fullName)
	release := findReleaseByID(repository, id)
	if release == nil {
		return writeJourneyJSON(writer, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
	for _, existing := range release.Assets {
		if existing.Name == name {
			return writeJourneyJSON(writer, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed: already_exists"})
		}
	}
	fixture.nextID++
	asset := &journeyAsset{ID: fixture.nextID, Name: name, ContentType: request.Header.Get("Content-Type"), Bytes: contents}
	release.Assets = append(release.Assets, asset)
	fixture.assetIndex[asset.ID] = asset
	fixture.assetOwner[asset.ID] = fullName
	return writeJourneyJSON(writer, http.StatusCreated, assetPayload(fullName, asset))
}

func (fixture *journeyGitHub) serveCodeload(writer http.ResponseWriter, request *http.Request) bool {
	parts := segments(request.URL.Path)
	if request.Method != http.MethodGet || len(parts) != 4 || parts[2] != "tar.gz" {
		return false
	}
	fullName := parts[0] + "/" + parts[1]
	fixture.mutex.Lock()
	archive, ok := fixture.repositoryLocked(fullName).Archives[parts[3]]
	fixture.mutex.Unlock()
	if !ok {
		return writeJourneyJSON(writer, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
	writer.Header().Set("Content-Type", "application/gzip")
	if _, err := writer.Write(archive); err != nil {
		fixture.t.Errorf("write archive: %v", err)
	}
	return true
}

// serveAssetMetadata answers the API asset URL acr verifies uploads through.
// A request that accepts octet-stream is redirected to the download origin,
// exactly as GitHub redirects one.
func (fixture *journeyGitHub) serveAssetMetadata(writer http.ResponseWriter, request *http.Request, id int64) bool {
	fixture.mutex.Lock()
	asset, ok := fixture.assetIndex[id]
	fullName := fixture.assetOwner[id]
	fixture.mutex.Unlock()
	if !ok {
		return writeJourneyJSON(writer, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
	if strings.Contains(request.Header.Get("Accept"), "application/octet-stream") {
		writer.Header().Set("Location", "https://"+journeyAssetHost+"/assets/"+strconv.FormatInt(id, 10))
		writer.WriteHeader(http.StatusFound)
		return true
	}
	return writeJourneyJSON(writer, http.StatusOK, assetPayload(fullName, asset))
}

func findRelease(repository *journeyRepository, tag string) *journeyRelease {
	for _, release := range repository.Releases {
		if release.Tag == tag {
			return release
		}
	}
	return nil
}

func findReleaseByID(repository *journeyRepository, id int64) *journeyRelease {
	for _, release := range repository.Releases {
		if release.ID == id {
			return release
		}
	}
	return nil
}

// resolveJourneyReference peels a tag, accepts a full commit, and resolves an
// abbreviated commit the way GitHub does.
func resolveJourneyReference(repository *journeyRepository, reference string) (string, bool) {
	if commit, ok := repository.Tags[reference]; ok {
		return commit, true
	}
	commits := make([]string, 0, len(repository.Archives))
	for commit := range repository.Archives {
		commits = append(commits, commit)
	}
	sort.Strings(commits)
	for _, commit := range commits {
		if commit == reference || (len(reference) >= 7 && strings.HasPrefix(commit, strings.ToLower(reference))) {
			return commit, true
		}
	}
	return "", false
}

func releasePayload(fullName string, release *journeyRelease) map[string]any {
	assets := make([]map[string]any, 0, len(release.Assets))
	for _, asset := range release.Assets {
		assets = append(assets, assetPayload(fullName, asset))
	}
	return map[string]any{
		"id": release.ID, "tag_name": release.Tag, "draft": release.Draft,
		"prerelease": release.Prerelease, "target_commitish": release.Target,
		"html_url": "https://github.com/" + fullName + "/releases/tag/" + release.Tag,
		"assets":   assets,
	}
}

func assetPayload(fullName string, asset *journeyAsset) map[string]any {
	return map[string]any{
		"id": asset.ID, "name": asset.Name, "content_type": asset.ContentType, "size": len(asset.Bytes),
		"url": "https://" + journeyAPIHost + "/repos/" + fullName + "/releases/assets/" + strconv.FormatInt(asset.ID, 10),
	}
}

// readAllBody reads an upload body with the same ceiling the production client
// enforces, so a fixture cannot accept what GitHub would reject.
func readAllBody(request *http.Request) ([]byte, error) {
	defer request.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(request.Body, (100<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("read uploaded asset: %w", err)
	}
	if len(contents) > 100<<20 {
		return nil, fmt.Errorf("uploaded asset exceeds 100 MiB")
	}
	return contents, nil
}

func writeJourneyJSON(writer http.ResponseWriter, status int, payload any) bool {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	_, _ = writer.Write(encoded)
	return true
}
