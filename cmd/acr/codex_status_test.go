package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCodexChangedPathsPreservesGitNames(t *testing.T) {
	root := t.TempDir()
	journeyGit(t, root, "init")
	put := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	put(".github/aw/actions-lock.json", "old")
	put("staged.txt", "old")
	put("rename-old.txt", "old")
	journeyGit(t, root, "add", ".")
	journeyGit(t, root, "commit", "-m", "baseline")
	put(".github/aw/actions-lock.json", "changed")
	put("staged.txt", "changed")
	journeyGit(t, root, "add", "staged.txt")
	journeyGit(t, root, "mv", "rename-old.txt", "rename-new.txt")
	names := []string{"untracked.txt", " white space ", "a\"quote.txt", "back\\slash.txt", "line\nbreak.txt", "café.txt"}
	want := map[string]bool{".github/aw/actions-lock.json": true, "staged.txt": true, "rename-old.txt": true, "rename-new.txt": true}
	for _, name := range names {
		put(name, "new")
		want[name] = true
	}
	got := codexChangedPaths(t, root)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changed paths differ: got %q want %q", sortedKeys(got), sortedKeys(want))
	}
}

func TestCodexChangedPathsRefusesBrokenFraming(t *testing.T) {
	for _, raw := range []string{" M .github/a", " M \x00", "R  new\x00old\x00", "??xname\x00"} {
		if _, err := codexParseChangedPaths(raw); err == nil {
			t.Fatalf("accepted malformed status %q", raw)
		}
	}
	if got, err := codexParseChangedPaths(""); err != nil || len(got) != 0 {
		t.Fatalf("clean tree: %v %v", got, err)
	}
}
