package main

import (
	"context"
	"io"
	"os"
	"runtime/debug"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/publishapp"
)

var version = "dev"

func main() {
	os.Exit(run(os.Stdout, os.Stderr, os.Args[1:]))
}

func run(stdout, stderr io.Writer, args []string) int {
	currentVersion := resolveVersion(version, debug.ReadBuildInfo)
	app := publishapp.NewApplication(dependency.NewGitHubClient(), currentVersion)
	return cli.New(stdout, stderr, app, currentVersion).Run(context.Background(), args)
}

func resolveVersion(linked string, readBuildInfo func() (*debug.BuildInfo, bool)) string {
	if linked != "dev" {
		return linked
	}
	info, ok := readBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return linked
	}
	return info.Main.Version
}
