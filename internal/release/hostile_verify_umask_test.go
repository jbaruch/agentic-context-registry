//go:build linux || darwin

package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"syscall"
	"testing"
)

func TestHostileReleaseArchiveIgnoresUmask(t *testing.T) {
	previous := syscall.Umask(0o022)
	defer syscall.Umask(previous)

	first := fixtureBundle(t, false)
	syscall.Umask(0o077)
	second := fixtureBundle(t, false)

	if len(first.Archives) != 4 {
		t.Fatalf("archives = %d, want 4", len(first.Archives))
	}
	for index, asset := range first.Archives {
		if !bytes.Equal(asset.Bytes, second.Archives[index].Bytes) {
			t.Fatalf("archive %q changed with process umask", asset.Name)
		}
		modes := tarModes(t, asset.Bytes)
		if modes["acr"] != 0o755 {
			t.Fatalf("umask masked the executable bit out of %q: %#v", asset.Name, modes)
		}
		if modes["LICENSE"] != 0o644 {
			t.Fatalf("umask changed LICENSE mode in %q: %#v", asset.Name, modes)
		}
	}
	if !bytes.Equal(first.Checksums.Bytes, second.Checksums.Bytes) {
		t.Fatal("checksums.txt changed with process umask")
	}
}

func tarModes(t *testing.T, archive []byte) map[string]int64 {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tarReader := tar.NewReader(reader)
	modes := map[string]int64{}
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return modes
		}
		if err != nil {
			t.Fatal(err)
		}
		modes[header.Name] = header.Mode
	}
}
