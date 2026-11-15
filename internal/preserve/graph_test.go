package preserve

import (
	"encoding/json"
	"errors"
	"fmt"
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

func TestDeepestSharedHostStopsWhereIncludesFanOut(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		files map[string]string
		roots []string
		want  string
	}{
		"root importing sibling files": {
			files: map[string]string{
				"CLAUDE.md":            "@docs/architecture.md\n@docs/conventions.md\n",
				"docs/architecture.md": "# Architecture\n",
				"docs/conventions.md":  "# Conventions\n",
			},
			roots: []string{"CLAUDE.md"},
			want:  "CLAUDE.md",
		},
		"chain ending in a fan-out": {
			files: map[string]string{
				"CLAUDE.md":           "@AGENTS.md\n",
				"AGENTS.md":           "@.tessl/RULES.md\n",
				".tessl/RULES.md":     "@plugins/a.md\n@plugins/b.md\n",
				".tessl/plugins/a.md": "# a\n",
				".tessl/plugins/b.md": "# b\n",
			},
			roots: []string{"CLAUDE.md"},
			want:  ".tessl/RULES.md",
		},
		"shared chain beside a sibling import": {
			files: map[string]string{
				"CLAUDE.md":           "@AGENTS.md\n@docs/claude-only.md\n",
				"AGENTS.md":           "@.tessl/RULES.md\n",
				".tessl/RULES.md":     "# Rules\n",
				"docs/claude-only.md": "# Claude only\n",
			},
			roots: []string{"AGENTS.md", "CLAUDE.md"},
			want:  ".tessl/RULES.md",
		},
		"Tessl-owned import is not a sibling": {
			files: map[string]string{
				"tessl.json": "{}\n",
				"AGENTS.md":  "@.tessl/plugins/example/alpha/rules/always.md\n@instructions/user-rules.md\n",
				".tessl/plugins/example/alpha/rules/always.md": "# Tessl-owned\n",
				"instructions/user-rules.md":                   "# User rules\n",
			},
			roots: []string{"AGENTS.md"},
			want:  "instructions/user-rules.md",
		},
		"two hostable imports beside a Tessl-owned one": {
			files: map[string]string{
				"tessl.json": "{}\n",
				"AGENTS.md":  "@.tessl/plugins/example/alpha/rules/always.md\n@docs/a.md\n@docs/b.md\n",
				".tessl/plugins/example/alpha/rules/always.md": "# Tessl-owned\n",
				"docs/a.md": "# a\n",
				"docs/b.md": "# b\n",
			},
			roots: []string{"AGENTS.md"},
			want:  "AGENTS.md",
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
			if err := graph.ValidateSelected(test.roots); err != nil {
				t.Fatal(err)
			}
			if host, ok := graph.DeepestSharedHost(test.roots); !ok || host != test.want {
				t.Fatalf("DeepestSharedHost(%q) = %q, %t, want %q, true", test.roots, host, ok, test.want)
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
		"duplicate spellings": {
			files: map[string]string{"CLAUDE.md": "@./AGENTS.md\n@rules/../AGENTS.md\n", "AGENTS.md": "# agents\n", "rules/style.md": "# Style\n"},
			code:  CodeDuplicateInclude,
		},
		"cycle": {
			files: map[string]string{"CLAUDE.md": "@AGENTS.md\n", "AGENTS.md": "@CLAUDE.md\n"},
			code:  CodeIncludeCycle,
		},
		"cycle spellings": {
			files: map[string]string{"CLAUDE.md": "@./AGENTS.md\n", "AGENTS.md": "@rules/../CLAUDE.md\n", "rules/style.md": "# Style\n"},
			code:  CodeIncludeCycle,
		},
		"unresolved": {
			files: map[string]string{"CLAUDE.md": "@missing.md\n"},
			code:  CodeUnresolvedInclude,
		},
		"escaping parent": {
			files: map[string]string{"CLAUDE.md": "@../outside.md\n"},
			code:  CodeInvalidInclude,
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

func TestDiscoverIncludeGraphResolvesParentIncludesInsideProject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGraphFile(t, root, ".claude/CLAUDE.md", "@../rules/style.md\n")
	writeGraphFile(t, root, "rules/style.md", "# Style\n")
	writeGraphFile(t, root, "nested/AGENTS.md", "@../../outside.md\n")

	graph, err := DiscoverIncludeGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.ValidateSelected([]string{".claude/CLAUDE.md"}); err != nil {
		t.Fatalf("in-project parent include blocked its root: %v", err)
	}
	if !graph.Reachable(".claude/CLAUDE.md", "rules/style.md") {
		t.Fatal("parent include did not resolve against the importing file's directory")
	}
	var graphErr *GraphError
	if err := graph.ValidateSelected([]string{"nested/AGENTS.md"}); !errors.As(err, &graphErr) {
		t.Fatalf("escaping parent include: ValidateSelected() error = %v", err)
	}
	if len(graphErr.Diagnostics) != 1 || graphErr.Diagnostics[0].Code != CodeInvalidInclude ||
		!strings.Contains(graphErr.Diagnostics[0].Message, "escapes the project") {
		t.Fatalf("escaping parent include diagnostics = %#v", graphErr.Diagnostics)
	}
}

func TestDiscoverIncludeGraphIncludeSpellings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, source, token, target, code, message string
	}{
		{name: "plain", token: "rules/style.md"},
		{name: "dot", token: "./rules/style.md"},
		{name: "parent component", token: "rules/../rules/style.md"},
		{name: "repeated separators", token: "rules//style.md"},
		{name: "absolute", token: "{root}/rules/style.md"},
		{name: "absolute parent component", token: "{root}/rules/../rules/style.md"},
		{name: "symlink parent escape", token: "bridge/../rules/style.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "absolute symlink parent escape", token: "{root}/bridge/../rules/style.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "symlink parent missing escape", token: "bridge/../rules/missing.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "symlink parent missing directory escape", token: "bridge/../new/rules.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "in-project symlink parent", token: "inside-bridge/../rules/style.md", target: "nested/rules/style.md"},
		{name: "absolute in-project symlink parent", token: "{root}/inside-bridge/../rules/style.md", target: "nested/rules/style.md"},
		{name: "leave and reenter", token: "../project/rules/style.md"},
		{name: "nested parent", source: ".claude/CLAUDE.md", token: "../rules/style.md"},
		{name: "nested dot", source: ".claude/CLAUDE.md", token: "../rules/./style.md"},
		{name: "nested leading dot", source: ".claude/CLAUDE.md", token: "./../rules/style.md"},
		{name: "nested repeated separators", source: ".claude/CLAUDE.md", token: "..//rules/style.md"},
		{name: "nested absolute", source: ".claude/CLAUDE.md", token: "{root}/rules/style.md"},
		{name: "parent escape", token: "../outside.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "dot escape", token: "./../outside.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "parent component escape", token: "rules/../../outside.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "repeated separators escape", token: "rules//../../outside.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "nested escape", source: ".claude/CLAUDE.md", token: "../../outside.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "nested dot escape", source: ".claude/CLAUDE.md", token: "../.././outside.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "nested leading dot escape", source: ".claude/CLAUDE.md", token: "./../../outside.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "absolute escape", token: "{root}/../outside.md", code: CodeInvalidInclude, message: `include path "../outside.md" escapes the project`},
		{name: "absolute prefix escape", token: "{root}-sibling/style.md", code: CodeInvalidInclude, message: `include path "../project-sibling/style.md" escapes the project`},
		{name: "bare parent escape", token: "..", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "symlink file", token: "./link.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "symlink directory", token: "linked//style.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "symlink chain", token: "chain.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "absolute symlink file", token: "{root}/link.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "absolute symlink directory", token: "{root}/linked/style.md", code: CodeInvalidInclude, message: "escapes the project"},
		{name: "in-project symlink", token: "inside-link.md"},
		{name: "missing", token: "./rules/missing.md", code: CodeUnresolvedInclude, message: "does not resolve to a regular file"},
		{name: "missing directory", token: "new/rules.md", code: CodeUnresolvedInclude, message: `include "new/rules.md" does not resolve to a regular file`},
		{name: "absolute missing parent traversal", token: "{root}/new/../rules/style.md", code: CodeUnresolvedInclude, message: `include "new/../rules/style.md" does not resolve to a regular file`},
		{name: "two missing directories", token: "new/nested/rules.md", code: CodeUnresolvedInclude, message: `include "new/nested/rules.md" does not resolve to a regular file`},
		{name: "missing directory inside symlink", token: "inside-bridge/new/rules.md", code: CodeUnresolvedInclude, message: `include "nested/child/new/rules.md" does not resolve to a regular file`},
		{name: "dangling symlink", token: "dangling", code: CodeInvalidInclude, message: "no such file or directory"},
		{name: "dangling symlink directory", token: "dangling/new/rules.md", code: CodeInvalidInclude, message: "no such file or directory"},
		{name: "directory", token: ".", code: CodeUnresolvedInclude, message: "does not resolve to a regular file"},
		{name: "NUL", token: "rules/\x00style.md", code: CodeInvalidInclude, message: "contains NUL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			root := filepath.Join(parent, "project")
			source := test.source
			if source == "" {
				source = "CLAUDE.md"
			}
			token := strings.ReplaceAll(test.token, "{root}", filepath.ToSlash(root))
			writeGraphFile(t, root, source, "@"+token+"\n")
			writeGraphFile(t, root, "rules/style.md", "# Style\n")
			writeGraphFile(t, root, "nested/child/placeholder.md", "# Nested\n")
			writeGraphFile(t, root, "nested/rules/style.md", "# Real style\n")
			writeGraphFile(t, parent, "outside.md", "# Outside\n")
			writeGraphFile(t, parent, "project-sibling/style.md", "# Outside\n")
			writeGraphFile(t, parent, "outside/nested/placeholder.md", "# Nested\n")
			writeGraphFile(t, parent, "outside/rules/style.md", "# Outside style\n")
			for name, target := range map[string]string{
				"bridge":         "../outside/nested",
				"link.md":        "../outside.md",
				"linked":         "../project-sibling",
				"chain.md":       "link.md",
				"inside-link.md": "rules/style.md",
				"inside-bridge":  "nested/child",
				"dangling":       "new",
			} {
				if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
					t.Fatal(err)
				}
			}
			workingDir, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			relativeRoot, err := filepath.Rel(workingDir, root)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := adapter.NewRootSnapshot(relativeRoot)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := snapshot.Close(); err != nil {
					t.Error(err)
				}
			})
			for _, discovery := range []string{"disk", "snapshot", "selected snapshot"} {
				t.Run(discovery, func(t *testing.T) {
					var graph *IncludeGraph
					var err error
					switch discovery {
					case "disk":
						graph, err = DiscoverIncludeGraph(root)
					case "snapshot":
						graph, err = DiscoverIncludeGraphSnapshot(snapshot)
					case "selected snapshot":
						graph, err = DiscoverSelectedIncludeGraph(snapshot, []string{source})
					}
					if err != nil {
						t.Fatal(err)
					}
					err = graph.ValidateSelected([]string{source})
					if test.code == "" {
						target := test.target
						if target == "" {
							target = "rules/style.md"
						}
						if err != nil || len(graph.Diagnostics) != 0 || !graph.Reachable(source, target) {
							t.Fatalf("include %q: error = %v, graph = %#v", token, err, graph)
						}
						if host, ok := graph.DeepestSharedHost([]string{source}); !ok || host != target {
							t.Fatalf("include %q: host = %q, %t, want %q", token, host, ok, target)
						}
						return
					}
					var graphErr *GraphError
					if !errors.As(err, &graphErr) || len(graphErr.Diagnostics) != 1 {
						t.Fatalf("include %q: error = %v, want one %s diagnostic", token, err, test.code)
					}
					diagnostic := graphErr.Diagnostics[0]
					if diagnostic.Code != test.code || diagnostic.Path != source || diagnostic.Line != 1 || !strings.Contains(diagnostic.Message, test.message) {
						t.Fatalf("include %q: diagnostic = %#v, want %s: %s", token, diagnostic, test.code, test.message)
					}
					encoded, err := json.Marshal(graphErr)
					if err != nil {
						t.Fatal(err)
					}
					for _, output := range []string{graphErr.Error(), string(encoded)} {
						if strings.Contains(output, filepath.ToSlash(parent)) {
							t.Fatalf("include %q: diagnostic exposes absolute prefix: %s", token, output)
						}
					}
				})
			}
		})
	}
}

