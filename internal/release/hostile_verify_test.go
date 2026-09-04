package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"go.yaml.in/yaml/v3"
)

func TestHostileReleaseArchiveIsByteIdentical(t *testing.T) {
	t.Parallel()

	first := fixtureBundle(t, false)
	second := fixtureBundle(t, true)
	if len(first.Archives) != 4 {
		t.Fatalf("archives = %d, want 4", len(first.Archives))
	}
	for index, asset := range first.Archives {
		if asset.Name != second.Archives[index].Name || !bytes.Equal(asset.Bytes, second.Archives[index].Bytes) {
			t.Fatalf("archive %q changed across two packs", asset.Name)
		}
		if !bytes.Equal(gzipPayload(t, asset.Bytes), gzipPayload(t, second.Archives[index].Bytes)) {
			t.Fatalf("tar stream for %q changed across two packs", asset.Name)
		}
		assertHostileArchiveShape(t, asset.Bytes)
	}
	if !bytes.Equal(first.Checksums.Bytes, second.Checksums.Bytes) {
		t.Fatal("checksums.txt changed across two packs")
	}
}

func TestHostileChecksumsManifestShapeAndEncoding(t *testing.T) {
	t.Parallel()

	bundle := fixtureBundle(t, true)
	manifest := bundle.Checksums.Bytes
	if bytes.Contains(manifest, []byte("sha256:")) {
		t.Fatal("checksums.txt used package contentHash encoding")
	}
	if !bytes.Equal(manifest, Checksums(bundle.Archives)) {
		t.Fatal("checksums.txt is not the canonical sorted digest of the packed archives")
	}
	lines := strings.Split(strings.TrimSuffix(string(manifest), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("checksums lines = %d, want 4", len(lines))
	}
	previous := ""
	seen := map[string]string{}
	for index, line := range lines {
		if len(line) < 67 || line[64:66] != "  " || strings.Contains(line, "\t") {
			t.Fatalf("checksum line %d = %q, want lowercase hex, two spaces, POSIX filename", index, line)
		}
		digest, name := line[:64], line[66:]
		if decoded, err := hex.DecodeString(digest); err != nil || len(decoded) != sha256.Size || digest != strings.ToLower(digest) {
			t.Fatalf("checksum digest %q is not 64 lowercase hex characters", digest)
		}
		if name != bundle.Archives[index].Name || strings.Contains(name, "\\") || strings.Contains(name, "/") {
			t.Fatalf("checksum filename %q is not the POSIX archive name %q", name, bundle.Archives[index].Name)
		}
		if previous != "" && name <= previous {
			t.Fatalf("checksums.txt is not lexicographically sorted: %q after %q", name, previous)
		}
		sum := sha256.Sum256(bundle.Archives[index].Bytes)
		if digest != hex.EncodeToString(sum[:]) {
			t.Fatalf("checksum for %q does not match archive bytes", name)
		}
		seen[name] = digest
		previous = name
	}
	if len(seen) != 4 {
		t.Fatalf("checksum filenames = %#v", seen)
	}

	digests, err := ParseArchiveDigests(manifest)
	if err != nil || len(digests) != 4 {
		t.Fatalf("ParseArchiveDigests() = %#v, %v", digests, err)
	}
	uppercase := bytes.ToUpper(manifest[:64])
	uppercase = append(uppercase, manifest[64:]...)
	if _, err := ParseArchiveDigests(uppercase); err == nil {
		t.Fatal("ParseArchiveDigests() accepted uppercase hex")
	}
	singleSpace := bytes.Replace(manifest, []byte("  "), []byte(" "), 1)
	if _, err := ParseArchiveDigests(singleSpace); err == nil {
		t.Fatal("ParseArchiveDigests() accepted a single-space sha256sum line")
	}

	asset := bundle.Archives[0]
	if _, err := VerifyArchiveChecksum(asset.Name, asset.Bytes, manifest); err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), asset.Bytes...)
	tampered[len(tampered)/2] ^= 0xff
	if _, err := VerifyArchiveChecksum(asset.Name, tampered, manifest); err == nil || !strings.Contains(err.Error(), "discard") {
		t.Fatalf("tampered archive error = %v", err)
	}
	rewritten := bytes.Replace(manifest, []byte(asset.Name), []byte("acr-missing.tar.gz"), 1)
	if _, err := VerifyArchiveChecksum(asset.Name, asset.Bytes, rewritten); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("sidecar mismatch error = %v", err)
	}
}

