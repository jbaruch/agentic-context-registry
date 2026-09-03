package migrateapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// TestVerify8VendorIdempotentSecondApply holds the whole project tree, not just
// the report flag, across a repeated --vendor-unmapped run. The existing
// coverage asserts only that the second report says it did not write; this
// asserts that nothing on disk moved, that the recorded content hash is the
// same value rather than merely a recomputed one, and that the second pass
// reached no resolver (vendorPanicRemote panics if it does).
func TestVerify8VendorIdempotentSecondApply(t *testing.T) {
	t.Parallel()
	root := writeUnmappedConsumer(t)
	service := newService(vendorPanicRemote{})
	first, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Wrote {
		t.Fatalf("first vendor apply did not write: %#v", first)
	}
	state, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	firstHash := state.Lock.Dependencies[0].ContentHash
	if firstHash == "" {
		t.Fatal("vendor lock recorded no content hash")
	}
	sealed := hashTreeWithModes(t, root)

	second, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.Wrote {
		t.Fatalf("second vendor apply wrote: %#v", second)
	}
	if after := hashTreeWithModes(t, root); !mapsEqual(sealed, after) {
		t.Fatalf("second vendor apply changed the project: before=%v after=%v", sealed, after)
	}
	reloaded, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Lock.Dependencies[0].ContentHash != firstHash {
		t.Fatalf("content hash moved across identical applies: %q -> %q", firstHash, reloaded.Lock.Dependencies[0].ContentHash)
	}

	// A third pass after a realization proves the vendored tree is a stable
	// realization input rather than something migration keeps rewriting.
	if _, err := service.realizer.Run(context.Background(), root, nil, realize.ModeCheck); err != nil {
		t.Fatalf("check between applies: %v", err)
	}
	third, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true})
	if err != nil {
		t.Fatal(err)
	}
	if third.Wrote {
		t.Fatalf("third vendor apply wrote: %#v", third)
	}
	if after := hashTreeWithModes(t, root); !mapsEqual(sealed, after) {
		t.Fatal("the third vendor apply changed the project")
	}
}

