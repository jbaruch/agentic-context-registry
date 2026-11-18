//go:build !windows

package adaptertest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// Exercise the actual realized command, without modifying its translator or
// signal traps. This models Codex's native cancellation, not Cursor internals.
func TestRealizedHookNativeProcessGroupCancellation(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
		for _, stage := range []string{"running-launcher", "exited-retained-streams", "exited-stdout", "exited-stderr", "closed-descendant-streams", "ignoring-grandchild"} {
			t.Run(native.Descriptor().ID+"/"+stage, func(t *testing.T) {
				root, command, hook := realizeFailureHook(t, native, manifest.HookPreToolUse)
				sync := newHookHandshake(t, root, true)
				mustHookWrite(t, hook, nativeGroupHookFixture(stage), 0o755)
				cmd := exec.Command("/bin/sh", "-c", command)
				cmd.Dir = root
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
				cmd.Stdin = strings.NewReader("{}")
				cmd.WaitDelay = 5 * time.Second
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
						select {
						case <-done:
						case <-time.After(10 * time.Second):
							t.Error("fixture command did not exit after failure cleanup")
						}
					}
				})
				sync.expect(t, "native-ready")
				for _, marker := range []string{"launcher", "translator", "descendant"} {
					group, err := os.ReadFile(filepath.Join(root, marker+"-pgid"))
					if err != nil || string(group) != strconv.Itoa(cmd.Process.Pid) {
						t.Fatalf("%s escaped native group %d: %q (%v)", marker, cmd.Process.Pid, group, err)
					}
				}
				if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				// Release the controlled side-effect opportunity immediately
				// after cancellation, before waiting for process disappearance.
				releaseHookSideEffect(t, root)
				select {
				case err := <-done:
					waited = true
					status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
					if err == nil || !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL || out.Len() != 0 || stderr.Len() != 0 {
						t.Fatalf("native cancellation: %v, %v, stdout=%q stderr=%q", err, cmd.ProcessState, out.String(), stderr.String())
					}
				case <-time.After(10 * time.Second):
					t.Fatal("native cancellation did not close command streams")
				}
				sync.terminated(t)
				for _, marker := range []string{"launcher", "translator", "descendant"} {
					assertHookProcessGone(t, root, marker+"-pid", false)
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
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
		for _, direct := range []bool{true, false} {
			for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
				for _, stage := range []string{"ordinary-descendant", "ignoring-descendant", "exited-launcher", "partial-reply", "awaiting-input", "spawn", "repeated-signals"} {
					t.Run(fmt.Sprintf("%s/direct=%v/%s/%s", native.Descriptor().ID, direct, sig, stage), func(t *testing.T) {
						root, command, hook := realizeFailureHook(t, native, manifest.HookPreToolUse)
						id := native.Descriptor().ID
						sync := newHookHandshake(t, root, stage != "awaiting-input")
						runtime := filepath.Join(root, "."+id, "hooks", adapter.HookTranslatorBasename)
						source := instrumentHookSupervisor(t, stage)
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
							if _, err := input.Write([]byte("{}")); err != nil {
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
								if repeated != syscall.SIGTERM {
									sync.expect(t, "cancelled")
								}
							}
							sync.release(t)
						}
						err = cmd.Wait()
						if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 128+int(sig) || out.Len() != 0 || stderr.Len() != 0 {
							t.Fatalf("cancellation: %v; status=%v stdout=%q stderr=%q", err, cmd.ProcessState, out.String(), stderr.String())
						}
						if stage == "awaiting-input" {
							if _, err := os.Stat(filepath.Join(root, "launcher-pid")); !os.IsNotExist(err) {
								t.Fatalf("launched before input EOF: %v", err)
							}
						} else {
							if stage == "ignoring-descendant" {
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

func assertHookProcessGone(t *testing.T, root, marker string, orphan bool) {
	t.Helper()
	pid, err := os.ReadFile(filepath.Join(root, marker))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err := exec.Command("ps", "-p", string(pid), "-o", "stat=").CombinedOutput()
		if err != nil {
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 1 || strings.TrimSpace(string(state)) != "" {
				t.Fatalf("inspect %s: %v; %s", marker, err, state)
			}
		}
		status := strings.TrimSpace(string(state))
		if status == "" || (orphan && strings.HasPrefix(status, "Z")) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %s survived or was not reaped: %s", marker, pid, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRealizedHookBrokenPipe(t *testing.T) {
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
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
				cmd.Stdin = strings.NewReader("{}")
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
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
		for _, event := range []manifest.HookEvent{manifest.HookPreToolUse, manifest.HookUserPromptSubmit, manifest.HookStop} {
			t.Run(native.Descriptor().ID+"/"+string(event), func(t *testing.T) {
				root, command, hook := realizeFailureHook(t, native, event)
				id := native.Descriptor().ID
				sync := newHookHandshake(t, root, true)
				mustHookWrite(t, hook, blockedHookFixture("ordinary-descendant", true), 0o755)
				source := instrumentHookSupervisor(t, "exception")
				mustHookWrite(t, filepath.Join(root, "."+id, "hooks", adapter.HookTranslatorBasename), source, 0o755)
				out, stderr, code := runFailureCommand(t, command, root, "{}", os.Getenv("PATH"))
				got := nativeReply(t, id, event, out, stderr, code)
				if !strings.Contains(stderr, "translator failed (RuntimeError)") || strings.Contains(stderr, "Traceback") {
					t.Fatalf("missing native failure: %d %s %s", code, out, stderr)
				}
				if event == manifest.HookStop {
					if code != 0 || got["decision"] == "block" || got["followup_message"] != nil {
						t.Fatalf("failure trapped Stop: %s", out)
					}
				} else if id == "codex" && code != 2 || id == "cursor" && event == manifest.HookPreToolUse && got["permission"] != "deny" || id == "cursor" && event == manifest.HookUserPromptSubmit && got["continue"] != false {
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
	for _, native := range []adapter.Adapter{codex.New(), cursor.New()} {
		for _, event := range adapter.SortedEvents() {
			for _, stage := range []string{"ignoring-descendant", "hook", "launcher", "forked-launcher", "partial-reply", "exited-launcher-stdout", "exited-launcher-stderr", "exited-launcher-partial"} {
				t.Run(fmt.Sprintf("%s/%s/%s", native.Descriptor().ID, event, stage), func(t *testing.T) {
					id := native.Descriptor().ID
					// 1 * 3 / 4 is exactly zero. Readiness and release establish the
					// intended process stage before the real communicate(timeout=0).
					root, command, hook := realizeFailureHook(t, native, event, 1)
					mustHookWrite(t, hook, blockedHookFixture(stage, stage != "launcher" && stage != "partial-reply"), 0o755)
					mustHookWrite(t, filepath.Join(root, "."+id, "hooks", adapter.HookTranslatorBasename), instrumentHookSupervisor(t, stage), 0o755)
					// Finish writing executable fixtures before running in parallel.
					t.Parallel()
					sync := newHookHandshake(t, root, true)
					cmd := exec.Command("/bin/sh", "-c", command)
					cmd.Dir = root
					cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
					cmd.Stdin = strings.NewReader("{}")
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
					sync.release(t)
					err := cmd.Wait()
					if cmd.ProcessState == nil {
						t.Fatalf("watchdog command: %v", err)
					}
					code := cmd.ProcessState.ExitCode()
					message := "ACR hook ." + id + "/hooks/acr__example__hooks__hook/hook.sh (" + string(event) + "): timed out; stdout and stderr discarded; Repair this hook and run acr realize before retrying."
					assertNativeFailureMessage(t, id, event, out.String(), stderr.String(), code, message, false)
					if stderr.String() != message+"\n" {
						t.Fatalf("watchdog stderr = %q", stderr.String())
					}

					if stage == "ignoring-descendant" {
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
