//go:build linux || darwin

package publish

import (
	"bytes"
	"syscall"
	"testing"
)

func TestHostileArchiveAndContentHashIgnoreUmask(t *testing.T) {
	previous := syscall.Umask(0o022)
	defer syscall.Umask(previous)

	first, err := BuildArchive("plugin", "1.2.3", hostileArchiveFiles())
	if err != nil {
		t.Fatal(err)
	}
	firstAssets := buildFixtureAssets(t)

	syscall.Umask(0o077)
	second, err := BuildArchive("plugin", "1.2.3", hostileArchiveFiles())
	if err != nil {
		t.Fatal(err)
	}
	secondAssets := buildFixtureAssets(t)

	if !bytes.Equal(first.Bytes, second.Bytes) {
		t.Fatal("archive bytes changed with process umask")
	}
	if !bytes.Equal(firstAssets.Archive.Bytes, secondAssets.Archive.Bytes) {
		t.Fatal("release archive bytes changed with process umask")
	}
	if firstAssets.Evidence.ContentHash != secondAssets.Evidence.ContentHash {
		t.Fatalf("content hash changed with process umask: %s != %s", firstAssets.Evidence.ContentHash, secondAssets.Evidence.ContentHash)
	}
	if tarFileMode(t, first.Bytes, "plugin-1.2.3/scripts/check.sh") != 0o755 {
		t.Fatal("umask masked the executable bit out of the archive")
	}
}
