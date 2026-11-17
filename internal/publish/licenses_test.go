package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
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
			var failure *manifest.ValidationErrors
			want := `manifest validation failed: invalid_artifact_type at LICENSE: root license "LICENSE" is a symbolic link; ACR automatically includes root license files; replace it with a regular-file copy or rename it so it no longer matches LICENSE*, LICENCE*, COPYING*, or UNLICENSE*`
			if kind == "gitlink" {
				want = `manifest validation failed: invalid_artifact_type at LICENSE: cannot inspect root license "LICENSE": unsupported Git mode 160000; replace it with a regular file committed to the tagged tree, then commit and retag before publication; ACR automatically includes root license files; make the file accessible before retrying`
			}
			if !errors.As(err, &failure) || failure.Issues[0].Code != manifest.CodeInvalidArtifactType || err.Error() != want {
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

			}
			_, err := readTreeFiles(context.Background(), ".", Identity{Commit: strings.Repeat("a", 40)}, manifest.Manifest{}, source)
			var publishErr *Error
			var validationErr *manifest.ValidationErrors
			validError := errors.As(err, &publishErr) && publishErr.Code == wantCode
			if failure == "special-mode" {
				validError = errors.As(err, &validationErr) && validationErr.Issues[0].Code == manifest.CodeInvalidArtifactType
			}
			if !validError || source.rootCalls != 1 || source.recursiveRoot {
				t.Fatalf("%s result = %v, calls %d, recursive %t", failure, err, source.rootCalls, source.recursiveRoot)
			}
			if failure == "special-mode" {
				want := `manifest validation failed: invalid_artifact_type at LICENSE: cannot inspect root license "LICENSE": unsupported Git mode 100600; replace it with a regular file committed to the tagged tree, then commit and retag before publication; ACR automatically includes root license files; make the file accessible before retrying`
				if err.Error() != want {
					t.Fatalf("special-mode error = %v; want %s", err, want)
				}
			}
		})
	}
}

