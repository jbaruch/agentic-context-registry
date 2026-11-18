package dependency

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOpaqueOrdinaryArchiveErrors(t *testing.T) {
	t.Parallel()
	var malformedTar bytes.Buffer
	writer := gzip.NewWriter(&malformedTar)
	if _, err := writer.Write([]byte(strings.Repeat("x", 512))); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, want string
		archive    []byte
		cause      error
	}{
		{"gzip", "open downloaded archive: gzip: invalid header; verify the repository provides a valid GitHub tarball and retry", []byte(strings.Repeat("x", 32)), gzip.ErrHeader},
		{"tar", "read downloaded archive: archive/tar: invalid tar header; retry the download or report a malformed package", malformedTar.Bytes(), tar.ErrHeader},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ExtractPackageArchive(test.archive, t.TempDir())
			if err == nil || err.Error() != test.want || !errors.Is(err, test.cause) {
				t.Fatalf("archive output=%v want=%q, original cause retained=%t", err, test.want, errors.Is(err, test.cause))
			}
		})
	}
}

func TestOpaqueOrdinaryGitHubJSONError(t *testing.T) {
	t.Parallel()
	client := NewGitHubClient(WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"bad"}`)), Request: request}, nil
	})}))
	client.tokenOnce.Do(func() {})
	_, err := client.ReleaseByTag(context.Background(), Repository{Owner: "example", Name: "alpha"}, "v1.0.0")
	want := `resolve release tag "v1.0.0" for "github:example/alpha": decode GitHub response: json: cannot unmarshal string into Go struct field releaseResponse.id of type int64; retry or report the malformed response`
	var original *json.UnmarshalTypeError
	if err == nil || err.Error() != want || !errors.As(err, &original) {
		t.Fatalf("ordinary JSON output=%v want=%q", err, want)
	}
}

func TestArchivePathOperandsAreBoundedSeparately(t *testing.T) {
	t.Parallel()
	_, _, err := archivePath(strings.Repeat("r", 1<<20)+"/file", strings.Repeat("s", 1<<20))
	want := `downloaded archive has multiple roots "` + strings.Repeat("s", 128) + `...[truncated]" and "` + strings.Repeat("r", 128) + `...[truncated]"; verify the downloaded archive and the source it was built from, then retry`
	if err == nil || err.Error() != want {
		t.Fatal("archive-controlled operands bypassed bounded rendering or lost recovery instructions")
	}
}
