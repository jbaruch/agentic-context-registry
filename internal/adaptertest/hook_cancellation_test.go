//go:build !windows

package adaptertest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/antigravity"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// Exercise the actual realized command, without modifying its translator or
// signal traps. Antigravity receives a catchable signal at the translator.
// The legacy cases model Codex's documented native group kill.
func TestRealizedHookNativeProcessGroupCancellation(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New(), antigravity.Adapter{}} {
		for _, stage := range []string{"running-launcher", "exited-retained-streams", "exited-stdout", "exited-stderr", "closed-descendant-streams", "ignoring-grandchild"} {
			t.Run(native.Descriptor().ID+"/"+stage, func(t *testing.T) {
				root, command, hook := realizeFailureHook(t, native, manifest.HookPreToolUse)
				id := native.Descriptor().ID
				sync := newHookHandshake(t, root, true)
				mustHookWrite(t, hook, nativeGroupHookFixture(stage), 0o755)
				cmd := exec.Command("/bin/sh", "-c", command)
				cmd.Dir = root
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
				cmd.Stdin = strings.NewReader(hookFixtureInput(id, manifest.HookPreToolUse))
				var out, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &out, &stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				waited := false
				t.Cleanup(func() {
					// A regression may detach the hook. Clean every recorded
					// fixture group, including groups that fail membership checks.
					groups := map[int]bool{cmd.Process.Pid: true}
					for _, marker := range []string{"launcher", "translator", "descendant"} {
						data, err := os.ReadFile(filepath.Join(root, marker+"-pgid"))
						if os.IsNotExist(err) {
							continue
						}
						group, parseErr := strconv.Atoi(string(data))
						if err != nil || parseErr != nil || group <= 0 || group == syscall.Getpgrp() {
							t.Errorf("unsafe fixture group %q: %v %v", data, err, parseErr)
							continue
						}
						groups[group] = true
					}
					for group := range groups {
						if err := syscall.Kill(-group, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
							t.Error(err)
						}
					}
					if !waited {
						<-done
					}
				})
				sync.expect(t, "native-ready")
				hookGroup := strconv.Itoa(cmd.Process.Pid)
				if id == "antigravity" {
					hookGroup = readWantFile(t, root+"/launcher-pid")
				}
				for _, marker := range []string{"launcher", "translator", "descendant"} {
					want := hookGroup
					if marker == "translator" {
						want = strconv.Itoa(cmd.Process.Pid)
					}
					group, err := os.ReadFile(filepath.Join(root, marker+"-pgid"))
					if err != nil || string(group) != want {
						t.Fatalf("%s group got %q, want %s (%v)", marker, group, want, err)
					}
				}
				if id == "antigravity" {
					translatorPID, err := strconv.Atoi(readWantFile(t, filepath.Join(root, "translator-pid")))
					if err != nil {
						t.Fatal(err)
					}
					if err := syscall.Kill(translatorPID, syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
						t.Fatal(err)
					}
					// Make the post-cancellation marker writable after SIGKILL.
					releaseHookSideEffect(t, root)
				}
				err := <-done
				waited = true
				status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
				cancelled := ok && status.Signaled() && status.Signal() == syscall.SIGKILL
				if id == "antigravity" {
					cancelled = cmd.ProcessState.ExitCode() == 143
				}
				if err == nil || !cancelled || out.Len() != 0 || stderr.Len() != 0 {
					t.Fatalf("native cancellation: %v, %v, stdout=%q stderr=%q", err, cmd.ProcessState, out.String(), stderr.String())
				}
				sync.terminated(t)
				for _, marker := range []string{"launcher", "translator", "descendant"} {
					assertHookProcessGone(t, root, marker+"-pid", true)
				}
				if _, err := os.Stat(filepath.Join(root, "post-reply-marker")); !os.IsNotExist(err) {
					t.Fatalf("hook wrote after native cancellation: %v", err)
				}
			})
		}
	}
}

func releaseHookSideEffect(t *testing.T, root string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(root, "hold"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("!")); err != nil {
		t.Fatal(err)
	}
}

