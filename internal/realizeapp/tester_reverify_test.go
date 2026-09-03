package realizeapp

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// TestTesterReverifyUninstallPrunesOnlyItsGitExclusions exercises uninstall in
// a real repository. The project tree digest excludes Git internals and records
// each product file's path, mode, and SHA-256 through hashProjectTree.
func TestTesterReverifyUninstallPrunesOnlyItsGitExclusions(t *testing.T) {
	t.Parallel()

	root, _, application := uninstallFixture(t, []string{"cursor"}, firstSource, secondSource)
	initRepository(t, root)
	realizeProject(t, application, root)

	previous := projectLedger(t, root)
	previousPaths := ledgerPaths(previous)
	beforePatterns := excludePatterns(t, root)
	beforeTree := hashProjectTree(t, root)
	if _, ok := beforeTree[".git/HEAD"]; ok {
		t.Fatalf("hash tree included Git internals: %#v", beforeTree)
	}

	removedPatterns := generatedPatternsOwnedOnlyBy(previous, firstSource)
	if want := []string{"/.cursor/rules/acr__example__first__guidance.mdc"}; !reflect.DeepEqual(removedPatterns, want) {
		t.Fatalf("removed package exclusion patterns = %#v, want %#v", removedPatterns, want)
	}
	wantPatterns := withoutPatterns(beforePatterns, removedPatterns)
	if len(wantPatterns) == len(beforePatterns) {
		t.Fatalf("exclusion block before uninstall did not contain the removed package: %#v", beforePatterns)
	}

	stdout, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", root, "--json")
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if got := excludePatterns(t, root); !reflect.DeepEqual(got, wantPatterns) {
		t.Fatalf("exclusion block after uninstall = %#v, want %#v", got, wantPatterns)
	}
	if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(gitExcludeFile))); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("%s did not survive as a regular file: info=%v err=%v", gitExcludeFile, info, err)
	}

	afterTree := hashProjectTree(t, root)
	allowed := map[string]struct{}{
		dependency.ProjectFilename: {},
		dependency.LockFilename:    {},
	}
	for path := range previousPaths {
		allowed[path] = struct{}{}
	}
	allPaths := make(map[string]struct{}, len(beforeTree)+len(afterTree))
	for path := range beforeTree {
		allPaths[path] = struct{}{}
	}
	for path := range afterTree {
		allPaths[path] = struct{}{}
	}
	for path := range allPaths {
		if _, mayChange := allowed[path]; mayChange {
			continue
		}
		if beforeTree[path] != afterTree[path] {
			t.Fatalf("path outside the previous ledger changed: %s before=%q after=%q", path, beforeTree[path], afterTree[path])
		}
	}
}

func generatedPatternsOwnedOnlyBy(ledger realize.Ledger, source string) []string {
	var patterns []string
	for _, target := range ledger.Targets {
		if target.Ownership != realize.OwnershipGenerated || len(target.Entries) == 0 {
			continue
		}
		onlySource := true
		for _, entry := range target.Entries {
			if entry.Source != source {
				onlySource = false
				break
			}
		}
		if onlySource {
			patterns = append(patterns, "/"+target.Path)
		}
	}
	sort.Strings(patterns)
	return patterns
}

func withoutPatterns(patterns, removed []string) []string {
	removedSet := make(map[string]struct{}, len(removed))
	for _, pattern := range removed {
		removedSet[pattern] = struct{}{}
	}
	kept := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if _, drop := removedSet[pattern]; !drop {
			kept = append(kept, pattern)
		}
	}
	return kept
}
