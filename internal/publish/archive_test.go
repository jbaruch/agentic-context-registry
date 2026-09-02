package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
	"time"
)

func TestArchiveBytesAreDeterministic(t *testing.T) {
	t.Parallel()

	files := archiveFixtureFiles()
	first, err := BuildArchive("plugin", "1.2.3", files)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildArchive("plugin", "1.2.3", []File{files[2], files[0], files[1]})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes, second.Bytes) || first.TarSHA256 != second.TarSHA256 {
		t.Fatal("identical file trees produced different archives")
	}
	if first.Name != "plugin-1.2.3.tar.gz" {
		t.Fatalf("archive name = %q", first.Name)
	}
}

func TestArchiveHeadersAreNormalized(t *testing.T) {
	t.Parallel()

	result, err := BuildArchive("plugin", "1.2.3", archiveFixtureFiles())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(result.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if reader.Name != "" || reader.Comment != "" || !reader.ModTime.IsZero() {
		t.Fatalf("gzip header is not normalized: %#v", reader.Header)
	}
	tarReader := tar.NewReader(reader)
	want := []struct {
		name string
		mode int64
	}{
		{name: "plugin-1.2.3/agent-plugin.yaml", mode: 0o644},
		{name: "plugin-1.2.3/rules/long-path-that-forces-a-pax-header-because-it-is-more-than-one-hundred-characters-guidance.md", mode: 0o644},
		{name: "plugin-1.2.3/scripts/check.sh", mode: 0o755},
	}
	for index, expected := range want {
		header, err := tarReader.Next()
		if err != nil {
			t.Fatalf("entry %d: %v", index, err)
		}
		if header.Name != expected.name || header.Mode != expected.mode || header.Typeflag != tar.TypeReg {
			t.Fatalf("entry %d = %#v, want %q mode %04o regular", index, header, expected.name, expected.mode)
		}
		if !header.ModTime.Equal(time.Unix(0, 0).UTC()) || !header.AccessTime.IsZero() || !header.ChangeTime.IsZero() || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" {
			t.Fatalf("entry %q carries non-normalized metadata: %#v", header.Name, header)
		}
	}
	if _, err := tarReader.Next(); err != io.EOF {
		t.Fatalf("unexpected trailing entry: %v", err)
	}
}

func TestArchiveTarDigestIsPinned(t *testing.T) {
	t.Parallel()

	result, err := BuildArchive("plugin", "1.2.3", archiveFixtureFiles())
	if err != nil {
		t.Fatal(err)
	}
	const want = "677d5ac81f52487378defc9bb95a210d5dbee5db506dee3be4bda1039d42481e"
	if result.TarSHA256 != want {
		t.Fatalf("tar SHA-256 = %s, want %s", result.TarSHA256, want)
	}

	reader, err := gzip.NewReader(bytes.NewReader(result.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != result.TarSHA256 {
		t.Fatal("reported tar digest does not match the decompressed stream")
	}
}

func archiveFixtureFiles() []File {
	return []File{
		{Path: "scripts/check.sh", Mode: 0o700, Content: []byte("#!/bin/sh\r\nexit 0\r\n")},
		{Path: "agent-plugin.yaml", Mode: 0o666, Content: []byte("schemaVersion: 1\n")},
		{Path: "rules/long-path-that-forces-a-pax-header-because-it-is-more-than-one-hundred-characters-guidance.md", Mode: 0o644, Content: []byte("guidance\n")},
	}
}
