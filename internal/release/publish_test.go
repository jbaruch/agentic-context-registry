package release

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

const fixtureCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestGuardRefusesPublishedTag(t *testing.T) {
	t.Parallel()

	remote := &fakeRemote{tagCommit: fixtureCommit, tagExists: true, exists: true, release: dependency.Release{ID: 1, Tag: "v1.2.3"}}
	_, err := Guard(context.Background(), remote, fixtureRepository(), "v1.2.3", fixtureCommit)
	assertReleaseCode(t, err, CodeReleaseExists)
	if remote.writeCalls() != 0 {
		t.Fatalf("Guard() performed %d writes", remote.writeCalls())
	}
}

func TestGuardRefusesTagAtDifferentCommit(t *testing.T) {
	t.Parallel()

	remote := &fakeRemote{tagCommit: strings.Repeat("b", 40), tagExists: true}
	_, err := Guard(context.Background(), remote, fixtureRepository(), "v1.2.3", fixtureCommit)
	assertReleaseCode(t, err, CodeTagCommit)
	if remote.lookupCalls != 0 || remote.writeCalls() != 0 {
		t.Fatalf("Guard() calls = lookup %d, writes %d", remote.lookupCalls, remote.writeCalls())
	}
}

func TestPublishRefusesForeignDraft(t *testing.T) {
	t.Parallel()

	remote := &fakeRemote{
		tagCommit: fixtureCommit, tagExists: true, exists: true,
		release: dependency.Release{ID: 1, Tag: "v1.2.3", Draft: true, Target: fixtureCommit, Assets: []dependency.ReleaseAsset{{Name: "notes.txt"}}},
	}
	_, err := Publish(context.Background(), remote, fixtureRepository(), "v1.2.3", fixtureCommit, fixtureReleaseAssets(t))
	assertReleaseCode(t, err, CodeForeignDraft)
	if remote.writeCalls() != 0 {
		t.Fatalf("Publish() performed %d writes", remote.writeCalls())
	}
}

func TestPublishRefusesDraftAtDifferentCommit(t *testing.T) {
	t.Parallel()

	remote := &fakeRemote{
		tagCommit: fixtureCommit, tagExists: true, exists: true,
		release: dependency.Release{
			ID: 1, Tag: "v1.2.3", Draft: true, Target: strings.Repeat("b", 40),
			Assets: []dependency.ReleaseAsset{{Name: ChecksumsAssetName}},
		},
	}
	_, err := Publish(context.Background(), remote, fixtureRepository(), "v1.2.3", fixtureCommit, fixtureReleaseAssets(t))
	assertReleaseCode(t, err, CodeForeignDraft)
	if remote.writeCalls() != 0 {
		t.Fatalf("Publish() performed %d writes", remote.writeCalls())
	}
}

func TestPublishReusesOwnStaleDraft(t *testing.T) {
	t.Parallel()

	remote := &fakeRemote{
		tagCommit: fixtureCommit, tagExists: true, exists: true,
		release: dependency.Release{ID: 1, Tag: "v1.2.3", Draft: true, Target: fixtureCommit, Assets: []dependency.ReleaseAsset{{Name: ChecksumsAssetName}}},
	}
	result, err := Publish(context.Background(), remote, fixtureRepository(), "v1.2.3", fixtureCommit, fixtureReleaseAssets(t))
	if err != nil {
		t.Fatal(err)
	}
	if remote.deleteCalls != 1 || remote.createCalls != 1 || remote.uploadCalls != 7 || remote.publishCalls != 1 {
		t.Fatalf("Publish() calls = delete %d create %d upload %d publish %d", remote.deleteCalls, remote.createCalls, remote.uploadCalls, remote.publishCalls)
	}
	if len(result.Assets) != 7 || result.ReleaseID != 2 || result.ReleaseURL == "" {
		t.Fatalf("Publish() result = %#v", result)
	}
}

func TestTornUploadStaysDraft(t *testing.T) {
	t.Parallel()

	remote := &fakeRemote{tagCommit: fixtureCommit, tagExists: true, uploadErrorAt: 4}
	_, err := Publish(context.Background(), remote, fixtureRepository(), "v1.2.3", fixtureCommit, fixtureReleaseAssets(t))
	assertReleaseCode(t, err, CodeReleaseUpload)
	if remote.createCalls != 1 || remote.uploadCalls != 4 || remote.publishCalls != 0 || !remote.draft {
		t.Fatalf("Publish() state = create %d upload %d publish %d draft %t", remote.createCalls, remote.uploadCalls, remote.publishCalls, remote.draft)
	}
}

func TestPublishKeepsDraftWhenRemoteBytesDiffer(t *testing.T) {
	t.Parallel()

	remote := &fakeRemote{tagCommit: fixtureCommit, tagExists: true, corruptAt: 4}
	_, err := Publish(context.Background(), remote, fixtureRepository(), "v1.2.3", fixtureCommit, fixtureReleaseAssets(t))
	assertReleaseCode(t, err, CodeReleaseUpload)
	if remote.uploadCalls != 4 || remote.publishCalls != 0 || !remote.draft {
		t.Fatalf("Publish() state = upload %d publish %d draft %t", remote.uploadCalls, remote.publishCalls, remote.draft)
	}
}