func TestDiscoverIncludeGraphUnrootedSnapshotRejectsSymlinkParentEscape(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	writeGraphFile(t, root, "CLAUDE.md", "@bridge/../rules/style.md\n")
	writeGraphFile(t, root, "rules/style.md", "# Local decoy\n")
	writeGraphFile(t, parent, "outside/nested/placeholder.md", "# Nested\n")
	writeGraphFile(t, parent, "outside/rules/style.md", "# Outside\n")
	if err := os.Symlink("../outside/nested", filepath.Join(root, "bridge")); err != nil {
		t.Fatal(err)
	}
	snapshot := adapter.NewFSSnapshot(os.DirFS(root))
	for _, selected := range []bool{false, true} {
		var graph *IncludeGraph
		var err error
		if selected {
			graph, err = DiscoverSelectedIncludeGraph(snapshot, []string{"CLAUDE.md"})
		} else {
			graph, err = DiscoverIncludeGraphSnapshot(snapshot)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := graph.ValidateSelected([]string{"CLAUDE.md"}); err == nil {
			t.Errorf("selected=%t: accepted an outside-resolving include", selected)
		}
		if graph.Reachable("CLAUDE.md", "rules/style.md") {
			t.Errorf("selected=%t: selected the local decoy", selected)
		}
	}
}

func TestDiscoverIncludeGraphRejectsMissingParentTraversal(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, token, target string
		symlink             bool
	}{
		{name: "existing regular file", token: "new/../rules/style.md", target: "rules/style.md"},
		{name: "pending symlinked host", token: "missing/../.claude/CLAUDE.md", target: ".claude/CLAUDE.md", symlink: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			root := filepath.Join(parent, "project")
			writeGraphFile(t, root, "CLAUDE.md", "@"+test.token+"\n")
			if test.symlink {
				writeGraphFile(t, parent, "outside/claude/CLAUDE.md", "# Outside\n")
				if err := os.Symlink("../outside/claude", filepath.Join(root, ".claude")); err != nil {
					t.Fatal(err)
				}
			} else {
				writeGraphFile(t, root, test.target, "# Style\n")
			}
			// Keep the raw spelling: Join would erase the missing parent.
			if _, err := os.ReadFile(root + "/" + test.token); !os.IsNotExist(err) {
				t.Fatalf("raw include error = %v, want ENOENT", err)
			}
			snapshot, err := adapter.NewRootSnapshot(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := snapshot.Close(); err != nil {
					t.Error(err)
				}
			})
			for _, discovery := range []string{"disk", "snapshot", "selected snapshot"} {
				t.Run(discovery, func(t *testing.T) {
					var graph *IncludeGraph
					var err error
					switch discovery {
					case "disk":
						graph, err = DiscoverIncludeGraph(root)
					case "snapshot":
						graph, err = DiscoverIncludeGraphSnapshot(snapshot)
					case "selected snapshot":
						graph, err = DiscoverSelectedIncludeGraph(snapshot, []string{"CLAUDE.md", test.target})
					}
					if err != nil {
						t.Fatal(err)
					}
					if len(graph.Edges) != 0 || graph.Reachable("CLAUDE.md", test.target) {
						t.Fatalf("unresolved include created edges: %#v", graph.Edges)
					}
					var graphErr *GraphError
					err = graph.ValidateSelected([]string{"CLAUDE.md"})
					if !errors.As(err, &graphErr) || len(graphErr.Diagnostics) != 1 {
						t.Fatalf("include %q: error = %v, want one unresolved diagnostic", test.token, err)
					}
					diagnostic := graphErr.Diagnostics[0]
					if diagnostic.Code != CodeUnresolvedInclude || diagnostic.Path != "CLAUDE.md" || diagnostic.Line != 1 ||
						diagnostic.Message != fmt.Sprintf("include %q does not resolve to a regular file", test.token) {
						t.Fatalf("include %q: unexpected diagnostic = %#v", test.token, diagnostic)
					}
					if host, ok := graph.DeepestSharedHost([]string{"CLAUDE.md"}); ok && host == test.target {
						t.Fatalf("unresolved include selected collapsed host %q", host)
					}
				})
			}
		})
	}
}

