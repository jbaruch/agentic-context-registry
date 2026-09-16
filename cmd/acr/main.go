package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/jbaruch/agentic-context-registry/internal/buildinfo"
	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshnessapp"
	"github.com/jbaruch/agentic-context-registry/internal/migrateapp"
	"github.com/jbaruch/agentic-context-registry/internal/setupapp"
	"github.com/jbaruch/agentic-context-registry/internal/versioncheck"
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

// runWith is the composition the binary runs. The freshness options are the
// only construction detail this seam lets a caller replace, and run passes
// none, so the shipped binary composes exactly what it always did.
func runWith(remote dependency.Remote, stdin io.Reader, stdout, stderr io.Writer, args []string, freshnessOptions ...freshnessapp.Option) int {
	return runComposed(remote, stdin, stdout, stderr, args, freshnessOptions, nil)
}

// runComposed is runWith with the release-notice construction details also
// replaceable. The notice is decided once, before the command, from the cache
// alone; the command runs; then, on the same terminal-only eligibility, the
// cache is refreshed after the command's output so the network is never in
// front of the operator's work. Neither half changes the command's output or
// its exit code: the refresh outcome is classified for tests and otherwise
// deliberately unobserved, because a version check that could fail a command
// would be worse than no version check.
func runComposed(remote dependency.Remote, stdin io.Reader, stdout, stderr io.Writer, args []string, freshnessOptions []freshnessapp.Option, versionOptions []versioncheck.Option) int {
	info, _ := debug.ReadBuildInfo()
	build := buildinfo.Resolve(version, commit, info)
	check := versioncheck.New(remote, append([]versioncheck.Option{versioncheck.WithTerminalProbe(terminalWriter)}, versionOptions...)...)
	eligible := check.Eligible(args, stdout, stderr)
	if eligible {
		if notice, _ := check.Notice(build.Version); notice != "" {
			// A terminal that refuses the notice will refuse the command's own
			// output next, and the runner reports that; the notice itself is
			// best-effort by contract.
			_, _ = io.WriteString(stderr, notice)
		}
	}
	inner := migrateapp.NewApplication(remote, build.Version, freshnessOptions...)
	prompter := setupapp.NewTerminalPrompter(stdin, stderr, interactiveStdin(stdin))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	exitCode := cli.New(stdout, stderr, setupapp.NewApplication(inner, prompter), build).Run(ctx, args)
	if eligible {
		check.Refresh(ctx)
	}
	return exitCode
}

// interactiveStdin is the terminal probe applied to standard input, and it is
// true only for a real terminal. See interactiveFile for why a
// character-device test alone is not that test.
func interactiveStdin(stdin io.Reader) bool {
	file, isFile := stdin.(*os.File)
	if !isFile {
		return false
	}
	return interactiveFile(file)
}

// terminalWriter is the same probe applied to an output stream. The release
// notice is printed only when both stdout and stderr pass it, so a piped or
// redirected run — the docs harness, CI, a script reading JSON — never sees
// an extra byte on either stream.
func terminalWriter(writer io.Writer) bool {
	file, isFile := writer.(*os.File)
	if !isFile {
		return false
	}
	return interactiveFile(file)
}

// interactiveFile is the one terminal probe in the binary, and it is true only
// for a real terminal. A character-device test alone is not that test: the Go
// runtime opens /dev/null into a closed standard descriptor before main, and
// /dev/null is a character device, so a process started with descriptor 0
// closed would read as a terminal and ask a question nobody can answer. The
// termios ioctl separates them — a pipe, a regular file, /dev/null and a closed
// descriptor all fail it. Every error along the way reports non-interactive and
// is never fatal: a piped or daemonized run has to reach the typed refusal, not
// crash before it.
func interactiveFile(file *os.File) bool {
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
