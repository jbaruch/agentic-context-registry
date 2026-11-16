package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestGitRootLicensesAgreeWithValidationAndArchive(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "nested-package"}[nested], func(t *testing.T) {
			repo, root := packageRepository(t, nested)
			writeHostileFile(t, root, ".gitignore", "LICENSE-ignored\nCOPYING-force\n")
			writeHostileFile(t, root, ".gitattributes", "LICENSE text eol=crlf\n")
			notices := []string{"LICENSE", "license-MIT", "LiCeNcE.txt", "COPYING-force", "UnLiCeNsE.md"}
			for _, name := range notices {
				writeHostileFile(t, root, name, "Original notice.\r\n")
			}
			if err := os.Chmod(filepath.Join(root, "LICENSE"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeHostileFile(t, root, "LICENSE-dir/hidden", "must not ship\n")
			writeHostileFile(t, root, "unrelated/LICENSE", "must not ship\n")
			writeHostileFile(t, root, "README.md", "must not ship\n")
			// An ignored matching symlink must not be locally inspected.
			if err := os.Symlink("missing", filepath.Join(root, "LICENSE-ignored")); err != nil {
				t.Fatal(err)
			}
			runTestGit(t, root, "add", "-f", "COPYING-force")
			tagPackage(t, repo)
			want := append(slices.Clone(notices), "agent-plugin.yaml", "guidance.md", "skills/advocate/SKILL.md")
			assertInventoryArchive(t, repo, root, want)
			prepared, err := NewBuilder("test").Prepare(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			committed := gitBytes(t, root, "show", "HEAD:"+map[bool]string{false: "", true: "package/"}[nested]+"LICENSE")
			if !bytes.Equal(committed, []byte("Original notice.\n")) || !bytes.Equal(tarFileContent(t, prepared.Assets.Archive.Bytes, "plugin-1.2.3/LICENSE"), committed) {
				t.Fatalf("notice filter entered archive: %q", committed)
			}
			if got := tarFileMode(t, prepared.Assets.Archive.Bytes, "plugin-1.2.3/LICENSE"); got != 0o755 {
				t.Fatalf("committed notice mode = %o", got)
			}
			if err := os.Remove(filepath.Join(root, "LICENSE")); err != nil {
				t.Fatal(err)
			}
			list, err := PackageListing(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			wantError := `manifest validation failed: path_not_found at LICENSE: root license "LICENSE" is missing; ACR automatically includes root license files; restore the file or commit its deletion before retrying`
			if _, _, err := manifest.LoadListed(root, list); err == nil || err.Error() != wantError {
				t.Fatalf("tracked deletion not diagnosed: %v", err)
			}
			if _, err := NewBuilder("test").Prepare(context.Background(), root); err == nil || err.Error() != wantError {
				t.Fatalf("publish tracked deletion error = %v; want %s", err, wantError)
			}
		})
	}
}

func TestCommittedRootLicenseRejectsSymlinkAndGitlink(t *testing.T) {
	for _, kind := range []string{"symlink", "gitlink"} {
		t.Run(kind, func(t *testing.T) {
			repo, root := packageRepository(t, false)
			if kind == "symlink" {
				if err := os.Symlink("guidance.md", filepath.Join(root, "LICENSE")); err != nil {
					t.Fatal(err)
				}
				tagPackage(t, repo)
			} else {
				tagPackage(t, repo)
				commit := strings.TrimSpace(string(gitBytes(t, root, "rev-parse", "HEAD")))
				runTestGit(t, root, "update-index", "--add", "--cacheinfo", "160000,"+commit+",LICENSE")
				runTestGit(t, root, "commit", "-qm", "Add fixture gitlink")
			}
			value, err := manifest.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			commit := strings.TrimSpace(string(gitBytes(t, root, "rev-parse", "HEAD")))
			_, err = readTreeFiles(context.Background(), root, Identity{Commit: commit, Tag: "v1.2.3"}, value, commandGitSource{})
			var failure *Error
			want := fmt.Sprintf(`root license "LICENSE" has unsupported Git mode %s; ACR automatically includes root license files; replace it with a regular file committed to the tagged tree or rename it so it no longer matches LICENSE*, LICENCE*, COPYING*, or UNLICENSE*, then commit and retag before publication`, map[string]string{"symlink": "120000", "gitlink": "160000"}[kind])
			if !errors.As(err, &failure) || failure.Code != CodeUnpublishable || err.Error() != want {
				t.Fatalf("committed %s accepted: %v", kind, err)
			}
			if kind == "symlink" {
				want := `manifest validation failed: invalid_artifact_type at LICENSE: root license "LICENSE" is a symbolic link; ACR automatically includes root license files; replace it with a regular-file copy or rename it so it no longer matches LICENSE*, LICENCE*, COPYING*, or UNLICENSE*`
				if _, err := NewBuilder("test").Prepare(context.Background(), root); err == nil || err.Error() != want {
					t.Fatalf("publish symlink error = %v; want %s", err, want)
				}
			}
		})
	}
}

func TestLicenseArchiveAndMetadataEvidencePreserveV1Hash(t *testing.T) {
	root := t.TempDir()
	manifestBytes := "schemaVersion: 1\nname: owner/plugin\nversion: 1.2.3\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  rules:\n    - id: guidance\n      path: rules/guidance.md\n      activation:\n        mode: always\n"
	writeHostileFile(t, root, manifest.Filename, manifestBytes)
	writeHostileFile(t, root, "rules/guidance.md", "Use deterministic tests.\n")
	writeHostileFile(t, root, "LICENSE", "worktree bytes differ\n")
	writeHostileFile(t, root, "COPYING", "second notice\n")
	value, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{Tag: "v1.2.3", Commit: strings.Repeat("a", 40)}
	source := &fakeGitSource{clean: true, head: identity.Commit, tags: []string{identity.Tag}, files: map[string]File{
		manifest.Filename:   {Path: manifest.Filename, Mode: 0o644, Content: []byte(manifestBytes)},
		"rules/guidance.md": {Path: "rules/guidance.md", Mode: 0o644, Content: []byte("Use deterministic tests.\n")},
		"LICENSE":           {Path: "LICENSE", Mode: 0o755, Content: []byte("Committed notice.\x00\r\n")},
		"COPYING":           {Path: "COPYING", Mode: 0o644, Content: []byte("Second committed notice.\n")},
	}}
	ordered := &licenseSource{gitSource: source, rootEntries: []treeEntry{
		{Path: "LICENSE", Mode: "100755", OID: "LICENSE"},
		{Path: "COPYING", Mode: "100644", OID: "COPYING"},
	}}
	first, err := newBuilder(ordered, NewGate(), "test").Prepare(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(ordered.rootEntries)
	second, err := newBuilder(ordered, NewGate(), "test").Prepare(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Assets.Archive.Bytes, second.Assets.Archive.Bytes) || !bytes.Equal(first.Assets.Metadata.Bytes, second.Assets.Metadata.Bytes) {
		t.Fatal("reordered tree listings changed assets")
	}
	const frozenHash = "sha256:9ad9979f91a1f15f020e7a47af71f1f4900f8424a3d49dca68f65f603060c351"
	if first.Assets.Evidence.ContentHash != frozenHash {
		t.Fatalf("published v1 hash = %s", first.Assets.Evidence.ContentHash)
	}
	if got := tarFileContent(t, first.Assets.Archive.Bytes, "plugin-1.2.3/LICENSE"); !bytes.Equal(got, source.files["LICENSE"].Content) {
		t.Fatalf("archived notice = %q", got)
	}
	licenseDigest := sha256.Sum256(source.files["LICENSE"].Content)
	noticeIndex := slices.IndexFunc(first.Assets.Evidence.Files, func(file FileMetadata) bool { return file.Path == "LICENSE" })
	if noticeIndex < 0 {
		t.Fatal("metadata omitted LICENSE")
	}
	if got := first.Assets.Evidence.Files[noticeIndex]; got.Mode != "0755" || got.Size != int64(len(source.files["LICENSE"].Content)) || got.SHA256 != hex.EncodeToString(licenseDigest[:]) {
		t.Fatalf("notice evidence = %#v", got)
	}
	files, err := readTreeFiles(context.Background(), root, identity, value, source)
	if err != nil {
		t.Fatal(err)
	}
	legacyFiles := slices.DeleteFunc(slices.Clone(files), func(file File) bool { return file.Path == "LICENSE" || file.Path == "COPYING" })
	legacy, err := BuildReleaseAssets(value, identity, legacyFiles, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, assets := range []ReleaseAssets{legacy, first.Assets} {
		destination := t.TempDir()
		if err := dependency.ExtractPackageArchive(assets.Archive.Bytes, destination); err != nil {
			t.Fatal(err)
		}
		extracted, err := manifest.Load(destination)
		if err != nil {
			t.Fatal(err)
		}
		if hash, err := dependency.HashPackageFiles(destination, extracted); err != nil || hash != frozenHash {
			t.Fatalf("archive consumer hash = %s, %v", hash, err)
		}
	}
	file := source.files["LICENSE"]
	file.Content = []byte("Changed notice only.\n")
	source.files["LICENSE"] = file
	changed, err := newBuilder(source, NewGate(), "test").Prepare(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Assets.Evidence.ContentHash != frozenHash || bytes.Equal(first.Assets.Archive.Bytes, changed.Assets.Archive.Bytes) || bytes.Equal(first.Assets.Metadata.Bytes, changed.Assets.Metadata.Bytes) || bytes.Equal(first.Assets.Checksums.Bytes, changed.Assets.Checksums.Bytes) {
		t.Fatal("notice change did not preserve identity and change asset evidence")
	}
}

// Intercepts only root selection and its selected blob to exercise failures
// without turning unrelated declared-file reads into failures first.
type licenseSource struct {
	gitSource
	rootErr, blobErr error
	rootEntries      []treeEntry
	rootCalls        int
	recursiveRoot    bool
}

func (source *licenseSource) Tree(ctx context.Context, root, commit, relative string, recursive bool) ([]treeEntry, error) {
	if relative == "." {
		source.rootCalls++
		source.recursiveRoot = recursive
		return source.rootEntries, source.rootErr
	}
	return source.gitSource.Tree(ctx, root, commit, relative, recursive)
}

func (source *licenseSource) Blob(ctx context.Context, root, oid string) ([]byte, error) {
	if oid == "notice" {
		return []byte("notice"), source.blobErr
	}
	return source.gitSource.Blob(ctx, root, oid)
}

func TestRootLicenseTreeAndBlobFailuresPropagate(t *testing.T) {
	for _, failure := range []string{"root-list", "blob", "special-mode"} {
		t.Run(failure, func(t *testing.T) {
			base := &fakeGitSource{files: map[string]File{manifest.Filename: {Path: manifest.Filename, Mode: 0o644, Content: []byte("manifest")}}}
			source := &licenseSource{gitSource: base, rootEntries: []treeEntry{{Path: "LICENSE", Mode: "100644", OID: "notice"}, {Path: "COPYING-dir", Mode: "040000"}, {Path: "README", Mode: "120000"}}}
			wantCode := CodeGitAccess
			switch failure {
			case "root-list":
				source.rootErr = errors.New("root listing unavailable")
			case "blob":
				source.blobErr = errors.New("notice blob unavailable")
			case "special-mode":
				source.rootEntries[0].Mode = "100600"
				wantCode = CodeUnpublishable
			}
			_, err := readTreeFiles(context.Background(), ".", Identity{Commit: strings.Repeat("a", 40)}, manifest.Manifest{}, source)
			var publishErr *Error
			if !errors.As(err, &publishErr) || publishErr.Code != wantCode || source.rootCalls != 1 || source.recursiveRoot {
				t.Fatalf("%s result = %v, calls %d, recursive %t", failure, err, source.rootCalls, source.recursiveRoot)
			}
			if failure == "special-mode" {
				want := `root license "LICENSE" has unsupported Git mode 100600; ACR automatically includes root license files; replace it with a regular file committed to the tagged tree or rename it so it no longer matches LICENSE*, LICENCE*, COPYING*, or UNLICENSE*, then commit and retag before publication`
				if err.Error() != want {
					t.Fatalf("special-mode error = %v; want %s", err, want)
				}
			}
		})
	}
}
