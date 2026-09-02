package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestHostileArchiveIsByteIdentical(t *testing.T) {
	t.Parallel()

	files := hostileArchiveFiles()
	first, err := BuildArchive("plugin", "1.2.3", files)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("twoRuns", func(t *testing.T) {
		t.Parallel()
		second, err := BuildArchive("plugin", "1.2.3", files)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first.Bytes, second.Bytes) || first.TarSHA256 != second.TarSHA256 {
			t.Fatal("two builds of the same tree produced different archives")
		}
	})

	t.Run("shuffledWalk", func(t *testing.T) {
		t.Parallel()
		reversed := append([]File(nil), files...)
		for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
			reversed[left], reversed[right] = reversed[right], reversed[left]
		}
		second, err := BuildArchive("plugin", "1.2.3", reversed)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first.Bytes, second.Bytes) {
			t.Fatal("shuffled input order changed archive bytes")
		}
		if got := tarEntryNames(t, second.Bytes); strings.Join(got, ",") != "plugin-1.2.3/agent-plugin.yaml,plugin-1.2.3/rules/a-first.md,plugin-1.2.3/rules/z-last.md,plugin-1.2.3/scripts/check.sh" {
			t.Fatalf("tar names = %q, want PackageFiles lexicographic order", got)
		}
	})

	t.Run("crlf", func(t *testing.T) {
		t.Parallel()
		content := tarFileContent(t, first.Bytes, "plugin-1.2.3/scripts/check.sh")
		if !bytes.Contains(content, []byte("\r\n")) || bytes.Contains(content, []byte("#!/bin/sh\n")) {
			t.Fatalf("archive rewrote CRLF content: %q", content)
		}
		second, err := BuildArchive("plugin", "1.2.3", files)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first.Bytes, second.Bytes) {
			t.Fatal("CRLF fixture was not byte-identical across two builds")
		}
	})

	t.Run("mtimeOwnerIgnored", func(t *testing.T) {
		t.Parallel()
		for _, header := range tarHeaders(t, first.Bytes) {
			if header.Typeflag != tar.TypeReg {
				t.Fatalf("entry %q type = %q, want regular file", header.Name, header.Typeflag)
			}
			if !header.ModTime.Equal(time.Unix(0, 0).UTC()) || !header.AccessTime.IsZero() || !header.ChangeTime.IsZero() {
				t.Fatalf("entry %q timestamps = mtime %v atime %v ctime %v", header.Name, header.ModTime, header.AccessTime, header.ChangeTime)
			}
			if header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" {
				t.Fatalf("entry %q owners = uid %d gid %d uname %q gname %q", header.Name, header.Uid, header.Gid, header.Uname, header.Gname)
			}
			if strings.Contains(header.Name, "\\") {
				t.Fatalf("entry %q used a non-POSIX path", header.Name)
			}
		}
	})
}

func TestHostileArchivePreservesExecuteBit(t *testing.T) {
	t.Parallel()

	manifestBytes := []byte("schemaVersion: 1\nname: owner/plugin\nversion: 1.2.3\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  scripts:\n    - id: check\n      path: scripts/check.sh\n")
	script := []byte("#!/bin/sh\r\nexit 0\r\n")
	value := manifest.Manifest{
		SchemaVersion: 1, Name: "owner/plugin", Version: "1.2.3",
		Source:    manifest.Source{Repository: "https://github.com/owner/plugin"},
		Artifacts: manifest.Artifacts{Scripts: []manifest.ScriptArtifact{{ID: "check", Path: "scripts/check.sh"}}},
	}
	regular := []File{
		{Path: manifest.Filename, Mode: 0o644, Content: manifestBytes},
		{Path: "scripts/check.sh", Mode: 0o644, Content: script},
	}
	executable := []File{
		{Path: manifest.Filename, Mode: 0o644, Content: manifestBytes},
		{Path: "scripts/check.sh", Mode: 0o755, Content: script},
	}
	regularAssets, err := BuildReleaseAssets(value, Identity{Tag: "v1.2.3", Commit: strings.Repeat("a", 40)}, regular, []adapter.Descriptor{{ID: "fixture", Version: "1.0.0", Boundary: 1}}, "acr test")
	if err != nil {
		t.Fatal(err)
	}
	executableAssets, err := BuildReleaseAssets(value, Identity{Tag: "v1.2.3", Commit: strings.Repeat("a", 40)}, executable, []adapter.Descriptor{{ID: "fixture", Version: "1.0.0", Boundary: 1}}, "acr test")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(regularAssets.Archive.Bytes, executableAssets.Archive.Bytes) {
		t.Fatal("0644 and 0755 produced identical archives")
	}
	if regularAssets.Evidence.ContentHash == executableAssets.Evidence.ContentHash {
		t.Fatal("0644 and 0755 produced identical content hashes")
	}
	if tarFileMode(t, executableAssets.Archive.Bytes, "plugin-1.2.3/scripts/check.sh") != 0o755 {
		t.Fatal("executable entry lost the 0755 mode")
	}
	if tarFileMode(t, regularAssets.Archive.Bytes, "plugin-1.2.3/agent-plugin.yaml") != 0o644 {
		t.Fatal("rule-equivalent file lost the 0644 mode")
	}

	destination := t.TempDir()
	if err := dependency.ExtractPackageArchive(executableAssets.Archive.Bytes, destination); err != nil {
		t.Fatal(err)
	}
	extracted, err := manifest.Load(destination)
	if err != nil {
		t.Fatal(err)
	}
	contentHash, err := dependency.HashPackageFiles(destination, extracted)
	if err != nil {
		t.Fatal(err)
	}
	if contentHash != executableAssets.Evidence.ContentHash {
		t.Fatalf("consumer hash = %s, published %s", contentHash, executableAssets.Evidence.ContentHash)
	}
}