func TestRootLicenseValidationAndPublishRefuseIdentically(t *testing.T) {
	for _, tc := range []struct {
		label string
		names []string
		mode  fs.FileMode
		code  manifest.ErrorCode
	}{
		{"backslash", []string{"LICENSE\\MIT"}, 0o644, manifest.CodeInvalidPath},
		{"NUL-provider", []string{"LICENSE\x00MIT"}, 0o644, manifest.CodeInvalidPath},
		{"case-alias-provider", []string{"LICENSE", "license"}, 0o644, manifest.CodeInvalidPath},
		{"unicode-case-alias-provider", []string{"LICENSE-K", "license-\u212a"}, 0o644, manifest.CodeInvalidPath},
		{"normalization-alias-provider", []string{"LICENSE-caf\u00e9", "LICENSE-cafe\u0301"}, 0o644, manifest.CodeInvalidPath},
		{"symlink", []string{"LICENSE"}, fs.ModeSymlink, manifest.CodeInvalidArtifactType},
		{"special-file", []string{"LICENSE"}, fs.ModeNamedPipe, manifest.CodeInvalidArtifactType},
	} {
		t.Run(tc.label, func(t *testing.T) {
			root := t.TempDir()
			manifestBytes := skillManifestYAML("guidance.md")
			files := map[string]File{
				manifest.Filename:          {Path: manifest.Filename, Mode: 0o644, Content: []byte(manifestBytes)},
				"guidance.md":              {Path: "guidance.md", Mode: 0o644, Content: []byte("# Guidance\n")},
				"skills/advocate/SKILL.md": {Path: "skills/advocate/SKILL.md", Mode: 0o644, Content: []byte("# Advocate\n")},
			}
			base := &fakeGitSource{inside: true, clean: true, head: strings.Repeat("a", 40), tags: []string{"v1.2.3"}, files: files}
			for name, file := range files {
				writeHostileFile(t, root, name, string(file.Content))
				base.visible = append(base.visible, name)
			}
			slices.Sort(base.visible)
			var rootEntries []treeEntry
			var archiveFiles []File
			for _, name := range tc.names {
				// Alias and NUL cases use provider records so host filesystem folding
				// cannot erase one of the deliberately conflicting names.
				if len(tc.names) == 1 && !strings.ContainsRune(name, '\x00') {
					switch tc.mode {
					case fs.ModeSymlink:
						if err := os.Symlink("guidance.md", filepath.Join(root, name)); err != nil {
							t.Fatal(err)
						}
					case fs.ModeNamedPipe:
						if out, err := exec.Command("mkfifo", filepath.Join(root, name)).CombinedOutput(); err != nil {
							t.Fatalf("mkfifo: %s: %v", out, err)
						}
					default:
						writeHostileFile(t, root, name, "notice")
					}
				}
				base.visible = append(base.visible, name)
				file := File{Path: name, Mode: tc.mode, Content: []byte("notice")}
				files[name] = file
				archiveFiles = append(archiveFiles, file)
				mode := "100644"
				if tc.mode == fs.ModeSymlink {
					mode = "120000"
				}
				rootEntries = append(rootEntries, treeEntry{Path: name, Mode: mode, OID: name})
			}
			source := &licenseSource{gitSource: base, rootEntries: rootEntries}
			list, err := packageListing(context.Background(), root, source)
			if err != nil {
				t.Fatal(err)
			}
			_, _, validationErr := manifest.LoadListed(root, list)
			_, prepareErr := newBuilder(source, NewGate(), "test").Prepare(context.Background(), root)
			value, err := manifest.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			_, committedErr := readTreeFiles(context.Background(), root, Identity{Commit: base.head, Tag: "v1.2.3"}, value, source)
			_, archiveErr := BuildArchive("plugin", "1.2.3", archiveFiles)
			_, _, contentErr := describeContent(value, archiveFiles)
			errorsToCheck := []error{validationErr, prepareErr, archiveErr, contentErr}
			// Git never stores FIFOs; direct archive/content construction supplies
			// the special-file metadata, while the local preflight checks the FIFO.
			if tc.mode != fs.ModeNamedPipe {
				errorsToCheck = append(errorsToCheck, committedErr)
			}
			for _, err := range errorsToCheck {
				var failure *manifest.ValidationErrors
				if !errors.As(err, &failure) || len(failure.Issues) != 1 || failure.Issues[0].Code != tc.code || failure.Issues[0].Field != tc.names[0] || err.Error() != validationErr.Error() {
					t.Fatalf("validation/publish disagree: %v; validation = %v", err, validationErr)
				}
				if tc.code == manifest.CodeInvalidPath && !strings.Contains(failure.Issues[0].Message, "LICENSE-MIT, then commit and retag") {
					t.Fatalf("missing repair guidance: %v", err)
				}
			}
		})
	}
}

func TestRootLicenseArchiveAllowsExistingPathContract(t *testing.T) {
	for _, name := range []string{"LICENSE\x01MIT", "LICENSE\nMIT", "LICENSE\x7fMIT", "LICENSE-caf\u00e9", "LICENSE-cafe\u0301", "LICENSE-" + strings.Repeat("long", 45)} {
		t.Run(name, func(t *testing.T) {
			content := []byte("Notice bytes.\x00\r\n")
			archive, err := BuildArchive("plugin", "1.2.3", []File{{Path: name, Mode: 0o755, Content: content}})
			if err != nil {
				t.Fatal(err)
			}
			if got := tarFileContent(t, archive.Bytes, "plugin-1.2.3/"+name); !bytes.Equal(got, content) {
				t.Fatalf("notice changed: %q", got)
			}
			if got := tarFileMode(t, archive.Bytes, "plugin-1.2.3/"+name); got != 0o755 {
				t.Fatalf("mode = %o", got)
			}
		})
	}
}
