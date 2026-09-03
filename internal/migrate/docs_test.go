package migrate

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

type readmeBullet struct {
	line int
	text string
}

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
	} {
		if !strings.Contains(readme, statement) {
			t.Errorf("README lacks deferred-capability statement %q", statement)
		}
	}
	issueURL := regexp.MustCompile(`https://github\.com/jbaruch/agentic-context-registry/issues/[1-9][0-9]*`)
	for _, bullet := range deferredCapabilityBullets(t, readme) {
		if !issueURL.MatchString(bullet.text) {
			t.Errorf("README.md:%d: deferred capability bullet has no issue URL: %s", bullet.line, bullet.text)
		}
	}
}

func deferredCapabilityBullets(t *testing.T, readme string) []readmeBullet {
	t.Helper()
	lines := strings.Split(readme, "\n")
	heading := -1
	for index, line := range lines {
		if line == "### Deferred capabilities" {
			heading = index
			break
		}
	}
	if heading < 0 {
		t.Fatal("README lacks Deferred capabilities section")
	}

	var bullets []readmeBullet
	for index := heading + 1; index < len(lines); index++ {
		line := lines[index]
		if strings.HasPrefix(line, "#") {
			break
		}
		if strings.HasPrefix(line, "- ") {
			bullets = append(bullets, readmeBullet{line: index + 1, text: line})
			continue
		}
		if len(bullets) != 0 && strings.TrimSpace(line) != "" {
			bullets[len(bullets)-1].text += " " + strings.TrimSpace(line)
		}
	}
	if len(bullets) == 0 {
		t.Fatal("README Deferred capabilities section has no bullets")
	}
	return bullets
}
