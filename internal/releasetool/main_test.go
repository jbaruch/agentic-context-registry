package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

func TestReleaseToolBindsNotesToTheExactTagObject(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_AUTHOR_DATE", "2026-11-17T03:10:32Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-11-17T03:10:32Z")
	git := func(args ...string) string {
		t.Helper()
		config := []string{"-c", "user.name=Release Fixture", "-c", "user.email=fixture@example.invalid"}
		output, err := exec.Command("git", append(config, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "-q", "--template=", "--object-format=sha1")
	git("commit", "-q", "--allow-empty", "-m", "Previous commit")
	git("commit", "-q", "--allow-empty", "-m", "Release commit", "-m", "Only a commit message.")
	commit := git("rev-parse", "HEAD")
	git("tag", "-a", "v1.2.3", "-m", "acr v1.2.3", "-m", "Explains a release change.")
	approvedObject := git("rev-parse", "refs/tags/v1.2.3")
	git("tag", "-f", "-a", "v1.2.3", "-m", "acr v1.2.3", "-m", "Different release notes.")
	alteredObject := git("rev-parse", "refs/tags/v1.2.3")
	git("tag", "-f", "-a", "v1.2.3", "-m", "acr v1.2.3")
	noNotesObject := git("rev-parse", "refs/tags/v1.2.3")
	git("update-ref", "refs/tags/v1.2.3", approvedObject)
	git("tag", "v1.2.4")
	git("tag", "-a", "v1.2.5", "-m", "acr v1.2.5")
	git("tag", "-a", "v1.2.6", "--cleanup=verbatim", "-m", "acr v1.2.6\n\n \t\n")
	git("tag", "-a", "v1.2.7", "HEAD^", "-m", "acr v1.2.7", "-m", "Notes for a different commit.")
	git("tag", "-a", "v1.2.8/nested", "-m", "A different tag", "-m", "Notes for a different tag.")

	for _, test := range []struct {
		name   string
		tag    string
		remote guardRemote
		code   string
	}{
		{name: "notes without a changelog", tag: "v1.2.3", remote: guardRemote{tagObject: approvedObject}},
		{name: "lightweight tag with a commit body", tag: "v1.2.4", code: "release_notes_missing"},
		{name: "title only", tag: "v1.2.5", code: "release_notes_missing"},
		{name: "whitespace notes", tag: "v1.2.6", code: "release_notes_missing"},
		{name: "local tag at another commit", tag: "v1.2.7", code: "tag_commit_mismatch"},
		{name: "only similarly named tag exists", tag: "v1.2.8", code: "tag_commit_mismatch"},
		{name: "remote tag moved", tag: "v1.2.3", remote: guardRemote{tagObject: git("rev-parse", "refs/tags/v1.2.7")}, code: "tag_commit_mismatch"},
		{name: "remote annotation changed on the same commit", tag: "v1.2.3", remote: guardRemote{tagObject: alteredObject}, code: "tag_commit_mismatch"},
		{name: "remote annotation lost its notes on the same commit", tag: "v1.2.3", remote: guardRemote{tagObject: noNotesObject}, code: "tag_commit_mismatch"},
		{name: "remote tag became lightweight on the same commit", tag: "v1.2.3", remote: guardRemote{tagObject: commit}, code: "tag_commit_mismatch"},
		{name: "already published", tag: "v1.2.3", remote: guardRemote{tagObject: approvedObject, published: true}, code: "release_already_exists"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exit := run(t.Context(), &stdout, &stderr, []string{"guard", "--tag", test.tag, "--commit", commit}, test.remote)
			if test.code == "" {
				if exit != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"version":"1.2.3"`) ||
					!strings.Contains(stdout.String(), `"tagObjectSha":"`+approvedObject+`"`) {
					t.Fatalf("guard exit = %d, stdout = %q, stderr = %q", exit, stdout.String(), stderr.String())
				}
				return
			}
			if exit == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), test.code) {
				t.Fatalf("guard exit = %d, stdout = %q, stderr = %q, want refusal %s", exit, stdout.String(), stderr.String(), test.code)
			}
		})
	}

	t.Run("publisher refuses a different checkout annotation on the same commit", func(t *testing.T) {
		git("update-ref", "refs/tags/v1.2.3", alteredObject)
		var stdout, stderr bytes.Buffer
		exit := run(t.Context(), &stdout, &stderr, []string{
			"publish", "--tag", "v1.2.3", "--commit", commit, "--tag-object", approvedObject, "--assets", "release-assets",
		}, rejectingRemote{})
		if exit == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "tag_commit_mismatch") {
			t.Fatalf("publish exit = %d, stdout = %q, stderr = %q, want tag object refusal", exit, stdout.String(), stderr.String())
		}
	})
}

type guardRemote struct {
	rejectingRemote
	tagObject string
	published bool
}

func (remote guardRemote) TagObject(context.Context, dependency.Repository, string) (string, bool, error) {
	if remote.tagObject == "" {
		panic("unexpected GitHub access before local tag validation")
	}
	return remote.tagObject, true, nil
}

func (remote guardRemote) LookupRelease(context.Context, dependency.Repository, string) (dependency.Release, bool, error) {
	return dependency.Release{}, remote.published, nil
}

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

func (rejectingRemote) TagObject(context.Context, dependency.Repository, string) (string, bool, error) {
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
