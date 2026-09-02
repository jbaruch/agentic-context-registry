package publishapp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/publish"
)

func TestPublishRefusesPublishedRelease(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{existing: dependency.Release{ID: 1, Tag: prepared.Identity.Tag}, exists: true, tagCommit: prepared.Identity.Commit, tagExists: true}
	_, err := NewService(fakePreparer{prepared: prepared}, remote).Publish(context.Background(), ".", false)
	assertPublishCode(t, err, publish.CodeReleaseExists)
	if remote.writeCalls() != 0 {
		t.Fatalf("immutable refusal performed %d writes", remote.writeCalls())
	}
}

func TestPublishRefusesTagAtDifferentCommit(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{tagCommit: strings.Repeat("b", 40), tagExists: true}
	_, err := NewService(fakePreparer{prepared: prepared}, remote).Publish(context.Background(), ".", false)
	assertPublishCode(t, err, publish.CodeTagCommit)
	if remote.writeCalls() != 0 {
		t.Fatalf("tag mismatch performed %d writes", remote.writeCalls())
	}
}

func TestPublishRefusesTagNotPushed(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{}
	_, err := NewService(fakePreparer{prepared: prepared}, remote).Publish(context.Background(), ".", false)
	assertPublishCode(t, err, publish.CodeTagNotPushed)
	if remote.writeCalls() != 0 {
		t.Fatalf("missing tag performed %d writes", remote.writeCalls())
	}
}

func TestPublishRefusesForeignDraft(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{
		existing: dependency.Release{ID: 1, Tag: prepared.Identity.Tag, Draft: true, Assets: []dependency.ReleaseAsset{{Name: "notes.txt"}}},
		exists:   true, tagCommit: prepared.Identity.Commit, tagExists: true,
	}
	_, err := NewService(fakePreparer{prepared: prepared}, remote).Publish(context.Background(), ".", false)
	assertPublishCode(t, err, publish.CodeForeignDraft)
	if remote.writeCalls() != 0 {
		t.Fatalf("foreign draft refusal performed %d writes", remote.writeCalls())
	}
}

func TestPublishReusesOwnStaleDraft(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{
		existing:   dependency.Release{ID: 1, Tag: prepared.Identity.Tag, Draft: true, Assets: []dependency.ReleaseAsset{{ID: 10, Name: publish.MetadataAssetName}}},
		exists:     true,
		tagCommit:  prepared.Identity.Commit,
		tagExists:  true,
		assetBytes: prepared.Assets.Metadata.Bytes,
	}
	result, err := NewService(fakePreparer{prepared: prepared}, remote).Publish(context.Background(), ".", false)
	if err != nil {
		t.Fatal(err)
	}
	if remote.assetDownloadCalls != 1 || remote.deleteCalls != 1 || remote.createCalls != 1 || remote.uploadCalls != 3 || remote.publishCalls != 1 {
		t.Fatalf("release calls = asset download %d delete %d create %d upload %d publish %d", remote.assetDownloadCalls, remote.deleteCalls, remote.createCalls, remote.uploadCalls, remote.publishCalls)
	}
	if result.ReleaseID != 2 || result.ReleaseURL == "" {
		t.Fatalf("Publish() result = %#v", result)
	}
}

func TestPublishRefusesUnverifiedStaleDraft(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	tests := []struct {
		name       string
		assets     []dependency.ReleaseAsset
		assetBytes []byte
		assetErr   error
	}{
		{name: "empty"},
		{name: "allowed names without marker", assets: []dependency.ReleaseAsset{{ID: 1, Name: prepared.Assets.Archive.Name}}},
		{name: "mismatched marker", assets: []dependency.ReleaseAsset{{ID: 1, Name: publish.MetadataAssetName}}, assetBytes: []byte("foreign metadata")},
		{name: "unavailable marker", assets: []dependency.ReleaseAsset{{ID: 1, Name: publish.MetadataAssetName}}, assetErr: errors.New("download failed")},
		{name: "duplicate marker", assets: []dependency.ReleaseAsset{{ID: 1, Name: publish.MetadataAssetName}, {ID: 2, Name: publish.MetadataAssetName}}, assetBytes: prepared.Assets.Metadata.Bytes},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			remote := &fakeReleases{
				existing: dependency.Release{ID: 1, Tag: prepared.Identity.Tag, Draft: true, Assets: test.assets},
				exists:   true, tagCommit: prepared.Identity.Commit, tagExists: true,
				assetBytes: test.assetBytes, assetErr: test.assetErr,
			}
			_, err := NewService(fakePreparer{prepared: prepared}, remote).Publish(context.Background(), ".", false)
			assertPublishCode(t, err, publish.CodeForeignDraft)
			if remote.writeCalls() != 0 {
				t.Fatalf("unverified draft performed %d writes", remote.writeCalls())
			}
		})
	}
}

