package realize

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
				// Only the test owns the writer. The helper cannot finish
				// unprompted, but also cannot outlive the test process.
				hold, release, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer hold.Close()
				defer release.Close()
				lifetime, lifeWriter, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer lifetime.Close()
				defer lifeWriter.Close()
				const shim = `#!/bin/bash
set -euo pipefail
(
	case "$ACR_TEST_GIT_STREAM" in
		stdout) exec 2>/dev/null ;;
		stderr) exec >/dev/null ;;
	esac
	exec 0<&3
	printf 'READY\n' >&4
	exec /bin/cat
) &
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
				command.ExtraFiles = []*os.File{hold, lifeWriter}
				command.Env = append(command.Environ(),
					"ACR_TEST_GIT_STREAM="+test.stream,
					"ACR_TEST_GIT_EXIT="+strconv.Itoa(exitCode),
					"ACR_TEST_DIAGNOSTIC_LIMIT="+strconv.Itoa(gitInspectionDiagnosticLimit))
				draining := make(chan struct{})
				proceed := make(chan struct{}, 1)
				finished := make(chan struct{})
				var result error
				// Bound missing events without canceling the inspection or
				// expiring either of its readers.
				watchdog, stop := context.WithTimeout(t.Context(), 10*time.Second)
				defer stop()
				deadline, _ := watchdog.Deadline()
				if err := lifetime.SetReadDeadline(deadline); err != nil {
					t.Fatal(err)
				}
				go func() {
					defer close(finished)
					_, result = runInspectionGitCommand(ctx, command, "inspect targets with git ls-files", func(stdout, stderr *os.File, diagnosticReady <-chan struct{}) error {
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
				}()
				defer func() {
					// Teardown cannot count as evidence of helper shutdown.
					cancel()
					if err := release.Close(); err != nil {
						t.Error(err)
					}
					select {
					case proceed <- struct{}{}:
					default:
					}
					select {
					case <-finished:
					case <-time.After(10 * time.Second):
						t.Error("Git inspection did not return after fixture teardown")
					}
				}()
				select {
				case <-draining:
				case <-finished:
					t.Fatalf("Git inspection returned before post-exit drain: %v", result)
				case <-watchdog.Done():
					t.Fatal("Git inspection did not reach the post-Wait drain with its diagnostic")
				}
				events := bufio.NewReader(lifetime)
				if line, err := events.ReadString('\n'); err != nil || line != "READY\n" {
					t.Fatalf("Git helper readiness = %q, %v, want READY", line, err)
				}
				if err := lifeWriter.Close(); err != nil {
					t.Fatal(err)
				}
				if test.cancel {
					cancel()
				}
				proceed <- struct{}{}
				select {
				case <-finished:
				case <-watchdog.Done():
					t.Fatal("Git inspection did not return after the post-Wait trigger")
				}
				err = result
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

				// Neither closing Git's output reader nor fixture teardown
				// can produce EOF on this independent lifetime pipe.
				if b, lifeErr := events.ReadByte(); !errors.Is(lifeErr, io.EOF) {
					t.Fatalf("Git helper lifetime did not reach EOF while the hold was unreleased: byte %q, %v\ninspection result: %v", b, lifeErr, err)
				}
			})
		}
	}
}
