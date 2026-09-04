//go:build darwin || linux

package main

import (
	"os"
	"testing"
)

// TestInteractiveStdinIsTrueOnlyForATerminal is the probe's positive half, and
// the reason no `return false` can pass this package: a pseudo-terminal's slave
// side must read interactive while /dev/null, which carries the same
// os.ModeCharDevice bit and is what the Go runtime opens into a closed
// descriptor 0, must not. The negative shapes live in
// TestPrompterIsNonInteractiveOnAPipeAndOnAClosedStdin; this one exists so that
// suppressing every question would fail too.
// openTerminal returns the slave side alone, for a test that only needs the
// probe to see a real terminal.
func openTerminal(t *testing.T) *os.File {
	t.Helper()
	_, slave := openTerminalPair(t)
	return slave
}

func TestInteractiveStdinIsTrueOnlyForATerminal(t *testing.T) {
	terminal := openTerminal(t)
	info, err := terminal.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("pseudo-terminal mode = %v, want a character device", info.Mode())
	}
	if !interactiveStdin(terminal) {
		t.Fatal("interactiveStdin(pseudo-terminal) = false, want true")
	}

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { devNull.Close() })
	nullInfo, err := devNull.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if nullInfo.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("%s mode = %v, want a character device", os.DevNull, nullInfo.Mode())
	}
	if interactiveStdin(devNull) {
		t.Fatalf("interactiveStdin(%s) = true, want false", os.DevNull)
	}
}
