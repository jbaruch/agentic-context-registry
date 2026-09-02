package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

func TestReleaseAssetSet(t *testing.T) {
	t.Parallel()

	bundle := fixtureBundle(t, false)
	want := []string{
		"acr-darwin-amd64.tar.gz",
		"acr-darwin-arm64.tar.gz",
		"acr-linux-amd64.tar.gz",
		"acr-linux-arm64.tar.gz",
	}
	if len(bundle.Archives) != len(want) || bundle.Checksums.Name != ChecksumsAssetName {
		t.Fatalf("bundle assets = %#v", bundle)
	}
	for index, asset := range bundle.Archives {
		if asset.Name != want[index] || strings.Contains(asset.Name, "windows") || strings.Contains(asset.Name, "x86_64") || strings.Contains(asset.Name, "aarch64") {
			t.Fatalf("archive %d name = %q, want %q", index, asset.Name, want[index])
		}
		assertArchiveEntries(t, asset.Bytes)
	}
}

func TestReleaseRefusesPartialAssetSet(t *testing.T) {
	t.Parallel()

	binaries := fixtureBinaries()
	binaries = binaries[:len(binaries)-1]
	_, err := Pack(binaries, []byte("license\n"))
	if err == nil || !strings.Contains(err.Error(), "linux/arm64") {
		t.Fatalf("Pack() error = %v, want missing linux/arm64", err)
	}
}

func TestReleaseArchiveIsByteIdentical(t *testing.T) {
	t.Parallel()

	first := fixtureBundle(t, false)
	second := fixtureBundle(t, true)
	for index := range first.Archives {
		if first.Archives[index].Name != second.Archives[index].Name || !bytes.Equal(first.Archives[index].Bytes, second.Archives[index].Bytes) {
			t.Fatalf("archive %d differs across input order", index)
		}
	}
	if !bytes.Equal(first.Checksums.Bytes, second.Checksums.Bytes) {
		t.Fatal("checksums differ across input order")
	}
}

func TestChecksumsFileMatchesUploadedBytes(t *testing.T) {
	t.Parallel()

	bundle := fixtureBundle(t, true)
	lines := strings.Split(strings.TrimSuffix(string(bundle.Checksums.Bytes), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("checksums lines = %d, want 4", len(lines))
	}
	for index, asset := range bundle.Archives {
		digest := sha256.Sum256(asset.Bytes)
		want := hex.EncodeToString(digest[:]) + "  " + asset.Name
		if lines[index] != want {
			t.Fatalf("checksum line %d = %q, want %q", index, lines[index], want)
		}
	}
}

func TestVerifyArchiveChecksum(t *testing.T) {
	t.Parallel()

	bundle := fixtureBundle(t, false)
	asset := bundle.Archives[0]
	t.Run("ok", func(t *testing.T) {
		binary, err := VerifyArchiveChecksum(asset.Name, asset.Bytes, bundle.Checksums.Bytes)
		if err != nil || string(binary) != "binary-darwin-amd64" {
			t.Fatalf("VerifyArchiveChecksum() binary = %q, err = %v", binary, err)
		}
	})
	t.Run("tamper", func(t *testing.T) {
		tampered := append([]byte(nil), asset.Bytes...)
		tampered[len(tampered)/2] ^= 0xff
		if _, err := VerifyArchiveChecksum(asset.Name, tampered, bundle.Checksums.Bytes); err == nil || !strings.Contains(err.Error(), "discard") {
			t.Fatalf("VerifyArchiveChecksum() error = %v", err)
		}
	})
	t.Run("sidecar mismatch", func(t *testing.T) {
		manifest := bytes.Replace(bundle.Checksums.Bytes, []byte(asset.Name), []byte("acr-missing.tar.gz"), 1)
		if _, err := VerifyArchiveChecksum(asset.Name, asset.Bytes, manifest); err == nil || !strings.Contains(err.Error(), "absent") {
			t.Fatalf("VerifyArchiveChecksum() error = %v", err)
		}
	})
}

func fixtureBundle(t *testing.T, reverse bool) Bundle {
	t.Helper()
	binaries := fixtureBinaries()
	if reverse {
		for left, right := 0, len(binaries)-1; left < right; left, right = left+1, right-1 {
			binaries[left], binaries[right] = binaries[right], binaries[left]
		}
	}
	bundle, err := Pack(binaries, []byte("license\n"))
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func fixtureBinaries() []Binary {
	targets := Targets()
	binaries := make([]Binary, len(targets))
	for index, target := range targets {
		binaries[index] = Binary{Target: target, Bytes: []byte("binary-" + target.GOOS + "-" + target.GOARCH)}
	}
	return binaries
}

func assertArchiveEntries(t *testing.T, archive []byte) {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
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
	}
	if _, err := tarReader.Next(); err != io.EOF {
		t.Fatalf("unexpected trailing entry: %v", err)
	}
}
