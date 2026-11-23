package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func skillManifestYAML(rulePath string) string {
	return "schemaVersion: 1\nname: owner/plugin\nversion: 1.2.3\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  skills:\n    - id: advocate\n      path: skills/advocate\n  rules:\n    - id: guidance\n      path: " + rulePath + "\n      activation:\n        mode: always\n"
}

func TestPackageListingSelectsGitsView(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeHostileFile(t, root, manifest.Filename, skillManifestYAML("guidance.md"))
	writeHostileFile(t, root, "guidance.md", "# Guidance\n")
	writeHostileFile(t, root, "skills/advocate/SKILL.md", "# Skill\n")
	writeHostileFile(t, root, "skills/advocate/runtime/worker.py", "print()\n")
	writeHostileFile(t, root, "skills/advocate/runtime/__pycache__/worker.cpython-311.pyc", "bytecode")
	writeHostileFile(t, root, "skills/advocate/untracked.md", "not ignored\n")
	source := &fakeGitSource{inside: true, visible: []string{
		"agent-plugin.yaml", "guidance.md", "skills/advocate/SKILL.md", "skills/advocate/runtime/worker.py", "skills/advocate/untracked.md",
	}}
	list, err := packageListing(context.Background(), root, source)
	if err != nil {
		t.Fatal(err)
	}
	_, files, err := manifest.LoadListed(root, list)
	if err != nil {
		t.Fatal(err)
	}
	want := "agent-plugin.yaml guidance.md skills/advocate/SKILL.md skills/advocate/runtime/worker.py skills/advocate/untracked.md"
	if got := strings.Join(files, " "); got != want {
		t.Fatalf("files = %q, want %q", got, want)
	}
}

func TestPackageListingFallsBackToFullDirectoryOutsideGit(t *testing.T) {
	root := t.TempDir()
	writeHostileFile(t, root, manifest.Filename, skillManifestYAML("guidance.md"))
	writeHostileFile(t, root, "guidance.md", "# Guidance")
	writeHostileFile(t, root, "skills/advocate/SKILL.md", "# Skill")
	writeHostileFile(t, root, "skills/advocate/cache.pyc", "bytecode")
	list, err := packageListing(context.Background(), root, &fakeGitSource{})
	if err != nil {
		t.Fatal(err)
	}
	_, files, err := manifest.LoadListed(root, list)
	if err != nil || !strings.Contains(strings.Join(files, " "), "cache.pyc") {
		t.Fatalf("fallback: %v, %v", files, err)
	}
}

func TestBuilderIgnoresGitIgnoredSkillFilesAndRefusesTagTreeMismatchWithAccurateAdvice(t *testing.T) {
	t.Parallel()

	manifestContent := skillManifestYAML("guidance.md")
	commit := strings.Repeat("a", 40)
	tree := map[string]File{
		"agent-plugin.yaml":        {Path: "agent-plugin.yaml", Mode: 0o644, Content: []byte(manifestContent)},
		"guidance.md":              {Path: "guidance.md", Mode: 0o644, Content: []byte("# Guidance\n")},
		"skills/advocate/SKILL.md": {Path: "skills/advocate/SKILL.md", Mode: 0o644, Content: []byte("---\nname: advocate\ndescription: Advocate.\n---\n# Skill\n")},
	}
	build := func(t *testing.T, tree map[string]File, ignored map[string]bool) error {
		root := t.TempDir()
		writeHostileFile(t, root, manifest.Filename, manifestContent)
		writeHostileFile(t, root, "guidance.md", "# Guidance\n")
		writeHostileFile(t, root, "skills/advocate/SKILL.md", "# Skill\n")
		writeHostileFile(t, root, "skills/advocate/__pycache__/worker.cpython-311.pyc", "bytecode")
		source := &fakeGitSource{
			clean: true, head: commit, tags: []string{"v1.2.3"}, files: tree, ignored: ignored, inside: true,
			visible: []string{"agent-plugin.yaml", "guidance.md", "skills/advocate/SKILL.md"},
		}
		_, err := newBuilder(source, NewGate(), "acr test").Prepare(context.Background(), root)
		return err
	}

	t.Run("ignored bytecode never reaches the tag tree lookup", func(t *testing.T) {
		t.Parallel()
		if err := build(t, tree, nil); err != nil {
			t.Fatalf("ignored bytecode blocked preparation: %v", err)
		}
	})

	t.Run("tag tree lacks a declared file", func(t *testing.T) {
		t.Parallel()
		missing := map[string]File{"agent-plugin.yaml": tree["agent-plugin.yaml"], "skills/advocate/SKILL.md": tree["skills/advocate/SKILL.md"]}
		err := build(t, missing, nil)
		assertUnpublishable(t, err, "does not contain \"guidance.md\"", "exact committed path spelling",
			"Otherwise, commit the declared file and retag, or stop declaring it")
		if strings.Contains(err.Error(), "commit every declared package file") {
			t.Fatalf("old advice survived: %v", err)
		}
	})

	t.Run("tag tree lacks a declared file that Git ignores", func(t *testing.T) {
		t.Parallel()
		missing := map[string]File{"agent-plugin.yaml": tree["agent-plugin.yaml"], "skills/advocate/SKILL.md": tree["skills/advocate/SKILL.md"]}
		err := build(t, missing, map[string]bool{"guidance.md": true})
		assertUnpublishable(t, err, "Git ignores \"guidance.md\"", "stop declaring it")
		if strings.Contains(err.Error(), "commit the declared file") || strings.Contains(err.Error(), "commit every") {
			t.Fatalf("advice tells the operator to commit an ignored file: %v", err)
		}
	})
}

