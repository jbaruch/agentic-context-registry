//go:build !windows

package adaptertest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Control FIFOs establish process stages without a clock. The hook launcher
// passes its lifetime writer to every descendant: readiness proves ownership
// and EOF proves they all exited, including orphans left after launcher reaping.
type hookHandshake struct {
	events   *bufio.Reader
	resume   *os.File
	lifetime <-chan error
}

func newHookHandshake(t *testing.T, root string, lifetime bool) hookHandshake {
	t.Helper()
	for _, name := range []string{"events", "resume", "hook-ready", "hold", "lifetime"} {
		if out, err := exec.Command("mkfifo", filepath.Join(root, name)).CombinedOutput(); err != nil {
			t.Fatalf("create %s FIFO: %v; %s", name, err, out)
		}
	}
	open := func(name string) *os.File {
		f, err := os.OpenFile(filepath.Join(root, name), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := f.Close(); err != nil {
				t.Error(err)
			}
		})
		return f
	}
	sync := hookHandshake{events: bufio.NewReader(open("events")), resume: open("resume")}
	if lifetime {
		done := make(chan error, 1)
		sync.lifetime = done
		go func() {
			f, err := os.Open(filepath.Join(root, "lifetime"))
			if err != nil {
				done <- err
				return
			}
			_, readErr := io.Copy(io.Discard, f)
			closeErr := f.Close()
			if readErr != nil {
				done <- readErr
			} else {
				done <- closeErr
			}
		}()
	}
	return sync
}

func (s hookHandshake) expect(t *testing.T, want string) {
	t.Helper()
	got, err := s.events.ReadString('\n')
	if err != nil {
		t.Fatalf("fixture stage: %q (%v)", got, err)
	}
	if got != want+"\n" {
		t.Fatalf("fixture stage = %q, want %q", got, want)
	}
}

func (s hookHandshake) release(t *testing.T) {
	t.Helper()
	if _, err := s.resume.Write([]byte("!")); err != nil {
		t.Fatal(err)
	}
}

func (s hookHandshake) terminated(t *testing.T) {
	t.Helper()
	if err := <-s.lifetime; err != nil {
		t.Fatalf("process lifetime pipe: %v", err)
	}
}

func nativeGroupHookFixture(stage string) string {
	child := `import os, signal
root = os.environ['CLAUDE_PROJECT_DIR']
`
	if stage == "ignoring-grandchild" {
		child += "signal.signal(signal.SIGTERM, signal.SIG_IGN)\n"
	}
	if stage == "closed-descendant-streams" {
		child += "os.close(1)\nos.close(2)\n"
	}
	child += `for name, value in [('descendant-pid', os.getpid()), ('descendant-pgid', os.getpgrp())]:
    with open(os.path.join(root, name), 'w') as stream:
        stream.write(str(value))
`
	if strings.HasPrefix(stage, "exited-") {
		child += "assert os.read(int(os.environ['PARENT_LIFETIME_FD']), 1) == b''\n"
	}
	child += `with open(os.path.join(root, 'events'), 'w') as stream:
    stream.write('native-ready\n')
with open(os.path.join(root, 'hold'), 'r+b', buffering=0) as stream:
    stream.read(1)
with open(os.path.join(root, 'post-reply-marker'), 'w') as stream:
    stream.write('survived')
`
	body := `#!/usr/bin/env python3
import os, subprocess, sys
root = os.environ['CLAUDE_PROJECT_DIR']
for name, value in [('launcher-pid', os.getpid()), ('launcher-pgid', os.getpgrp()),
                    ('translator-pid', os.getppid()), ('translator-pgid', os.getpgid(os.getppid()))]:
    with open(os.path.join(root, name), 'w') as stream:
        stream.write(str(value))
life = os.open(os.path.join(root, 'lifetime'), os.O_WRONLY)
sys.stdout.write('{"partial":')
sys.stdout.flush()
`
	redirect := ""
	if stage == "exited-stdout" {
		redirect = ", stderr=subprocess.DEVNULL"
	} else if stage == "exited-stderr" {
		redirect = ", stdout=subprocess.DEVNULL"
	}
	passFDs := "life,"
	if strings.HasPrefix(stage, "exited-") {
		// Only the launcher owns this writer. Its death releases the child's
		// blocking read, even if the launcher exits before the child starts.
		body += "parent_read, parent_write = os.pipe()\nos.environ['PARENT_LIFETIME_FD'] = str(parent_read)\n"
		passFDs += " parent_read,"
	}
	body += fmt.Sprintf("child = subprocess.Popen([sys.executable, '-I', '-c', %q], pass_fds=(%s)%s)\n", child, passFDs, redirect)
	if strings.HasPrefix(stage, "exited-") {
		body += "os.close(parent_read)\n"
	} else {
		body += "child.wait()\n"
	}
	return body
}

