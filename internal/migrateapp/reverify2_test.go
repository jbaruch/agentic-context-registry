package migrateapp

import (
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshnessapp"
)

func TestReverify2NewApplicationTakesRemote(t *testing.T) {
	t.Parallel()

	var constructor func(dependency.Remote, string, ...freshnessapp.Option) *Application = NewApplication
	// Called with no options it is the application the binary composes.
	if application := constructor(nil, "test"); application == nil {
		t.Fatal("NewApplication returned nil")
	}
	if application := constructor(nil, "test", freshnessapp.WithClock(nil)); application == nil {
		t.Fatal("NewApplication returned nil for an ignored option")
	}
}