func TestFailedPublishUploadsNothingVisible(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{tagCommit: prepared.Identity.Commit, tagExists: true, uploadErrorAt: 2}
	_, err := NewService(fakePreparer{prepared: prepared}, remote).Publish(context.Background(), ".", false)
	assertPublishCode(t, err, publish.CodeReleaseUpload)
	if remote.createCalls != 1 || remote.uploadCalls != 2 || remote.publishCalls != 0 || !remote.draft {
		t.Fatalf("failed upload state = create %d upload %d publish %d draft %t", remote.createCalls, remote.uploadCalls, remote.publishCalls, remote.draft)
	}
}

func TestPublishRefusesLostCreateRace(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{
		tagCommit: prepared.Identity.Commit, tagExists: true,
		createErr: &dependency.GitHubAPIError{StatusCode: http.StatusUnprocessableEntity, Message: "already exists"},
	}
	_, err := NewService(fakePreparer{prepared: prepared}, remote).Publish(context.Background(), ".", false)
	assertPublishCode(t, err, publish.CodeReleaseExists)
	if remote.createCalls != 1 || remote.uploadCalls != 0 || remote.publishCalls != 0 {
		t.Fatalf("create race writes = create %d upload %d publish %d", remote.createCalls, remote.uploadCalls, remote.publishCalls)
	}
}

func TestPublishKeepsDraftWhenRemoteBytesDiffer(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{tagCommit: prepared.Identity.Commit, tagExists: true, corruptAt: 2}
	_, err := NewService(fakePreparer{prepared: prepared}, remote).Publish(context.Background(), ".", false)
	assertPublishCode(t, err, publish.CodeReleaseUpload)
	if remote.uploadCalls != 2 || remote.publishCalls != 0 || !remote.draft {
		t.Fatalf("digest mismatch state = upload %d publish %d draft %t", remote.uploadCalls, remote.publishCalls, remote.draft)
	}
}

func TestPublishKeepsDraftWhenTagMovesBeforePublication(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{
		tagCommits: []string{prepared.Identity.Commit, strings.Repeat("b", 40)},
		tagExists:  true,
	}
	_, err := NewService(fakePreparer{prepared: prepared}, remote).Publish(context.Background(), ".", false)
	assertPublishCode(t, err, publish.CodeTagCommit)
	if remote.tagCalls != 2 || remote.createCalls != 1 || remote.uploadCalls != 3 || remote.publishCalls != 0 || !remote.draft {
		t.Fatalf("moved-tag state = tag %d create %d upload %d publish %d draft %t", remote.tagCalls, remote.createCalls, remote.uploadCalls, remote.publishCalls, remote.draft)
	}
}

func TestPublishDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{tagCommit: prepared.Identity.Commit, tagExists: true}
	result, err := NewService(fakePreparer{prepared: prepared}, remote).Publish(context.Background(), ".", true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || len(result.Assets) != 3 || remote.writeCalls() != 0 {
		t.Fatalf("dry-run result = %#v, writes = %d", result, remote.writeCalls())
	}
}

func TestPublishGateFailurePrecedesGitHubAccess(t *testing.T) {
	t.Parallel()

	remote := &fakeReleases{}
	_, err := NewService(fakePreparer{err: &publish.Error{Code: publish.CodeAdapterRealization, Message: "gate failed"}}, remote).Publish(context.Background(), ".", false)
	assertPublishCode(t, err, publish.CodeAdapterRealization)
	if remote.lookupCalls != 0 || remote.tagCalls != 0 || remote.writeCalls() != 0 {
		t.Fatalf("GitHub accessed before gate: %#v", remote)
	}
}

func assertPublishCode(t *testing.T, err error, want string) {
	t.Helper()
	var publishErr *publish.Error
	if !errors.As(err, &publishErr) || publishErr.Code != want {
		t.Fatalf("error = %#v, want publish code %s", err, want)
	}
}

