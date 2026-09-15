//go:build darwin || linux

package main

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// terminalCapture drains a pseudo-terminal's master side into a buffer, so a
// command writing to the slave never blocks on a full terminal buffer, and a
// test can wait for a sentinel it writes after the command returns.
type terminalCapture struct {
	mutex   sync.Mutex
	content strings.Builder
	updates chan struct{}
}

func captureTerminal(t *testing.T, master *os.File) *terminalCapture {
	t.Helper()
	capture := &terminalCapture{updates: make(chan struct{}, 1)}
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, err := master.Read(buffer)
			if n > 0 {
				capture.mutex.Lock()
				capture.content.Write(buffer[:n])
				capture.mutex.Unlock()
				select {
				case capture.updates <- struct{}{}:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return capture
}

// text returns everything captured before the sentinel, with the terminal's
// carriage returns folded away, once the sentinel has arrived.
func (capture *terminalCapture) text(t *testing.T, sentinel string) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		capture.mutex.Lock()
		current := capture.content.String()
		capture.mutex.Unlock()
		if before, _, found := strings.Cut(current, sentinel); found {
			return strings.ReplaceAll(before, "\r\n", "\n")
		}
		select {
		case <-capture.updates:
		case <-deadline:
			t.Fatalf("terminal never delivered the sentinel; captured %q", current)
		}
	}
}

// runOnTerminal executes one command with both output streams on the slave
// side of a real pseudo-terminal and the binary's own probe deciding, and
// returns the exit code and the text the terminal received.
func (harness *noticeHarness) runOnTerminal(args ...string) (int, string) {
	harness.t.Helper()
	master, slave := openTerminalPair(harness.t)
	capture := captureTerminal(harness.t, master)
	harness.probe = nil
	exit := harness.run(slave, slave, args...)
	const sentinel = "<<acr-terminal-end>>"
	if _, err := slave.WriteString(sentinel); err != nil {
		harness.t.Fatal(err)
	}
	return exit, capture.text(harness.t, sentinel)
}

// TestReleaseNoticeReachesARealTerminalAndNothingElse is the end-to-end proof
// through the binary's own termios probe: a pseudo-terminal sees the notice
// first, a --json run on the same terminal sees exactly the JSON line, and
// that line is byte-identical to the run with the check switched off.
func TestReleaseNoticeReachesARealTerminalAndNothingElse(t *testing.T) {
	harness := newNoticeHarness(t)
	harness.seedCache(noticeLatest)

	exit, text := harness.runOnTerminal("version")
	if exit != 0 || text != noticeLine+noticeVersionOutput {
		t.Fatalf("terminal run = %d, %q; want the notice before the version", exit, text)
	}

	exit, structured := harness.runOnTerminal("version", "--json")
	if exit != 0 || strings.Contains(structured, "available") || !strings.HasPrefix(structured, `{"ok":true,"command":"version"`) {
		t.Fatalf("terminal --json run = %d, %q; want the JSON envelope alone", exit, structured)
	}
	t.Setenv("ACR_VERSION_CHECK", "off")
	exit, off := harness.runOnTerminal("version", "--json")
	if exit != 0 || off != structured {
		t.Fatalf("--json with the check off = %d, %q; want byte parity with %q", exit, off, structured)
	}
	if harness.source.count() != 1 {
		t.Fatalf("the release source was reached %d times; only the eligible text run refreshes", harness.source.count())
	}
}