func assertIgnoringHookDescendant(t *testing.T, root string, sync hookHandshake) {
	t.Helper()
	pid, err := strconv.Atoi(readWantFile(t, filepath.Join(root, "descendant-pid")))
	if err != nil {
		t.Fatal(err)
	}
	// The valid reply/status has already returned. Prove the documented
	// residual can perform a side effect, then explicitly clean up the fixture.
	defer func() {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			t.Error(err)
		}
	}()
	releaseHookSideEffect(t, root)
	sync.expect(t, "post-reply-write")
	if got := readWantFile(t, filepath.Join(root, "post-reply-marker")); got != "survived" {
		t.Fatalf("ignoring descendant marker = %q", got)
	}
}

// Both direct execution and the realized shell command must retain ownership
// through cancellation, including signals delivered during spawn and cleanup.
func TestHookCancellation(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New(), antigravity.Adapter{}} {
		for _, direct := range []bool{true, false} {
			for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
				for _, stage := range []string{"ordinary-descendant", "ignoring-descendant", "exited-launcher", "partial-reply", "awaiting-input", "spawn", "repeated-signals"} {
					t.Run(fmt.Sprintf("%s/direct=%v/%s/%s", native.Descriptor().ID, direct, sig, stage), func(t *testing.T) {
						root, command, hook := realizeFailureHook(t, native, manifest.HookPreToolUse)
						id := native.Descriptor().ID
						sync := newHookHandshake(t, root, stage != "awaiting-input")
						runtime := filepath.Join(root, hookDirectory(id), "hooks", adapter.HookTranslatorBasename)
						source := instrumentHookSupervisor(t, stage, runtime)
						mustHookWrite(t, runtime, source, 0o755)
						mustHookWrite(t, hook, blockedHookFixture(stage, true), 0o755)
						cmd := exec.Command("/bin/sh", "-c", command)
						if direct {
							rel, err := filepath.Rel(root, hook)
							if err != nil {
								t.Fatal(err)
							}
							cmd = exec.Command("python3", "-I", runtime, id, "pre-tool-use", filepath.ToSlash(rel))
						}
						cmd.Dir = root
						cmd.Env = append(os.Environ(), "acr_hook_timeout=86400")
						cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
						var out, stderr bytes.Buffer
						cmd.Stdout, cmd.Stderr = &out, &stderr
						input, err := cmd.StdinPipe()
						if err != nil {
							t.Fatal(err)
						}
						defer input.Close()
						if err := cmd.Start(); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
								t.Error(err)
							}
							if pid, err := os.ReadFile(filepath.Join(root, "launcher-pid")); err == nil {
								n, err := strconv.Atoi(string(pid))
								if err != nil {
									t.Error(err)
								} else if err := syscall.Kill(-n, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
									t.Error(err)
								}
							} else if !os.IsNotExist(err) {
								t.Error(err)
							}
						})
						if stage == "awaiting-input" {
							sync.expect(t, "input-ready")
						} else {
							if _, err := input.Write([]byte(hookFixtureInput(id, manifest.HookPreToolUse))); err != nil {
								t.Fatal(err)
							}
							if err := input.Close(); err != nil {
								t.Fatal(err)
							}
							if stage == "spawn" {
								sync.expect(t, "spawn-ready")
							} else {
								sync.expect(t, "stage-ready")
							}
						}
						assertDedicatedHookGroup(t, root, cmd.Process.Pid, direct)
						// Signal the acknowledged translator owner. The native shell
						// remains alive to return its numeric cancellation status.
						pidText, err := os.ReadFile(filepath.Join(root, "supervisor-pid"))
						if err != nil {
							t.Fatal(err)
						}
						supervisorPID, err := strconv.Atoi(string(pidText))
						if err != nil {
							t.Fatal(err)
						}
						if err := syscall.Kill(supervisorPID, sig); err != nil {
							t.Fatal(err)
						}
						if stage == "awaiting-input" {
							if err := input.Close(); err != nil {
								t.Fatal(err)
							}
						}
						sync.expect(t, "cancelled")
						if stage == "spawn" {
							sync.release(t)
						}
						if stage == "repeated-signals" {
							sync.expect(t, "cleanup-ready")
							for _, repeated := range []syscall.Signal{syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM} {
								if err := syscall.Kill(supervisorPID, repeated); err != nil {
									t.Fatal(err)
								}
								if repeated != syscall.SIGTERM || id == "antigravity" {
									sync.expect(t, "cancelled")
								}
							}
							sync.release(t)
						}
						done := make(chan error, 1)
						go func() { done <- cmd.Wait() }()
						err = <-done
						if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 128+int(sig) || out.Len() != 0 || stderr.Len() != 0 {
							t.Fatalf("cancellation: %v; status=%v stdout=%q stderr=%q", err, cmd.ProcessState, out.String(), stderr.String())
						}
						if stage == "awaiting-input" {
							if _, err := os.Stat(filepath.Join(root, "launcher-pid")); !os.IsNotExist(err) {
								t.Fatalf("launched before input EOF: %v", err)
							}
						} else {
							if stage == "ignoring-descendant" && id != "antigravity" {
								assertIgnoringHookDescendant(t, root, sync)
							}
							sync.terminated(t)
							assertHookCleanup(t, root)
							assertHookProcessGone(t, root, "descendant-pid", true)
						}
					})
				}
			}
		}
	}
}

