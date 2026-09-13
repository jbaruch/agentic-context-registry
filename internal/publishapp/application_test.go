package publishapp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/publish"
)

func TestPublishJSONStdoutUncontaminated(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{tagCommit: prepared.Identity.Commit, tagExists: true}
	application := newApplication(NewService(fakePreparer{prepared: prepared}, remote), cli.UnavailableApplication{})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := cli.New(&stdout, &stderr, application, cli.Build{Version: "test"}).Run(context.Background(), []string{"publish", "--dry-run", "--json"})
	if exitCode != cli.ExitSuccess || stderr.Len() != 0 {
		t.Fatalf("exit = %d, stderr = %q", exitCode, stderr.String())
	}
	var document map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("stdout is not one JSON document: %v: %q", err, stdout.String())
	}
	if document["ok"] != true || document["command"] != "publish" {
		t.Fatalf("JSON envelope = %#v", document)
	}
}

func TestPublishExistingReleaseUsesOperationalExit(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{existing: dependency.Release{ID: 1, Tag: prepared.Identity.Tag}, exists: true, tagCommit: prepared.Identity.Commit, tagExists: true}
	application := newApplication(NewService(fakePreparer{prepared: prepared}, remote), cli.UnavailableApplication{})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := cli.New(&stdout, &stderr, application, cli.Build{Version: "test"}).Run(context.Background(), []string{"publish", "--json"})
	if exitCode != cli.ExitOperational || stdout.Len() != 0 || !bytes.Contains(stderr.Bytes(), []byte(`"code":"release_already_exists"`)) {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
	}
}

func TestValidationProjectPathPrecedence(t *testing.T) {
	root := t.TempDir()
	for _, sub := range []string{"cwd", "project", "project/nested", "absolute"} {
		dir := filepath.Join(root, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		name := strings.ReplaceAll(sub, "/", "-")
		manifest := "schemaVersion: 1\nname: example/" + name + "\nversion: 1.2.3\nsource:\n  repository: https://github.com/example/" + name + "\nartifacts:\n  skills:\n    - id: check\n      path: check\n"
		if err := os.WriteFile(filepath.Join(dir, "agent-plugin.yaml"), []byte(manifest), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, "check"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "check/SKILL.md"), []byte("# Check\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(filepath.Join(root, "cwd"))
	before := validationInventory(t, root)
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"default", nil, "cwd"},
		{"relative-path", []string{"../project"}, "project"},
		{"absolute-path", []string{filepath.Join(root, "absolute")}, "absolute"},
		{"relative-project", []string{"--project", "../project"}, "project"},
		{"absolute-project", []string{"--project", filepath.Join(root, "project")}, "project"},
		{"project-relative-path", []string{"nested", "--project", "../project"}, "project-nested"},
		{"project-absolute-path", []string{filepath.Join(root, "absolute"), "--project", "../project"}, "absolute"},
		{"invalid", []string{"missing", "--project", "../project"}, ""},
	} {
		for _, jsonOutput := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/json=%t", c.name, jsonOutput), func(t *testing.T) {
				args := append([]string{"validate"}, c.args...)
				if jsonOutput {
					args = append(args, "--json")
				}
				var stdout, stderr bytes.Buffer
				// No service, remote or fallback is supplied: any such access fails the test.
				app := newApplication(nil, nil)
				exit := cli.New(&stdout, &stderr, app, cli.Build{Version: "test"}).Run(context.Background(), args)
				if c.want == "" {
					if exit != cli.ExitOperational {
						t.Fatalf("invalid target exit=%d", exit)
					}
				} else {
					if exit != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "example/"+c.want) {
						t.Fatalf("selected package: exit=%d out=%s err=%s", exit, &stdout, &stderr)
					}
					if jsonOutput {
						var doc map[string]any
						if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
							t.Fatal(err)
						}
					}
				}
			})
		}
	}
	if !reflect.DeepEqual(before, validationInventory(t, root)) {
		t.Fatal("validation changed files or modes")
	}
}

