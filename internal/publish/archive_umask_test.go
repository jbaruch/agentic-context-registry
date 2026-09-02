//go:build linux || darwin

package publish

import (
	"bytes"
	"syscall"
	"testing"
)

func TestReleaseAssetsIgnoreUmask(t *testing.T) {
	previous := syscall.Umask(0o022)
	defer syscall.Umask(previous)
	first := buildFixtureAssets(t)
	syscall.Umask(0o077)
	second := buildFixtureAssets(t)
	if !bytes.Equal(first.Archive.Bytes, second.Archive.Bytes) {
		t.Fatal("archive bytes changed with process umask")
	}
	if first.Evidence.ContentHash != second.Evidence.ContentHash {
		t.Fatalf("content hash changed with process umask: %s != %s", first.Evidence.ContentHash, second.Evidence.ContentHash)
	}
}
