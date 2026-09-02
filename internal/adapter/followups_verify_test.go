package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFollowupsRootSnapshotDocMatchesParentAndLeafRejection(t *testing.T) {
	t.Parallel()

	docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "adapters.md"))
	if err != nil {
		t.Fatal(err)
	}
	sentence := rootSnapshotDocSentence(string(docs))
	if !strings.Contains(sentence, "rejects symlinks and special files at the leaf") {
		t.Fatalf("RootSnapshot doc sentence missing leaf rejection: %q", sentence)
	}
	if !strings.Contains(sentence, "rejects symlinks in parent path components") {
		t.Fatalf("RootSnapshot doc sentence missing parent-component rejection: %q", sentence)
	}

	dir := t.TempDir()
	writeTestFile(t, dir, "real/AGENTS.md", "hello\n")
	if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
		t.Fatalf("create in-root parent symlink: %v", err)
	}
	if err := os.Symlink("real/AGENTS.md", filepath.Join(dir, "leaf.md")); err != nil {
		t.Fatalf("create leaf symlink: %v", err)
	}
	snapshot, err := NewRootSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	if _, err := snapshot.ReadFile("leaf.md"); err == nil {
		t.Fatal("ReadFile(leaf symlink) succeeded, want rejection matching the docs")
	}
	if _, err := snapshot.ReadFile("link/AGENTS.md"); err == nil {
		t.Fatal("ReadFile through a symlinked parent succeeded, want rejection matching the docs")
	}
}

func rootSnapshotDocSentence(docs string) string {
	marker := "`RootSnapshot` (`NewRootSnapshot(dir)`)"
	start := strings.Index(docs, marker)
	if start < 0 {
		return ""
	}
	rest := docs[start:]
	end := strings.Index(rest, ". `FSSnapshot`")
	if end < 0 {
		return rest
	}
	return rest[:end+1]
}