func TestHostileReleaseAssetNaming(t *testing.T) {
	t.Parallel()

	bundle := fixtureBundle(t, false)
	want := []string{
		"acr-darwin-amd64.tar.gz",
		"acr-darwin-arm64.tar.gz",
		"acr-linux-amd64.tar.gz",
		"acr-linux-arm64.tar.gz",
	}
	if len(bundle.Archives) != len(want) {
		t.Fatalf("archives = %d, want 4", len(bundle.Archives))
	}
	checksumLines := strings.Split(strings.TrimSuffix(string(bundle.Checksums.Bytes), "\n"), "\n")
	digests := map[string]struct{}{}
	for index, asset := range bundle.Archives {
		if asset.Name != want[index] {
			t.Fatalf("archive %d name = %q, want %q", index, asset.Name, want[index])
		}
		for _, forbidden := range []string{"windows", "x86_64", "aarch64", "macos"} {
			if strings.Contains(asset.Name, forbidden) {
				t.Fatalf("archive name %q contains %q", asset.Name, forbidden)
			}
		}
		names := hostileTarNames(t, asset.Bytes)
		if strings.Join(names, ",") != "LICENSE,acr" {
			t.Fatalf("archive %q entries = %q, want LICENSE,acr", asset.Name, names)
		}
		lineDigest := strings.Fields(checksumLines[index])[0]
		if _, duplicate := digests[lineDigest]; duplicate {
			t.Fatalf("checksum digest %s is shared by multiple archives", lineDigest)
		}
		digests[lineDigest] = struct{}{}
	}

	windows := []Binary{{Target: Target{GOOS: "windows", GOARCH: "amd64"}, Bytes: []byte("windows")}}
	if _, err := Pack(append(fixtureBinaries(), windows...), []byte("license\n")); err == nil || !strings.Contains(err.Error(), "windows") {
		t.Fatalf("Pack() windows error = %v", err)
	}
}

func TestHostileStampedBinaryMatchesTag(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	output := t.TempDir()
	stamped := filepath.Join(output, "acr-stamped")
	unstamped := filepath.Join(output, "acr-unstamped")
	const version = "1.2.3"
	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ldflags := "-s -w -X main.version=" + version + " -X main.commit=" + commit
	command := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", stamped, "./cmd/acr")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build stamped acr: %v\n%s", err, output)
	}
	command = exec.Command("go", "build", "-o", unstamped, "./cmd/acr")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build unstamped acr: %v\n%s", err, output)
	}

	stampedVersion := runVersionJSON(t, stamped)
	if !dependency.TagMatchesVersion("v"+version, stampedVersion.Version) || !dependency.TagMatchesVersion(version, stampedVersion.Version) || stampedVersion.Commit != commit {
		t.Fatalf("stamped version = %#v", stampedVersion)
	}
	if dependency.TagMatchesVersion("v1.2.4", stampedVersion.Version) {
		t.Fatal("TagMatchesVersion accepted a version that does not match the tag")
	}
	unstampedVersion := runVersionJSON(t, unstamped)
	if unstampedVersion.Version == version || unstampedVersion.Commit == commit || dependency.TagMatchesVersion("v"+version, unstampedVersion.Version) {
		t.Fatalf("unstamped version = %#v, collided with the release stamp", unstampedVersion)
	}

	source := string(releaseWorkflow(t))
	for _, required := range []string{
		`if [[ "v${reported_version}" != "${TAG}" || "${reported_commit}" != "${COMMIT}" ]]; then`,
		"-X main.version=${VERSION} -X main.commit=${COMMIT}",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("workflow omits tag/commit agreement %q", required)
		}
	}
}