// TestVerify8VendorHashIgnoresDirectoryWriteOrder builds the same logical
// package twice, writing its files in opposite order in each tree, and requires
// one content hash. Readdir order is filesystem-dependent, so an implementation
// that hashed entries in walk order rather than in sorted POSIX order would
// produce a lock value that differs between machines.
func TestVerify8VendorHashIgnoresDirectoryWriteOrder(t *testing.T) {
	t.Parallel()
	names := []string{"a.md", "m.md", "z.md", "nested/b.md", "nested/y.md"}
	build := func(order []string) string {
		root := t.TempDir()
		for _, name := range order {
			filename := filepath.Join(root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filename, []byte("body of "+name+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	reversed := make([]string, len(names))
	for index, name := range names {
		reversed[len(names)-1-index] = name
	}
	forward, err := dependency.HashVendorTree(build(names))
	if err != nil {
		t.Fatal(err)
	}
	backward, err := dependency.HashVendorTree(build(reversed))
	if err != nil {
		t.Fatal(err)
	}
	if forward != backward {
		t.Fatalf("write order changed the vendor hash: %q != %q", forward, backward)
	}
}

// TestVerify8VendorHashIsModeSensitive pins the execute bit as load-bearing: a
// hook script vendored 0755 and one vendored 0644 are different packages, and a
// hash that dropped the mode record would let a chmod pass re-realization
// unnoticed.
func TestVerify8VendorHashIsModeSensitive(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	script := filepath.Join(root, "hooks", "check.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	executable, err := dependency.HashVendorTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o644); err != nil {
		t.Fatal(err)
	}
	plain, err := dependency.HashVendorTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if executable == plain {
		t.Fatalf("dropping the execute bit did not change the vendor hash: %q", executable)
	}
}

// TestVerify8FinalizeLeavesGitignoreAndToolOwnedStateByteEqual holds the
// retention half of the removal contract that the existing finalize coverage
// leaves implicit. The seeded project carries a user .gitignore, ACR's own lock
// and generated natives, and an unmanaged file; every one of them must survive
// finalization byte-for-byte while the Tessl installation goes away.
func TestVerify8FinalizeLeavesGitignoreAndToolOwnedStateByteEqual(t *testing.T) {
	root := writeUnmappedConsumer(t)
	gitignore := []byte("# user policy\n/build/\n*.tmp\n")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), gitignore, 0o644); err != nil {
		t.Fatal(err)
	}
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	gitCommitFixture(t, root)

	toolOwned := map[string][]byte{}
	for _, relative := range []string{".gitignore", ".agents/registry.lock", "agents.yaml"} {
		content, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatalf("seed %s: %v", relative, err)
		}
		toolOwned[relative] = content
	}
	vendored := hashTreeWithModes(t, filepath.Join(root, ".agents/vendor"))

	if _, err := service.Migrate(context.Background(), root, Options{Finalize: true}); err != nil {
		t.Fatal(err)
	}

	for relative, want := range toolOwned {
		got, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatalf("read %s after finalize: %v", relative, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("finalization rewrote %s:\nbefore=%q\nafter=%q", relative, want, got)
		}
	}
	if after := hashTreeWithModes(t, filepath.Join(root, ".agents/vendor")); !mapsEqual(vendored, after) {
		t.Fatalf("finalization changed the vendored tree: before=%v after=%v", vendored, after)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents")); err != nil {
		t.Fatalf(".agents directory removed by finalization: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".tessl")); !os.IsNotExist(err) {
		t.Fatalf(".tessl survived finalization: %v", err)
	}
}

// TestVerify8FinalizeDoesNotFollowATesslNativeOutOfTheTesslTree points a
// tessl__ skill native at a directory the user owns outside .tessl/. Whatever
// finalization decides about the link itself, it must never reach through it:
// the user's bytes are not Tessl output and deleting them would be silent data
// loss behind a path that only looks Tessl-owned.
//
// The link is gitignored on purpose. A tracked symlink is read by the
// stale-reference scan, which is where TestVerify8StaleScanRefusesATrackedDirectorySymlink
// and TestVerify8StaleScanDoesNotReadOutsideTheProjectRoot bite; keeping it
// untracked isolates the retention property from those two defects.
func TestVerify8FinalizeDoesNotFollowATesslNativeOutOfTheTesslTree(t *testing.T) {
	root := writeUnmappedConsumer(t)
	userSkill := filepath.Join(root, "user-skills", "notes")
	if err := os.MkdirAll(userSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("# User notes\n\nThese are mine.\n")
	if err := os.WriteFile(filepath.Join(userSkill, "SKILL.md"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, ".claude/skills/tessl__notes")
	if err := os.Symlink("../../user-skills/notes", link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".claude/skills/tessl__notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	gitCommitFixture(t, root)
	if _, err := service.Migrate(context.Background(), root, Options{Finalize: true}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(userSkill, "SKILL.md"))
	if err != nil {
		t.Fatalf("finalization followed the link and removed the user's skill: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("finalization rewrote the link target:\nbefore=%q\nafter=%q", body, got)
	}
	if _, err := os.Stat(filepath.Join(root, ".tessl")); !os.IsNotExist(err) {
		t.Fatalf(".tessl survived finalization: %v", err)
	}
}

// TestVerify8StaleScanRefusesATrackedDirectorySymlink is a regression test for
// a defect this branch introduces. findStaleReferences reads every tracked path
// with os.ReadFile, which follows symlinks; a tracked link to a directory
// returns EISDIR, which the scan neither tolerates nor classifies, so the whole
// finalization aborts on an ordinary repository shape with a message that tells
// the user nothing they can act on.
//
// A directory symlink is not exotic -- docs/ shortcuts, shared skill trees and
// monorepo aliases all take this form, and the fixture below contains nothing
// Tessl-specific. The scan should skip a path that is not a regular file, the
// way every other read surface in this codebase does.
func TestVerify8StaleScanRefusesATrackedDirectorySymlink(t *testing.T) {
	root := writeUnmappedConsumer(t)
	real := filepath.Join(root, "docs", "guide")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "index.md"), []byte("# Guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("docs/guide", filepath.Join(root, "guide")); err != nil {
		t.Fatal(err)
	}

	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	gitCommitFixture(t, root)
	if _, err := service.Migrate(context.Background(), root, Options{Finalize: true}); err != nil {
		t.Fatalf("an ordinary tracked directory symlink aborted finalization: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "guide", "index.md")); err != nil {
		t.Fatalf("finalization disturbed the symlink target: %v", err)
	}
}

// TestVerify8StaleScanDoesNotReadOutsideTheProjectRoot is a regression test for
// the second half of the same defect. Because findStaleReferences reads tracked
// paths with os.ReadFile rather than through the project root, a tracked
// symlink pointing outside the project is followed, and any line of the outside
// file mentioning .tessl/ or tessl__ is copied verbatim into staleReferences[],
// which is printed on stdout and emitted in --json.
//
// Every other read surface here is confined with os.OpenRoot or RootSnapshot
// for exactly this reason (see TestRootSnapshotRejectsSymlinkTarget). The scan
// must either refuse the escape or skip the link, never echo bytes from a path
// outside the project the operator asked it to finalize.
func TestVerify8StaleScanDoesNotReadOutsideTheProjectRoot(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "private.txt")
	if err := os.WriteFile(secret, []byte("deploy key for .tessl/ pipeline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := writeUnmappedConsumer(t)
	if err := os.Symlink(secret, filepath.Join(root, "outside-link.md")); err != nil {
		t.Fatal(err)
	}

	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	gitCommitFixture(t, root)
	report, err := service.Migrate(context.Background(), root, Options{Finalize: true})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	for _, reference := range report.StaleReferences {
		if reference.Path == "outside-link.md" {
			t.Fatalf("the stale-reference scan read outside the project root and echoed the content: %+v", reference)
		}
	}
}

// TestVerify8FinalizeSplicesOnlyTheTesslGitignoreBlock is the one removal shape
// the AC row names that no test held: the Tessl-generated block leaves
// .gitignore while every user line around it stays byte-identical, and the
// splice is reported as one managed-span removal rather than a file delete.
func TestVerify8FinalizeSplicesOnlyTheTesslGitignoreBlock(t *testing.T) {
	root := writeUnmappedConsumer(t)
	// The Tessl block deliberately ignores a path under .tessl/ rather than
	// .agents/: a block that ignored the vendor tree would trip the untracked
	// vendor gate first and never reach the splice.
	content := "# user policy\n/build/\n\n# === Tessl-generated artifacts (managed by tessl) ===\n.tessl/cache/\n# === end Tessl-generated artifacts ===\n\n*.tmp\n"
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	gitCommitFixture(t, root)
	report, err := service.Migrate(context.Background(), root, Options{Finalize: true})
	if err != nil {
		t.Fatal(err)
	}

	spliced := false
	for _, removal := range report.Removed {
		if removal.Path != ".gitignore" {
			continue
		}
		if removal.Operation != "splice" || removal.ID != "tessl-gitignore" {
			t.Fatalf("gitignore removal = %+v, want a tessl-gitignore splice", removal)
		}
		spliced = true
	}
	if !spliced {
		t.Fatalf("finalization reported no .gitignore splice: %+v", report.Removed)
	}

	after, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# user policy", "/build/", "*.tmp"} {
		if !bytes.Contains(after, []byte(want)) {
			t.Fatalf("the splice dropped the user line %q:\n%s", want, after)
		}
	}
	for _, gone := range []string{"Tessl-generated artifacts", ".tessl/cache/"} {
		if bytes.Contains(after, []byte(gone)) {
			t.Fatalf("the Tessl block survived the splice:\n%s", after)
		}
	}
}

// TestVerify8LossyRuleProseBlocksFinalizationEndToEnd closes the one
// equivalence sub-case the plan names that only the predicate unit test covers.
// TestFinalizeRequiresEquivalence calls finalizationReady directly, so it proves
// the conjunct but not the row's Expected column: exit 4 through the CLI, the
// blocked code on stderr, and a project that did not move.
//
// The lossy input is an applyTo: whose prose half has no v1 field. The rule
// still migrates, so nothing else blocks; only the recorded loss should.
func TestVerify8LossyRuleProseBlocksFinalizationEndToEnd(t *testing.T) {
	root := writeUnmappedConsumer(t)
	rule := []byte("---\nalwaysApply: false\napplyTo: internal/**/*.go — Go source only\ndescription: prose with no v1 home\n---\n\nHandle errors.\n")
	if err := os.WriteFile(filepath.Join(root, ".tessl/plugins/example/orphan/rules/always.md"), rule, 0o644); err != nil {
		t.Fatal(err)
	}
	application := &Application{service: newService(vendorPanicRemote{}), fallback: cli.UnavailableApplication{}}
	if stdout, stderr, exitCode := runCLI(t, application,
		"migrate", "tessl", "--project", root, "--vendor-unmapped", "--non-interactive", "--json"); exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("vendor exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	gitCommitFixture(t, root)
	sealed := hashTreeWithModes(t, root)

	stdout, stderr, exitCode := runCLI(t, application,
		"migrate", "tessl", "--project", root, "--finalize", "--non-interactive", "--json")
	if exitCode != cli.ExitConflict {
		t.Fatalf("lossy finalize exit = %d, stdout = %q, stderr = %q, want %d", exitCode, stdout, stderr, cli.ExitConflict)
	}
	if stdout != "" || !strings.Contains(stderr, `"code":"finalization_blocked"`) {
		t.Fatalf("lossy refusal stdout = %q, stderr = %q", stdout, stderr)
	}
	if after := hashTreeWithModes(t, root); !mapsEqual(sealed, after) {
		t.Fatalf("the blocked finalization changed the project: before=%v after=%v", sealed, after)
	}
}

// TestVerify8SupersedeRemovesTheVendorTreeOnlyAfterTheCommit holds the ordering
// half of the supersede row: the vendor tree is still the realization input
// while the transaction runs, and only a committed switch to the GitHub source
// may remove it.
//
// The second half of the row -- a kill between the ownership commit and the
// separately journaled removal -- remains tracked in issue #57. What that
// window leaves behind is an orphaned tree no lock references, so this pins
// the property that protects the user: such a tree is inert, and neither check
// nor a later migration is destabilised by it.
func TestVerify8SupersedeRemovesTheVendorTreeOnlyAfterTheCommit(t *testing.T) {
	root := writeUnmappedConsumer(t)
	if _, err := newService(vendorPanicRemote{}).Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	vendorRoot := filepath.Join(root, ".agents/vendor/example/orphan")
	saved := map[string][]byte{}
	if err := filepath.WalkDir(vendorRoot, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		content, readErr := os.ReadFile(name)
		if readErr != nil {
			return readErr
		}
		saved[name] = content
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(saved) == 0 {
		t.Fatal("the vendor fixture staged no files")
	}

	remote := &integrationGitHub{release: dependency.Release{ID: 7, Tag: "v1.0.0"}, commit: strings.Repeat("7", 40), archive: orphanPackageArchive(t)}
	mappings, err := migrate.ParseInlineMappings([]string{"example/orphan=github:example/orphan@latest"})
	if err != nil {
		t.Fatal(err)
	}
	service := newService(remote)
	if _, err := service.Migrate(context.Background(), root, Options{CLIMappings: mappings}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vendorRoot); !os.IsNotExist(err) {
		t.Fatalf("the committed supersede left the vendor tree behind: %v", err)
	}

	// Restore the tree: this is the state a crash in the unjournaled removal
	// window leaves. Nothing references it any more, so it must not break the
	// project.
	for name, content := range saved {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.realizer.Run(context.Background(), root, nil, realize.ModeCheck); err != nil {
		t.Fatalf("an orphaned vendor tree broke check: %v", err)
	}
	report, err := service.Migrate(context.Background(), root, Options{CLIMappings: mappings})
	if err != nil {
		t.Fatalf("an orphaned vendor tree broke a later migration: %v", err)
	}
	if report.Wrote {
		t.Fatalf("a converged migration wrote because of an orphaned vendor tree: %#v", report)
	}
}
