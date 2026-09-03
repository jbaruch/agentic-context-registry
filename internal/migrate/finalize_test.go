package migrate

import (
	"bytes"
	"testing"
)

func TestRemoveGitignoreBlockKeepsOneBlankLine(t *testing.T) {
	content := []byte("# user policy\n/build/\n\n# === Tessl-generated artifacts (managed by tessl) ===\n.tessl/cache/\n# === end Tessl-generated artifacts ===\n\n*.tmp\n")
	want := []byte("# user policy\n/build/\n\n*.tmp\n")

	got, removed := removeGitignoreBlock(content)
	if !removed || !bytes.Equal(got, want) {
		t.Fatalf("removeGitignoreBlock() = %q, %t, want %q, true", got, removed, want)
	}
}