// Instrument only the realized copy, retaining the real communicate call,
// timeout handler, signal recording and process-group cleanup.
func instrumentHookSupervisor(t *testing.T, stage, runtime string) string {
	t.Helper()
	content, err := os.ReadFile(runtime)
	if err != nil {
		t.Fatal(err)
	}
	source := string(content)
	replace := func(old, new string) {
		if strings.Count(source, old) != 1 {
			t.Fatalf("supervisor fixture boundary missing or ambiguous: %q", old)
		}
		source = strings.Replace(source, old, new, 1)
	}
	replace(`if __name__ == "__main__":`, `import select
fixture_root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
fixture_resume = os.open(os.path.join(fixture_root, 'resume'), os.O_RDWR | os.O_NONBLOCK)
signal.set_wakeup_fd(fixture_resume)
for name, value in [('supervisor-pid', os.getpid()), ('supervisor-ppid', os.getppid()),
                    ('supervisor-pgid', os.getpgrp())]:
    with open(os.path.join(fixture_root, name), 'w') as stream:
        stream.write(str(value))
def fixture_notify(stage):
    with open(os.path.join(fixture_root, 'events'), 'w') as stream:
        stream.write(stage + '\n')
def fixture_release():
    while True:
        select.select([fixture_resume], [], [])
        token = os.read(fixture_resume, 1)
        if token == b'!':
            return
        assert token in (bytes([sig]) for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP))
def fixture_ready():
    with open(os.path.join(fixture_root, 'hook-ready'), 'rb', buffering=0) as stream:
        assert stream.read(1) == b'!'

if __name__ == "__main__":`)
	replace("    if not defer_cancellation:\n", "    fixture_notify('cancelled')\n    if not defer_cancellation:\n")
	replace("                            hook.wait()", "                            hook.wait()\n                            open(os.path.join(root, 'reaped'), 'w').write(str(hook.returncode))")
	replace("                                    pipe.close()", "                                    pipe.close()\n                            open(os.path.join(root, 'closed'), 'w').write(str(all(pipe is None or pipe.closed for pipe in (hook.stdin, hook.stdout, hook.stderr))))")
	if stage == "awaiting-input" {
		replace("    raw = sys.stdin.buffer.read()", "    fixture_notify('input-ready')\n    raw = sys.stdin.buffer.read()")
	} else if stage == "spawn" {
		const finishSpawn = "            finally:\n                defer_cancellation = False\n            if cancelled:"
		replace(finishSpawn, "                fixture_ready()\n                fixture_notify('spawn-ready')\n                fixture_release()\n"+finishSpawn)
	} else {
		before := "                fixture_ready()\n"
		antigravity := strings.Contains(source, "def antigravity_input(")
		if strings.HasPrefix(stage, "exited-launcher") && !antigravity {
			before += "                hook.wait()\n"
		}
		if stage == "exception" {
			before += "                raise RuntimeError('injected after launch')\n"
		} else {
			before += "                fixture_notify('stage-ready')\n                fixture_release()\n"
		}
		const communicate = "                stdout, stderr = hook.communicate(canonical, timeout=timeout)"
		replace(communicate, before+communicate)
	}
	if stage == "repeated-signals" {
		if strings.Contains(source, "def antigravity_input(") {
			const groupCleanup = "                            if not captured and hook.returncode is None:"
			replace(groupCleanup, "                            fixture_notify('cleanup-ready')\n                            fixture_release()\n"+groupCleanup)
		} else {
			const groupCleanup = "                            pgid = os.getpgrp()"
			replace(groupCleanup, "                            fixture_notify('cleanup-ready')\n                            fixture_release()\n"+groupCleanup)
		}
	}
	return source
}

