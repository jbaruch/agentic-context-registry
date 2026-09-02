package main

import (
	"context"
	"io"
	"os"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/publishapp"
)

var version = "dev"

func main() {
	os.Exit(run(os.Stdout, os.Stderr, os.Args[1:]))
}

func run(stdout, stderr io.Writer, args []string) int {
	app := publishapp.NewApplication(dependency.NewGitHubClient(), version)
	return cli.New(stdout, stderr, app, version).Run(context.Background(), args)
}