func assertDedicatedHookGroup(t *testing.T, root string, commandPID int, direct bool) {
	t.Helper()
	group := readWantFile(t, filepath.Join(root, "supervisor-pgid"))
	pid := readWantFile(t, filepath.Join(root, "supervisor-pid"))
	parent := readWantFile(t, filepath.Join(root, "supervisor-ppid"))
	if group != strconv.Itoa(commandPID) || (direct && pid != group) || (!direct && (pid == group || parent == group)) {
		t.Fatalf("dedicated topology: command=%d group=%s translator=%s parent=%s direct=%v", commandPID, group, pid, parent, direct)
	}
}

// A foreign host owns the group. Its sibling and the host must remain untouched
// while the translator terminates its owned processes and closes retained pipes.
func TestHookSharedGroup(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New(), antigravity.Adapter{}} {
		for _, direct := range []bool{false, true} {
			for _, sig := range []syscall.Signal{0, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
				t.Run(fmt.Sprintf("%s/direct=%v/signal=%d", native.Descriptor().ID, direct, sig), func(t *testing.T) {
					id := native.Descriptor().ID
					var root, command, hook string
					if sig == 0 {
						root, command, hook = realizeFailureHook(t, native, manifest.HookPreToolUse, 1)
					} else {
						root, command, hook = realizeFailureHook(t, native, manifest.HookPreToolUse)
					}
					sync := newHookHandshake(t, root, true)
					runtime := filepath.Join(root, hookDirectory(id), "hooks", adapter.HookTranslatorBasename)
					mustHookWrite(t, runtime, instrumentHookSupervisor(t, "partial-reply", runtime), 0o755)
					// Default TERM handling deliberately survives: in a shared group
					// the grandchild receives no translator-originated signal.
					body := blockedHookFixture("ignoring-descendant", true)
					body = strings.Replace(body, "signal.signal(signal.SIGTERM, signal.SIG_IGN)\n", "", 1)
					body = strings.Replace(body, "life = os.open", "sys.stdout.write('{\"partial\":')\nsys.stdout.flush()\nlife = os.open", 1)
					mustHookWrite(t, hook, body, 0o755)
					args := []string{"/bin/sh", "-c", command}
					budget := "86400"
					if sig == 0 {
						budget = "1"
					}
					if direct {
						rel, err := filepath.Rel(root, hook)
						if err != nil {
							t.Fatal(err)
						}
						args = []string{"python3", "-I", runtime, id, "pre-tool-use", filepath.ToSlash(rel)}
					}
					input := hookFixtureInput(id, manifest.HookPreToolUse)
					cmd := exec.Command("python3", append([]string{"-I", "-c", sharedGroupHostFixture, budget, input}, args...)...)
					cmd.Dir = root
					cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
					control, err := cmd.StdinPipe()
					if err != nil {
						t.Fatal(err)
					}
					defer control.Close()
					replies, err := cmd.StdoutPipe()
					if err != nil {
						t.Fatal(err)
					}
					var hostErrors bytes.Buffer
					cmd.Stderr = &hostErrors
					if err := cmd.Start(); err != nil {
						t.Fatal(err)
					}
					waited := false
					t.Cleanup(func() {
						// This group belongs solely to this synthetic host fixture.
						if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
							t.Error(err)
						}
						if !waited {
							if err := cmd.Wait(); err == nil {
								t.Error("aborted host unexpectedly succeeded")
							}
						}
					})
					reader := bufio.NewReader(replies)
					started, err := reader.ReadString('\n')
					if err != nil || started != "host-ready\n" {
						t.Fatalf("host readiness: %q %v", started, err)
					}
					sync.expect(t, "stage-ready")
					group := strconv.Itoa(cmd.Process.Pid)
					pid := readWantFile(t, filepath.Join(root, "supervisor-pid"))
					parent := readWantFile(t, filepath.Join(root, "supervisor-ppid"))
					wrapper := readWantFile(t, filepath.Join(root, "command-pid"))
					if readWantFile(t, filepath.Join(root, "supervisor-pgid")) != group || pid == group || wrapper == group || (direct && parent != group) || (!direct && parent == group) {
						t.Fatalf("shared topology: host=%s command=%s translator=%s parent=%s direct=%v", group, wrapper, pid, parent, direct)
					}
					if sig == 0 {
						sync.release(t)
					} else {
						n, err := strconv.Atoi(pid)
						if err != nil {
							t.Fatal(err)
						}
						if err := syscall.Kill(n, sig); err != nil {
							t.Fatal(err)
						}
						sync.expect(t, "cancelled")
					}
					completed, err := reader.ReadString('\n')
					if err != nil || completed != "command-complete\n" {
						t.Fatalf("host completion: %q %v", completed, err)
					}
					if _, err := control.Write([]byte("query\n")); err != nil {
						t.Fatal(err)
					}
					var result struct {
						Code                    int
						Out, Err                string
						HostTerms, SiblingTerms int
					}
					line, err := reader.ReadBytes('\n')
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(line, &result); err != nil {
						t.Fatalf("host reply %q: %v", line, err)
					}
					if result.HostTerms != 0 || result.SiblingTerms != 0 {
						t.Fatalf("foreign group signalled: host=%d sibling=%d", result.HostTerms, result.SiblingTerms)
					}
					if sig == 0 {
						message := "ACR hook " + hookDirectory(id) + "/hooks/acr__example__hooks__hook/hook.sh (pre-tool-use): timed out; stdout and stderr discarded; " + hookFixtureRecovery(id)
						assertNativeFailureMessage(t, id, manifest.HookPreToolUse, result.Out, result.Err, result.Code, message, false)
					} else if result.Code != 128+int(sig) || result.Out != "" || result.Err != "" {
						t.Fatalf("shared cancellation: %+v", result)
					}
					if strings.Contains(result.Out, "partial") || strings.Contains(result.Err, "partial") {
						t.Fatalf("partial output escaped: %+v", result)
					}
					assertHookCleanup(t, root)
					if id != "antigravity" {
						assertIgnoringHookDescendant(t, root, sync)
					}
					sync.terminated(t)
					assertHookProcessGone(t, root, "descendant-pid", true)
					if _, err := control.Write([]byte("exit\n")); err != nil {
						t.Fatal(err)
					}
					err = cmd.Wait()
					waited = true
					if err != nil || hostErrors.Len() != 0 {
						t.Fatalf("host completion: %v %s", err, hostErrors.String())
					}
				})
			}
		}
	}
}

