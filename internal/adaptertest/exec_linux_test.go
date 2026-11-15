package adaptertest

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRunCommandRetriesTextFileBusy(t *testing.T) {
	t.Parallel()
	for _, release := range []bool{true, false} {
		name := "persistent writer"
		if release {
			name = "writer closes"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			filename := filepath.Join(t.TempDir(), "probe with spaces.sh")
			if err := os.WriteFile(filename, []byte("#!/bin/sh\nprintf 'ran\\n'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			writer, err := os.OpenFile(filename, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if writer != nil {
					if err := writer.Close(); err != nil {
						t.Error(err)
					}
				}
			})
			attempts := 0
			var output bytes.Buffer
			err = RunCommand(func() *exec.Cmd {
				attempts++
				if release && attempts == 2 {
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					writer = nil
				}
				command := exec.Command(filename)
				command.Stdout = &output
				return command
			})
			if release {
				if err != nil || attempts != 2 || output.String() != "ran\n" {
					t.Fatalf("closed writer: attempts=%d output=%q err=%v", attempts, output.String(), err)
				}
			} else if !errors.Is(err, syscall.ETXTBSY) || attempts != 3 || output.Len() != 0 {
				t.Fatalf("persistent writer: attempts=%d output=%q err=%v", attempts, output.String(), err)
			}
		})
	}
}

func TestRunCommandDoesNotRetryOtherFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		cmd  func() *exec.Cmd
		want error
	}{
		{"missing executable", func() *exec.Cmd { return exec.Command(filepath.Join(t.TempDir(), "missing")) }, os.ErrNotExist},
		{"exit status", func() *exec.Cmd { return exec.Command("/bin/sh", "-c", "exit 7") }, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			err := RunCommand(func() *exec.Cmd {
				attempts++
				return test.cmd()
			})
			if attempts != 1 {
				t.Fatalf("attempts=%d, want 1", attempts)
			}
			if test.want != nil {
				if !errors.Is(err, test.want) {
					t.Fatalf("err=%v, want %v", err, test.want)
				}
			} else {
				var failure *exec.ExitError
				if !errors.As(err, &failure) || failure.ExitCode() != 7 {
					t.Fatalf("err=%v, want exit 7", err)
				}
			}
		})
	}
}
