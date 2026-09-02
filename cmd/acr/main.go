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
	info, _ := debug.ReadBuildInfo()
	build := buildinfo.Resolve(version, commit, info)
	inner := migrateapp.NewApplication(dependency.NewGitHubClient(), build.Version)
	prompter := setupapp.NewTerminalPrompter(stdin, stderr, interactiveStdin(stdin))
	return cli.New(stdout, stderr, setupapp.NewApplication(inner, prompter), build).Run(context.Background(), args)
}

// interactiveStdin is the one character-device probe in the binary. A Stat
// error on a closed or otherwise invalid descriptor reports non-interactive
// and is never fatal: a piped or daemonized run has to reach the typed
// refusal, not crash before it.
func interactiveStdin(stdin io.Reader) bool {
	file, isFile := stdin.(*os.File)
	if !isFile {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
