package realizeapp

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestHostileNoneAfterOutdatedRemovesOnlyOwnedBytes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	seedFreshnessUserConfigs(t, root)
	before := map[string][]byte{
		".claude/settings.json": readProjectFile(t, root, ".claude/settings.json"),
		".codex/config.toml":    readProjectFile(t, root, ".codex/config.toml"),
		".cursor/hooks.json":    readProjectFile(t, root, ".cursor/hooks.json"),
	}
	writeFreshnessState(t, root, "outdated")
	service := NewService(noPackageLoader{})
	if _, err := service.Run(context.Background(), root, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	assertFreshnessHooks(t, root, "outdated")
	for path, seed := range before {
		content := readProjectFile(t, root, path)
		if !bytes.Contains(content, []byte("user-command")) {
			t.Fatalf("%s lost user-command after outdated realize", path)
		}
		if path == ".codex/config.toml" {
			if !bytes.Contains(content, []byte("[hooks.state.user]\nlast_run = 123")) {
				t.Fatalf("Codex trust state changed after outdated realize: %s", content)
			}
			if !bytes.Contains(content, seed[:bytes.Index(seed, []byte("[[hooks.Stop]]"))]) {
				t.Fatalf("Codex unmanaged prefix changed: %s", content)
			}
		}
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
			t.Fatalf("owned hook %q remains after none: %v", path, err)
		}
	}
	afterClaude := readProjectFile(t, root, ".claude/settings.json")
	afterCodex := readProjectFile(t, root, ".codex/config.toml")
	afterCursor := readProjectFile(t, root, ".cursor/hooks.json")
	if bytes.Contains(afterClaude, []byte("freshness-session-start")) || bytes.Contains(afterCodex, []byte("freshness-session-start")) || bytes.Contains(afterCursor, []byte("freshness-session-start")) {
		t.Fatalf("none left an owned freshness entry")
	}
	if !bytes.Equal(userCommandSpan(afterClaude), userCommandSpan(before[".claude/settings.json"])) {
		t.Fatalf("Claude user-command span changed: %s vs %s", afterClaude, before[".claude/settings.json"])
	}
	if !bytes.Contains(afterCodex, []byte("[hooks.state.user]\nlast_run = 123")) {
		t.Fatalf("Codex trust state not byte-stable: %s", afterCodex)
	}
	if !bytes.Equal(userCommandSpan(afterCursor), userCommandSpan(before[".cursor/hooks.json"])) {
		t.Fatalf("Cursor user-command span changed: %s vs %s", afterCursor, before[".cursor/hooks.json"])
	}
}

func TestHostileRepeatedRealizeDoesNotDuplicateFreshnessHook(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFreshnessState(t, root, "outdated")
	service := NewService(noPackageLoader{})
	if _, err := service.Run(context.Background(), root, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	first := map[string][]byte{
		".claude/settings.json": readProjectFile(t, root, ".claude/settings.json"),
		".codex/config.toml":    readProjectFile(t, root, ".codex/config.toml"),
		".cursor/hooks.json":    readProjectFile(t, root, ".cursor/hooks.json"),
	}
	second, err := service.Run(context.Background(), root, nil, realize.ModeApply)
	if err != nil {
		t.Fatal(err)
	}
	if second.Plan.HasChanges() {
		t.Fatalf("second apply plan = %#v, want empty", second.Plan)
	}
	for path, want := range first {
		got := readProjectFile(t, root, path)
		if !bytes.Equal(got, want) {
			t.Fatalf("%s changed on repeated realize:\n got %s\nwant %s", path, got, want)
		}
		if count := bytes.Count(got, []byte("freshness-session-start")); count != 1 {
			t.Fatalf("%s freshness entry count = %d", path, count)
		}
	}
}

func TestHostileRenderedWrapperNeverPromptsWithStdinClosed(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFreshnessState(t, root, "outdated")
	if _, err := NewService(noPackageLoader{}).Run(context.Background(), root, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(root, filepath.FromSlash(".claude/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh"))
	fakeACR := filepath.Join(root, "fake-acr")
	fakeBody := []byte("#!/usr/bin/env bash\nset -euo pipefail\nprintf '%s\\n' \"$*\" >\"$ACR_TEST_CALL\"\nif IFS= read -r line; then\n  printf 'PROMPT:%s\\n' \"$line\"\n  exit 2\nfi\nprintf 'nested freshness failure\\n' >&2\nexit 4\n")
	if err := os.WriteFile(fakeACR, fakeBody, 0o755); err != nil {
		t.Fatal(err)
	}
	callPath := filepath.Join(root, "call.txt")
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	command := exec.Command("bash", hook, "--policy", "outdated")
	command.Env = append(os.Environ(), "ACR_BIN="+fakeACR, "ACR_TEST_CALL="+callPath)
	command.Stdin = stdin
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("wrapper exit = %v, stderr = %q", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("wrapper stdout = %q, want empty", stdout.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "PROMPT:") {
		t.Fatalf("wrapper prompted: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	call, err := os.ReadFile(callPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(call), "freshness run --project ") || !strings.Contains(string(call), " --policy outdated") {
		t.Fatalf("wrapper call = %q", call)
	}
}

func readProjectFile(t *testing.T, root, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func userCommandSpan(content []byte) []byte {
	start := bytes.Index(content, []byte("user-command"))
	if start < 0 {
		return nil
	}
	return content[start : start+len("user-command")]
}