func TestHostileGuardNeverOverwritesPublishedRelease(t *testing.T) {
	t.Parallel()

	original := []dependency.ReleaseAsset{{ID: 42, Name: "acr-darwin-amd64.tar.gz", URL: "https://example.invalid/acr-darwin-amd64.tar.gz"}}
	published := dependency.Release{ID: 7, Tag: "v1.2.3", Assets: append([]dependency.ReleaseAsset(nil), original...)}
	t.Run("nonDraft", func(t *testing.T) {
		t.Parallel()
		remote := &fakeRemote{tagCommit: fixtureCommit, tagExists: true, exists: true, release: published}
		_, err := Publish(context.Background(), remote, fixtureRepository(), "v1.2.3", fixtureCommit, fixtureReleaseAssets(t))
		assertReleaseCode(t, err, CodeReleaseExists)
		if remote.createCalls != 0 || remote.uploadCalls != 0 || remote.deleteCalls != 0 || remote.publishCalls != 0 {
			t.Fatalf("Publish() writes = create %d upload %d delete %d publish %d", remote.createCalls, remote.uploadCalls, remote.deleteCalls, remote.publishCalls)
		}
		if len(remote.release.Assets) != 1 || remote.release.Assets[0] != original[0] {
			t.Fatalf("existing assets changed: %#v", remote.release.Assets)
		}
	})
	t.Run("prerelease", func(t *testing.T) {
		t.Parallel()
		release := published
		release.Prerelease = true
		remote := &fakeRemote{tagCommit: fixtureCommit, tagExists: true, exists: true, release: release}
		_, err := Guard(context.Background(), remote, fixtureRepository(), "v1.2.3", fixtureCommit)
		assertReleaseCode(t, err, CodeReleaseExists)
		if remote.writeCalls() != 0 {
			t.Fatalf("Guard() performed %d writes against a prerelease", remote.writeCalls())
		}
	})
	t.Run("createRace", func(t *testing.T) {
		t.Parallel()
		remote := &fakeRemote{
			tagCommit: fixtureCommit, tagExists: true,
			createErr: &dependency.GitHubAPIError{StatusCode: http.StatusUnprocessableEntity, Message: "already exists"},
		}
		_, err := Publish(context.Background(), remote, fixtureRepository(), "v1.2.3", fixtureCommit, fixtureReleaseAssets(t))
		assertReleaseCode(t, err, CodeReleaseExists)
		if remote.createCalls != 1 || remote.uploadCalls != 0 || remote.deleteCalls != 0 || remote.publishCalls != 0 {
			t.Fatalf("Publish() writes = create %d upload %d delete %d publish %d", remote.createCalls, remote.uploadCalls, remote.deleteCalls, remote.publishCalls)
		}
	})
	t.Run("bareTag", func(t *testing.T) {
		t.Parallel()
		remote := &fakeRemote{tagCommit: fixtureCommit, tagExists: true}
		_, err := Guard(context.Background(), remote, fixtureRepository(), "1.2.3", fixtureCommit)
		assertReleaseCode(t, err, CodeTagCommit)
		if remote.writeCalls() != 0 || remote.tagCalls != 0 || remote.lookupCalls != 0 {
			t.Fatalf("Guard() probed GitHub for a non-canonical tag: %#v", remote)
		}
	})
}

func TestHostileFormulaPinsPublishedChecksums(t *testing.T) {
	t.Parallel()

	bundle := fixtureBundle(t, true)
	digests, err := ParseArchiveDigests(bundle.Checksums.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	formula, err := RenderFormula("1.2.3", digests)
	if err != nil {
		t.Fatal(err)
	}
	source := string(formula)
	for _, digest := range digests {
		url := "https://github.com/jbaruch/agentic-context-registry/releases/download/v1.2.3/" + digest.Target.Name()
		if !strings.Contains(source, url) {
			t.Errorf("formula omits pinned URL %s", url)
		}
		if !strings.Contains(source, `sha256 "`+digest.SHA256+`"`) {
			t.Errorf("formula omits checksum %s for %s", digest.SHA256, digest.Target.Name())
		}
	}
	for _, forbidden := range []string{"windows", "x86_64", "aarch64", "v1.2.3-rc", "releases/latest/download", "aaaaaaaaaaaaaaaa"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("formula contains %q", forbidden)
		}
	}
	if _, err := RenderFormula("1.2.3-rc.1", digests); err == nil {
		t.Fatal("RenderFormula() accepted a prerelease")
	}
}