type fakePreparer struct {
	prepared publish.Prepared
	err      error
}

func (fake fakePreparer) Prepare(context.Context, string) (publish.Prepared, error) {
	return fake.prepared, fake.err
}

type fakeReleases struct {
	existing           dependency.Release
	exists             bool
	tagCommit          string
	tagCommits         []string
	tagExists          bool
	lookupCalls        int
	tagCalls           int
	createCalls        int
	uploadCalls        int
	publishCalls       int
	deleteCalls        int
	uploadErrorAt      int
	corruptAt          int
	createErr          error
	assetBytes         []byte
	assetErr           error
	assetDownloadCalls int
	draft              bool
}

func (fake *fakeReleases) LookupRelease(context.Context, dependency.Repository, string) (dependency.Release, bool, error) {
	fake.lookupCalls++
	return fake.existing, fake.exists, nil
}

func (fake *fakeReleases) DownloadReleaseAsset(context.Context, dependency.Repository, dependency.ReleaseAsset) ([]byte, error) {
	fake.assetDownloadCalls++
	return append([]byte(nil), fake.assetBytes...), fake.assetErr
}

func (fake *fakeReleases) TagCommit(context.Context, dependency.Repository, string) (string, bool, error) {
	fake.tagCalls++
	if fake.tagCalls <= len(fake.tagCommits) {
		return fake.tagCommits[fake.tagCalls-1], fake.tagExists, nil
	}
	return fake.tagCommit, fake.tagExists, nil
}

func (fake *fakeReleases) CreateRelease(_ context.Context, _ dependency.Repository, tag, commit string) (dependency.Release, error) {
	fake.createCalls++
	if fake.createErr != nil {
		return dependency.Release{}, fake.createErr
	}
	fake.draft = true
	return dependency.Release{ID: 2, Tag: tag, Target: commit, Draft: true}, nil
}

func (fake *fakeReleases) UploadAsset(_ context.Context, _ dependency.Repository, _ int64, name, _ string, contents []byte) (dependency.ReleaseAsset, []byte, error) {
	fake.uploadCalls++
	if fake.uploadErrorAt == fake.uploadCalls {
		return dependency.ReleaseAsset{}, nil, errors.New("injected upload failure")
	}
	if fake.corruptAt == fake.uploadCalls {
		return dependency.ReleaseAsset{ID: int64(fake.uploadCalls), Name: name}, []byte("corrupt"), nil
	}
	return dependency.ReleaseAsset{ID: int64(fake.uploadCalls), Name: name}, append([]byte(nil), contents...), nil
}

func (fake *fakeReleases) PublishRelease(_ context.Context, _ dependency.Repository, releaseID int64) (dependency.Release, error) {
	fake.publishCalls++
	fake.draft = false
	return dependency.Release{ID: releaseID, Tag: "v1.0.0", HTMLURL: "https://github.com/example/all-agents/releases/tag/v1.0.0"}, nil
}

func (fake *fakeReleases) DeleteRelease(context.Context, dependency.Repository, int64) error {
	fake.deleteCalls++
	return nil
}

func (fake *fakeReleases) writeCalls() int {
	return fake.createCalls + fake.uploadCalls + fake.publishCalls + fake.deleteCalls
}

func fixturePrepared(t *testing.T) publish.Prepared {
	t.Helper()
	value := manifest.Manifest{
		SchemaVersion: 1, Name: "example/all-agents", Version: "1.0.0",
		Source:    manifest.Source{Repository: "https://github.com/example/all-agents"},
		Artifacts: manifest.Artifacts{Rules: []manifest.RuleArtifact{{ID: "guidance", Path: "guidance.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}}},
	}
	identity := publish.Identity{Tag: "v1.0.0", Commit: strings.Repeat("a", 40)}
	assets, err := publish.BuildReleaseAssets(value, identity, []publish.File{
		{Path: manifest.Filename, Mode: 0o644, Content: []byte("schemaVersion: 1\n")},
		{Path: "guidance.md", Mode: 0o644, Content: []byte("guidance\n")},
	}, []adapter.Descriptor{{ID: "fixture", Version: "1.0.0", Boundary: 1}}, "acr test")
	if err != nil {
		t.Fatal(err)
	}
	return publish.Prepared{Manifest: value, Identity: identity, Assets: assets}
}
