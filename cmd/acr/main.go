package main

import (
	"context"
	"io"
	"os"
	"runtime/debug"

	"github.com/jbaruch/agentic-context-registry/internal/buildinfo"
	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrateapp"
	"github.com/jbaruch/agentic-context-registry/internal/setupapp"
)

var (
	version = "dev"
	commit  = ""
)

func main() {
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr, os.Args[1:]))
}

func run(stdin io.Reader, stdout, stderr io.Writer, args []string) int {
	return runWith(dependency.NewGitHubClient(), stdin, stdout, stderr, args)
}

func runWith(remote dependency.Remote, stdin io.Reader, stdout, stderr io.Writer, args []string) int {
	info, _ := debug.ReadBuildInfo()
	build := buildinfo.Resolve(version, commit, info)
	inner := migrateapp.NewApplication(remote, build.Version)
	prompter := setupapp.NewTerminalPrompter(stdin, stderr, interactiveStdin(stdin))
	return cli.New(stdout, stderr, setupapp.NewApplication(inner, prompter), build).Run(context.Background(), args)
}

// interactiveStdin is the one terminal probe in the binary, and it is true only
// for a real terminal. A character-device test alone is not that test: the Go
// runtime opens /dev/null into a closed standard descriptor before main, and
// /dev/null is a character device, so a process started with descriptor 0
// closed would read as a terminal and ask a question nobody can answer. The
// termios ioctl separates them — a pipe, a regular file, /dev/null and a closed
// descriptor all fail it. Every error along the way reports non-interactive and
// is never fatal: a piped or daemonized run has to reach the typed refusal, not
// crash before it.
func interactiveStdin(stdin io.Reader) bool {
	file, isFile := stdin.(*os.File)
	if !isFile {
		return false
	}
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 || isDevNull(info) {
		return false
	}
	connection, err := file.SyscallConn()
	if err != nil {
		return false
	}
	terminal := false
	if err := connection.Control(func(descriptor uintptr) {
		terminal = isTerminal(descriptor)
	}); err != nil {
		return false
	}
	return terminal
}

// isDevNull is the belt-and-braces half of the probe: it names the one
// character device the runtime is known to substitute, so the refusal survives
// a platform whose isTerminal is the non-interactive fallback. Being unable to
// stat /dev/null decides nothing here — the ioctl above stays the gate.
func isDevNull(info os.FileInfo) bool {
	devNull, err := os.Stat(os.DevNull)
	if err != nil {
		return false
	}
	return os.SameFile(info, devNull)
}