func assertUnpublishable(t *testing.T, err error, wants ...string) {
	t.Helper()
	var refusal *Error
	if err == nil || !errors.As(err, &refusal) || refusal.Code != CodeUnpublishable {
		t.Fatalf("error = %v, want %s", err, CodeUnpublishable)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err.Error(), want)
		}
	}
}

func mustLoad(t *testing.T, root string) manifest.Manifest {
	t.Helper()
	value, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// packageRepository creates a real repository with a package at the requested
// subdirectory; all tests below exercise the production Git implementation.
func packageRepository(t *testing.T, nested bool) (string, string) {
	t.Helper()
	repo := filepath.Join(isolatedGit(t), "repo")
	initRepository(t, repo)
	for key, value := range map[string]string{"user.name": "Fixture", "user.email": "fixture@example.invalid", "commit.gpgsign": "false", "tag.gpgsign": "false"} {
		runTestGit(t, repo, "config", key, value)
	}
	root := repo
	if nested {
		root = filepath.Join(repo, "package")
	}
	writeHostileFile(t, root, manifest.Filename, skillManifestYAML("guidance.md"))
	writeHostileFile(t, root, "guidance.md", "# Guidance\n")
	writeHostileFile(t, root, "skills/advocate/SKILL.md", "---\nname: advocate\ndescription: Advocate.\n---\n# Skill\n")
	return repo, root
}

func tagPackage(t *testing.T, repo string) {
	t.Helper()
	runTestGit(t, repo, "add", ".")
	runTestGit(t, repo, "commit", "-qm", "Package fixture")
	runTestGit(t, repo, "tag", "-f", "v1.2.3")
}

func gitBytes(t *testing.T, root string, args ...string) []byte {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return output
}

func listedPackage(t *testing.T, root string) (manifest.Manifest, []string) {
	t.Helper()
	list, err := PackageListing(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	value, files, err := manifest.LoadListed(root, list)
	if err != nil {
		t.Fatal(err)
	}
	return value, files
}

func assertInventoryArchive(t *testing.T, repo, root string, want []string) {
	t.Helper()
	index := gitBytes(t, repo, "rev-parse", "--git-path", "index")
	indexPath := strings.TrimSpace(string(index))
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(repo, indexPath)
	}
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	_, files := listedPackage(t, root)
	slices.Sort(want)
	if !slices.Equal(files, want) {
		t.Fatalf("inventory = %+q, want %+q", files, want)
	}
	prepared, err := NewBuilder("test").Prepare(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	names := tarEntryNames(t, prepared.Assets.Archive.Bytes)
	prefix, err := filepath.Rel(repo, root)
	if err != nil {
		t.Fatal(err)
	}
	if prefix == "." {
		prefix = ""
	} else {
		prefix = filepath.ToSlash(prefix) + "/"
	}
	for i := range names {
		archiveName := names[i]
		names[i] = strings.TrimPrefix(archiveName, "plugin-1.2.3/")
		committed := gitBytes(t, repo, "show", "HEAD:"+prefix+names[i])
		if !bytes.Equal(tarFileContent(t, prepared.Assets.Archive.Bytes, archiveName), committed) {
			t.Fatalf("archive bytes differ for %q", names[i])
		}
	}
	if !slices.Equal(names, files) {
		t.Fatalf("archive = %+q, validation = %+q", names, files)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("validation/publication changed Git index")
	}
}

func TestDirectGitInventoryAndArchiveEquality(t *testing.T) {
	repo, root := packageRepository(t, true)
	writeHostileFile(t, repo, ".gitignore", "*.parent\n*.pyc\n!package/skills/advocate/keep.pyc\n")
	writeHostileFile(t, root, ".gitignore", "*.local\ncache/\n")
	writeHostileFile(t, repo, ".git/info/exclude", "*.excluded\n")
	global := filepath.Join(t.TempDir(), "global-ignore")
	if err := os.WriteFile(global, []byte("*.global\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "config", "core.excludesFile", global)
	writeHostileFile(t, root, "skills/advocate/nested/SKILL.md", "---\nname: nested\ndescription: Nested.\n---\n# Nested\n")
	body := strings.Replace(skillManifestYAML("guidance.md"), "  rules:", "    - id: nested\n      path: skills/advocate/nested\n  rules:", 1)
	writeHostileFile(t, root, manifest.Filename, body)
	want := []string{manifest.Filename, "guidance.md", "skills/advocate/SKILL.md", "skills/advocate/nested/SKILL.md"}
	for _, leaf := range []string{"worker.py", "keep.pyc", "pinned.pyc", "space name", "notes[1].md", ":(glob)*.md", "nested/data"} {
		name := "skills/advocate/" + leaf
		writeHostileFile(t, root, name, "content of "+leaf+"\n")
		want = append(want, name)
	}
	runTestGit(t, repo, "add", "-f", "package/skills/advocate/pinned.pyc")
	writeHostileFile(t, repo, "sibling/skills/advocate/extra.md", "unrelated")
	writeHostileFile(t, root, "unrelated.md", "unrelated")
	tagPackage(t, repo)
	for _, leaf := range []string{"drop.pyc", "drop.parent", "drop.local", "drop.excluded", "drop.global"} {
		writeHostileFile(t, root, "skills/advocate/"+leaf, "ignored")
	}
	writeHostileFile(t, root, "skills/advocate/nested/.gitignore", "*.cache\n")
	// A nested ignore file is package content when tracked.
	runTestGit(t, repo, "add", "package/skills/advocate/nested/.gitignore")
	tagPackage(t, repo)
	want = append(want, "skills/advocate/nested/.gitignore")
	writeHostileFile(t, root, "skills/advocate/nested/drop.cache", "ignored")
	if err := os.MkdirAll(filepath.Join(root, "skills/advocate/cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(root, "skills/advocate/cache/link")); err != nil {
		t.Fatal(err)
	}
	// Compare against real Git independently of the application listing callback.
	raw := gitBytes(t, root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "agent-plugin.yaml", "guidance.md", "skills/advocate")
	gitNames := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	slices.Sort(gitNames)
	slices.Sort(want)
	if !slices.Equal(gitNames, want) {
		t.Fatalf("Git fixture = %+q, want %+q", gitNames, want)
	}
	assertInventoryArchive(t, repo, root, want)
}

func TestDeletedTrackedSupportFileIsDiagnosed(t *testing.T) {
	repo, root := packageRepository(t, false)
	name := "skills/advocate/deleted.md"
	writeHostileFile(t, root, name, "tracked")
	tagPackage(t, repo)
	if err := os.Remove(filepath.Join(root, name)); err != nil {
		t.Fatal(err)
	}
	list, err := PackageListing(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = manifest.LoadListed(root, list)
	assertManifestCode(t, err, manifest.CodePathNotFound)
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("missing path diagnostic: %v", err)
	}
}

func TestCommittedUnsupportedModesHavePathAdvice(t *testing.T) {
	for _, test := range []struct {
		name, path, mode string
		directory        bool
	}{
		{"skill root symlink", "skills/advocate", "120000", true},
		{"skill root gitlink", "skills/advocate", "160000", true},
		{"skill root blob", "skills/advocate", "100644", true},
		{"support symlink", "skills/advocate/link", "120000", false},
		{"support gitlink", "skills/advocate/submodule", "160000", false},
		{"declared directory", "guidance.md", "040000", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo, root := packageRepository(t, false)
			tagPackage(t, repo)
			value := mustLoad(t, root)
			oid := strings.TrimSpace(string(gitBytes(t, repo, "rev-parse", "HEAD:guidance.md")))
			if test.mode == "160000" {
				oid = strings.TrimSpace(string(gitBytes(t, repo, "rev-parse", "HEAD")))
			}
			if test.mode == "040000" {
				runTestGit(t, repo, "rm", "--cached", "guidance.md")
				if err := os.Remove(filepath.Join(root, "guidance.md")); err != nil {
					t.Fatal(err)
				}
				writeHostileFile(t, root, "guidance.md/child", "directory")
				runTestGit(t, repo, "add", ".")
			} else {
				if test.directory {
					runTestGit(t, repo, "rm", "-r", "--cached", "skills/advocate")
				}
				runTestGit(t, repo, "update-index", "--add", "--cacheinfo", test.mode, oid, test.path)
			}
			runTestGit(t, repo, "commit", "-qm", "Unsupported selected entry")
			commit := strings.TrimSpace(string(gitBytes(t, repo, "rev-parse", "HEAD")))
			_, err := readTreeFiles(context.Background(), root, Identity{Tag: "fixture", Commit: commit}, value, commandGitSource{})
			assertUnpublishable(t, err, test.path, test.mode, "regular file")
		})
	}
}

func TestMissingOrCorruptSelectedBlobIsGitAccessFailure(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt-%t", corrupt), func(t *testing.T) {
			repo, root := packageRepository(t, false)
			tagPackage(t, repo)
			oid := strings.TrimSpace(string(gitBytes(t, repo, "rev-parse", "HEAD:guidance.md")))
			object := filepath.Join(repo, ".git", "objects", oid[:2], oid[2:])
			if corrupt {
				if err := os.Chmod(object, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(object, []byte("invalid object"), 0o644); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(object); err != nil {
				t.Fatal(err)
			}
			_, err := NewBuilder("test").Prepare(context.Background(), root)
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != CodeGitAccess {
				t.Fatalf("error = %v, want Git access failure", err)
			}
			if !strings.Contains(err.Error(), "guidance.md") || !strings.Contains(err.Error(), "git fsck") || strings.Contains(err.Error(), "regular file") {
				t.Fatalf("incorrect blob advice: %v", err)
			}
			var gitFailure *gitError
			if !errors.As(err, &gitFailure) || gitFailure.exitCode == 0 || gitFailure.diagnostic == "" || gitFailure.cause == nil {
				t.Fatalf("lost Git diagnostic/cause: %v", err)
			}
		})
	}
}

type failingIgnoreSource struct {
	commandGitSource
	failure error
}

func (source failingIgnoreSource) Ignored(context.Context, string, string) (bool, error) {
	return false, source.failure
}

func TestAbsentDeclarationPropagatesIgnoreFailureAndCancellation(t *testing.T) {
	repo, root := packageRepository(t, false)
	tagPackage(t, repo)
	value := mustLoad(t, root)
	runTestGit(t, repo, "rm", "--cached", "guidance.md")
	runTestGit(t, repo, "commit", "-qm", "Remove declared entry from tree")
	identity := Identity{Tag: "fixture", Commit: strings.TrimSpace(string(gitBytes(t, repo, "rev-parse", "HEAD")))}
	cause := &gitError{exitCode: 128, diagnostic: "cannot read ignore configuration", cause: os.ErrPermission}
	_, err := readTreeFiles(context.Background(), root, identity, value, failingIgnoreSource{failure: cause})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != CodeGitAccess || !errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "commit the declared") {
		t.Fatalf("ignore failure = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = readTreeFiles(ctx, root, identity, value, commandGitSource{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

type movingTagSource struct {
	commandGitSource
	t           *testing.T
	destination string
}

func (source movingTagSource) TagsAtHead(ctx context.Context, root string) ([]string, error) {
	tags, err := source.commandGitSource.TagsAtHead(ctx, root)
	if err == nil {
		runTestGit(source.t, root, "tag", "-f", tags[0], source.destination)
	}
	return tags, err
}

func TestPublicationReadsResolvedCommitWhenTagMoves(t *testing.T) {
	repo, root := packageRepository(t, false)
	tagPackage(t, repo)
	old := strings.TrimSpace(string(gitBytes(t, repo, "rev-parse", "HEAD")))
	writeHostileFile(t, root, "guidance.md", "# New committed guidance\n")
	runTestGit(t, repo, "add", ".")
	runTestGit(t, repo, "commit", "-qm", "New guidance")
	runTestGit(t, repo, "tag", "-f", "v1.2.3")
	current := strings.TrimSpace(string(gitBytes(t, repo, "rev-parse", "HEAD")))
	prepared, err := newBuilder(movingTagSource{t: t, destination: old}, NewGate(), "test").Prepare(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Identity.Commit != current || string(tarFileContent(t, prepared.Assets.Archive.Bytes, "plugin-1.2.3/guidance.md")) != "# New committed guidance\n" {
		t.Fatal("publication followed mutable tag instead of resolved commit")
	}
}

func TestPublicationIgnoresReplacementObjects(t *testing.T) {
	for _, kind := range []string{"blob", "tree", "commit"} {
		t.Run(kind, func(t *testing.T) {
			// The factory may disable replacements globally. Ordinary Git below
			// must see them to prove that production enforces its own policy.
			t.Setenv("GIT_NO_REPLACE_OBJECTS", "")
			if err := os.Unsetenv("GIT_NO_REPLACE_OBJECTS"); err != nil {
				t.Fatal(err)
			}
			repo, root := packageRepository(t, false)
			const name = "skills/advocate/support.txt"
			const original = "selected committed bytes\n"
			const replacement = "replacement bytes\n"
			const extra = "skills/advocate/replacement-only.txt"
			writeHostileFile(t, root, name, original)
			tagPackage(t, repo)
			commit := strings.TrimSpace(string(gitBytes(t, repo, "rev-parse", "HEAD")))
			var from, to string
			if kind == "blob" {
				from = strings.TrimSpace(string(gitBytes(t, repo, "rev-parse", "HEAD:"+name)))
				outside := filepath.Join(t.TempDir(), "replacement.txt")
				if err := os.WriteFile(outside, []byte(replacement), 0o644); err != nil {
					t.Fatal(err)
				}
				to = strings.TrimSpace(string(gitBytes(t, repo, "hash-object", "-w", outside)))
			} else {
				writeHostileFile(t, root, name, replacement)
				writeHostileFile(t, root, extra, "replacement-only member\n")
				runTestGit(t, repo, "add", ".")
				runTestGit(t, repo, "commit", "-qm", "Replacement fixture")
				selector := "HEAD"
				if kind == "tree" {
					selector += "^{tree}"
				}
				to = strings.TrimSpace(string(gitBytes(t, repo, "rev-parse", selector)))
				runTestGit(t, repo, "checkout", "--detach", commit)
				from = strings.TrimSpace(string(gitBytes(t, repo, "rev-parse", selector)))
			}
			runTestGit(t, repo, "replace", from, to)
			if got := string(gitBytes(t, repo, "show", "HEAD:"+name)); got != replacement {
				t.Fatalf("ordinary Git did not apply %s replacement: %q", kind, got)
			}
			if kind != "blob" && !strings.Contains(string(gitBytes(t, repo, "ls-tree", "-r", "HEAD")), extra) {
				t.Fatal("ordinary Git did not expose replacement-only member")
			}
			prepared, err := NewBuilder("test").Prepare(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Identity.Commit != commit || prepared.Identity.Tag != "v1.2.3" {
				t.Fatalf("replaced identity: %+v", prepared.Identity)
			}
			if got := string(tarFileContent(t, prepared.Assets.Archive.Bytes, "plugin-1.2.3/"+name)); got != original {
				t.Fatalf("archive used replaced %s: %q", kind, got)
			}
			if slices.Contains(tarEntryNames(t, prepared.Assets.Archive.Bytes), "plugin-1.2.3/"+extra) {
				t.Fatal("archive contains replacement-only member")
			}
			if kind != "blob" {
				value := prepared.Manifest
				value.Artifacts.Rules[0].Path = extra
				_, err := readTreeFiles(context.Background(), root, prepared.Identity, value, commandGitSource{})
				var failure *Error
				if !errors.As(err, &failure) || failure.Code != CodeUnpublishable || !strings.Contains(err.Error(), extra) {
					t.Fatalf("replacement-only declaration = %v", err)
				}
			}
		})
	}
}

func TestMalformedGitRecordsAreOperationalFailures(t *testing.T) {
	for _, output := range [][]byte{[]byte("unterminated"), []byte{0}} {
		if _, err := nulPaths(output); err == nil {
			t.Fatalf("accepted malformed records %q", output)
		}
	}
	for _, fields := range [][]string{{"100644", "tree", strings.Repeat("a", 40)}, {"100644", "blob", "not-an-oid"}, {"999999", "blob", strings.Repeat("a", 40)}} {
		if validTreeRecord(fields) {
			t.Fatalf("accepted malformed metadata %v", fields)
		}
	}
}

func TestLiteralControlNamesReachInventoryAndCommittedArchive(t *testing.T) {
	repo, root := packageRepository(t, false)
	for _, name := range []string{"tab\tname", "line\nname"} {
		writeHostileFile(t, root, "skills/advocate/"+name, "literal "+name)
	}
	tagPackage(t, repo)
	value, files := listedPackage(t, root)
	identity := Identity{Tag: "v1.2.3", Commit: strings.TrimSpace(string(gitBytes(t, repo, "rev-parse", "HEAD")))}
	selected, err := readTreeFiles(context.Background(), root, identity, value, commandGitSource{})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := BuildArchive("plugin", "1.2.3", selected)
	if err != nil {
		t.Fatal(err)
	}
	names := tarEntryNames(t, archive.Bytes)
	for i := range names {
		names[i] = strings.TrimPrefix(names[i], "plugin-1.2.3/")
	}
	if !slices.Equal(names, files) {
		t.Fatalf("literal names = %+q, inventory = %+q", names, files)
	}
	for _, name := range []string{"tab\tname", "line\nname"} {
		if !slices.Contains(files, "skills/advocate/"+name) || string(tarFileContent(t, archive.Bytes, "plugin-1.2.3/skills/advocate/"+name)) != "literal "+name {
			t.Fatalf("lost literal name %q", name)
		}
	}
	// Existing adapter safety still rejects control characters during realization.
	_, err = NewBuilder("test").Prepare(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "control character") {
		t.Fatalf("realization gate = %v", err)
	}
}

func TestExplicitGitSelectorsAreLiteral(t *testing.T) {
	for _, name := range []string{"notes[1].md", ":(glob)*.md", "space name.md"} {
		t.Run(name, func(t *testing.T) {
			repo, root := packageRepository(t, false)
			writeHostileFile(t, root, name, "# Literal rule\n")
			writeHostileFile(t, root, "notes1.md", "unrelated pattern match")
			writeHostileFile(t, root, manifest.Filename, skillManifestYAML(fmt.Sprintf("%q", name)))
			tagPackage(t, repo)
			assertInventoryArchive(t, repo, root, []string{manifest.Filename, name, "skills/advocate/SKILL.md"})
		})
	}
}

func TestPreparationPreservesIndexWhenGitWouldRefreshStatData(t *testing.T) {
	repo, root := packageRepository(t, false)
	tagPackage(t, repo)
	stamp := time.Unix(946684800, 0)
	if err := os.Chtimes(filepath.Join(root, "guidance.md"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	assertInventoryArchive(t, repo, root, []string{manifest.Filename, "guidance.md", "skills/advocate/SKILL.md"})
}

func TestIgnoredRequiredSkillDocumentWithoutCommittedSkillTree(t *testing.T) {
	repo, root := packageRepository(t, false)
	writeHostileFile(t, root, ".gitignore", "SKILL.md\n")
	tagPackage(t, repo)
	_, files := listedPackage(t, root)
	if !slices.Contains(files, "skills/advocate/SKILL.md") {
		t.Fatalf("required skill document omitted: %v", files)
	}
	_, err := NewBuilder("test").Prepare(context.Background(), root)
	assertUnpublishable(t, err, "Git ignores \"skills/advocate/SKILL.md\"", "stop declaring it")
	if strings.Contains(err.Error(), "commit the declared file") {
		t.Fatalf("ignored document received commit advice: %v", err)
	}
}