const sharedGroupHostFixture = `import json, os, signal, subprocess, sys
terms = []
signal.signal(signal.SIGTERM, lambda signum, frame: terms.append(signum))
sibling_code = '''import signal, sys
terms = []
signal.signal(signal.SIGTERM, lambda signum, frame: terms.append(signum))
print('ready', flush=True)
for command in sys.stdin:
    if command == 'exit\\n':
        break
    print(len(terms), flush=True)
'''
sibling = subprocess.Popen([sys.executable, '-I', '-c', sibling_code], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
assert sibling.stdout.readline() == 'ready\n'
env = dict(os.environ, acr_hook_timeout=sys.argv[1])
env.pop('acr_hook_command_pid', None)
child = subprocess.Popen(sys.argv[3:], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env)
with open('command-pid', 'w') as stream:
    stream.write(str(child.pid))
print('host-ready', flush=True)
out, err = child.communicate(sys.argv[2].encode())
print('command-complete', flush=True)
assert sys.stdin.readline() == 'query\n'
sibling.stdin.write('query\n')
sibling.stdin.flush()
sibling_terms = int(sibling.stdout.readline())
print(json.dumps(dict(Code=child.returncode, Out=out.decode(), Err=err.decode(), HostTerms=len(terms), SiblingTerms=sibling_terms)), flush=True)
assert sys.stdin.readline() == 'exit\n'
sibling.stdin.write('exit\n')
sibling.stdin.flush()
assert sibling.wait() == 0
`