func TestHostileArchiveRejectsEscapingPath(t *testing.T) {
	t.Parallel()

	_, err := BuildArchive("plugin", "1.2.3", []File{
		{Path: "agent-plugin.yaml", Mode: 0o644, Content: []byte("schemaVersion: 1\n")},
		{Path: "../outside.md", Mode: 0o644, Content: []byte("escaped\n")},
	})
	if err == nil || !strings.Contains(err.Error(), "../outside.md") {
		t.Fatalf("BuildArchive() error = %v, want escaping path refusal", err)
	}
}

func TestHostileTagVersionAgreement(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)
	tests := []struct {
		name    string
		tag     string
		version string
		want    string
	}{
		{name: "tagAhead", tag: "v1.2.4", version: "1.2.3", want: CodeTagVersion},
		{name: "manifestAhead", tag: "v1.2.3", version: "1.2.4", want: CodeTagVersion},
		{name: "vPrefix", tag: "v1.2.3", version: "1.2.3"},
		{name: "bareTag", tag: "1.2.3", version: "1.2.3"},
		{name: "doubleV", tag: "vv1.2.3", version: "1.2.3", want: CodeTagVersion},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			identity, err := resolveIdentity(context.Background(), ".", test.version, &fakeGitSource{clean: true, head: commit, tags: []string{test.tag}})
			if test.want == "" {
				if err != nil || identity.Tag != test.tag || identity.Commit != commit {
					t.Fatalf("resolveIdentity() = %#v, %v", identity, err)
				}
				return
			}
			var publishErr *Error
			if !errors.As(err, &publishErr) || publishErr.Code != test.want {
				t.Fatalf("resolveIdentity() error = %#v, want %s", err, test.want)
			}
		})
	}
}

func TestHostilePublishRejectsSymlinkAndOmitsUndeclared(t *testing.T) {
	t.Parallel()

	t.Run("declared", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeHostileFile(t, root, manifest.Filename, hostileManifestYAML("1.2.3", "rules/link.md"))
		writeHostileFile(t, root, "rules/target.md", "# Target\n")
		if err := os.Symlink("target.md", filepath.Join(root, "rules", "link.md")); err != nil {
			t.Fatal(err)
		}
		_, err := newBuilder(&fakeGitSource{clean: true, head: strings.Repeat("a", 40), tags: []string{"v1.2.3"}}, newGate(), "acr test").Prepare(context.Background(), root)
		assertManifestCode(t, err, manifest.CodeInvalidArtifactType)
	})

	t.Run("insideSkill", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeHostileFile(t, root, manifest.Filename, `schemaVersion: 1
name: owner/plugin
version: 1.2.3
source:
  repository: https://github.com/owner/plugin
artifacts:
  skills:
    - id: review
      path: skills/review
`)
		writeHostileFile(t, root, "skills/review/SKILL.md", "# Review\n")
		if err := os.Symlink("SKILL.md", filepath.Join(root, "skills", "review", "notes.md")); err != nil {
			t.Fatal(err)
		}
		_, err := newBuilder(&fakeGitSource{clean: true, head: strings.Repeat("a", 40), tags: []string{"v1.2.3"}}, newGate(), "acr test").Prepare(context.Background(), root)
		assertManifestCode(t, err, manifest.CodeInvalidSkillTree)
	})

	t.Run("undeclaredOmitted", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		manifestBytes := []byte(hostileManifestYAML("1.2.3", "rules/guidance.md"))
		guidance := []byte("# Guidance\n")
		writeHostileFile(t, root, manifest.Filename, string(manifestBytes))
		writeHostileFile(t, root, "rules/guidance.md", string(guidance))
		if err := os.Symlink("guidance.md", filepath.Join(root, "rules", "extra.md")); err != nil {
			t.Fatal(err)
		}
		source := &fakeGitSource{
			clean: true, head: strings.Repeat("a", 40), tags: []string{"v1.2.3"},
			files: map[string]File{
				manifest.Filename:   {Path: manifest.Filename, Mode: 0o644, Content: manifestBytes},
				"rules/guidance.md": {Path: "rules/guidance.md", Mode: 0o644, Content: guidance},
			},
		}
		prepared, err := newBuilder(source, newGate(), "acr test").Prepare(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range tarEntryNames(t, prepared.Assets.Archive.Bytes) {
			if strings.HasSuffix(name, "/extra.md") {
				t.Fatalf("undeclared symlink leaked into archive as %q", name)
			}
			header := tarHeaderNamed(t, prepared.Assets.Archive.Bytes, name)
			if header.Typeflag != tar.TypeReg {
				t.Fatalf("archive entry %q type = %q", name, header.Typeflag)
			}
		}
	})
}

