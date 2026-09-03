package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verify8Round3StaleReferences drives a full vendor-then-finalize arc through a
// separately built acr binary against a consumer carrying the symlinks the
// round-3 fix has to survive, and returns the finalization report's
// staleReferences rows together with the raw process output.
//
// Every symlink is committed before the vendoring run, so the scan sees them as
// tracked paths rather than as ignorable working-tree noise: an untracked link
// never reaches `git ls-files` and would make the fixture vacuous.
func verify8Round3StaleReferences(t *testing.T, root string) ([]map[string]any, string, string) {
	t.Helper()
	binary := reverifyBuildACR(t)
	stateHome := t.TempDir()
	verify8GitCommit(t, root)

	stdout, stderr, exitCode := hostileRunBinary(t, binary, stateHome, strings.NewReader(""),
		"migrate", "tessl", "--project", root, "--vendor-unmapped", "--non-interactive", "--json")
	if exitCode != 0 || stderr != "" {
		t.Fatalf("vendor exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	verify8GitCommit(t, root)

	stdout, stderr, exitCode = hostileRunBinary(t, binary, stateHome, strings.NewReader(""),
		"migrate", "tessl", "--project", root, "--finalize", "--non-interactive", "--json")
	if exitCode != 0 || stderr != "" {
		t.Fatalf("finalize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	result, ok := verify8Envelope(t, stdout)["result"].(map[string]any)
	if !ok || result["wrote"] != true || result["mode"] != "finalized" {
		t.Fatalf("finalize result = %#v", result)
	}
	raw, ok := result["staleReferences"].([]any)
	if !ok {
		t.Fatalf("staleReferences = %#v, want an array", result["staleReferences"])
	}
	rows := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("staleReferences row = %#v", item)
		}
		rows = append(rows, row)
	}
	return rows, stdout, stderr
}

// TestVerify8Round3BinaryFinalizesPastATrackedDirectorySymlink is V1 at the
// process contract: an ordinary docs shortcut used to abort the whole
// finalization with an EISDIR the scan neither tolerated nor classified.
func TestVerify8Round3BinaryFinalizesPastATrackedDirectorySymlink(t *testing.T) {
	root := verify8SeedTesslConsumer(t)
	if err := os.MkdirAll(filepath.Join(root, "docs/guide"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The target carries a .tessl/ mention so the scan has a reason to want to
	// read through the link; a link to inert content could pass vacuously.
	if err := os.WriteFile(filepath.Join(root, "docs/guide/index.md"),
		[]byte("Run tessl__review-alpha from .tessl/plugins/example/alpha.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("docs/guide", filepath.Join(root, "guide")); err != nil {
		t.Fatal(err)
	}

	rows, _, _ := verify8Round3StaleReferences(t, root)

	info, err := os.Lstat(filepath.Join(root, "guide"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the directory symlink did not survive finalization: %v, mode = %v", err, info)
	}
	target, err := os.Readlink(filepath.Join(root, "guide"))
	if err != nil || target != "docs/guide" {
		t.Fatalf("symlink target = %q, %v, want %q", target, err, "docs/guide")
	}
	for _, row := range rows {
		if row["path"] == "guide" || strings.HasPrefix(row["path"].(string), "guide/") {
			t.Fatalf("the scan reported a path through the directory symlink: %#v", row)
		}
	}
	// The real file behind the link is still reported on its own tracked path,
	// so the fix skipped the link rather than switching the whole scan off.
	found := false
	for _, row := range rows {
		if row["path"] == "docs/guide/index.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the scan stopped reporting real tracked files: %#v", rows)
	}
}

// TestVerify8Round3BinaryNeverEchoesContentFromOutsideTheRoot is V2 at the
// process contract: the scan used to follow a tracked link out of the project
// and copy the target's matching lines into staleReferences[], which is printed
// on stdout and emitted in --json.
func TestVerify8Round3BinaryNeverEchoesContentFromOutsideTheRoot(t *testing.T) {
	const sentinel = "verify8-round3-sentinel-must-never-be-echoed"
	root := verify8SeedTesslConsumer(t)
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("deploy key for .tessl/ pipeline "+sentinel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside-link.md")); err != nil {
		t.Fatal(err)
	}
	// An in-project link to a regular file that genuinely matches: the target
	// must be reported once on its own path, and never again through the link.
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs/notes.md"),
		[]byte("see .tessl/plugins/example/alpha for the rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("docs/notes.md", filepath.Join(root, "notes-link.md")); err != nil {
		t.Fatal(err)
	}

	rows, stdout, stderr := verify8Round3StaleReferences(t, root)

	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	for name, stream := range map[string]string{"stdout": stdout, "stderr": stderr, "staleReferences": string(encoded)} {
		if strings.Contains(stream, sentinel) {
			t.Fatalf("%s echoed content read from outside the project root: %q", name, stream)
		}
	}
	paths := make([]string, 0, len(rows))
	for _, row := range rows {
		paths = append(paths, row["path"].(string))
	}
	for _, forbidden := range []string{"outside-link.md", "notes-link.md"} {
		for _, path := range paths {
			if path == forbidden {
				t.Fatalf("the scan reported the tracked symlink %q: %#v", forbidden, rows)
			}
		}
	}
	matches := 0
	for _, path := range paths {
		if path == "docs/notes.md" {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("docs/notes.md reported %d times, want exactly 1: %#v", matches, rows)
	}
}