func validationInventory(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		body := []byte(nil)
		if !entry.IsDir() {
			body, err = os.ReadFile(name)
			if err != nil {
				return err
			}
		}
		result[name] = fmt.Sprintf("%o:%s", info.Mode(), body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Exercise argv, application selection, the service and the real tagged-tree
// builder together. Every valid root can publish, so selecting the caller cannot
// hide behind a missing manifest or an argument-ignoring fake preparer.
func TestPublishProjectPathSelection(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "Publication Fixture")
	t.Setenv("GIT_AUTHOR_EMAIL", "fixture@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "Publication Fixture")
	t.Setenv("GIT_COMMITTER_EMAIL", "fixture@example.test")
	t.Setenv("GIT_AUTHOR_DATE", "2000-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2000-01-01T00:00:00Z")
	// Read-only Git inspection must not opportunistically refresh the index.
	t.Setenv("GIT_OPTIONAL_LOCKS", "0")
	for _, dir := range []string{"tmp", "state", "invalid"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", filepath.Join(root, "tmp"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	packages := map[string]publicationPackage{}
	for index, name := range []string{"caller", "caller/nested", "project", "project/nested", "absolute"} {
		packages[name] = newPublicationPackage(t, filepath.Join(root, name), strings.ReplaceAll(name, "/", "-"), fmt.Sprintf("1.0.%d", index))
	}
	if err := os.WriteFile(filepath.Join(root, "invalid", "agent-plugin.yaml"), []byte("schemaVersion: 999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(root, "caller"))
	caller, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
		want string
		code string
	}{
		{"default", nil, "caller", ""},
		{"dot", []string{"."}, "caller", ""},
		{"relative-path", []string{"nested"}, "caller/nested", ""},
		{"relative-project", []string{"--project", "../project"}, "project", ""},
		{"absolute-project", []string{"--project", filepath.Join(root, "project")}, "project", ""},
		{"dot-relative-project", []string{".", "--project", "../project"}, "project", ""},
		{"dot-absolute-project", []string{".", "--project", filepath.Join(root, "project")}, "project", ""},
		{"nested-relative-project", []string{"nested", "--project", "../project"}, "project/nested", ""},
		{"nested-absolute-project", []string{"nested", "--project", filepath.Join(root, "project")}, "project/nested", ""},
		{"absolute-path", []string{filepath.Join(root, "absolute")}, "absolute", ""},
		{"absolute-over-project", []string{filepath.Join(root, "absolute"), "--project", "../project"}, "absolute", ""},
		{"absolute-over-missing-project", []string{filepath.Join(root, "absolute"), "--project", "../missing"}, "absolute", ""},
		{"missing-project", []string{"--project", "../missing"}, "", "publish_failed"},
		{"missing-path", []string{"missing", "--project", "../project"}, "", "publish_failed"},
		{"invalid-project", []string{"--project", "../invalid"}, "", "unsupported_schema_version"},
		{"visible-release", []string{"--project", "../project"}, "project", publish.CodeReleaseExists},
	}
	for _, c := range cases {
		for _, dryRun := range []bool{true, false} {
			for _, jsonOutput := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/dry=%t/json=%t", c.name, dryRun, jsonOutput), func(t *testing.T) {
					remote := &publicationRemote{packages: packages, uploads: map[string][]byte{}}
					if c.code == publish.CodeReleaseExists {
						remote.exists = true
					}
					builder := &recordPublication{builder: publish.NewBuilder("test")}
					app := newApplication(NewService(builder, remote), cli.UnavailableApplication{})
					args := append([]string{"publish"}, c.args...)
					if dryRun {
						args = append(args, "--dry-run")
					}
					if jsonOutput {
						args = append(args, "--json")
					}
					before := publicationInventory(t, root)
					var stdout, stderr bytes.Buffer
					exit := cli.New(&stdout, &stderr, app, cli.Build{Version: "test"}).Run(context.Background(), args)
					assertPublicationInventory(t, before, publicationInventory(t, root))
					if cwd, err := os.Getwd(); err != nil || cwd != caller {
						t.Fatalf("publication changed cwd: %q, %v", cwd, err)
					}
					if c.code != "" {
						if exit != cli.ExitOperational || stdout.Len() != 0 || stderr.Len() == 0 || remote.writeCalls() != 0 {
							t.Fatalf("refusal: exit=%d out=%s err=%s writes=%d", exit, &stdout, &stderr, remote.writeCalls())
						}
						if jsonOutput {
							var envelope struct {
								OK      bool
								Command string
								Error   struct{ Code string }
							}
							if err := json.Unmarshal(stderr.Bytes(), &envelope); err != nil || envelope.OK || envelope.Command != "publish" || envelope.Error.Code != c.code {
								t.Fatalf("refusal JSON: %s (%v)", &stderr, err)
							}
						}
						if c.want == "" {
							if builder.prepared.Manifest.Name != "" || remote.lookupCalls != 0 || remote.tagCalls != 0 {
								t.Fatalf("invalid selected target fell back to another package: %#v", builder.prepared)
							}
							return
						}
					} else if exit != cli.ExitSuccess || stderr.Len() != 0 {
						t.Fatalf("publish: exit=%d out=%s err=%s prepared=%s", exit, &stdout, &stderr, builder.prepared.Manifest.Name)
					}
					want := packages[c.want]
					assertPublicationPackage(t, builder.prepared, want)
					if remote.repository.FullName() != want.name || remote.tag != want.tag {
						t.Fatalf("remote selected %s@%s, want %s@%s", remote.repository.FullName(), remote.tag, want.name, want.tag)
					}
					if c.code != "" {
						return
					}
					if jsonOutput {
						var envelope struct {
							OK      bool
							Command string
							Result  Result
						}
						if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
							t.Fatalf("success JSON: %s (%v)", &stdout, err)
						}
						got := envelope.Result
						if !envelope.OK || envelope.Command != "publish" || got.DryRun != dryRun || got.Tag != want.tag || got.Commit != want.commit || got.ContentHash != builder.prepared.Assets.Evidence.ContentHash || len(got.Assets) != 3 {
							t.Fatalf("publication result: %#v", envelope)
						}
						if !dryRun && got.ReleaseURL != "https://github.com/"+want.name+"/releases/tag/"+want.tag {
							t.Fatalf("publication URL: %q", got.ReleaseURL)
						}
						if dryRun && got.ReleaseID != 0 || !dryRun && got.ReleaseID != 2 {
							t.Fatalf("publication release ID: %#v", got)
						}
					} else {
						message := fmt.Sprintf("Published immutable release %s with 3 assets.\n", want.tag)
						if dryRun {
							message = fmt.Sprintf("Release %s is publishable with 3 assets; rerun without --dry-run to upload it.\n", want.tag)
						}
						if stdout.String() != message {
							t.Fatalf("publication text = %q, want %q", stdout.String(), message)
						}
					}
					if dryRun {
						if remote.writeCalls() != 0 || len(remote.uploads) != 0 {
							t.Fatal("preview wrote to fake remote")
						}
					} else {
						if remote.createCalls != 1 || remote.uploadCalls != 3 || remote.publishCalls != 1 || remote.deleteCalls != 0 || remote.draft || remote.commit != want.commit {
							t.Fatalf("publication did not finish only the intended fake release: %#v", remote)
						}
						for _, asset := range []publish.Asset{builder.prepared.Assets.Archive, builder.prepared.Assets.Metadata, builder.prepared.Assets.Checksums} {
							if !bytes.Equal(remote.uploads[asset.Name], asset.Bytes) {
								t.Fatalf("uploaded %s differs from selected package", asset.Name)
							}
						}
					}
				})
			}
		}
	}
}

