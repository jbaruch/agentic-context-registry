package release

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"
)

func TestDocumentationRelativeLinksAndAnchorsResolve(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	files := []string{filepath.Join(root, "README.md")}
	docs, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(docs)
	files = append(files, docs...)
	linkPattern := regexp.MustCompile(`!?\[[^]]*\]\(([^)[:space:]]+)(?:\s+"[^"]*")?\)`)

	for _, source := range files {
		content, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range linkPattern.FindAllStringSubmatch(string(content), -1) {
			target := match[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			pathPart, anchor, _ := strings.Cut(target, "#")
			decoded, err := url.PathUnescape(pathPart)
			if err != nil {
				t.Errorf("%s has invalid escaped link %q: %v", source, target, err)
				continue
			}
			destination := source
			if decoded != "" {
				destination = filepath.Clean(filepath.Join(filepath.Dir(source), filepath.FromSlash(decoded)))
			}
			info, err := os.Stat(destination)
			if err != nil {
				t.Errorf("%s link %q does not resolve: %v", source, target, err)
				continue
			}
			if anchor == "" || info.IsDir() || !strings.EqualFold(filepath.Ext(destination), ".md") {
				continue
			}
			headings := markdownHeadingAnchors(t, destination)
			if !headings[anchor] {
				t.Errorf("%s link %q names absent anchor %q in %s", source, target, anchor, destination)
			}
		}
	}
}

func markdownHeadingAnchors(t *testing.T, filename string) map[string]bool {
	t.Helper()
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]bool{}
	counts := map[string]int{}
	for _, line := range strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		title := strings.TrimSpace(strings.TrimLeft(line, "#"))
		if title == "" {
			continue
		}
		anchor := githubHeadingAnchor(title)
		if count := counts[anchor]; count != 0 {
			counts[anchor] = count + 1
			anchor += "-" + strconvItoa(count)
		} else {
			counts[anchor] = 1
		}
		result[anchor] = true
	}
	return result
}

func githubHeadingAnchor(title string) string {
	var anchor strings.Builder
	space := false
	for _, character := range strings.ToLower(title) {
		switch {
		case unicode.IsLetter(character) || unicode.IsDigit(character) || character == '-' || character == '_':
			if space && anchor.Len() != 0 {
				anchor.WriteByte('-')
			}
			space = false
			anchor.WriteRune(character)
		case unicode.IsSpace(character):
			space = true
		}
	}
	return anchor.String()
}

func strconvItoa(value int) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	var reversed [20]byte
	index := len(reversed)
	for value > 0 {
		index--
		reversed[index] = digits[value%10]
		value /= 10
	}
	return string(reversed[index:])
}
