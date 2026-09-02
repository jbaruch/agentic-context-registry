package main

import (
	"context"
	"io"
	"os"
	"runtime/debug"

	"github.com/jbaruch/agentic-context-registry/internal/buildinfo"
	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/publishapp"
)

var (
	version = "dev"
	commit  = ""
)

func main() {
	os.Exit(run(os.Stdout, os.Stderr, os.Args[1:]))
}

func run(stdout, stderr io.Writer, args []string) int {
	info, _ := debug.ReadBuildInfo()
	build := buildinfo.Resolve(version, commit, info)
	app := publishapp.NewApplication(dependency.NewGitHubClient(), build.Version)
	return cli.New(stdout, stderr, app, build).Run(context.Background(), args)
}
