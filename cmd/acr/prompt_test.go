package main

import (
	"os"
	"strings"
	"testing"
)

func TestPrompterIsNonInteractiveOnAPipeAndOnAClosedStdin(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		reader.Close()
		writer.Close()
	})
	regular, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	closed, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { regular.Close() })

	tests := map[string]*os.File{"pipe": reader, "regular file": regular, "closed descriptor": closed}
	for name, stdin := range tests {
		name, stdin := name, stdin
		t.Run(name, func(t *testing.T) {
			if interactiveStdin(stdin) {
				t.Fatalf("interactiveStdin(%s) = true, want false", name)
			}
		})
	}
	t.Run("not a file", func(t *testing.T) {
		if interactiveStdin(strings.NewReader("")) {
			t.Fatal("interactiveStdin(non-file reader) = true, want false")
		}
	})
}
