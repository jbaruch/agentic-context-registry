package codex_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// These regressions execute the SessionStart command the Codex adapter actually
// renders, rather than inspecting its bytes. The bug #119 fixes — resolving the
// hook executable through `git rev-parse --show-toplevel` — passed every
// text-level and Git-only check while failing in a real non-Git session with
// exit 127, so the discriminating oracle has to run the materialized command
// and observe which executable it reaches, with which argv, cwd and stdin.

// The recording hook writes one tab-separated line per fact to $ACR_PROBE_LOG:
// its own resolved path (proving which project root the command located), its
// working directory, the stdin it received, and each argument in order. It
// reaches no network and writes only to that log.
const codexProbeHookBody = `#!/bin/sh
stdin="$(cat)"
{
  printf 'exe\t%s\n' "$0"
  printf 'cwd\t%s\n' "$(pwd -P)"
  printf 'stdin\t%s\n' "$stdin"
  for argument in "$@"; do printf 'arg\t%s\n' "$argument"; done
} >>"$ACR_PROBE_LOG"
`

// codexProbeArgs exercises an empty argument and every shell-special byte the
// command must carry through to the hook literally.
var codexProbeArgs = []string{"", "two words", "one'quote", "$literal", "`literal`", "back\\slash", "semi;colon"}

const codexProbeStdin = `{"session_id":"fixed-session","hook_event_name":"SessionStart","source":"startup"}`

const codexProbeHookRelative = ".codex/hooks/acr__example__all-agents__session-start/session-start.sh"

// codexRootProbeCommand realizes the probe package into projectRoot through the
// real coordinator and returns the single SessionStart command Codex rendered.
func codexRootProbeCommand(t *testing.T, projectRoot string) string {
	t.Helper()
	packageRoot := t.TempDir()
	writeFixtureFile(t, packageRoot, "hooks/session-start.sh", []byte(codexProbeHookBody), 0o755)
	pkg := adapter.Package{
		Source: "github:example/all-agents", Root: os.DirFS(packageRoot),
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Hooks: []manifest.HookArtifact{{
			ID: "session-start", Event: manifest.HookSessionStart, Path: "hooks/session-start.sh", Args: codexProbeArgs,
		}}}},
	}
	intents := mustRealize(t, codex.New(), projectRoot, []adapter.Package{pkg})
	ledger := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}
	if _, err := realize.NewEngine().Run(projectRoot, ledger, intents, realize.ModeApply, func(realize.Ledger) error { return nil }); err != nil {
		t.Fatal(err)
	}
	commands := realizedCodexCommands(t, intents)["SessionStart"]
	if len(commands) != 1 {
		t.Fatalf("SessionStart commands = %#v, want exactly one", commands)
	}
	return commands[0]
}

type codexProbeResult struct {
	exit   int
	stderr string
	ran    bool
	exe    string
	cwd    string
	stdin  string
	args   []string
}

// runCodexRootCommand executes command through /bin/sh from sessionCwd, with PWD
// set to sessionCwd exactly as Codex presents its session working directory,
// feeding the fixed hook stdin.
func runCodexRootCommand(t *testing.T, command, sessionCwd string) codexProbeResult {
	t.Helper()
	log := filepath.Join(t.TempDir(), "probe.log")
	process := exec.Command("/bin/sh", "-c", command)
	process.Dir = sessionCwd
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PWD=") || strings.HasPrefix(entry, "ACR_PROBE_LOG=") {
			continue
		}
		env = append(env, entry)
	}
	process.Env = append(env, "PWD="+sessionCwd, "ACR_PROBE_LOG="+log)
	process.Stdin = strings.NewReader(codexProbeStdin)
	var stderr bytes.Buffer
	process.Stderr = &stderr
	runErr := process.Run()
	if runErr != nil {
		if _, ok := runErr.(*exec.ExitError); !ok {
			t.Fatalf("run command: %v", runErr)
		}
	}
	result := codexProbeResult{exit: process.ProcessState.ExitCode(), stderr: stderr.String()}
	content, readErr := os.ReadFile(log)
	if readErr != nil {
		if !os.IsNotExist(readErr) {
			t.Fatal(readErr)
		}
		return result
	}
	result.ran = true
	for _, line := range strings.Split(strings.TrimRight(string(content), "\n"), "\n") {
		key, value, _ := strings.Cut(line, "\t")
		switch key {
		case "exe":
			result.exe = value
		case "cwd":
			result.cwd = value
		case "stdin":
			result.stdin = value
		case "arg":
			result.args = append(result.args, value)
		}
	}
	return result
}

// assertReachedRoot asserts the command located wantRoot, executed the managed
// hook there, and delivered the exact argv, stdin and session cwd.
func assertReachedRoot(t *testing.T, result codexProbeResult, wantRoot, sessionCwd string) {
	t.Helper()
	if result.exit != 0 {
		t.Fatalf("command exit = %d, stderr %q", result.exit, result.stderr)
	}
	if !result.ran {
		t.Fatal("hook did not run")
	}
	if want := filepath.Join(wantRoot, filepath.FromSlash(codexProbeHookRelative)); result.exe != want {
		t.Fatalf("hook executable = %q, want %q (command reached the wrong project root)", result.exe, want)
	}
	physical, err := filepath.EvalSymlinks(sessionCwd)
	if err != nil {
		t.Fatal(err)
	}
	if result.cwd != physical {
		t.Fatalf("hook cwd = %q, want %q", result.cwd, physical)
	}
	if result.stdin != codexProbeStdin {
		t.Fatalf("hook stdin = %q, want %q", result.stdin, codexProbeStdin)
	}
	if strings.Join(result.args, "\x00") != strings.Join(codexProbeArgs, "\x00") {
		t.Fatalf("hook argv = %#v, want %#v", result.args, codexProbeArgs)
	}
}

