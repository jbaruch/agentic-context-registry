package publish

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestPublishGateRealizesAllAdapters(t *testing.T) {
	archive := allAgentsArchive(t, filepath.Join("..", "adaptertest", "testdata", "all-agents", "package"))
	gate := NewGate()
	if err := gate.Validate(context.Background(), archive.Bytes); err != nil {
		t.Fatal(err)
	}
	descriptors := gate.Descriptors()
	if len(descriptors) != 3 || descriptors[0].ID != "claude-code" || descriptors[1].ID != "codex" || descriptors[2].ID != "cursor" {
		t.Fatalf("gate descriptors = %#v", descriptors)
	}
}

func TestPublishGateRejectsUnsupportedEvent(t *testing.T) {
	archive := allAgentsArchive(t, filepath.Join("..", "adaptertest", "testdata", "all-agents", "package"))
	gate := newGate(noEventAdapter{Adapter: claudecode.New()})
	err := gate.Validate(context.Background(), archive.Bytes)
	publishErr, ok := err.(*Error)
	if !ok || publishErr.Code != CodeAdapterRealization || !strings.Contains(err.Error(), "claude-code") {
		t.Fatalf("gate error = %#v", err)
	}
}

func TestPublishGateUsesArchiveNotWorktree(t *testing.T) {
	source := filepath.Join("..", "adaptertest", "testdata", "all-agents", "package")
	worktree := t.TempDir()
	if err := os.CopyFS(worktree, os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	archive := allAgentsArchive(t, worktree)
	if err := os.WriteFile(filepath.Join(worktree, "rules", "always.md"), []byte("---\nbroken: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewGate().Validate(context.Background(), archive.Bytes); err != nil {
		t.Fatalf("gate read changed worktree instead of archive: %v", err)
	}
}

type noEventAdapter struct{ adapter.Adapter }

func (noEventAdapter) SupportedEvents() []manifest.HookEvent { return nil }

func allAgentsArchive(t *testing.T, root string) Archive {
	t.Helper()
	value, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	names, err := manifest.PackageFiles(root, value)
	if err != nil {
		t.Fatal(err)
	}
	files := make([]File, 0, len(names))
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, File{Path: name, Mode: info.Mode(), Content: content})
	}
	archive, err := BuildArchive("all-agents", value.Version, files)
	if err != nil {
		t.Fatal(err)
	}
	return archive
}