func TestDiscoverSelectedIncludeGraphMissingHost(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, token, target string
		refuse              bool
	}{
		{name: "dot", token: "./AGENTS.md"},
		{name: "parent", token: "rules/../AGENTS.md"},
		{name: "absolute", token: "{root}/AGENTS.md"},
		{name: "missing directory", token: "new/AGENTS.md", target: "new/AGENTS.md"},
		{name: "two missing directories", token: "new/nested/AGENTS.md", target: "new/nested/AGENTS.md"},
		{name: "symlink escape", token: "bridge/../AGENTS.md", refuse: true},
		{name: "absolute symlink escape", token: "{root}/bridge/../AGENTS.md", refuse: true},
		{name: "dangling symlink", token: "dangling.md", refuse: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			root := filepath.Join(parent, "project")
			token := strings.ReplaceAll(test.token, "{root}", filepath.ToSlash(root))
			target := test.target
			if target == "" {
				target = "AGENTS.md"
			}
			writeGraphFile(t, root, "CLAUDE.md", "@"+token+"\n")
			writeGraphFile(t, root, "rules/style.md", "# Style\n")
			writeGraphFile(t, parent, "outside/nested/placeholder.md", "# Outside\n")
			for name, target := range map[string]string{
				"bridge":      "../outside/nested",
				"dangling.md": "../outside/AGENTS.md",
			} {
				if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := adapter.NewRootSnapshot(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := snapshot.Close(); err != nil {
					t.Error(err)
				}
			})
			graph, err := DiscoverSelectedIncludeGraph(snapshot, []string{"CLAUDE.md", target, "dangling.md"})
			if err != nil {
				t.Fatal(err)
			}
			err = graph.ValidateSelected([]string{"CLAUDE.md"})
			if test.refuse {
				var graphErr *GraphError
				if !errors.As(err, &graphErr) || len(graphErr.Diagnostics) != 1 || graphErr.Diagnostics[0].Code != CodeInvalidInclude {
					t.Fatalf("unsafe missing host accepted: %v", err)
				}
			} else if err != nil || !graph.Reachable("CLAUDE.md", target) {
				t.Fatalf("missing selected host: reachable = %t, error = %v", graph.Reachable("CLAUDE.md", target), err)
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
