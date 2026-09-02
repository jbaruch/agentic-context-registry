package main

import (
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/release"
)

func productionRemote() release.Remote {
	return dependency.NewGitHubClient()
}