type publicationPackage struct {
	name, version, tag, commit string
	files                      map[string]string
}

func newPublicationPackage(t *testing.T, root, name, version string) publicationPackage {
	t.Helper()
	p := publicationPackage{name: "example/" + name, version: version, tag: "v" + version}
	p.files = map[string]string{
		"agent-plugin.yaml": fmt.Sprintf("schemaVersion: 1\nname: %s\nversion: %s\nsource:\n  repository: https://github.com/%s\nartifacts:\n  rules:\n    - id: guidance\n      path: guidance.md\n      activation:\n        mode: always\n", p.name, version, p.name),
		"guidance.md":       "# Guidance for " + name + "\nPreserve this package's unique content.\n",
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range p.files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	publicationGit(t, root, "init", "--template=", "-q")
	// Each nested package is a separate valid tagged repository.
	if err := os.MkdirAll(filepath.Join(root, ".git/info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git/info/exclude"), []byte("nested/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	publicationGit(t, root, "add", "agent-plugin.yaml", "guidance.md")
	publicationGit(t, root, "-c", "commit.gpgsign=false", "commit", "-qm", "Create publication fixture")
	publicationGit(t, root, "-c", "tag.gpgsign=false", "tag", p.tag)
	p.commit = publicationGit(t, root, "rev-parse", "HEAD")
	return p
}

func publicationGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	body, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s (%v)", args, body, err)
	}
	return strings.TrimSpace(string(body))
}

type recordPublication struct {
	builder  *publish.Builder
	prepared publish.Prepared
}

func (record *recordPublication) Prepare(ctx context.Context, root string) (publish.Prepared, error) {
	prepared, err := record.builder.Prepare(ctx, root)
	record.prepared = prepared
	return prepared, err
}

// All valid fixture repositories have pushed tags in this fake remote. A wrong
// selection therefore reaches successful preparation/publication, exposing the
// wrong identity rather than failing for unrelated missing remote state.
type publicationRemote struct {
	fakeReleases
	packages    map[string]publicationPackage
	repository  dependency.Repository
	tag, commit string
	uploads     map[string][]byte
}

func (remote *publicationRemote) LookupRelease(ctx context.Context, repository dependency.Repository, tag string) (dependency.Release, bool, error) {
	remote.repository, remote.tag = repository, tag
	return remote.fakeReleases.LookupRelease(ctx, repository, tag)
}

func (remote *publicationRemote) TagCommit(_ context.Context, repository dependency.Repository, tag string) (string, bool, error) {
	remote.tagCalls++
	for _, p := range remote.packages {
		if repository.FullName() == p.name && tag == p.tag {
			return p.commit, true, nil
		}
	}
	return "", false, nil
}

func (remote *publicationRemote) CreateRelease(ctx context.Context, repository dependency.Repository, tag, commit string) (dependency.Release, error) {
	remote.repository, remote.tag, remote.commit = repository, tag, commit
	return remote.fakeReleases.CreateRelease(ctx, repository, tag, commit)
}

func (remote *publicationRemote) UploadAsset(ctx context.Context, repository dependency.Repository, id int64, name, contentType string, body []byte) (dependency.ReleaseAsset, []byte, error) {
	remote.uploads[name] = append([]byte(nil), body...)
	return remote.fakeReleases.UploadAsset(ctx, repository, id, name, contentType, body)
}

func (remote *publicationRemote) PublishRelease(ctx context.Context, repository dependency.Repository, id int64) (dependency.Release, error) {
	result, err := remote.fakeReleases.PublishRelease(ctx, repository, id)
	result.Tag, result.Target = remote.tag, remote.commit
	result.HTMLURL = "https://github.com/" + repository.FullName() + "/releases/tag/" + remote.tag
	return result, err
}

func assertPublicationPackage(t *testing.T, prepared publish.Prepared, want publicationPackage) {
	t.Helper()
	metadata := prepared.Assets.Evidence
	if prepared.Manifest.Name != want.name || metadata.Name != want.name || metadata.Version != want.version || metadata.Tag != want.tag || metadata.Commit != want.commit || metadata.Repository != "https://github.com/"+want.name || metadata.ContentHash == "" {
		t.Fatalf("prepared package %s@%s (%s), want %s@%s (%s)", metadata.Name, metadata.Tag, metadata.Commit, want.name, want.tag, want.commit)
	}
	var decoded publish.Metadata
	if err := json.Unmarshal(prepared.Assets.Metadata.Bytes, &decoded); err != nil || !reflect.DeepEqual(decoded, metadata) {
		t.Fatalf("prepared metadata differs: %v", err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(prepared.Assets.Archive.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	archive := tar.NewReader(reader)
	got := map[string]string{}
	prefix := strings.TrimPrefix(want.name, "example/") + "-" + want.version + "/"
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(header.Name, prefix) || header.Mode != 0o644 {
			t.Fatalf("unexpected archive entry: %#v", header)
		}
		body, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		got[strings.TrimPrefix(header.Name, prefix)] = string(body)
	}
	if !reflect.DeepEqual(got, want.files) {
		t.Fatalf("archive content = %#v, want %#v", got, want.files)
	}
}

type publicationFile struct {
	info fs.FileInfo
	body string
}

func publicationInventory(t *testing.T, root string) map[string]publicationFile {
	t.Helper()
	files := map[string]publicationFile{}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var body string
		if info.Mode()&os.ModeSymlink != 0 {
			body, err = os.Readlink(name)
		} else if !entry.IsDir() {
			var content []byte
			content, err = os.ReadFile(name)
			body = string(content)
		}
		if err != nil {
			return err
		}
		files[name] = publicationFile{info: info, body: body}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func assertPublicationInventory(t *testing.T, before, after map[string]publicationFile) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("publication changed path count: %d -> %d", len(before), len(after))
	}
	for name, a := range before {
		b, exists := after[name]
		if !exists || a.info.Mode() != b.info.Mode() || a.body != b.body || !os.SameFile(a.info, b.info) {
			t.Errorf("publication changed path, bytes, mode, link or inode: %s", name)
		}
	}
}
