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
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

// Control FIFOs establish process stages without a clock. Only the blocked
// process owns the lifetime FIFO's writer: readiness proves ownership and EOF
// proves termination, including an orphan killed after its launcher was reaped.
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
	done := make(chan string, 1)
	go func() {
		got, err := s.events.ReadString('\n')
		if err != nil {
			done <- fmt.Sprintf("%q (%v)", got, err)
			return
		}
		done <- got
	}()
	select {
	case got := <-done:
		if got != want+"\n" {
			t.Fatalf("fixture stage = %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("fixture never reached %q", want)
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
	select {
	case err := <-s.lifetime:
		if err != nil {
			t.Fatalf("process lifetime pipe: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("hook retained its lifetime pipe after cleanup")
	}
}

func nativeGroupHookFixture(stage string) string {
	child := `import os, signal, time
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
life = open(os.path.join(root, 'lifetime'), 'wb', buffering=0)
`
	if strings.HasPrefix(stage, "exited-") {
		child += "parent = os.getppid()\nwhile os.getppid() == parent:\n    time.sleep(0.001)\n"
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
sys.stdout.write('{"partial":')
sys.stdout.flush()
`
	redirect := ""
	if stage == "exited-stdout" {
		redirect = ", stderr=subprocess.DEVNULL"
	} else if stage == "exited-stderr" {
		redirect = ", stdout=subprocess.DEVNULL"
	}
	body += fmt.Sprintf("child = subprocess.Popen([sys.executable, '-I', '-c', %q]%s)\n", child, redirect)
	if strings.HasPrefix(stage, "exited-") {
		// Child readiness must not mistake an already-reparented PID for the
		// launcher. Record the original launcher PID in the child instead.
		body = strings.Replace(body, "parent = os.getppid()", "parent = int(open(os.path.join(root, 'launcher-pid')).read())", 1)
	} else {
		body += "child.wait()\n"
	}
	return body
}

// Instrument only the realized copy, retaining the real communicate call,
// timeout handler, signal recording and process-group cleanup.
func instrumentHookSupervisor(t *testing.T, stage string) string {
	t.Helper()
	source := string(adapter.HookTranslator())
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
with open(os.path.join(fixture_root, 'supervisor-pid'), 'w') as stream:
    stream.write(str(os.getpid()))
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
		replace(`if __name__ == "__main__":`, `original_popen = subprocess.Popen
def held_popen(*args, **kwargs):
    child = original_popen(*args, **kwargs)
    fixture_ready()
    fixture_notify('spawn-ready')
    fixture_release()
    return child
subprocess.Popen = held_popen

if __name__ == "__main__":`)
	} else {
		before := "                fixture_ready()\n"
		if strings.HasPrefix(stage, "exited-launcher") {
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
		replace("                            os.killpg(os.getpgrp(), signal.SIGTERM)", "                            fixture_notify('cleanup-ready')\n                            fixture_release()\n                            os.killpg(os.getpgrp(), signal.SIGTERM)")
	}
	return source
}

func blockedHookFixture(stage string, descendant bool) string {
	// The descendant installs its signal policy and owns the lifetime writer
	// before announcing readiness. No other process inherits that writer.
	blocked := `import os, signal
root = os.environ['CLAUDE_PROJECT_DIR']
with open(os.path.join(root, 'descendant-pid'), 'w') as stream:
    stream.write(str(os.getpid()))
life = open(os.path.join(root, 'lifetime'), 'wb', buffering=0)
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
	body += fmt.Sprintf("child = subprocess.Popen([sys.executable, '-I', '-c', %q]%s)\n", blocked, redirect)
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
