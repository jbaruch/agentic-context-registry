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
)

// GitHubAPIError preserves the HTTP status needed for safe immutable-release
// race handling while retaining actionable diagnostics.
type GitHubAPIError struct {
	StatusCode int
	Message    string
}

func (err *GitHubAPIError) Error() string { return err.Message }

// IsGitHubStatus reports whether err came from GitHub with status.
func IsGitHubStatus(err error, status int) bool {
	var apiErr *GitHubAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status
}

// LookupRelease returns any release for tag, including a draft or prerelease.
func (client *GitHubClient) LookupRelease(ctx context.Context, repository Repository, tag string) (Release, bool, error) {
	endpoint := fmt.Sprintf("/repos/%s/%s/releases/tags/%s", url.PathEscape(repository.Owner), url.PathEscape(repository.Name), url.PathEscape(tag))
	request, err := client.request(ctx, endpoint)
	if err != nil {
		return Release{}, false, err
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return Release{}, false, fmt.Errorf("look up release tag %q for %s: %w; check network access and retry", tag, repository.String(), err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return Release{}, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return Release{}, false, client.responseError(response, repository)
	}
	var decoded releaseResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&decoded); err != nil {
		return Release{}, false, fmt.Errorf("decode release tag %q for %s: %w", tag, repository.String(), err)
	}
	release := decoded.release()
	if release.ID <= 0 || release.Tag != tag {
		return Release{}, false, fmt.Errorf("GitHub returned invalid release metadata for tag %q; retry or report the repository response", tag)
	}
	return release, true, nil
}

