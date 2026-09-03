package migrate

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestReadmeNamesEveryUncoveredTesslTree(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(content)
	var uncovered []string
	for _, tree := range tesslAgentTrees {
		if tree.covered {
			continue
		}
		family := "." + tree.id
		if tree.id == "agents" {
			family = ".agents/skills"
		}
		uncovered = append(uncovered, family)
	}
	sort.Strings(uncovered)
	want := []string{".agents/skills", ".gemini", ".github", ".vscode"}
	if strings.Join(uncovered, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("uncovered source trees = %q, want %q", uncovered, want)
	}
	for _, family := range []string{".gemini", ".vscode", ".github", ".agents/skills"} {
		if !strings.Contains(readme, "`"+family+"`") {
			t.Errorf("README does not name uncovered Tessl tree %s", family)
		}
	}
	for _, statement := range []string{
		"ACR never realizes or removes them",
		"WSL counts as Linux",
		"issues/14",
		"issues/13",
		"issues/4",
	} {
		if !strings.Contains(readme, statement) {
			t.Errorf("README lacks deferred-capability statement %q", statement)
		}
	}
}
