package adapter_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestDocumentedInstructionRootsMatchAdapterBehavior(t *testing.T) {
	t.Parallel()

	packageRoot := t.TempDir()
	writeRuleBundleFile(t, packageRoot, "rules/always.md", "# Always\n")
	pkg := adapter.Package{
		Source: "github:example/documented-roots",
		Root:   os.DirFS(packageRoot),
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Rules: []manifest.RuleArtifact{{
			ID: "always-rule", Path: "rules/always.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways},
		}}}},
	}

	tests := []struct {
		name       string
		selected   adapter.Adapter
		candidates []string
		fallback   string
	}{
		{name: "claude-code", selected: claudecode.New(), candidates: []string{".claude/CLAUDE.md", "CLAUDE.md"}, fallback: "CLAUDE.md"},
		{name: "codex", selected: codex.New(), candidates: []string{"AGENTS.md", "AGENTS.override.md"}, fallback: "AGENTS.md"},
	}

	document, err := os.ReadFile(filepath.Join("..", "..", "docs", "shared-files.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, candidate := range test.candidates {
				candidate := candidate
				t.Run(candidate, func(t *testing.T) {
					got := realizedInstructionRoots(t, pkg, test.selected, []string{candidate})
					if want := []string{candidate}; !reflect.DeepEqual(got, want) {
						t.Fatalf("realized instruction roots = %q, want %q", got, want)
					}
				})
			}
			t.Run("all-existing", func(t *testing.T) {
				got := realizedInstructionRoots(t, pkg, test.selected, test.candidates)
				want := append([]string(nil), test.candidates...)
				sort.Strings(want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("realized instruction roots = %q, want %q", got, want)
				}
			})
			t.Run("fallback", func(t *testing.T) {
				got := realizedInstructionRoots(t, pkg, test.selected, nil)
				if want := []string{test.fallback}; !reflect.DeepEqual(got, want) {
					t.Fatalf("realized instruction roots = %q, want %q", got, want)
				}
			})

			for _, candidate := range test.candidates {
				if !strings.Contains(string(document), "`"+candidate+"`") {
					t.Errorf("shared-files documentation does not name behaviorally selected root %s", candidate)
				}
			}
		})
	}
}

func realizedInstructionRoots(t *testing.T, pkg adapter.Package, selected adapter.Adapter, existing []string) []string {
	t.Helper()
	projectRoot := t.TempDir()
	for _, path := range existing {
		writeRuleBundleFile(t, projectRoot, path, "# Existing instructions\n")
	}
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), selected)
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(
		context.Background(),
		adapter.NewFSSnapshot(os.DirFS(projectRoot)),
		[]adapter.Package{pkg},
		realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion},
	)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, len(intents))
	for index, intent := range intents {
		paths[index] = intent.Path
	}
	sort.Strings(paths)
	return paths
}
