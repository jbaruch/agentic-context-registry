package preserve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

type countingDirectorySnapshot struct {
	adapter.DirectorySnapshot
	reads map[string]int
}

func (snapshot *countingDirectorySnapshot) ReadFile(path string) (adapter.ObservedFile, error) {
	snapshot.reads[path]++
	return snapshot.DirectorySnapshot.ReadFile(path)
}

func TestDiscoverIncludeGraphReusesNestedInstructions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGraphFile(t, root, "CLAUDE.md", "# Claude\n@AGENTS.md follow instructions\n")
	writeGraphFile(t, root, "AGENTS.md", "@.tessl/RULES.md follow rules\n")
	writeGraphFile(t, root, ".tessl/RULES.md", "# Rules\n")

	graph, err := DiscoverIncludeGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.ValidateSelected([]string{"CLAUDE.md"}); err != nil {
		t.Fatal(err)
	}
	if !graph.Reachable("CLAUDE.md", ".tessl/RULES.md") {
		t.Fatal("nested rules file is not reachable")
	}
	if host, ok := graph.DeepestSharedHost([]string{"CLAUDE.md"}); !ok || host != ".tessl/RULES.md" {
		t.Fatalf("DeepestSharedHost() = %q, %t", host, ok)
	}
}

func TestDeepestSharedHostExcludesManifestEvidencedTesslPaths(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		included string
	}{
		"plugin tree":   {included: ".tessl/plugins/example/alpha/rules/always.md"},
		"native prefix": {included: "instructions/tessl__example__alpha.md"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeGraphFile(t, root, "tessl.json", "{}\n")
			writeGraphFile(t, root, "AGENTS.md", "@"+test.included+"\n")
			writeGraphFile(t, root, test.included, "# Tessl-owned\n")

			graph, err := DiscoverIncludeGraph(root)
			if err != nil {
				t.Fatal(err)
			}
			if host, ok := graph.DeepestSharedHost([]string{"AGENTS.md"}); !ok || host != "AGENTS.md" {
				t.Fatalf("DeepestSharedHost() = %q, %t, want AGENTS.md, true", host, ok)
			}
		})
	}
}

func TestDiscoverIncludeGraphSnapshotReadsOnlyReachableFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGraphFile(t, root, "CLAUDE.md", "@notes.txt\n")
	writeGraphFile(t, root, "notes.txt", "# Included non-Markdown target\n")
	writeGraphFile(t, root, "assets/unrelated.bin", "must not be opened\n")
	snapshot := &countingDirectorySnapshot{
		DirectorySnapshot: adapter.NewFSSnapshot(os.DirFS(root)),
		reads:             make(map[string]int),
	}

	graph, err := DiscoverIncludeGraphSnapshot(snapshot, "CLAUDE.md")
	if err != nil {
		t.Fatal(err)
	}
	if !graph.Reachable("CLAUDE.md", "notes.txt") {
		t.Fatal("non-Markdown include target is not reachable")
	}
	if snapshot.reads["CLAUDE.md"] != 1 || snapshot.reads["notes.txt"] != 1 {
		t.Fatalf("reachable reads = %#v, want CLAUDE.md and notes.txt once", snapshot.reads)
	}
	if snapshot.reads["assets/unrelated.bin"] != 0 || len(snapshot.reads) != 2 {
		t.Fatalf("unrelated file was opened: reads = %#v", snapshot.reads)
	}
}

func TestDiscoverIncludeGraphReportsAffectedFailures(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		files map[string]string
		code  string
	}{
		"duplicate": {
			files: map[string]string{"CLAUDE.md": "@AGENTS.md\n@AGENTS.md again\n", "AGENTS.md": "# agents\n"},
			code:  CodeDuplicateInclude,
		},
		"cycle": {
			files: map[string]string{"CLAUDE.md": "@AGENTS.md\n", "AGENTS.md": "@CLAUDE.md\n"},
			code:  CodeIncludeCycle,
		},
		"unresolved": {
			files: map[string]string{"CLAUDE.md": "@missing.md\n"},
			code:  CodeUnresolvedInclude,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for filename, content := range test.files {
				writeGraphFile(t, root, filename, content)
			}
			graph, err := DiscoverIncludeGraph(root)
			if err != nil {
				t.Fatal(err)
			}
			var graphErr *GraphError
			if err := graph.ValidateSelected([]string{"CLAUDE.md"}); !errors.As(err, &graphErr) {
				t.Fatalf("ValidateSelected() error = %v", err)
			}
			found := false
			for _, diagnostic := range graphErr.Diagnostics {
				if diagnostic.Code == test.code && len(diagnostic.Chain) != 0 {
					found = true
				}
			}
			if !found {
				t.Fatalf("diagnostics = %#v, want %s with chain", graphErr.Diagnostics, test.code)
			}
		})
	}
}

