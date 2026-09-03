package cli

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The README command fence is tagged non-executable, so the command harness
// never reads it and a shipped command can go unlisted. Bind it to the same
// usage lines the safety matrix derives its rows from.
func TestReadmeCommandFenceMatchesCommandSurface(t *testing.T) {
	t.Parallel()

	root := docsRepositoryRoot(t)
	bases := map[string]bool{}
	for _, command := range commandOrder {
		for _, usage := range strings.Split(commandSpecs[command].usage, "\n") {
			base := commandWords(usage)
			if base == "" {
				t.Fatalf("command %q usage %q has no command words", command, usage)
			}
			bases[base] = true
		}
	}

	listed := map[string]bool{}
	for _, line := range readmeCommandFence(t, readDocsFile(t, filepath.Join(root, "README.md"))) {
		base := longestCommandBase(bases, line)
		if base == "" {
			t.Errorf("README command fence lists unknown command %q", line)
			continue
		}
		listed[base] = true
	}
	assertStringSet(t, "README command fence commands", listed, bases)
}

// The invalid_include remedy has to say that removal is the exit-0 recovery and
// that substituting an import can move the managed block instead.
func TestInvalidIncludeRemedyStatesTheHostMoveRisk(t *testing.T) {
	t.Parallel()

	remedy := troubleshootingRemedy(t, docsRepositoryRoot(t), "invalid_include")
	for _, clause := range []string{
		"Remove the invalid import",
		"deepest reachable host",
		"https://github.com/jbaruch/agentic-context-registry/issues/55",
	} {
		if !strings.Contains(remedy, clause) {
			t.Errorf("invalid_include remedy does not state %q: %s", clause, remedy)
		}
	}
}

func troubleshootingRemedy(t *testing.T, root, code string) string {
	t.Helper()
	for _, row := range markdownTable(t, filepath.Join(root, "docs", "troubleshooting.md"), "") {
		if len(row) >= 4 && plainCode(row[2]) == code {
			return row[3]
		}
	}
	t.Fatalf("troubleshooting index has no row for code %q", code)
	return ""
}

func readmeCommandFence(t *testing.T, readme string) []string {
	t.Helper()
	lines := strings.Split(readme, "\n")
	index := 0
	for ; index < len(lines) && lines[index] != "## CLI"; index++ {
	}
	if index == len(lines) {
		t.Fatal("README lacks a CLI section")
	}
	for ; index < len(lines) && !strings.HasPrefix(lines[index], "```"); index++ {
	}
	if index == len(lines) {
		t.Fatal("README CLI section has no command fence")
	}
	var commands []string
	for index++; index < len(lines) && lines[index] != "```"; index++ {
		if line := strings.TrimSpace(lines[index]); line != "" {
			commands = append(commands, line)
		}
	}
	if len(commands) == 0 {
		t.Fatal("README command fence is empty")
	}
	return commands
}

// commandWords keeps a usage line's leading literal command words, dropping the
// first placeholder or flag, so "acr resume SOURCE [--dry-run]" becomes
// "acr resume" and "acr migrate tessl-plugin [PATH]" keeps both words.
func commandWords(usage string) string {
	word := regexp.MustCompile(`^[a-z][a-z-]*$`)
	var words []string
	for _, field := range strings.Fields(usage) {
		if !word.MatchString(field) {
			break
		}
		words = append(words, field)
	}
	return strings.Join(words, " ")
}

// longestCommandBase resolves a fence line to its usage base, preferring the
// longest match so "acr migrate tessl" cannot claim "acr migrate tessl-plugin".
func longestCommandBase(bases map[string]bool, line string) string {
	longest := ""
	for base := range bases {
		if line != base && !strings.HasPrefix(line, base+" ") {
			continue
		}
		if len(base) > len(longest) {
			longest = base
		}
	}
	return longest
}
