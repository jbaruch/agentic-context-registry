package publish

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/antigravity"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestPublishGateRealizesAllAdapters(t *testing.T) {
	worktree := t.TempDir()
	if err := os.CopyFS(worktree, os.DirFS(filepath.Join("..", "adaptertest", "testdata", "all-agents", "package"))); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(worktree, manifest.Filename)
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, append(body, []byte("    - id: end\n      event: session-end\n      path: hooks/stop.sh\n    - id: prompt\n      event: user-prompt-submit\n      path: hooks/stop.sh\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	gate := NewGate()
	assets := allAgentsAssets(t, worktree, gate)
	workspace := t.TempDir()
	if err := gate.validateArchive(context.Background(), assets, workspace); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"AGENTS.md": "Keep changes focused.",
		".agents/skills/acr__example__all-agents__review-change/SKILL.md": "Review a proposed code change",
		".agent/hooks.json":  "SessionStart",
		".codex/config.toml": "hooks.UserPromptSubmit",
	} {
		content, err := os.ReadFile(filepath.Join(workspace, "project", path))
		if err != nil || !strings.Contains(string(content), want) {
			t.Fatalf("paired project %s: want %q, read error %v", path, want, err)
		}
	}
	var metadata Metadata
	if err := json.Unmarshal(assets.Metadata.Bytes, &metadata); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, descriptor := range metadata.Adapters {
		ids = append(ids, descriptor.ID)
	}
	if !slices.Equal(ids, []string{"antigravity", "claude-code", "codex", "cursor"}) {
		t.Fatalf("acr-package.json adapters = %v", ids)
	}
	native := antigravity.Adapter{}.Descriptor()
	if metadata.Adapters[0].Version != native.Version || metadata.Adapters[0].Boundary != native.Boundary {
		t.Fatalf("acr-package.json Antigravity descriptor = %#v, want %#v", metadata.Adapters[0], native)
	}
}

func TestPublishGateRejectsUnsupportedEvent(t *testing.T) {
	gate := newGate(noEventAdapter{Adapter: claudecode.New()})
	assets := allAgentsAssets(t, filepath.Join("..", "adaptertest", "testdata", "all-agents", "package"), gate)
	err := gate.Validate(context.Background(), assets)
	var publishErr *Error
	if !errors.As(err, &publishErr) || publishErr.Code != CodeAdapterRealization || !strings.Contains(err.Error(), "claude-code") {
		t.Fatalf("gate error = %#v", err)
	}
}

func TestPublishGateUsesArchiveNotWorktree(t *testing.T) {
	source := filepath.Join("..", "adaptertest", "testdata", "all-agents", "package")
	worktree := t.TempDir()
	if err := os.CopyFS(worktree, os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	gate := NewGate()
	assets := allAgentsAssets(t, worktree, gate)
	if err := os.WriteFile(filepath.Join(worktree, "rules", "always.md"), []byte("---\nbroken: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gate.Validate(context.Background(), assets); err != nil {
		t.Fatalf("gate read changed worktree instead of archive: %v", err)
	}
}

type noEventAdapter struct{ adapter.Adapter }

func (noEventAdapter) SupportedEvents() []manifest.HookEvent { return nil }

func allAgentsAssets(t *testing.T, root string, gate *Gate) ReleaseAssets {
	t.Helper()
	value, names, err := manifest.LoadListed(root, nil)
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
	assets, err := BuildReleaseAssets(value, Identity{Tag: "v" + value.Version, Commit: strings.Repeat("a", 40)}, files, gate.Descriptors(), "acr test")
	if err != nil {
		t.Fatal(err)
	}
	return assets
}
