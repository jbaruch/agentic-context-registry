//go:build linux || darwin

package publish

import (
	"bytes"
	"syscall"
	"testing"
)

func TestArchiveIgnoresUmask(t *testing.T) {
	previous := syscall.Umask(0o022)
	first, firstErr := BuildArchive("plugin", "1.2.3", archiveFixtureFiles())
	syscall.Umask(0o077)
	second, secondErr := BuildArchive("plugin", "1.2.3", archiveFixtureFiles())
	syscall.Umask(previous)
	if firstErr != nil || secondErr != nil {
		t.Fatalf("BuildArchive errors = %v, %v", firstErr, secondErr)
	}
	if !bytes.Equal(first.Bytes, second.Bytes) {
		t.Fatal("archive bytes changed with process umask")
	}
}