func assertHookProcessGone(t *testing.T, root, marker string, orphan bool) {
	t.Helper()
	pid, err := os.ReadFile(filepath.Join(root, marker))
	if err != nil {
		t.Fatal(err)
	}
	// Call only after lifetime EOF and command completion. An orphan may
	// remain a zombie until the OS reaps it; it cannot execute or write files.
	state, err := exec.Command("ps", "-p", string(pid), "-o", "stat=").CombinedOutput()
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 1 || strings.TrimSpace(string(state)) != "" {
			t.Fatalf("inspect %s: %v; %s", marker, err, state)
		}
	}
	status := strings.TrimSpace(string(state))
	if status != "" && !(orphan && strings.HasPrefix(status, "Z")) {
		t.Fatalf("%s %s survived or was not reaped: %s", marker, pid, status)
	}
}

func TestRealizedHookBrokenPipe(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New(), antigravity.Adapter{}} {
		for _, stream := range []string{"stdout", "stderr"} {
			t.Run(native.Descriptor().ID+"/"+stream, func(t *testing.T) {
				root, command, hook := realizeFailureHook(t, native, manifest.HookPreToolUse)
				body := "#!/bin/sh\nprintf '{}'\n"
				if stream == "stderr" {
					body += "printf reason >&2\n"
				}
				mustHookWrite(t, hook, body, 0o755)
				read, write, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := read.Close(); err != nil {
					t.Fatal(err)
				}
				defer write.Close()
				cmd := exec.Command("/bin/sh", "-c", command)
				cmd.Dir = root
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				cmd.Stdin = strings.NewReader(hookFixtureInput(native.Descriptor().ID, manifest.HookPreToolUse))
				var out, stderr bytes.Buffer
				if stream == "stdout" {
					cmd.Stdout, cmd.Stderr = write, &stderr
				} else {
					cmd.Stdout, cmd.Stderr = &out, write
				}
				err = cmd.Run()
				if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 141 || out.Len() != 0 || stderr.Len() != 0 {
					t.Fatalf("host EPIPE: %v status=%v stdout=%q stderr=%q", err, cmd.ProcessState, out.String(), stderr.String())
				}
			})
		}
	}
}

