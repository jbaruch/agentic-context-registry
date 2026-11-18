package codex_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
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
const codexProbeHookBody = `#!/usr/bin/env bash
set -euo pipefail
stdin="$(cat)"
{
  printf 'exe\t%s\n' "$0"
  printf 'cwd\t%s\n' "$(pwd -P)"
  printf 'root\t%s\n' "$CLAUDE_PROJECT_DIR"
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
	stdout string
	stderr string
	ran    bool
	exe    string
	cwd    string
	root   string
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
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	var stdout, stderr bytes.Buffer
	process.Stdout = &stdout
	process.Stderr = &stderr
	runErr := process.Run()
	if runErr != nil {
		if _, ok := runErr.(*exec.ExitError); !ok {
			t.Fatalf("run command: %v", runErr)
		}
	}
	result := codexProbeResult{exit: process.ProcessState.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}
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
		case "root":
			result.root = value
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
	if result.root != wantRoot {
		t.Fatalf("CLAUDE_PROJECT_DIR = %q, want %q", result.root, wantRoot)
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
	// Go finishes sequential tests before releasing parallel tests, so no sibling
	// can fork while an executable's write descriptor is open (Linux ETXTBSY).
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
	// Go finishes sequential tests before releasing parallel tests, so no sibling
	// can fork while an executable's write descriptor is open (Linux ETXTBSY).
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
	// Go finishes sequential tests before releasing parallel tests, so no sibling
	// can fork while an executable's write descriptor is open (Linux ETXTBSY).
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
	// Go finishes sequential tests before releasing parallel tests, so no sibling
	// can fork while an executable's write descriptor is open (Linux ETXTBSY).
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
	// Go finishes sequential tests before releasing parallel tests, so no sibling
	// can fork while an executable's write descriptor is open (Linux ETXTBSY).
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
	// Go finishes sequential tests before releasing parallel tests, so no sibling
	// can fork while an executable's write descriptor is open (Linux ETXTBSY).
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
	if result.exit != 0 || !strings.Contains(result.stdout, "systemMessage") {
		t.Fatalf("command exit = %d, want an accepted diagnostic when the nearest project has no managed hook: %s", result.exit, result.stdout)
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
	if result.exit != 0 || !strings.Contains(result.stdout, "systemMessage") {
		t.Fatalf("command exit = %d, want an accepted diagnostic when no marker is found: %s", result.exit, result.stdout)
	}
	if result.ran {
		t.Fatalf("hook ran at %q with no marker present", result.exe)
	}
	message := "ACR hook .codex/hooks/acr__example__all-agents__session-start/session-start.sh (session-start): runtime failure: no agents.yaml above session cwd; Launch inside the ACR project."
	if result.stderr != message+"\n" {
		t.Fatalf("stderr = %q; want %q", result.stderr, message+"\n")
	}
	var reply map[string]any
	if err := json.Unmarshal([]byte(result.stdout), &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply) != 1 || reply["systemMessage"] != message {
		t.Fatalf("diagnostic = %s; want %q", result.stdout, message)
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

func TestTranslatedCommandsInNestedConsumer(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		event                       manifest.HookEvent
		native, reply, want, stderr string
		code                        int
	}{
		{"ask", manifest.HookPreToolUse, "PreToolUse", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask","permissionDecisionReason":"R"}}`, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"ACR hook .codex/hooks/acr__example__all-agents__session-start/start.sh (pre-tool-use): denied the tool call: R\nCodex cannot ask for confirmation here. Ask the user in the conversation and retry if they approve."}}`, "", 0},
		{"deny with unsupported fields", manifest.HookPreToolUse, "PreToolUse", `{"continue":false,"stopReason":"X","hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"R"}}`, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"ACR hook .codex/hooks/acr__example__all-agents__session-start/start.sh (pre-tool-use): denied the tool call: R\nReview this hook's policy before retrying."}}`, "dropped unsupported fields", 0},
		{"exit two", manifest.HookPreToolUse, "PreToolUse", "", "", "R", 2},
		{"post block", manifest.HookPostToolUse, "PostToolUse", `{"decision":"block","reason":"R"}`, `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"ACR hook .codex/hooks/acr__example__all-agents__session-start/start.sh (post-tool-use): provided feedback after the tool call: R\nReview this hook's policy before continuing."}}`, "", 0},
		{"plain stop", manifest.HookStop, "Stop", "stop", `{}`, "dropped plain text", 0},
		{"session unchanged", manifest.HookSessionStart, "SessionStart", ` {"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"S"}} `, ` {"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"S"}} `, "", 0},
		{"prompt unchanged", manifest.HookUserPromptSubmit, "UserPromptSubmit", ` {"decision":"block","reason":"R"} `, `{"decision":"block","reason":"ACR hook .codex/hooks/acr__example__all-agents__session-start/start.sh (user-prompt-submit): blocked the prompt: R\nReview this hook's policy before retrying."}`, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enclosing := t.TempDir()
			initCodexRepository(t, enclosing)
			root := filepath.Join(enclosing, "consumer with 'quotes'")
			cwd := filepath.Join(root, "src", "nested")
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			writeCodexMarker(t, root)
			packageRoot := t.TempDir()
			pkg := codexHookPackage(t)
			pkg.Root = os.DirFS(packageRoot)
			hook := &pkg.Manifest.Artifacts.Hooks[0]
			hook.Event = tc.event
			hook.Args = codexProbeArgs
			body := codexProbeHookBody + "printf '%s' " + adapter.ShellQuote(tc.reply) + "\n"
			if tc.code == 2 {
				body += "printf R >&2\nexit 2\n"
			}
			writeFixtureFile(t, packageRoot, hook.Path, []byte(body), 0o755)
			intents := mustRealize(t, codex.New(), root, []adapter.Package{pkg})
			if _, err := realize.NewEngine().Run(root, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, intents, realize.ModeApply, func(realize.Ledger) error { return nil }); err != nil {
				t.Fatal(err)
			}
			command := realizedCodexCommands(t, intents)[tc.native][0]
			// Capture native output in addition to the recording hook's input/root/cwd/argv.
			log := filepath.Join(t.TempDir(), "probe.log")
			cmd := exec.Command("sh", "-c", command)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Dir = cwd
			cmd.Env = append(os.Environ(), "PWD="+cwd, "ACR_PROBE_LOG="+log, "CLAUDE_PROJECT_DIR=/wrong")
			cmd.Stdin = strings.NewReader(codexProbeStdin)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.code {
				t.Fatalf("run: %v, stderr: %s", err, stderr.String())
			}
			if tc.code == 2 && stderr.String() != "ACR hook .codex/hooks/acr__example__all-agents__session-start/start.sh (pre-tool-use): denied the tool call: R\nReview this hook's policy before retrying.\n" {
				t.Fatalf("exit two stderr = %q", stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.stderr) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.stderr)
			}
			if tc.want == "" {
				if stdout.String() != tc.want {
					t.Fatalf("stdout = %q, want %q", stdout.String(), tc.want)
				}
			} else {
				var got, want any
				if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("stdout = %s, want %s", stdout.String(), tc.want)
				}
			}
			recorded, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			physical, err := filepath.EvalSymlinks(cwd)
			if err != nil {
				t.Fatal(err)
			}
			for _, fact := range []string{"root\t" + root + "\n", "cwd\t" + physical + "\n", "stdin\t" + codexProbeStdin + "\n"} {
				if !strings.Contains(string(recorded), fact) {
					t.Fatalf("missing %q in %s", fact, recorded)
				}
			}
		})
	}
}
