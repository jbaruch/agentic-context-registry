package realizeapp

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

type noPackageLoader struct{}

func (noPackageLoader) MaterializeLocked(context.Context, dependency.LockedDependency) (dependency.MaterializedPackage, func() error, error) {
	return dependency.MaterializedPackage{}, nil, errors.New("unexpected package materialization")
}

func TestDefaultFreshnessPersistsOutdatedAndRealizesHook(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFreshnessState(t, root, "")
	service := NewService(noPackageLoader{})
	result, err := service.Run(context.Background(), root, nil, realize.ModeApply)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Agents) != 3 {
		t.Fatalf("agents = %#v", result.Agents)
	}
	loaded, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Project.Freshness != "outdated" {
		t.Fatalf("freshness = %q, want outdated", loaded.Project.Freshness)
	}
	assertFreshnessHooks(t, root, "outdated")

	checked, err := service.Run(context.Background(), root, nil, realize.ModeCheck)
	if err != nil {
		t.Fatal(err)
	}
	if checked.Plan.HasChanges() {
		t.Fatalf("second realization plan = %#v, want no changes", checked.Plan)
	}
}

func TestFreshnessPolicyArgsRender(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFreshnessState(t, root, "install")
	if _, err := NewService(noPackageLoader{}).Run(context.Background(), root, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	assertFreshnessHooks(t, root, "install")
}

func TestFreshnessPolicyNoneRemovesOnlyOwnedEntry(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	seedFreshnessUserConfigs(t, root)
	writeFreshnessState(t, root, "outdated")
	service := NewService(noPackageLoader{})
	if _, err := service.Run(context.Background(), root, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	state, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	state.Project.Freshness = "none"
	if err := dependency.WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), root, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		".claude/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh",
		".codex/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh",
		".cursor/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("owned hook %q remains: %v", path, err)
		}
	}
	for _, path := range []string{".claude/settings.json", ".codex/config.toml", ".cursor/hooks.json"} {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(content), "user-command") || strings.Contains(string(content), "freshness-session-start") {
			t.Fatalf("cleanup %s = %s", path, content)
		}
	}
	codex, err := os.ReadFile(filepath.Join(root, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(codex), "[hooks.state.user]\nlast_run = 123") {
		t.Fatalf("Codex trust state changed: %s", codex)
	}
}

func TestFreshnessPolicyNoneCreatesNothing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFreshnessState(t, root, "none")
	result, err := NewService(noPackageLoader{}).Run(context.Background(), root, nil, realize.ModeApply)
	if err != nil {
		t.Fatal(err)
	}
	if result.Plan.HasChanges() {
		t.Fatalf("none plan = %#v, want no changes", result.Plan)
	}
	for _, directory := range []string{".claude", ".codex", ".cursor"} {
		if _, err := os.Stat(filepath.Join(root, directory)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("none created %s: %v", directory, err)
		}
	}
}

func writeFreshnessState(t *testing.T, root, policy string) {
	t.Helper()
	state := dependency.State{
		Project: dependency.Project{
			SchemaVersion: dependency.CurrentSchemaVersion,
			Agents:        []string{"cursor", "claude-code", "codex"},
			Freshness:     policy,
		},
		Lock: dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion},
	}
	if err := dependency.WriteState(root, state); err != nil {
		t.Fatal(err)
	}
}

func assertFreshnessHooks(t *testing.T, root, policy string) {
	t.Helper()
	for _, path := range []string{
		".claude/settings.json", ".codex/config.toml", ".cursor/hooks.json",
	} {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if count := strings.Count(string(content), "freshness-session-start"); count != 1 {
			t.Fatalf("%s freshness entry count = %d, content = %s", path, count, content)
		}
		if !strings.Contains(string(content), policy) {
			t.Fatalf("%s does not contain policy %q: %s", path, policy, content)
		}
	}
	for _, path := range []string{
		".claude/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh",
		".codex/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh",
		".cursor/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh",
	} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode = %04o, want 0755", path, info.Mode().Perm())
		}
	}
}

func seedFreshnessUserConfigs(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		".claude/settings.json": `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"user-command"}]}]},"user":true}` + "\n",
		".codex/config.toml":    "model = \"gpt-5\"\n\n[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ntype = \"command\"\ncommand = \"user-command\"\n\n[hooks.state.user]\nlast_run = 123\n",
		".cursor/hooks.json":    `{"version":1,"hooks":{"stop":[{"command":"user-command"}]},"user":true}` + "\n",
	}
	for path, content := range files {
		filename := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