func TestRealizedHookExceptionAfterLaunch(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New(), antigravity.Adapter{}} {
		for _, event := range native.SupportedEvents() {
			if event != manifest.HookPreToolUse && event != manifest.HookUserPromptSubmit && event != manifest.HookStop {
				continue
			}
			t.Run(native.Descriptor().ID+"/"+string(event), func(t *testing.T) {
				root, command, hook := realizeFailureHook(t, native, event)
				id := native.Descriptor().ID
				sync := newHookHandshake(t, root, true)
				mustHookWrite(t, hook, blockedHookFixture("ordinary-descendant", true), 0o755)
				runtime := filepath.Join(root, hookDirectory(id), "hooks", adapter.HookTranslatorBasename)
				source := instrumentHookSupervisor(t, "exception", runtime)
				mustHookWrite(t, filepath.Join(root, hookDirectory(id), "hooks", adapter.HookTranslatorBasename), source, 0o755)
				out, stderr, code := runFailureCommand(t, command, root, hookFixtureInput(id, event), os.Getenv("PATH"))
				got := nativeReply(t, id, event, out, stderr, code)
				if !strings.Contains(stderr, "translator failed (RuntimeError)") || strings.Contains(stderr, "Traceback") {
					t.Fatalf("missing native failure: %d %s %s", code, out, stderr)
				}
				if event == manifest.HookStop {
					if code != 0 || got["decision"] == "block" || got["decision"] == "continue" || got["followup_message"] != nil {
						t.Fatalf("failure trapped Stop: %s", out)
					}
				} else if id == "antigravity" && got["decision"] != "deny" || id == "codex" && code != 2 || id == "cursor" && event == manifest.HookPreToolUse && got["permission"] != "deny" || id == "cursor" && event == manifest.HookUserPromptSubmit && got["continue"] != false {
					t.Fatalf("exception failed open: %d %s %s", code, out, stderr)
				}
				sync.terminated(t)
				assertHookCleanup(t, root)
				assertHookProcessGone(t, root, "launcher-pid", false)
				assertHookProcessGone(t, root, "descendant-pid", true)
			})
		}
	}
}

func TestRealizedHookWatchdog(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New(), antigravity.Adapter{}} {
		for _, event := range native.SupportedEvents() {
			for _, stage := range []string{"ignoring-descendant", "hook", "launcher", "forked-launcher", "partial-reply", "exited-launcher-stdout", "exited-launcher-stderr", "exited-launcher-partial"} {
				t.Run(fmt.Sprintf("%s/%s/%s", native.Descriptor().ID, event, stage), func(t *testing.T) {
					id := native.Descriptor().ID
					// 1 * 3 / 4 is exactly zero. Readiness and release establish the
					// intended process stage before the real communicate(timeout=0).
					root, command, hook := realizeFailureHook(t, native, event, 1)
					mustHookWrite(t, hook, blockedHookFixture(stage, stage != "launcher" && stage != "partial-reply"), 0o755)
					runtime := filepath.Join(root, hookDirectory(id), "hooks", adapter.HookTranslatorBasename)
					mustHookWrite(t, runtime, instrumentHookSupervisor(t, stage, runtime), 0o755)
					// Finish writing executable fixtures before running in parallel.
					t.Parallel()
					sync := newHookHandshake(t, root, true)
					cmd := exec.Command("/bin/sh", "-c", command)
					cmd.Dir = root
					cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
					cmd.Stdin = strings.NewReader(hookFixtureInput(id, event))
					var out, stderr bytes.Buffer
					cmd.Stdout, cmd.Stderr = &out, &stderr
					if err := cmd.Start(); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
							t.Error(err)
						}
					})
					sync.expect(t, "stage-ready")
					assertDedicatedHookGroup(t, root, cmd.Process.Pid, false)
					sync.release(t)
					done := make(chan error, 1)
					go func() { done <- cmd.Wait() }()
					err := <-done
					if cmd.ProcessState == nil {
						t.Fatalf("watchdog command: %v", err)
					}
					code := cmd.ProcessState.ExitCode()
					message := "ACR hook " + hookDirectory(id) + "/hooks/acr__example__hooks__hook/hook.sh (" + string(event) + "): timed out; stdout and stderr discarded; " + hookFixtureRecovery(id)
					assertNativeFailureMessage(t, id, event, out.String(), stderr.String(), code, message, false)
					if stderr.String() != message+"\n" {
						t.Fatalf("watchdog stderr = %q", stderr.String())
					}

					if stage == "ignoring-descendant" && id != "antigravity" {
						assertIgnoringHookDescendant(t, root, sync)
					}
					sync.terminated(t)
					assertHookCleanup(t, root)
					if stage != "launcher" && stage != "partial-reply" {
						assertHookProcessGone(t, root, "descendant-pid", true)
					}
				})
			}
		}
	}
}
