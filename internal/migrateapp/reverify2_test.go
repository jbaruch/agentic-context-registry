package migrateapp

import (
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

func TestReverify2NewApplicationTakesConcreteGitHubClient(t *testing.T) {
	t.Parallel()

	var constructor func(*dependency.GitHubClient, string) *Application = NewApplication
	if application := constructor(nil, "test"); application == nil {
		t.Fatal("NewApplication returned nil")
	}
}
