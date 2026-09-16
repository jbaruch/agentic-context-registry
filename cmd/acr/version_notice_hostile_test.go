//go:build darwin || linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/versioncheck"
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

// plantDirectoryReplacement moves the version directory aside and plants a
// replacement at its name. It runs inside the acquisition seam, off the test
// goroutine, so it records its error for the test to check afterwards.
func (harness *noticeHarness) plantDirectoryReplacement(kind string) (func(), *error) {
	directory := filepath.Join(harness.store.BaseDirectory, "version")
	var failure error
	calls := 0
	return func() {
		calls++
		if calls != 1 {
			return
		}
		if err := os.Rename(directory, directory+".moved"); err != nil {
			failure = err
			return
		}
		switch kind {
		case "named pipe":
			failure = syscall.Mkfifo(directory, 0o600)
		case "different directory":
			if err := os.Mkdir(directory, 0o700); err != nil {
				failure = err
				return
			}
			failure = os.WriteFile(filepath.Join(directory, "latest.json"), []byte(`{"schemaVersion":1,"checkedAt":"2001-02-03T04:05:06Z","latestVersion":"v42.0.0"}`+"\n"), 0o600)
		default:
			failure = os.ErrInvalid
		}
	}, &failure
}

// TestReleaseNoticeDirectoryReplacementNeverBlocksTheCommand is the CLI
// control for the directory replacement race, on both paths and with exact
// output ordering. Swapped during startup, a named pipe used to hold the
// command before its first byte; swapped after the command, it held the exit.
// A different directory carrying a newer record is the control that must be
// refused rather than trusted at the acquisition it races. Now the startup
// swap leaves the command's output alone with no notice, the post-command
// swap leaves the notice from the original cache followed by the output, the
// exit is the command's own, and the moved original is never written. A
// directory that is still at the name when the later, separate post-command
// acquisition runs is the store's directory by then and is acquired as one;
// a pipe there is refused by the inspection alone, so the source is reached
// only in the one case where a real directory stands at the name.
func TestReleaseNoticeDirectoryReplacementNeverBlocksTheCommand(t *testing.T) {
	for _, phase := range []struct {
		name       string
		acquiredOn int
		want       string
	}{
		{name: "startup", acquiredOn: 1, want: noticeVersionOutput},
		{name: "post-command", acquiredOn: 2, want: noticeLine + noticeVersionOutput},
	} {
		for _, kind := range []string{"named pipe", "different directory"} {
			t.Run(phase.name+"/"+kind, func(t *testing.T) {
				harness := newNoticeHarness(t)
				harness.seedCache(noticeLatest)
				original := harness.cacheBytes()
				plant, failure := harness.plantDirectoryReplacement(kind)
				acquisitions := 0
				hook := func() {
					acquisitions++
					if acquisitions == phase.acquiredOn {
						plant()
					}
				}
				harness.extra = []versioncheck.Option{versioncheck.WithDirectoryInspectionHook(hook)}

				var combined bytes.Buffer
				var exit int
				noticeWatchdog(t, "acr version with the directory replaced by a "+kind+" at "+phase.name, func() {
					exit = harness.run(&combined, &combined, "version")
				})
				if *failure != nil {
					t.Fatal(*failure)
				}
				if exit != 0 {
					t.Fatalf("exit = %d, want the command's own 0", exit)
				}
				if got := combined.String(); got != phase.want {
					t.Fatalf("terminal stream = %q, want %q", got, phase.want)
				}
				if acquisitions < phase.acquiredOn {
					t.Fatalf("the directory was acquired %d times; the %s acquisition never happened", acquisitions, phase.name)
				}
				wantCalls := 0
				if phase.name == "startup" && kind == "different directory" {
					wantCalls = 1
				}
				if harness.source.count() != wantCalls {
					t.Fatalf("the release source was reached %d times, want %d", harness.source.count(), wantCalls)
				}
				moved := filepath.Join(harness.store.BaseDirectory, "version.moved")
				if got, err := os.ReadFile(filepath.Join(moved, "latest.json")); err != nil || !bytes.Equal(got, original) {
					t.Fatalf("the moved original changed: %q, %v", got, err)
				}
				if entries, err := os.ReadDir(moved); err != nil || len(entries) != 1 {
					t.Fatalf("the moved original gained entries: %v, %v", entries, err)
				}
				if kind == "different directory" && phase.name == "post-command" {
					if entries, err := os.ReadDir(filepath.Join(harness.store.BaseDirectory, "version")); err != nil || len(entries) != 1 {
						t.Fatalf("the refused replacement directory gained entries: %v, %v", entries, err)
					}
				}
			})
		}
	}
}

// TestReleaseNoticeDirectoryReplacementOnARealTerminal repeats the startup
// named-pipe swap through the binary's own terminal probe.
func TestReleaseNoticeDirectoryReplacementOnARealTerminal(t *testing.T) {
	harness := newNoticeHarness(t)
	harness.seedCache(noticeLatest)
	plant, failure := harness.plantDirectoryReplacement("named pipe")
	acquisitions := 0
	harness.extra = []versioncheck.Option{versioncheck.WithDirectoryInspectionHook(func() {
		acquisitions++
		if acquisitions == 1 {
			plant()
		}
	})}
	master, slave := openTerminalPair(t)
	capture := captureTerminal(t, master)
	harness.probe = nil

	var exit int
	noticeWatchdog(t, "acr version on a terminal with the directory swapped for a pipe at startup", func() { exit = harness.run(slave, slave, "version") })
	if *failure != nil {
		t.Fatal(*failure)
	}
	const sentinel = "<<acr-terminal-end>>"
	if _, err := slave.WriteString(sentinel); err != nil {
		t.Fatal(err)
	}
	if text := capture.text(t, sentinel); exit != 0 || text != noticeVersionOutput {
		t.Fatalf("terminal run = %d, %q; want the version alone", exit, text)
	}
}
