package adaptertest

import (
	"path/filepath"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
)

func TestAllAgentsGolden(t *testing.T) {
	runNativeGoldenMatrix(t, "all-agents")
}

func TestFreshnessSessionStartGolden(t *testing.T) {
	runNativeGoldenMatrix(t, "freshness-session-start")
}

func runNativeGoldenMatrix(t *testing.T, name string) {
	t.Helper()
	fixture := filepath.Join("testdata", name)
	for _, native := range []adapter.Adapter{claudecode.New(), codex.New(), cursor.New()} {
		native := native
		t.Run(native.Descriptor().ID, func(t *testing.T) {
			RunGoldenFixture(t, fixture, filepath.Join(fixture, "want", native.Descriptor().ID), native, preserve.NewCompiler())
		})
	}
}
