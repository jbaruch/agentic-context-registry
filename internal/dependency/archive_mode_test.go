package dependency

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// modeFile is one archive entry with an explicit permission bit set.
type modeFile struct {
	content string
	mode    int64
}

// modeArchive builds a single-root archive whose entries carry the supplied
// permissions verbatim.
func modeArchive(t *testing.T, root string, files map[string]modeFile) []byte {
	t.Helper()
	var result bytes.Buffer
	gzipWriter := gzip.NewWriter(&result)
	tarWriter := tar.NewWriter(gzipWriter)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file := files[name]
		header := &tar.Header{Name: root + "/" + name, Typeflag: tar.TypeReg, Mode: file.mode, Size: int64(len(file.content))}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(file.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

const modeManifest = "schemaVersion: 1\nname: owner/plugin\nversion: 1.2.3\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  rules:\n    - id: guidance\n      path: rules/guidance.md\n      activation:\n        mode: always\n  scripts:\n    - id: report\n      path: scripts/report.sh\n"

// TestPackageIdentityIgnoresArchiveGroupWriteBits pins the identity contract
// that GitHub source tarballs broke: GitHub serves 0664 / 0775 where every
// publisher records 0644 / 0755, so a content hash computed from the source
// tree never matched the hash in the release metadata and every install
// refused after the archive was read.
func TestPackageIdentityIgnoresArchiveGroupWriteBits(t *testing.T) {
	t.Parallel()

	repository := Repository{Owner: "owner", Name: "plugin"}
	publisher := map[string]modeFile{
		"agent-plugin.yaml": {modeManifest, 0o644},
		"rules/guidance.md": {"Use deterministic tests.\n", 0o644},
		"scripts/report.sh": {"#!/bin/sh\necho report\n", 0o755},
	}
	github := map[string]modeFile{
		"agent-plugin.yaml": {modeManifest, 0o664},
		"rules/guidance.md": {"Use deterministic tests.\n", 0o664},
		"scripts/report.sh": {"#!/bin/sh\necho report\n", 0o775},
	}

	normalized, err := verifyPackageArchive(modeArchive(t, "owner-plugin-abc1234", publisher), repository)
	if err != nil {
		t.Fatalf("verifyPackageArchive(publisher) error = %v", err)
	}
	served, err := verifyPackageArchive(modeArchive(t, "owner-plugin-abc1234", github), repository)
	if err != nil {
		t.Fatalf("verifyPackageArchive(github) error = %v", err)
	}
	if normalized != served {
		t.Fatalf("verified packages differ: publisher = %#v, github = %#v", normalized, served)
	}
}

// TestExtractPackageArchiveNormalizesModes keeps the extracted tree at the two
// modes package identity distinguishes, so realization never carries a
// group-write bit into native output.
func TestExtractPackageArchiveNormalizesModes(t *testing.T) {
	t.Parallel()

	archive := modeArchive(t, "owner-plugin-abc1234", map[string]modeFile{
		"agent-plugin.yaml": {modeManifest, 0o664},
		"rules/guidance.md": {"Use deterministic tests.\n", 0o664},
		"scripts/report.sh": {"#!/bin/sh\necho report\n", 0o775},
	})
	destination := t.TempDir()
	if err := ExtractPackageArchive(archive, destination); err != nil {
		t.Fatalf("ExtractPackageArchive() error = %v", err)
	}
	for relative, want := range map[string]os.FileMode{
		"agent-plugin.yaml": 0o644,
		"rules/guidance.md": 0o644,
		"scripts/report.sh": 0o755,
	} {
		info, err := os.Lstat(filepath.Join(destination, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("inspect extracted %q: %v", relative, err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("extracted %q mode = %04o, want %04o", relative, info.Mode().Perm(), want)
		}
	}
}