func blockedHookFixture(stage string, descendant bool) string {
	// The launcher owns the lifetime writer before spawning. Every child
	// inherits it, so EOF covers the entire hook tree after cleanup.
	blocked := `import os, signal
root = os.environ['CLAUDE_PROJECT_DIR']
if 'PARENT_LIFETIME_FD' in os.environ:
    assert os.read(int(os.environ['PARENT_LIFETIME_FD']), 1) == b''
with open(os.path.join(root, 'descendant-pid'), 'w') as stream:
    stream.write(str(os.getpid()))
with open(os.path.join(root, 'hook-ready'), 'wb', buffering=0) as stream:
    stream.write(b'!')
with open(os.path.join(root, 'hold'), 'r+b', buffering=0) as stream:
    stream.read(1)
`
	if stage == "ignoring-descendant" {
		blocked = strings.Replace(blocked, "import os, signal", "import os, signal\nsignal.signal(signal.SIGTERM, signal.SIG_IGN)", 1)
		blocked += `with open(os.path.join(root, 'post-reply-marker'), 'w') as stream:
    stream.write('survived')
with open(os.path.join(root, 'events'), 'w') as stream:
    stream.write('post-reply-write\n')
with open(os.path.join(root, 'hold'), 'r+b', buffering=0) as stream:
    stream.read(1)
`
	}
	body := `#!/usr/bin/env python3
import os, signal, subprocess, sys
root = os.environ['CLAUDE_PROJECT_DIR']
life = os.open(os.path.join(root, 'lifetime'), os.O_WRONLY)
for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
    signal.signal(sig, lambda signum, frame: None)
with open(os.path.join(root, 'launcher-pid'), 'w') as stream:
    stream.write(str(os.getpid()))
`
	if stage == "partial-reply" || stage == "exited-launcher-partial" {
		body += "sys.stdout.write('{\"permission\":\"allow\"')\nsys.stdout.flush()\n"
	}
	if !descendant {
		return body + strings.Replace(blocked, "'descendant-pid'", "'launcher-pid'", 1)
	}
	redirect := ""
	if stage == "exited-launcher-stderr" {
		redirect = ", stdout=subprocess.DEVNULL"
	}
	if stage == "exited-launcher-stdout" {
		redirect = ", stderr=subprocess.DEVNULL"
	}
	passFDs := "life,"
	if strings.HasPrefix(stage, "exited-launcher") {
		body += "parent_read, parent_write = os.pipe()\nos.environ['PARENT_LIFETIME_FD'] = str(parent_read)\n"
		passFDs += " parent_read,"
	}
	body += fmt.Sprintf("child = subprocess.Popen([sys.executable, '-I', '-c', %q], pass_fds=(%s)%s)\n", blocked, passFDs, redirect)
	if !strings.HasPrefix(stage, "exited-launcher") {
		body += "child.wait()\n"
	}
	return body
}

func assertHookCleanup(t *testing.T, root string) {
	t.Helper()
	if _, err := os.ReadFile(filepath.Join(root, "reaped")); err != nil {
		t.Fatalf("launcher wait did not finish: %v", err)
	}
	closed, err := os.ReadFile(filepath.Join(root, "closed"))
	if err != nil || string(closed) != "True" {
		t.Fatalf("capture pipes not closed: %q; %v", closed, err)
	}
	assertHookProcessGone(t, root, "launcher-pid", false)
}
