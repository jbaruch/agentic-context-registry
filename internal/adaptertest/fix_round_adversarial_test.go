package adaptertest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// F5: a wrapped permission error must still propagate from Detect. A bare
// `err != fs.ErrNotExist` check would swallow it as "not detected".
func TestFixRoundDetectPropagatesWrappedPermissionError(t *testing.T) {
	t.Parallel()

	injected := fmt.Errorf("open AGENTS.md: %w", os.ErrPermission)
	_, err := NewReferenceAdapter("1.0.0").Detect(context.Background(), adapter.DetectRequest{Project: blockedSnapshot{err: injected}})
	if err == nil {
		t.Fatal("Detect() swallowed a wrapped permission error as absence")
	}
	if !strings.Contains(err.Error(), "permission") && !strings.Contains(err.Error(), "AGENTS.md") {
		t.Fatalf("Detect() error = %v, want the injected wrapped permission error", err)
	}
}

// F6: the duplicate-structural-config-entries golden must fail the same way
// when the two colliding hooks are declared in reverse order.
func TestFixRoundDuplicateGoldenIndependentOfHookOrder(t *testing.T) {
	t.Parallel()

	hooks := []manifest.HookArtifact{
		{ID: "session-start-a", Path: "hooks/a.sh", Event: manifest.HookSessionStart, Args: []string{"test-collide-key=shared-key"}},
		{ID: "session-start-b", Path: "hooks/b.sh", Event: manifest.HookSessionStart, Args: []string{"test-collide-key=shared-key"}},
	}
	realizeDuplicate := func(order []manifest.HookArtifact) error {
		t.Helper()
		pkg := adapter.Package{
			Source:   "github:acr-fixtures/duplicate-structural-config-entries",
			Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Hooks: order}},
		}
		coordinator, err := adapter.NewCoordinator(NewCompiler(), NewReferenceAdapter("1.0.0"))
		if err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		_, err = coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(root)), []adapter.Package{pkg}, realize.Ledger{})
		return err
	}

	forward := realizeDuplicate(hooks)
	reversed := realizeDuplicate([]manifest.HookArtifact{hooks[1], hooks[0]})
	if forward == nil || reversed == nil {
		t.Fatalf("forward = %v, reversed = %v, want duplicate_config_entry both ways", forward, reversed)
	}
	if !strings.Contains(forward.Error(), adapter.CodeDuplicateConfigEntry) || !strings.Contains(reversed.Error(), adapter.CodeDuplicateConfigEntry) {
		t.Fatalf("forward = %v, reversed = %v, want %s", forward, reversed, adapter.CodeDuplicateConfigEntry)
	}
	if forward.Error() != reversed.Error() {
		t.Fatalf("duplicate diagnostic depends on hook order:\nforward:  %s\nreversed: %s", forward, reversed)
	}
}
