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

func TestRuleBundlesRejectIncludeCycleAndAgentsDuplicateWithTypedCodes(t *testing.T) {
	t.Parallel()

	packageRoot := t.TempDir()
	writeRuleBundleFile(t, packageRoot, "rules/always.md", "# Always\n")
	pkg := adapter.Package{
		Source: "github:example/all-agents", Root: os.DirFS(packageRoot),
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Rules: []manifest.RuleArtifact{{
			ID: "always-rule", Path: "rules/always.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways},
		}}}},
	}

	t.Run("agents-include-cycle", func(t *testing.T) {
		t.Parallel()
		projectRoot := t.TempDir()
		writeRuleBundleFile(t, projectRoot, "AGENTS.md", "@CLAUDE.md\n")
		writeRuleBundleFile(t, projectRoot, "CLAUDE.md", "@AGENTS.md\n")
		coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), codex.New())
		if err != nil {
			t.Fatal(err)
		}
		_, err = coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(projectRoot)), []adapter.Package{pkg}, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion})
		assertTypedGraphCode(t, err, preserve.CodeIncludeCycle)
	})

	t.Run("agents-duplicate-include", func(t *testing.T) {
		t.Parallel()
		projectRoot := t.TempDir()
		writeRuleBundleFile(t, projectRoot, "AGENTS.md", "@notes.md\n@notes.md\n")
		writeRuleBundleFile(t, projectRoot, "notes.md", "# Notes\n")
		coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), codex.New())
		if err != nil {
			t.Fatal(err)
		}
		_, err = coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(projectRoot)), []adapter.Package{pkg}, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion})
		assertTypedGraphCode(t, err, preserve.CodeDuplicateInclude)
	})
}

type countingDirectorySnapshot struct {
	adapter.DirectorySnapshot
	reads map[string]int
}

func (snapshot *countingDirectorySnapshot) ReadFile(path string) (adapter.ObservedFile, error) {
	snapshot.reads[path]++
	return snapshot.DirectorySnapshot.ReadFile(path)
}

func TestRuleBundlesRenderIncludeNextToOversizedSibling(t *testing.T) {
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
	writeRuleBundleFile(t, projectRoot, "AGENTS.md", "@docs/included.md\n")
	writeRuleBundleFile(t, projectRoot, "docs/included.md", "# Included notes\n")
	hugePath := filepath.Join(projectRoot, "docs", "huge.bin")
	huge, err := os.Create(hugePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := huge.Truncate(40 << 20); err != nil {
		t.Fatal(errors.Join(err, huge.Close()))
	}
	if err := huge.Close(); err != nil {
		t.Fatal(err)
	}

	rootSnapshot, err := adapter.NewRootSnapshot(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rootSnapshot.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	snapshot := &countingDirectorySnapshot{
		DirectorySnapshot: rootSnapshot,
		reads:             make(map[string]int),
	}

	graph, err := preserve.DiscoverIncludeGraphSnapshot(snapshot, "AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	if !graph.Reachable("AGENTS.md", "docs/included.md") {
		t.Fatal("include next to oversized sibling did not resolve")
	}
	if host, ok := graph.DeepestSharedHost([]string{"AGENTS.md"}); !ok || host != "docs/included.md" {
		t.Fatalf("DeepestSharedHost() = %q, %t, want docs/included.md", host, ok)
	}

	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), codex.New())
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(context.Background(), snapshot, []adapter.Package{pkg}, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 || intents[0].Path != "docs/included.md" {
		t.Fatalf("intents = %#v, want one projection onto the include target", intents)
	}
	content := string(intents[0].Content)
	if !strings.Contains(content, "# Always") {
		t.Fatalf("include target = %q, want rendered rule bundle", content)
	}
	if !strings.Contains(content, "# Included notes") {
		t.Fatalf("include target = %q, want preserved include body", content)
	}
	if snapshot.reads["docs/huge.bin"] != 0 {
		t.Fatalf("oversized sibling was opened: reads = %#v", snapshot.reads)
	}
	if snapshot.reads["docs/included.md"] == 0 || snapshot.reads["AGENTS.md"] == 0 {
		t.Fatalf("reachable files were not read: reads = %#v", snapshot.reads)
	}
}

func assertTypedGraphCode(t *testing.T, err error, want string) {
	t.Helper()
	var graphErr *preserve.GraphError
	if !errors.As(err, &graphErr) {
		t.Fatalf("Realize() error = %v (%T), want *preserve.GraphError with %s", err, err, want)
	}
	found := false
	for _, diagnostic := range graphErr.Diagnostics {
		found = found || diagnostic.Code == want
	}
	if !found {
		t.Fatalf("graph diagnostics = %#v, want typed %s", graphErr.Diagnostics, want)
	}
	if err.Error() == want {
		t.Fatalf("error is a bare %q string, want preserve GraphError wrapping", want)
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
