package migrateapp

import (
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

func TestReverify2NewApplicationTakesRemote(t *testing.T) {
	t.Parallel()

	var constructor func(dependency.Remote, string) *Application = NewApplication
	if application := constructor(nil, "test"); application == nil {
		t.Fatal("NewApplication returned nil")
	}
}