func TestHostileReleaseWorkflowContract(t *testing.T) {
	t.Parallel()

	contents := releaseWorkflow(t)
	var workflow struct {
		On          map[string]any `yaml:"on"`
		Permissions map[string]any `yaml:"permissions"`
		Concurrency struct {
			CancelInProgress bool `yaml:"cancel-in-progress"`
		} `yaml:"concurrency"`
		Jobs map[string]struct {
			Needs       any            `yaml:"needs"`
			Permissions map[string]any `yaml:"permissions"`
			Strategy    struct {
				Matrix any `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}
	if _, hasDispatch := workflow.On["workflow_dispatch"]; hasDispatch {
		t.Fatal("release workflow accepts workflow_dispatch inputs")
	}
	if _, hasPR := workflow.On["pull_request"]; hasPR {
		t.Fatal("release workflow triggers on pull_request")
	}
	if _, hasMain := workflow.On["workflow_call"]; hasMain {
		t.Fatal("release workflow is reusable")
	}
	push, _ := workflow.On["push"].(map[string]any)
	tags, _ := push["tags"].([]any)
	if len(workflow.On) != 1 || len(tags) != 1 || tags[0] != "v*" {
		t.Fatalf("workflow trigger = %#v, want tag-only v*", workflow.On)
	}
	if len(workflow.Permissions) != 0 || workflow.Concurrency.CancelInProgress {
		t.Fatalf("workflow root permissions/concurrency = %#v", workflow)
	}
	releaseJob := workflow.Jobs["release"]
	if releaseJob.Permissions["contents"] != "write" || releaseJob.Permissions["id-token"] != "write" || releaseJob.Permissions["attestations"] != "write" {
		t.Fatalf("release permissions = %#v", releaseJob.Permissions)
	}
	source := string(contents)
	for _, required := range []string{
		"--tag \"${GITHUB_REF_NAME}\"",
		"--commit \"${GITHUB_SHA}\"",
		"VERSION: ${{ needs.guard.outputs.version }}",
		"needs: [guard, build, verify]",
		"cancel-in-progress: false",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("workflow omits identity input %q", required)
		}
	}
	for _, forbidden := range []string{"pull_request_target", "workflow_call", "acr publish", "acr-package.json", "branches: [main]"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("workflow contains forbidden %q", forbidden)
		}
	}
}

func TestHostileWorkflowNeverPrintsSecrets(t *testing.T) {
	t.Parallel()

	contents := releaseWorkflow(t)
	var workflow map[string]any
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}
	jobs, _ := workflow["jobs"].(map[string]any)
	secretPattern := regexp.MustCompile(`(?i)(GH_TOKEN|GITHUB_TOKEN|HOMEBREW_TAP_DEPLOY_KEY|COSIGN_|APPLE_|NOTARY_|\$\{\{\s*secrets\.)`)
	for jobName, jobValue := range jobs {
		job, _ := jobValue.(map[string]any)
		steps, _ := job["steps"].([]any)
		for index, stepValue := range steps {
			step, _ := stepValue.(map[string]any)
			script, _ := step["run"].(string)
			if script == "" {
				continue
			}
			if strings.Contains(script, "set -x") || strings.Contains(script, "printenv") {
				t.Errorf("%s step %d prints the process environment", jobName, index)
			}
			for _, line := range strings.Split(script, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "#") {
					continue
				}
				if (strings.Contains(trimmed, "echo") || strings.Contains(trimmed, "printf")) && secretPattern.MatchString(trimmed) {
					t.Errorf("%s step %d logs a secret: %s", jobName, index, trimmed)
				}
				if strings.Contains(trimmed, "${{ secrets.") {
					t.Errorf("%s step %d interpolates a secret into a log line: %s", jobName, index, trimmed)
				}
			}
		}
	}
	source := string(contents)
	if strings.Contains(source, "echo ${{ secrets.") || strings.Contains(source, "printenv") || strings.Contains(source, "set -x") {
		t.Fatal("workflow prints secret material")
	}
}

func TestHostileInstallDocsMatchReleaseContract(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	docs := map[string]string{}
	for _, path := range []string{
		filepath.Join("docs", "install.md"),
		filepath.Join("docs", "cli.md"),
		"README.md",
	} {
		contents, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		docs[path] = string(contents)
	}
	install := docs[filepath.Join("docs", "install.md")]
	workflow := string(releaseWorkflow(t))
	for _, asset := range []string{
		"acr-darwin-amd64.tar.gz",
		"acr-darwin-arm64.tar.gz",
		"acr-linux-amd64.tar.gz",
		"acr-linux-arm64.tar.gz",
	} {
		if !strings.Contains(install, asset) {
			t.Errorf("docs/install.md omits %q", asset)
		}
	}
	for _, required := range []string{
		"brew install jbaruch/acr/acr",
		"Homebrew is the recommended installation path on macOS and Linux",
		"## Direct download (supported, not recommended)",
		"this supported path has no upgrade path",
		"checksums.txt",
		"checksums.txt.sigstore.json",
		"cosign verify-blob",
		"gh attestation verify",
		"go install github.com/jbaruch/agentic-context-registry/cmd/acr@v1.2.3",
		"pins the version requested and has no upgrade path",
		"supported macOS install paths are Homebrew",
		"a browser download is not a supported install path",
		`-ldflags "-s -w -X main.version=$version -X main.commit=$commit"`,
	} {
		if !strings.Contains(install, required) {
			t.Errorf("docs/install.md omits %q", required)
		}
	}
	for _, required := range []string{
		"CGO_ENABLED=0",
		"go build -trimpath -ldflags",
		"cosign sign-blob --yes",
		"actions/attest-build-provenance@",
		"brew install --formula",
		"macos-latest, ubuntu-latest",
		"jbaruch/homebrew-acr",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("workflow omits documented install path %q", required)
		}
	}
	for _, forbidden := range []string{"codesign", "notarytool", "staple", "spctl"} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("workflow claims deferred Apple signing via %q", forbidden)
		}
	}
	for _, forbidden := range []string{"acr-windows-", "acr-macos-", "v1.2.3-rc"} {
		if strings.Contains(install, forbidden) {
			t.Errorf("docs/install.md advertises %q", forbidden)
		}
	}
}

func assertHostileArchiveShape(t *testing.T, archive []byte) {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if reader.Name != "" || reader.Comment != "" || !reader.ModTime.IsZero() {
		t.Fatalf("gzip header is not normalized: %#v", reader.Header)
	}
	tarReader := tar.NewReader(reader)
	want := []struct {
		name string
		mode int64
	}{{name: "LICENSE", mode: 0o644}, {name: "acr", mode: 0o755}}
	for index, expected := range want {
		header, err := tarReader.Next()
		if err != nil {
			t.Fatalf("entry %d: %v", index, err)
		}
		if header.Name != expected.name || header.Mode != expected.mode || header.Typeflag != tar.TypeReg {
			t.Fatalf("entry %d = %#v", index, header)
		}
		if !header.ModTime.Equal(time.Unix(0, 0).UTC()) || !header.AccessTime.IsZero() || !header.ChangeTime.IsZero() {
			t.Fatalf("entry %q timestamps = mtime %v atime %v ctime %v", header.Name, header.ModTime, header.AccessTime, header.ChangeTime)
		}
		if header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" {
			t.Fatalf("entry %q owners = uid %d gid %d uname %q gname %q", header.Name, header.Uid, header.Gid, header.Uname, header.Gname)
		}
	}
	if _, err := tarReader.Next(); err != io.EOF {
		t.Fatalf("unexpected trailing entry: %v", err)
	}
}

func gzipPayload(t *testing.T, archive []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	payload, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func hostileTarNames(t *testing.T, archive []byte) []string {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tarReader := tar.NewReader(reader)
	var names []string
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
}

func runVersionJSON(t *testing.T, binary string) struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
} {
	t.Helper()
	command := exec.Command(binary, "version", "--json")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("run %s version --json: %v, stderr = %q", binary, err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("%s version --json stderr = %q", binary, stderr.String())
	}
	var envelope struct {
		OK      bool   `json:"ok"`
		Command string `json:"command"`
		Result  struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %s version JSON %q: %v", binary, stdout.String(), err)
	}
	if !envelope.OK || envelope.Command != "version" {
		t.Fatalf("%s version envelope = %#v", binary, envelope)
	}
	return envelope.Result
}
