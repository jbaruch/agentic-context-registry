//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// noticeWatchdog runs operation off the test goroutine and fails the test if
// it has not returned within the bound. The bound guards against a regression
// to a blocking open on a planted special file; no assertion measures it.
func noticeWatchdog(t *testing.T, what string, operation func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		operation()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return; a special record blocked the command", what)
	}
}

func (harness *noticeHarness) plantFIFO(record string) string {
	harness.t.Helper()
	path := filepath.Join(harness.store.BaseDirectory, "version", record)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		harness.t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		harness.t.Fatal(err)
	}
	return path
}

// TestReleaseNoticeSpecialRecordsNeverBlockTheCommand is the CLI control for
// the named-pipe finding: with a FIFO at either record, acr version used to
// hang — before its first output byte for latest.json, after it for
// attempt.json. Now the command completes with stdout and exit byte-identical
// to the run with the check switched off, stderr stays empty, and the
// post-command refresh replaces the pipe with a regular record.
func TestReleaseNoticeSpecialRecordsNeverBlockTheCommand(t *testing.T) {
	for _, record := range []string{"latest.json", "attempt.json"} {
		t.Run(record, func(t *testing.T) {
			harness := newNoticeHarness(t)
			path := harness.plantFIFO(record)

			t.Setenv("ACR_VERSION_CHECK", "off")
			offStdout, offStderr, offExit := harness.capture("version")
			t.Setenv("ACR_VERSION_CHECK", "")
			var stdout, stderr string
			var exit int
			noticeWatchdog(t, "acr version with a FIFO at "+record, func() { stdout, stderr, exit = harness.capture("version") })

			if exit != 0 || exit != offExit {
				t.Fatalf("exit = %d with the check, %d without", exit, offExit)
			}
			if stdout != offStdout || stdout != noticeVersionOutput {
				t.Fatalf("stdout = %q with the check, %q without", stdout, offStdout)
			}
			if stderr != "" || offStderr != "" {
				t.Fatalf("stderr = %q with the check, %q without; want both empty", stderr, offStderr)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() {
				t.Fatalf("%s after the run is %v, want the pipe replaced by a regular record", record, info.Mode())
			}
			if harness.source.count() != 1 {
				t.Fatalf("source called %d times, want the one post-command refresh", harness.source.count())
			}
		})
	}
}

// TestReleaseNoticeSpecialRecordOnARealTerminal repeats the latest.json case
// through the binary's own terminal probe: a real pseudo-terminal sees the
// version line alone, and the command returns.
func TestReleaseNoticeSpecialRecordOnARealTerminal(t *testing.T) {
	harness := newNoticeHarness(t)
	harness.plantFIFO("latest.json")
	master, slave := openTerminalPair(t)
	capture := captureTerminal(t, master)
	harness.probe = nil

	var exit int
	noticeWatchdog(t, "acr version on a terminal with a FIFO cache", func() { exit = harness.run(slave, slave, "version") })
	const sentinel = "<<acr-terminal-end>>"
	if _, err := slave.WriteString(sentinel); err != nil {
		t.Fatal(err)
	}
	text := capture.text(t, sentinel)
	if exit != 0 || text != noticeVersionOutput {
		t.Fatalf("terminal run = %d, %q; want the version alone", exit, text)
	}
}