func TestPublishKeepsDraftWhenTagMovesBeforePublication(t *testing.T) {
	t.Parallel()

	remote := &fakeRemote{
		tagCommit: fixtureCommit, tagCommits: []string{fixtureCommit, strings.Repeat("b", 40)}, tagExists: true,
	}
	_, err := Publish(context.Background(), remote, fixtureRepository(), "v1.2.3", fixtureCommit, fixtureReleaseAssets(t))
	assertReleaseCode(t, err, CodeTagCommit)
	if remote.tagCalls != 2 || remote.uploadCalls != 7 || remote.publishCalls != 0 || !remote.draft {
		t.Fatalf("Publish() state = tag %d upload %d publish %d draft %t", remote.tagCalls, remote.uploadCalls, remote.publishCalls, remote.draft)
	}
}

func TestPublishRefusesLostCreateRace(t *testing.T) {
	t.Parallel()

	remote := &fakeRemote{
		tagCommit: fixtureCommit, tagExists: true,
		createErr: &dependency.GitHubAPIError{StatusCode: http.StatusUnprocessableEntity, Message: "already exists"},
	}
	_, err := Publish(context.Background(), remote, fixtureRepository(), "v1.2.3", fixtureCommit, fixtureReleaseAssets(t))
	assertReleaseCode(t, err, CodeReleaseExists)
	if remote.createCalls != 1 || remote.uploadCalls != 0 || remote.publishCalls != 0 {
		t.Fatalf("Publish() calls = create %d upload %d publish %d", remote.createCalls, remote.uploadCalls, remote.publishCalls)
	}
}

func TestReleaseAssetContractIsExactlySeven(t *testing.T) {
	t.Parallel()

	assets := fixtureReleaseAssets(t)
	ordered, err := validateCompleteAssets("1.2.3", assets)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != 7 {
		t.Fatalf("release assets = %d, want 7", len(ordered))
	}
	partial := append([]Asset(nil), assets[:len(assets)-1]...)
	if _, err := validateCompleteAssets("1.2.3", partial); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("partial asset error = %v", err)
	}
}

func fixtureReleaseAssets(t *testing.T) []Asset {
	t.Helper()
	bundle := fixtureBundle(t, true)
	assets := append([]Asset(nil), bundle.Archives...)
	assets = append(assets, bundle.Checksums)
	assets = append(assets,
		Asset{Name: SignatureAssetName, ContentType: "application/json", Bytes: []byte(`{"verificationMaterial":{}}`)},
		Asset{Name: SBOMAssetName, ContentType: "application/json", Bytes: []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","metadata":{"component":{"name":"acr","version":"1.2.3"}}}`)},
	)
	return assets
}

func fixtureRepository() dependency.Repository {
	return dependency.Repository{Owner: "jbaruch", Name: "agentic-context-registry"}
}

func assertReleaseCode(t *testing.T, err error, code string) {
	t.Helper()
	if !IsCode(err, code) {
		t.Fatalf("error = %#v, want release code %s", err, code)
	}
}

type fakeRemote struct {
	tagCommit     string
	tagCommits    []string
	tagExists     bool
	exists        bool
	release       dependency.Release
	lookupCalls   int
	tagCalls      int
	createCalls   int
	uploadCalls   int
	publishCalls  int
	deleteCalls   int
	uploadErrorAt int
	corruptAt     int
	createErr     error
	draft         bool
}

func (fake *fakeRemote) LookupRelease(context.Context, dependency.Repository, string) (dependency.Release, bool, error) {
	fake.lookupCalls++
	return fake.release, fake.exists, nil
}

func (fake *fakeRemote) TagCommit(context.Context, dependency.Repository, string) (string, bool, error) {
	fake.tagCalls++
	if fake.tagCalls <= len(fake.tagCommits) {
		return fake.tagCommits[fake.tagCalls-1], fake.tagExists, nil
	}
	return fake.tagCommit, fake.tagExists, nil
}

func (fake *fakeRemote) CreateRelease(_ context.Context, _ dependency.Repository, tag, commit string) (dependency.Release, error) {
	fake.createCalls++
	if fake.createErr != nil {
		return dependency.Release{}, fake.createErr
	}
	fake.draft = true
	return dependency.Release{ID: 2, Tag: tag, Target: commit, Draft: true}, nil
}

func (fake *fakeRemote) UploadAsset(_ context.Context, _ dependency.Repository, _ int64, name, _ string, contents []byte) (dependency.ReleaseAsset, []byte, error) {
	fake.uploadCalls++
	if fake.uploadCalls == fake.uploadErrorAt {
		return dependency.ReleaseAsset{}, nil, errors.New("injected upload failure")
	}
	if fake.uploadCalls == fake.corruptAt {
		return dependency.ReleaseAsset{ID: int64(fake.uploadCalls), Name: name}, []byte("corrupt"), nil
	}
	return dependency.ReleaseAsset{ID: int64(fake.uploadCalls), Name: name}, append([]byte(nil), contents...), nil
}

func (fake *fakeRemote) PublishRelease(_ context.Context, _ dependency.Repository, releaseID int64) (dependency.Release, error) {
	fake.publishCalls++
	fake.draft = false
	return dependency.Release{ID: releaseID, Tag: "v1.2.3", HTMLURL: "https://github.com/jbaruch/agentic-context-registry/releases/tag/v1.2.3"}, nil
}

func (fake *fakeRemote) DeleteRelease(context.Context, dependency.Repository, int64) error {
	fake.deleteCalls++
	return nil
}

func (fake *fakeRemote) writeCalls() int {
	return fake.createCalls + fake.uploadCalls + fake.publishCalls + fake.deleteCalls
}
