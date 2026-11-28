package manifest

import (
	"errors"
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func TestOpaqueManifestDecoderErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, document, detail string }{
		{"megabyte key", "schemaVersion: 1\n? \"" + strings.Repeat("k", 1<<20) + "\"\n: true\n",
			"yaml: unmarshal errors:\n  line 2: field " + strings.Repeat("k", 16344) + "...[truncated]"},
		{"ordinary YAML", "schemaVersion: 1\nunknown: true\n",
			"yaml: unmarshal errors:\n  line 2: field unknown not found in type manifest.Manifest"},
		{"multiline YAML", "schemaVersion: 1\nunknown: true\nanother: true\nthird: true\n",
			"yaml: unmarshal errors:\n  line 2: field unknown not found in type manifest.Manifest\n  line 3: field another not found in type manifest.Manifest\n  line 4: field third not found in type manifest.Manifest"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeManifest([]byte(test.document), "/fixture")
			want := "decode /fixture/agent-plugin.yaml: " + test.detail
			if err == nil || err.Error() != want {
				t.Fatalf("producer output differs from exact expectation (%d bytes)", len(want))
			}
			var original *yaml.TypeError
			if !errors.As(err, &original) {
				t.Fatal("lost YAML decoder error")
			}
			if test.name == "megabyte key" && !strings.Contains(original.Errors[0], strings.Repeat("k", 1<<20)) {
				t.Fatal("original decoder error was truncated")
			}
		})
	}
}

type failingManifestRoot struct{ err error }

func (root failingManifestRoot) Lstat(string) (os.FileInfo, error) { return nil, root.err }
func (root failingManifestRoot) Open(string) (*os.File, error)     { return nil, root.err }

func TestOpaqueManifestFilesystemErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, path, detail string }{
		{"ordinary", "agent-plugin.yaml", "lstat agent-plugin.yaml: permission denied"},
		{"megabyte OS detail", strings.Repeat("p", 1<<20), "lstat " + strings.Repeat("p", 16378) + "...[truncated]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := &os.PathError{Op: "lstat", Path: test.path, Err: os.ErrPermission}
			_, err := readManifestFromRoot(failingManifestRoot{err: original}, "/fixture")
			if err == nil || err.Error() != "inspect /fixture/agent-plugin.yaml: "+test.detail || !errors.Is(err, os.ErrPermission) {
				t.Fatal("filesystem output differs or permission classification lost")
			}
			var pathErr *os.PathError
			if !errors.As(err, &pathErr) || pathErr != original || pathErr.Path != test.path {
				t.Fatal("original filesystem error changed")
			}
		})
	}
}