func TestDiscoverIncludeGraphRejectsTransitiveDuplicatePath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGraphFile(t, root, "CLAUDE.md", "@one.md\n@two.md\n")
	writeGraphFile(t, root, "one.md", "@leaf.md\n")
	writeGraphFile(t, root, "two.md", "@leaf.md\n")
	writeGraphFile(t, root, "leaf.md", "# leaf\n")
	graph, err := DiscoverIncludeGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	var graphErr *GraphError
	if err := graph.ValidateSelected([]string{"CLAUDE.md"}); !errors.As(err, &graphErr) {
		t.Fatalf("ValidateSelected() error = %v", err)
	}
	found := false
	for _, diagnostic := range graphErr.Diagnostics {
		if diagnostic.Code == CodeDuplicateInclude && strings.Contains(diagnostic.Message, "more than one include path") {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics = %#v", graphErr.Diagnostics)
	}
}

func TestDiscoverIncludeGraphLeavesUntouchedFailureAsWarning(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGraphFile(t, root, "CLAUDE.md", "@nested/good.md\n")
	writeGraphFile(t, root, "nested/good.md", "# good\n")
	writeGraphFile(t, root, "other/AGENTS.md", "@missing.md\n")

	graph, err := DiscoverIncludeGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.ValidateSelected([]string{"CLAUDE.md"}); err != nil {
		t.Fatalf("unrelated component blocked selected root: %v", err)
	}
	if len(graph.Diagnostics) != 1 || graph.Diagnostics[0].Code != CodeUnresolvedInclude {
		t.Fatalf("Diagnostics = %#v", graph.Diagnostics)
	}
}

func TestVendoredInstructionFileIsNotAnIncludeRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeGraphFile(t, root, "CLAUDE.md", "# Project instructions\n")
	writeGraphFile(t, root, ".agents/vendor/example/orphan/AGENTS.md", "@missing.md\n")

	filesystemGraph, err := DiscoverIncludeGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	snapshotGraph, err := DiscoverIncludeGraphSnapshot(adapter.NewFSSnapshot(os.DirFS(root)))
	if err != nil {
		t.Fatal(err)
	}
	for name, graph := range map[string]*IncludeGraph{"filesystem": filesystemGraph, "snapshot": snapshotGraph} {
		if len(graph.Roots) != 1 || graph.Roots[0] != "CLAUDE.md" || len(graph.Diagnostics) != 0 {
			t.Fatalf("%s graph = %#v, want vendored instructions excluded", name, graph)
		}
	}
}

func TestDiscoverIncludeGraphIgnoresFencesAndManagedBlocks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	managed := compileMissingMarkdown(t, testMarkdownInsertion("rule-a", "@also-missing.md\n"))
	content := append([]byte("```md\n@missing.md\n```\n"), managed.Candidate.Content...)
	content = append(content, []byte("@AGENTS.md\n")...)
	writeGraphFile(t, root, "CLAUDE.md", string(content))
	writeGraphFile(t, root, "AGENTS.md", "# agents\n")
	graph, err := DiscoverIncludeGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Diagnostics) != 0 || !graph.Reachable("CLAUDE.md", "AGENTS.md") {
		t.Fatalf("graph = %#v", graph)
	}
}

func TestDiscoverIncludeGraphDoesNotToggleOnMarkerDocumentation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGraphFile(t, root, "CLAUDE.md", "<!-- acr:begin this is documentation, not a marker -->\n@AGENTS.md\n")
	writeGraphFile(t, root, "AGENTS.md", "# agents\n")
	graph, err := DiscoverIncludeGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Diagnostics) != 0 || !graph.Reachable("CLAUDE.md", "AGENTS.md") {
		t.Fatalf("marker documentation changed graph state: %#v", graph)
	}
}

func writeGraphFile(t *testing.T, root, filename, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(filename))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
