package adapter_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestClaudeAndCodexRulesCoalesceOnExistingAgentsInclude(t *testing.T) {
	t.Parallel()

	packageRoot := t.TempDir()
	writeRuleBundleFile(t, packageRoot, "rules/always.md", "# Always\n")
	writeRuleBundleFile(t, packageRoot, "rules/go.md", "# Go only\n")
	pkg := adapter.Package{
		Source: "github:example/all-agents", Root: os.DirFS(packageRoot),
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Rules: []manifest.RuleArtifact{
			{ID: "always-rule", Path: "rules/always.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}},
			{ID: "go-paths-rule", Path: "rules/go.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationPaths, Paths: []string{"**/*.go", "scripts/**"}}},
		}}},
	}
	projectRoot := t.TempDir()
	writeRuleBundleFile(t, projectRoot, "CLAUDE.md", "@AGENTS.md\r\n")
	writeRuleBundleFile(t, projectRoot, "AGENTS.md", "User instructions\r\nwithout final newline")
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), claudecode.New(), codex.New())
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(projectRoot)), []adapter.Package{pkg}, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 || intents[0].Path != "AGENTS.md" {
		t.Fatalf("intents = %#v, want one AGENTS.md projection", intents)
	}
	content := string(intents[0].Content)
	if strings.Count(content, "# Always") != 1 || strings.Count(content, "# Go only") != 1 || !strings.Contains(content, "Apply only when a working path matches: **/*.go, scripts/**") {
		t.Fatalf("AGENTS.md candidate = %q", content)
	}
	if len(intents[0].Entries) != 1 || intents[0].Entries[0].Adapter != "codex" {
		t.Fatalf("AGENTS.md entries = %#v, want one Codex-owned package block", intents[0].Entries)
	}
	if got := string(intents[0].PreservedContent[0]); got != "User instructions\r\nwithout final newline" {
		t.Fatalf("preserved bytes = %q", got)
	}
}

func TestRuleBundlesRejectDuplicateIncludesWithTypedDiagnostic(t *testing.T) {
	t.Parallel()

	packageRoot := t.TempDir()
	writeRuleBundleFile(t, packageRoot, "rules/always.md", "# Always\n")
	pkg := adapter.Package{
		Source: "github:example/all-agents", Root: os.DirFS(packageRoot),
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Rules: []manifest.RuleArtifact{{
			ID: "always-rule", Path: "rules/always.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways},
		}}}},
	}
	projectRoot := t.TempDir()
	writeRuleBundleFile(t, projectRoot, "CLAUDE.md", "@AGENTS.md\n@AGENTS.md\n")
	writeRuleBundleFile(t, projectRoot, "AGENTS.md", "# Shared instructions\n")
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), claudecode.New())
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(projectRoot)), []adapter.Package{pkg}, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion})
	var graphErr *preserve.GraphError
	if !errors.As(err, &graphErr) {
		t.Fatalf("Realize() error = %v, want *preserve.GraphError", err)
	}
	found := false
	for _, diagnostic := range graphErr.Diagnostics {
		found = found || diagnostic.Code == preserve.CodeDuplicateInclude
	}
	if !found {
		t.Fatalf("graph diagnostics = %#v, want %s", graphErr.Diagnostics, preserve.CodeDuplicateInclude)
	}
}

func writeRuleBundleFile(t *testing.T, root, relative, content string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