func writeCodexMarker(t *testing.T, root string) {
	t.Helper()
	writeFixtureFile(t, root, "agents.yaml", []byte("schemaVersion: 1\nagents: [codex]\n"), 0o644)
}

func TestCodexRootCommandRunsInNonGitProject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	command := codexRootProbeCommand(t, root)
	writeCodexMarker(t, root)

	t.Run("root", func(t *testing.T) {
		assertReachedRoot(t, runCodexRootCommand(t, command, root), root, root)
	})
	t.Run("nested", func(t *testing.T) {
		nested := filepath.Join(root, "docs", "nested space")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		assertReachedRoot(t, runCodexRootCommand(t, command, nested), root, nested)
	})
}

func TestCodexRootCommandRunsInGitProject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	command := codexRootProbeCommand(t, root)
	writeCodexMarker(t, root)
	initCodexRepository(t, root)

	t.Run("root", func(t *testing.T) {
		assertReachedRoot(t, runCodexRootCommand(t, command, root), root, root)
	})
	t.Run("nested", func(t *testing.T) {
		nested := filepath.Join(root, "docs", "nested space")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		assertReachedRoot(t, runCodexRootCommand(t, command, nested), root, nested)
	})
}

func TestCodexRootCommandRunsInLinkedWorktree(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	command := codexRootProbeCommand(t, base)
	writeCodexMarker(t, base)
	initCodexRepository(t, base)
	gitRun(t, base, "add", "agents.yaml")
	gitRun(t, base, "-c", "user.email=acr@example.test", "-c", "user.name=ACR", "commit", "-q", "-m", "seed")

	worktree := filepath.Join(t.TempDir(), "linked worktree")
	gitRun(t, base, "worktree", "add", "-q", worktree, "HEAD")
	// The linked worktree checks out the tracked marker but not the
	// generated-only .codex tree, so materialize the hook there too.
	codexRootProbeCommand(t, worktree)

	assertReachedRoot(t, runCodexRootCommand(t, command, worktree), worktree, worktree)
}

func TestCodexRootCommandResolvesConsumerInsideEnclosingGitRepo(t *testing.T) {
	t.Parallel()
	// The enclosing directory is a Git repository with no ACR marker; a
	// git-rev-parse resolver would exec the enclosing root's (absent) hook. The
	// ACR consumer lives below it, so the command must resolve to the consumer.
	enclosing := t.TempDir()
	initCodexRepository(t, enclosing)
	consumer := filepath.Join(enclosing, "context consumer")
	if err := os.MkdirAll(consumer, 0o755); err != nil {
		t.Fatal(err)
	}
	command := codexRootProbeCommand(t, consumer)
	writeCodexMarker(t, consumer)

	t.Run("root", func(t *testing.T) {
		assertReachedRoot(t, runCodexRootCommand(t, command, consumer), consumer, consumer)
	})
	t.Run("nested", func(t *testing.T) {
		nested := filepath.Join(consumer, "docs")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		assertReachedRoot(t, runCodexRootCommand(t, command, nested), consumer, nested)
	})
}

func TestCodexRootCommandSurvivesRelocation(t *testing.T) {
	t.Parallel()
	origin := t.TempDir()
	command := codexRootProbeCommand(t, origin)
	writeCodexMarker(t, origin)

	moved := filepath.Join(t.TempDir(), "relocated 'quote $x `tick`")
	copyTree(t, origin, moved)
	nested := filepath.Join(moved, "docs", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// The same rendered command bytes, run from the relocated tree.
	assertReachedRoot(t, runCodexRootCommand(t, command, nested), moved, nested)
}

func TestCodexRootCommandStopsAtNearestProject(t *testing.T) {
	t.Parallel()
	// A nearer initialized ACR project owns the launch; the command must run its
	// hook, never climb to the outer project's executable.
	outer := t.TempDir()
	command := codexRootProbeCommand(t, outer)
	writeCodexMarker(t, outer)
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	codexRootProbeCommand(t, inner)
	writeCodexMarker(t, inner)

	assertReachedRoot(t, runCodexRootCommand(t, command, inner), inner, inner)
}

func TestCodexRootCommandRefusesNearestProjectWithMissingTarget(t *testing.T) {
	t.Parallel()
	// The nearest marker owns the launch even when its managed hook is absent:
	// the command must refuse rather than fall through to the outer project's
	// present executable.
	outer := t.TempDir()
	command := codexRootProbeCommand(t, outer)
	writeCodexMarker(t, outer)
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCodexMarker(t, inner)

	result := runCodexRootCommand(t, command, inner)
	if result.exit == 0 {
		t.Fatalf("command exit = 0, want nonzero when the nearest project has no managed hook")
	}
	if result.ran {
		t.Fatalf("command reached %q, want refusal at the nearest project", result.exe)
	}
}

func TestCodexRootCommandRefusesWithoutMarker(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	command := codexRootProbeCommand(t, root)
	// No agents.yaml is written anywhere above the session cwd.
	bare := t.TempDir()

	result := runCodexRootCommand(t, command, bare)
	if result.exit != 1 {
		t.Fatalf("command exit = %d, want 1 when no marker is found", result.exit)
	}
	if result.ran {
		t.Fatalf("hook ran at %q with no marker present", result.exe)
	}
	if !strings.Contains(result.stderr, "no agents.yaml above session cwd") {
		t.Fatalf("stderr = %q, want the launch-inside-ACR diagnostic", result.stderr)
	}
}

func initCodexRepository(t *testing.T, root string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("Git is required for realization integration tests; install Git and ensure it is on PATH: %v", err)
	}
	gitRun(t, root, "init", "-q")
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func copyTree(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.WalkDir(source, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		content, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, content, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}
