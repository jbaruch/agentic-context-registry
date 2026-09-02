package adaptertest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostileFreshnessSessionStartGoldenChecklist(t *testing.T) {
	t.Parallel()

	seedClaude, err := os.ReadFile(filepath.Join("testdata", "freshness-session-start", "project", ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	seedCodex, err := os.ReadFile(filepath.Join("testdata", "freshness-session-start", "project", ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	seedCursor, err := os.ReadFile(filepath.Join("testdata", "freshness-session-start", "project", ".cursor", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("claude-code", func(t *testing.T) {
		t.Parallel()
		want := filepath.Join("testdata", "freshness-session-start", "want", "claude-code", "files")
		hook := filepath.Join(want, ".claude", "hooks", "acr__jbaruch__agentic-context-registry__freshness-session-start", "session-start.sh")
		assertExecutableHook(t, hook)
		settings := readWantFile(t, filepath.Join(want, ".claude", "settings.json"))
		if !strings.Contains(settings, `${CLAUDE_PROJECT_DIR}/.claude/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh`) {
			t.Fatalf("claude settings missing owned command: %s", settings)
		}
		if !strings.Contains(settings, `"args":["--policy","outdated"]`) {
			t.Fatalf("claude settings missing policy args: %s", settings)
		}
		if !strings.Contains(settings, `"Stop"`) || !strings.Contains(settings, "user-command") || !strings.Contains(settings, `"user":true`) {
			t.Fatalf("claude seeded Stop/user changed: %s", settings)
		}
		if _, err := os.Stat(filepath.Join(want, ".claude", "settings.local.json")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("claude settings.local.json exists: %v", err)
		}
		assertFileEquals(t, filepath.Join(want, ".codex", "config.toml"), seedCodex)
		assertFileEquals(t, filepath.Join(want, ".cursor", "hooks.json"), seedCursor)
	})

	t.Run("codex", func(t *testing.T) {
		t.Parallel()
		want := filepath.Join("testdata", "freshness-session-start", "want", "codex", "files")
		hook := filepath.Join(want, ".codex", "hooks", "acr__jbaruch__agentic-context-registry__freshness-session-start", "session-start.sh")
		assertExecutableHook(t, hook)
		config := readWantFile(t, filepath.Join(want, ".codex", "config.toml"))
		if !strings.Contains(config, `$(git rev-parse --show-toplevel)/.codex/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh`) || !strings.Contains(config, "--policy outdated") {
			t.Fatalf("codex command missing quoted git-root path: %s", config)
		}
		if !strings.Contains(config, "[hooks.state.user]\nlast_run = 123") {
			t.Fatalf("codex trust state changed: %s", config)
		}
		if !strings.Contains(config, "[[hooks.Stop]]") || !strings.Contains(config, `model = "gpt-5"`) {
			t.Fatalf("codex seeded Stop/model changed: %s", config)
		}
		assertFileEquals(t, filepath.Join(want, ".claude", "settings.json"), seedClaude)
		assertFileEquals(t, filepath.Join(want, ".cursor", "hooks.json"), seedCursor)
	})

	t.Run("cursor", func(t *testing.T) {
		t.Parallel()
		want := filepath.Join("testdata", "freshness-session-start", "want", "cursor", "files")
		hook := filepath.Join(want, ".cursor", "hooks", "acr__jbaruch__agentic-context-registry__freshness-session-start", "session-start.sh")
		assertExecutableHook(t, hook)
		hooks := readWantFile(t, filepath.Join(want, ".cursor", "hooks.json"))
		if !strings.Contains(hooks, `.cursor/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh --policy outdated`) {
			t.Fatalf("cursor command missing: %s", hooks)
		}
		if !strings.Contains(hooks, `"version":1`) || !strings.Contains(hooks, `"stop"`) || !strings.Contains(hooks, `"user":true`) {
			t.Fatalf("cursor seeded version/stop/user changed: %s", hooks)
		}
		if _, err := os.Stat(filepath.Join(want, ".cursor", "mcp.json")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("cursor mcp.json exists: %v", err)
		}
		assertFileEquals(t, filepath.Join(want, ".claude", "settings.json"), seedClaude)
		assertFileEquals(t, filepath.Join(want, ".codex", "config.toml"), seedCodex)
	})
}

func assertExecutableHook(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("%s mode = %04o, want 0755", path, info.Mode().Perm())
	}
}

func readWantFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func assertFileEquals(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s = %s, want seeded %s", path, got, want)
	}
}
