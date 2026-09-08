package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

func TestRunPackAndFormula(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	input := filepath.Join(root, "bin")
	assets := filepath.Join(root, "assets")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"acr-darwin-amd64", "acr-darwin-arm64", "acr-linux-amd64", "acr-linux-arm64"} {
		if err := os.WriteFile(filepath.Join(input, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	license := filepath.Join(root, "LICENSE")
	if err := os.WriteFile(license, []byte("license\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exit := run(context.Background(), &stdout, &stderr, []string{"pack", "--input", input, "--output", assets, "--license", license}, rejectingRemote{}); exit != 0 {
		t.Fatalf("run(pack) exit = %d, stdout = %q, stderr = %q", exit, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"checksums.txt"`) || stderr.Len() != 0 {
		t.Fatalf("run(pack) stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}

	formula := filepath.Join(root, "Formula", "acr.rb")
	stdout.Reset()
	if exit := run(context.Background(), &stdout, &stderr, []string{"formula", "--version", "1.2.3", "--checksums", filepath.Join(assets, "checksums.txt"), "--output", formula}, rejectingRemote{}); exit != 0 {
		t.Fatalf("run(formula) exit = %d, stdout = %q, stderr = %q", exit, stdout.String(), stderr.String())
	}
	contents, err := os.ReadFile(formula)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), `version "1.2.3"`) || !strings.Contains(string(contents), "acr-linux-arm64.tar.gz") {
		t.Fatalf("formula =\n%s", contents)
	}
}

func TestRunVerifySBOMKeepsJSONStdoutUncontaminated(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "acr-linux-amd64.cdx.json")
	contents := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","metadata":{"component":{"name":"acr","version":"1.2.3","purl":"pkg:golang/github.com/jbaruch/agentic-context-registry@v0.1.3?goarch=amd64&goos=linux&type=module#cmd/acr","properties":[{"name":"cdx:gomod:build:env:CGO_ENABLED","value":"0"},{"name":"cdx:gomod:build:env:GOARCH","value":"amd64"},{"name":"cdx:gomod:build:env:GOOS","value":"linux"}]}}}`)
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exit := run(context.Background(), &stdout, &stderr, []string{"verify-sbom", "--version", "1.2.3", "--path", path, "--goos", "linux", "--goarch", "amd64"}, rejectingRemote{})
	if exit != 0 || !strings.HasPrefix(stdout.String(), "{") || !strings.HasSuffix(stdout.String(), "}\n") || stderr.Len() != 0 {
		t.Fatalf("run(verify-sbom) exit = %d, stdout = %q, stderr = %q", exit, stdout.String(), stderr.String())
	}
}

func TestRunVerifySBOMRejectsMismatchedPath(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "acr-linux-amd64.cdx.json")
	contents := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","metadata":{"component":{"name":"acr","version":"1.2.3","purl":"pkg:golang/github.com/jbaruch/agentic-context-registry@v0.1.3?goarch=amd64&goos=linux&type=module#cmd/acr","properties":[{"name":"cdx:gomod:build:env:CGO_ENABLED","value":"0"},{"name":"cdx:gomod:build:env:GOARCH","value":"amd64"},{"name":"cdx:gomod:build:env:GOOS","value":"linux"}]}}}`)
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exit := run(context.Background(), &stdout, &stderr, []string{"verify-sbom", "--version", "1.2.3", "--path", path, "--goos", "darwin", "--goarch", "arm64"}, rejectingRemote{})
	if exit == 0 || !strings.Contains(stderr.String(), "acr-darwin-arm64.cdx.json") {
		t.Fatalf("run(verify-sbom) exit = %d, stdout = %q, stderr = %q", exit, stdout.String(), stderr.String())
	}
}

func TestLoadAssetsRequiresTenCLIFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadAssets(dir)
	if err == nil || !strings.Contains(err.Error(), "acr-darwin-amd64.cdx.json") || !strings.Contains(err.Error(), "10") {
		t.Fatalf("loadAssets() error = %v, want missing SBOM and 10-asset contract", err)
	}
}

type rejectingRemote struct{}

func (rejectingRemote) LookupRelease(context.Context, dependency.Repository, string) (dependency.Release, bool, error) {
	panic("unexpected GitHub access")
}

func (rejectingRemote) TagCommit(context.Context, dependency.Repository, string) (string, bool, error) {
	panic("unexpected GitHub access")
}

func (rejectingRemote) CreateRelease(context.Context, dependency.Repository, string, string) (dependency.Release, error) {
	panic("unexpected GitHub access")
}

func (rejectingRemote) UploadAsset(context.Context, dependency.Repository, int64, string, string, []byte) (dependency.ReleaseAsset, []byte, error) {
	panic("unexpected GitHub access")
}

func (rejectingRemote) PublishRelease(context.Context, dependency.Repository, int64) (dependency.Release, error) {
	panic("unexpected GitHub access")
}

func (rejectingRemote) DeleteRelease(context.Context, dependency.Repository, int64) error {
	panic("unexpected GitHub access")
}
