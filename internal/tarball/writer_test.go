package tarball

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"testing"
	"time"
)

func TestWriterProducesDeterministicNormalizedArchive(t *testing.T) {
	t.Parallel()

	first := buildFixture(t, []string{"LICENSE", "acr"})
	second := buildFixture(t, []string{"acr", "LICENSE"})
	if !bytes.Equal(first, second) {
		t.Fatal("equivalent inputs produced different tarballs")
	}

	gzipReader, err := gzip.NewReader(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	if gzipReader.Name != "" || gzipReader.Comment != "" || !gzipReader.ModTime.IsZero() {
		t.Fatalf("gzip header is not normalized: %#v", gzipReader.Header)
	}
	tarReader := tar.NewReader(gzipReader)
	want := []struct {
		name string
		mode int64
	}{{name: "LICENSE", mode: 0o644}, {name: "acr", mode: 0o755}}
	for index, expected := range want {
		header, err := tarReader.Next()
		if err != nil {
			t.Fatalf("entry %d: %v", index, err)
		}
		if header.Name != expected.name || header.Mode != expected.mode || header.Typeflag != tar.TypeReg || !header.ModTime.Equal(time.Unix(0, 0).UTC()) || !header.AccessTime.IsZero() || !header.ChangeTime.IsZero() || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" {
			t.Fatalf("entry %d is not normalized: %#v", index, header)
		}
	}
	if _, err := tarReader.Next(); err != io.EOF {
		t.Fatalf("unexpected trailing entry: %v", err)
	}
	if err := gzipReader.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriterRejectsUnsafeAndDuplicateNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"", ".", "../acr", "/acr", "dir\\acr", "dir//acr", "acr\x00bad"} {
		var writer Writer
		if err := writer.Add(name, 0o755, []byte("binary")); err == nil {
			t.Errorf("Add(%q) succeeded", name)
		}
	}
	var writer Writer
	if err := writer.Add("acr", 0o755, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Add("acr", 0o755, []byte("second")); err == nil {
		t.Fatal("duplicate Add succeeded")
	}
}

func buildFixture(t *testing.T, names []string) []byte {
	t.Helper()

	var writer Writer
	for _, name := range names {
		mode := 0o666
		if name == "acr" {
			mode = 0o700
		}
		if err := writer.Add(name, fs.FileMode(mode), []byte(name+"\n")); err != nil {
			t.Fatal(err)
		}
	}
	compressed, raw, err := writer.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("raw tar stream is empty")
	}
	return compressed
}
