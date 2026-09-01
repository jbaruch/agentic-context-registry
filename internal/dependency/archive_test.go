package dependency

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestVerifyPackageArchiveIsDeterministic(t *testing.T) {
	t.Parallel()

	repository := Repository{Owner: "owner", Name: "plugin"}
	files := map[string]string{
		"agent-plugin.yaml": "schemaVersion: 1\nname: owner/plugin\nversion: 1.2.3\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  rules:\n    - id: guidance\n      path: rules/guidance.md\n      activation:\n        mode: always\n",
		"rules/guidance.md": "Use deterministic tests.\n",
		"README.md":         "not published\n",
	}
	first, err := verifyPackageArchive(testArchive(t, "owner-plugin-a", files), repository)
	if err != nil {
		t.Fatalf("verifyPackageArchive(first) error = %v", err)
	}
	second, err := verifyPackageArchive(testArchive(t, "different-root", files), repository)
	if err != nil {
		t.Fatalf("verifyPackageArchive(second) error = %v", err)
	}
	if first != second || !strings.HasPrefix(first.ContentHash, "sha256:") || first.Version != "1.2.3" {
		t.Fatalf("verified packages = %#v, %#v, want equal deterministic hashes", first, second)
	}
}

func TestVerifyPackageArchiveRejectsMalformedAndMismatchedContent(t *testing.T) {
	t.Parallel()

	repository := Repository{Owner: "owner", Name: "plugin"}
	tests := []struct {
		name    string
		archive []byte
		want    string
	}{
		{name: "not gzip", archive: []byte("invalid"), want: "valid GitHub tarball"},
		{
			name: "identity mismatch",
			archive: testArchive(t, "root", map[string]string{
				"agent-plugin.yaml": "schemaVersion: 1\nname: other/plugin\nversion: 1.0.0\nsource:\n  repository: https://github.com/other/plugin\nartifacts:\n  rules:\n    - id: guidance\n      path: guidance.md\n      activation:\n        mode: always\n",
				"guidance.md":       "content\n",
			}),
			want: "does not match",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := verifyPackageArchive(test.archive, repository)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifyPackageArchive() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestArchivePathRejectsTraversalAndMultipleRoots(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"../escape", "/absolute", "root/../../escape", `root\windows`, "root/C:/escape"} {
		if _, _, err := archivePath(name, ""); err == nil {
			t.Errorf("archivePath(%q) succeeded, want traversal rejection", name)
		}
	}
	if _, _, err := archivePath("second/file", "first"); err == nil || !strings.Contains(err.Error(), "multiple roots") {
		t.Fatalf("archivePath(multiple roots) error = %v", err)
	}
}

func testArchive(t *testing.T, root string, files map[string]string) []byte {
	t.Helper()
	var result bytes.Buffer
	gzipWriter := gzip.NewWriter(&result)
	tarWriter := tar.NewWriter(gzipWriter)
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
		header := &tar.Header{Name: fmt.Sprintf("%s/%s", root, name), Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(contents))}
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