// TagCommit returns the peeled commit for a pushed tag. A missing ref returns
// exists=false without conflating it with another API failure.
func (client *GitHubClient) TagCommit(ctx context.Context, repository Repository, tag string) (commit string, exists bool, err error) {
	var reference struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/git/ref/tags/%s", url.PathEscape(repository.Owner), url.PathEscape(repository.Name), url.PathEscape(tag))
	request, err := client.request(ctx, endpoint)
	if err != nil {
		return "", false, err
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return "", false, fmt.Errorf("look up pushed tag %q for %s: %w; check network access and retry", tag, repository.String(), err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if response.StatusCode != http.StatusOK {
		return "", false, client.responseError(response, repository)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&reference); err != nil {
		return "", false, fmt.Errorf("decode pushed tag %q for %s: %w", tag, repository.String(), err)
	}
	if reference.Object.SHA == "" {
		return "", false, fmt.Errorf("GitHub returned an empty object for tag %q; retry or report the repository response", tag)
	}
	commit, err = client.ResolveCommit(ctx, repository, tag)
	if err != nil {
		return "", false, err
	}
	return commit, true, nil
}

// CreateRelease creates an unpublished draft for one immutable tag.
func (client *GitHubClient) CreateRelease(ctx context.Context, repository Repository, tag, commit string) (Release, error) {
	payload := struct {
		Tag        string `json:"tag_name"`
		Target     string `json:"target_commitish"`
		Name       string `json:"name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}{Tag: tag, Target: commit, Name: tag, Draft: true}
	var response releaseResponse
	endpoint := fmt.Sprintf("/repos/%s/%s/releases", url.PathEscape(repository.Owner), url.PathEscape(repository.Name))
	if err := client.sendJSON(ctx, http.MethodPost, endpoint, payload, http.StatusCreated, &response); err != nil {
		return Release{}, fmt.Errorf("create draft release %q for %s: %w", tag, repository.String(), err)
	}
	release := response.release()
	if release.ID <= 0 || !release.Draft || release.Tag != tag {
		return Release{}, fmt.Errorf("GitHub returned invalid draft metadata for tag %q; delete the malformed release and retry", tag)
	}
	return release, nil
}

// UploadAsset uploads one draft asset and downloads it again from GitHub so
// the publisher can compare remote bytes before making the release visible.
func (client *GitHubClient) UploadAsset(ctx context.Context, repository Repository, releaseID int64, name, contentType string, contents []byte) (ReleaseAsset, []byte, error) {
	endpoint := fmt.Sprintf("/repos/%s/%s/releases/%d/assets?name=%s", url.PathEscape(repository.Owner), url.PathEscape(repository.Name), releaseID, url.QueryEscape(name))
	request, err := client.request(ctx, endpoint)
	if err != nil {
		return ReleaseAsset{}, nil, err
	}
	request.Method = http.MethodPost
	request.Body = io.NopCloser(bytes.NewReader(contents))
	request.ContentLength = int64(len(contents))
	request.Header.Set("Content-Type", contentType)
	if client.baseURL == "https://api.github.com" {
		request.URL.Scheme = "https"
		request.URL.Host = "uploads.github.com"
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return ReleaseAsset{}, nil, fmt.Errorf("upload release asset %q: %w; check network access and retry", name, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return ReleaseAsset{}, nil, client.responseError(response, repository)
	}
	var asset ReleaseAsset
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&asset); err != nil {
		return ReleaseAsset{}, nil, fmt.Errorf("decode uploaded release asset %q: %w", name, err)
	}
	if asset.ID <= 0 || asset.Name != name || asset.URL == "" {
		return ReleaseAsset{}, nil, fmt.Errorf("GitHub returned invalid uploaded asset metadata for %q; leave the release as a draft and retry", name)
	}
	verified, err := client.downloadReleaseAsset(ctx, repository, asset)
	if err != nil {
		return ReleaseAsset{}, nil, err
	}
	return asset, verified, nil
}

func (client *GitHubClient) downloadReleaseAsset(ctx context.Context, repository Repository, asset ReleaseAsset) ([]byte, error) {
	location, err := url.Parse(asset.URL)
	if err != nil {
		return nil, fmt.Errorf("parse uploaded asset URL for %q: %w", asset.Name, err)
	}
	base, err := url.Parse(client.baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub API base URL: %w", err)
	}
	if urlOrigin(location) != urlOrigin(base) {
		return nil, fmt.Errorf("refuse uploaded asset verification for untrusted origin %s", urlOrigin(location))
	}
	request, err := client.request(ctx, "")
	if err != nil {
		return nil, err
	}
	request.URL = location
	request.Header.Set("Accept", "application/octet-stream")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download uploaded release asset %q: %w; retry while the release remains a draft", asset.Name, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, client.responseError(response, repository)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxArchiveBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read uploaded release asset %q: %w", asset.Name, err)
	}
	if len(contents) > maxArchiveBytes {
		return nil, fmt.Errorf("uploaded release asset %q exceeds %d MiB; reduce package size and retry", asset.Name, maxArchiveBytes>>20)
	}
	return contents, nil
}

// PublishRelease atomically exposes a fully verified draft.
func (client *GitHubClient) PublishRelease(ctx context.Context, repository Repository, releaseID int64) (Release, error) {
	var response releaseResponse
	endpoint := fmt.Sprintf("/repos/%s/%s/releases/%d", url.PathEscape(repository.Owner), url.PathEscape(repository.Name), releaseID)
	if err := client.sendJSON(ctx, http.MethodPatch, endpoint, struct {
		Draft bool `json:"draft"`
	}{Draft: false}, http.StatusOK, &response); err != nil {
		return Release{}, fmt.Errorf("publish draft release %d for %s: %w", releaseID, repository.String(), err)
	}
	return response.release(), nil
}

// DeleteRelease removes an ACR-owned stale draft before a clean retry.
func (client *GitHubClient) DeleteRelease(ctx context.Context, repository Repository, releaseID int64) error {
	endpoint := fmt.Sprintf("/repos/%s/%s/releases/%d", url.PathEscape(repository.Owner), url.PathEscape(repository.Name), releaseID)
	request, err := client.request(ctx, endpoint)
	if err != nil {
		return err
	}
	request.Method = http.MethodDelete
	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("delete stale draft release %d for %s: %w", releaseID, repository.String(), err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return client.responseError(response, repository)
	}
	return nil
}

func (client *GitHubClient) sendJSON(ctx context.Context, method, endpoint string, payload any, wantStatus int, target any) error {
	contents, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode GitHub request: %w", err)
	}
	request, err := client.request(ctx, endpoint)
	if err != nil {
		return err
	}
	request.Method = method
	request.Body = io.NopCloser(bytes.NewReader(contents))
	request.ContentLength = int64(len(contents))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("request GitHub API: %w; check network access and retry", err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		return client.responseError(response, Repository{})
	}
	if target == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(target); err != nil {
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	return nil
}
