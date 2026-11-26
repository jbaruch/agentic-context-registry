package realize

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGitInspectionCleansHelpersAfterLeaderExit(t *testing.T) {
	for _, test := range []struct {
		name   string
		stream string
		cancel bool
	}{
		{name: "stdout", stream: "stdout"},
		{name: "stderr", stream: "stderr"},
		{name: "caller cancellation", stream: "stdout", cancel: true},
	} {
		for _, exitCode := range []int{0, 23} {
			t.Run(fmt.Sprintf("%s/exit-%d", test.name, exitCode), func(t *testing.T) {
				root := t.TempDir()
				ready := filepath.Join(root, "git.pids")
				// Only the test owns the writer. The helper cannot finish
				// unprompted, but also cannot outlive the test process.
				hold, release, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer hold.Close()
				defer release.Close()
				const shim = `#!/bin/bash
set -euo pipefail
case "$ACR_TEST_GIT_STREAM" in
	stdout) /bin/cat <&3 2>/dev/null & ;;
	stderr) /bin/cat <&3 >/dev/null & ;;
esac
printf '%s %s\n' "$$" "$!" > "$ACR_TEST_GIT_PIDS"
printf 'Git leader diagnostic\n' >&2
printf '%*s' "$ACR_TEST_DIAGNOSTIC_LIMIT" '' >&2
exit "$ACR_TEST_GIT_EXIT"
`
				writeFile(t, root, "git", shim)
				if err := os.Chmod(filepath.Join(root, "git"), 0o755); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				command := exec.CommandContext(ctx, filepath.Join(root, "git"), "-C", root, "ls-files")
				command.ExtraFiles = []*os.File{hold}
				command.Env = append(command.Environ(),
					"ACR_TEST_GIT_STREAM="+test.stream, "ACR_TEST_GIT_PIDS="+ready,
					"ACR_TEST_GIT_EXIT="+strconv.Itoa(exitCode),
					"ACR_TEST_DIAGNOSTIC_LIMIT="+strconv.Itoa(gitInspectionDiagnosticLimit))
				draining := make(chan struct{})
				proceed := make(chan struct{})
				result := make(chan error, 1)
				go func() {
					_, err := runInspectionGitCommand(ctx, command, "inspect targets with git ls-files", func(stdout, stderr *os.File, diagnosticReady <-chan struct{}) error {
						// Wait has reaped the leader. The diagnostic prefix is
						// readable even when the helper holds stderr open.
						<-diagnosticReady
						close(draining)
						<-proceed
						if test.cancel {
							return nil
						}
						if test.stream == "stderr" {
							return stderr.SetReadDeadline(time.Unix(1, 0))
						}
						return stdout.SetReadDeadline(time.Unix(1, 0))
					})
					result <- err
				}()
				defer func() { close(proceed) }()
				select {
				case <-draining:
				case err := <-result:
					t.Fatalf("Git inspection returned before post-exit drain: %v", err)
				}
				var leader, pid int
				pids := readFile(t, root, "git.pids")
				if _, err := fmt.Sscan(pids, &leader, &pid); err != nil || pid <= 1 {
					t.Fatalf("invalid Git test PIDs %q: %v", pids, err)
				}
				if err := syscall.Kill(leader, 0); !errors.Is(err, syscall.ESRCH) {
					t.Errorf("Git leader %d has not been reaped: %v", leader, err)
				}
				if err := syscall.Kill(pid, 0); err != nil {
					t.Errorf("held-pipe helper %d was not alive before the test trigger: %v", pid, err)
				}
				if test.cancel {
					cancel()
				}
				proceed <- struct{}{}
				err = <-result
				rawPID := strconv.Itoa(pid)
				wantErr := exec.ErrWaitDelay
				if test.cancel {
					wantErr = context.Canceled
					if errors.Is(err, exec.ErrWaitDelay) {
						t.Errorf("cancellation waited for the pipe deadline: %v", err)
					}
				}
				if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "git ls-files") ||
					!strings.Contains(err.Error(), "core.fsmonitor") || !strings.Contains(err.Error(), "retry") {
					t.Errorf("pipe stall error = %v, want actionable %v", err, wantErr)
				}
				var exitErr *exec.ExitError
				if exitCode == 0 {
					if errors.As(err, &exitErr) {
						t.Errorf("successful leader reported as failed: %v", err)
					}
				} else if !errors.As(err, &exitErr) || exitErr.ExitCode() != exitCode ||
					!strings.Contains(string(exitErr.Stderr), "Git leader diagnostic") {
					t.Errorf("error = %v, want Git exit %d and stderr", err, exitCode)
				}

				// The OS may still be finishing SIGKILL when the command
				// returns. A zombie is already dead, even if not reaped.
				for {
					status, psErr := exec.Command("ps", "-o", "stat=", "-p", rawPID).Output()
					if psErr != nil {
						var absent *exec.ExitError
						if errors.As(psErr, &absent) && absent.ExitCode() == 1 && len(status) == 0 {
							break
						}
						t.Fatalf("check helper %d: %v: %s", pid, psErr, status)
					}
					if strings.HasPrefix(strings.TrimSpace(string(status)), "Z") {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			})
		}
	}
}
