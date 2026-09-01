package adaptertest

import (
	"context"
	"os"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestReferenceAdapterGolden(t *testing.T) {
	RunGolden(t, NewReferenceAdapter("1.0.0"), NewCompiler())
}

func TestReferenceAdapterRenderIsOrderIndependent(t *testing.T) {
	t.Parallel()

	buildPackage := func(hookOrder []string) adapter.Package {
		hooks := make([]manifest.HookArtifact, len(hookOrder))
		for index, id := range hookOrder {
			hooks[index] = manifest.HookArtifact{ID: id, Path: "hooks/" + id + ".sh", Event: manifest.HookSessionStart}
		}
		return adapter.Package{
			Source:   "github:owner/pkg",
			Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Hooks: hooks}},
		}
	}
	render := func(hookOrder []string) []byte {
		t.Helper()
		coordinator, err := adapter.NewCoordinator(NewCompiler(), NewReferenceAdapter("1.0.0"))
		if err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		intents, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(root)), []adapter.Package{buildPackage(hookOrder)}, realize.Ledger{})
		if err != nil {
			t.Fatal(err)
		}
		if len(intents) != 1 {
			t.Fatalf("intents = %#v, want exactly one hooks.json intent", intents)
		}
		return intents[0].Content
	}

	forward := render([]string{"hook-a", "hook-b", "hook-c"})
	shuffled := render([]string{"hook-c", "hook-a", "hook-b"})
	if string(forward) != string(shuffled) {
		t.Fatalf("rendered config depends on input order:\nforward: %s\nshuffled: %s", forward, shuffled)
	}
}