func TestHostilePublishRefusesMissingOrEscapingFile(t *testing.T) {
	t.Parallel()

	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeHostileFile(t, root, manifest.Filename, hostileManifestYAML("1.2.3", "rules/missing.md"))
		_, err := newBuilder(&fakeGitSource{clean: true, head: strings.Repeat("a", 40), tags: []string{"v1.2.3"}}, newGate(), "acr test").Prepare(context.Background(), root)
		assertManifestCode(t, err, manifest.CodePathNotFound)
	})

	t.Run("escaping", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeHostileFile(t, root, manifest.Filename, hostileManifestYAML("1.2.3", "../outside.md"))
		writeHostileFile(t, root, "rules/guidance.md", "# Guidance\n")
		_, err := newBuilder(&fakeGitSource{clean: true, head: strings.Repeat("a", 40), tags: []string{"v1.2.3"}}, newGate(), "acr test").Prepare(context.Background(), root)
		assertManifestCode(t, err, manifest.CodeInvalidPath)
	})
}

func TestHostileSymlinkModeDoesNotCreateLinkEntry(t *testing.T) {
	t.Parallel()

	result, err := BuildArchive("plugin", "1.2.3", []File{
		{Path: "agent-plugin.yaml", Mode: fs.ModeSymlink | 0o644, Content: []byte("schemaVersion: 1\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	headers := tarHeaders(t, result.Bytes)
	if len(headers) != 1 || headers[0].Typeflag != tar.TypeReg {
		t.Fatalf("headers = %#v, want one regular file", headers)
	}
}

func hostileArchiveFiles() []File {
	return []File{
		{Path: "scripts/check.sh", Mode: 0o755, Content: []byte("#!/bin/sh\r\nexit 0\r\n")},
		{Path: "rules/z-last.md", Mode: 0o644, Content: []byte("z\n")},
		{Path: "agent-plugin.yaml", Mode: 0o644, Content: []byte("schemaVersion: 1\n")},
		{Path: "rules/a-first.md", Mode: 0o644, Content: []byte("a\n")},
	}
}

func hostileManifestYAML(version, rulePath string) string {
	return "schemaVersion: 1\nname: owner/plugin\nversion: " + version + "\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  rules:\n    - id: guidance\n      path: " + rulePath + "\n      activation:\n        mode: always\n"
}

func writeHostileFile(t *testing.T, root, relative, content string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertManifestCode(t *testing.T, err error, want manifest.ErrorCode) {
	t.Helper()
	var validation *manifest.ValidationErrors
	if !errors.As(err, &validation) || !validation.Has(want) {
		t.Fatalf("error = %#v, want manifest code %s", err, want)
	}
}

func tarEntryNames(t *testing.T, archive []byte) []string {
	t.Helper()
	var names []string
	for _, header := range tarHeaders(t, archive) {
		names = append(names, header.Name)
	}
	return names
}

func tarFileContent(t *testing.T, archive []byte, name string) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			t.Fatalf("archive omitted %q", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == name {
			content, err := io.ReadAll(tarReader)
			if err != nil {
				t.Fatal(err)
			}
			return content
		}
	}
}

func tarFileMode(t *testing.T, archive []byte, name string) int64 {
	t.Helper()
	return tarHeaderNamed(t, archive, name).Mode
}

func tarHeaderNamed(t *testing.T, archive []byte, name string) *tar.Header {
	t.Helper()
	for _, header := range tarHeaders(t, archive) {
		if header.Name == name {
			return header
		}
	}
	t.Fatalf("archive omitted %q", name)
	return nil
}

func tarHeaders(t *testing.T, archive []byte) []*tar.Header {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tarReader := tar.NewReader(reader)
	var headers []*tar.Header
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return headers
		}
		if err != nil {
			t.Fatal(err)
		}
		copied := *header
		headers = append(headers, &copied)
	}
}
