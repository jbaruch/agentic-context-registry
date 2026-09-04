package dependency

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// dogfoodCommit is the commit GitHub recorded in the global PAX header of the
// archive that first exposed this defect.
const dogfoodCommit = "769950e1ab14ad5df4ac2bed45efa6f353a97674"

// paxArchive builds a GitHub-shaped source tarball: an optional leading global
// PAX header entry, then one root holding files, some of which carry a
// per-file extended header.
func paxArchive(t *testing.T, root string, global bool, files map[string]string, extended map[string]bool) []byte {
	t.Helper()
	var result bytes.Buffer
	gzipWriter := gzip.NewWriter(&result)
	tarWriter := tar.NewWriter(gzipWriter)
	if global {
		header := &tar.Header{
			Typeflag:   tar.TypeXGlobalHeader,
			Name:       "pax_global_header",
			PAXRecords: map[string]string{"comment": dogfoodCommit},
			Format:     tar.FormatPAX,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.WriteHeader(&tar.Header{Name: root + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	for _, name := range paths {
		contents := files[name]
		header := &tar.Header{Name: root + "/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(contents))}
		if extended[name] {
			header.PAXRecords = map[string]string{"path": header.Name}
			header.Format = tar.FormatPAX
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(contents)); err != nil {
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

// TestExtractPackageArchiveAcceptsPAXMetadata pins the defect that made every
// `acr install github:...` fail: GitHub's leading pax_global_header entry was
// counted as a second package root.
func TestExtractPackageArchiveAcceptsPAXMetadata(t *testing.T) {
	t.Parallel()

	files := map[string]string{
		"agent-plugin.yaml": "schemaVersion: 1\n",
		"rules/guidance.md": "Use deterministic tests.\n",
	}
	tests := []struct {
		name     string
		global   bool
		extended map[string]bool
	}{
		{name: "global", global: true},
		{name: "per-file", extended: map[string]bool{"rules/guidance.md": true}},
		{name: "both", global: true, extended: map[string]bool{"rules/guidance.md": true}},
		{name: "neither"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			destination := t.TempDir()
			archive := paxArchive(t, "jbaruch-ffa-acr-dogfood-769950e", test.global, files, test.extended)
			if err := ExtractPackageArchive(archive, destination); err != nil {
				t.Fatalf("ExtractPackageArchive() error = %v", err)
			}
			for relative, want := range files {
				got, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(relative)))
				if err != nil {
					t.Fatalf("read extracted %q: %v", relative, err)
				}
				if string(got) != want {
					t.Errorf("extracted %q = %q, want %q", relative, got, want)
				}
			}
			if _, err := os.Lstat(filepath.Join(destination, "pax_global_header")); !os.IsNotExist(err) {
				t.Errorf("pax_global_header materialized: %v", err)
			}
		})
	}
}

// TestExtractPackageArchiveRejectsMetadataOnlyArchive keeps a stream of PAX
// metadata from passing as package content.
func TestExtractPackageArchiveRejectsMetadataOnlyArchive(t *testing.T) {
	t.Parallel()

	var result bytes.Buffer
	gzipWriter := gzip.NewWriter(&result)
	tarWriter := tar.NewWriter(gzipWriter)
	header := &tar.Header{
		Typeflag:   tar.TypeXGlobalHeader,
		Name:       "pax_global_header",
		PAXRecords: map[string]string{"comment": dogfoodCommit},
		Format:     tar.FormatPAX,
	}
	if err := tarWriter.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	err := ExtractPackageArchive(result.Bytes(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("ExtractPackageArchive(metadata only) error = %v, want an empty-archive refusal", err)
	}
}
