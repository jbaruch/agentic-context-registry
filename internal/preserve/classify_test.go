package preserve

import (
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestExistingHostBecomesSharedRegardlessOfName(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"CLAUDE.md", "settings.json", "settings.yaml", "settings.toml", "notes.txt"} {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			observed := observedFile(path, []byte("user-owned content\n"))
			ownership, promoted, err := classifyTarget(
				adapter.SharedTarget{Path: path, Observed: &observed},
				[][]byte{observed.Content},
			)
			if err != nil || promoted || ownership != realize.OwnershipShared {
				t.Fatalf("classification = %q, promoted=%t, err=%v", ownership, promoted, err)
			}
		})
	}
}
