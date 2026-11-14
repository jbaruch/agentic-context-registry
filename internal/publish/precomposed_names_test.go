package publish

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// macOS stores these names decomposed (what the walk returns) while Git, with
// core.precomposeunicode=true, lists them precomposed. Both spellings are built
// from escapes so editors and file systems cannot rewrite the fixture.
const (
	composedDirectory   = "skills/advocate/caf\u00e9"
	decomposedDirectory = "skills/advocate/cafe\u0301"
	composedNaive       = "skills/advocate/na\u00efve.py"
	decomposedNaive     = "skills/advocate/nai\u0308ve.py"
	composedResume      = composedDirectory + "/r\u00e9sum\u00e9.py"
	decomposedResume    = decomposedDirectory + "/re\u0301sume\u0301.py"
)

// decomposedPackage writes the package onto disk the way macOS does: every
// non-ASCII name decomposed.
func decomposedPackage(t *testing.T, ignoredBytecode bool) string {
	t.Helper()
	root := t.TempDir()
	writeHostileFile(t, root, manifest.Filename, skillManifestYAML("guidance.md"))
	writeHostileFile(t, root, "guidance.md", "# Guidance\n")
	writeHostileFile(t, root, "skills/advocate/SKILL.md", "---\nname: advocate\ndescription: Advocate.\n---\n# Skill\n")
	writeHostileFile(t, root, decomposedNaive, "print('naive')\n")
	writeHostileFile(t, root, decomposedResume, "print('resume')\n")
	if ignoredBytecode {
		writeHostileFile(t, root, "skills/advocate/naïve.pyc", "bytecode")
		writeHostileFile(t, root, decomposedDirectory+"/résumé.pyc", "bytecode")
	}
	return root
}

// precomposedRepository commits the decomposed package with
// core.precomposeunicode=true. On macOS Git then lists precomposed names for
// files the walk finds decomposed; elsewhere Git keeps the bytes it was given,
// and the same assertions hold with exact matches.
func precomposedRepository(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root := decomposedPackage(t, false)
	writeHostileFile(t, root, ".gitignore", "*.pyc\n")
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", root,
			"-c", "user.name=Fixture", "-c", "user.email=fixture@example.test",
			"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s (%v)", args, output, err)
		}
	}
	git("init", "--template=", "-q")
	git("config", "core.precomposeunicode", "true")
	git("add", ".")
	git("commit", "-qm", "Create package")
	git("tag", "v1.2.3")
	writeHostileFile(t, root, "skills/advocate/naïve.pyc", "bytecode")
	writeHostileFile(t, root, decomposedDirectory+"/résumé.pyc", "bytecode")
	return root
}

func TestCommandGitSourceKeepsExactGitSpellingThroughValidationAndArchive(t *testing.T) {
	root := precomposedRepository(t)
	list, err := PackageListing(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	_, files, err := manifest.LoadListed(root, list)
	if err != nil {
		t.Fatal(err)
	}
	want, err := (commandGitSource{}).ListWorktree(context.Background(), root, ".")
	if err != nil {
		t.Fatal(err)
	}
	want = slices.DeleteFunc(want, func(name string) bool { return name == ".gitignore" })
	slices.Sort(want)
	if !slices.Equal(files, want) {
		t.Fatalf("files = %+q, Git = %+q", files, want)
	}
	prepared, err := NewBuilder("test").Prepare(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	names := tarEntryNames(t, prepared.Assets.Archive.Bytes)
	for i := range names {
		names[i] = strings.TrimPrefix(names[i], "plugin-1.2.3/")
	}
	if !slices.Equal(names, want) {
		t.Fatalf("archive = %+q, Git = %+q", names, want)
	}
}

func TestDistinctUnicodeNamesComeOnlyFromGit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires normalization-sensitive Linux filesystem")
	}
	for _, bothTracked := range []bool{false, true} {
		t.Run(fmt.Sprintf("both-tracked-%t", bothTracked), func(t *testing.T) {
			repo, root := packageRepository(t, false)
			writeHostileFile(t, root, composedNaive, "composed\n")
			want := []string{manifest.Filename, "guidance.md", "skills/advocate/SKILL.md", composedNaive}
			if bothTracked {
				writeHostileFile(t, root, decomposedNaive, "decomposed\n")
				want = append(want, decomposedNaive)
			} else {
				writeHostileFile(t, root, ".gitignore", decomposedNaive+"\n")
			}
			tagPackage(t, repo)
			if !bothTracked {
				if err := os.Symlink("absent", filepath.Join(root, decomposedNaive)); err != nil {
					t.Fatal(err)
				}
			}
			assertInventoryArchive(t, repo, root, want)
		})
	}
}

func TestMacOSDiskSpellingDoesNotChangeGitInventory(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires macOS filesystem spelling behavior")
	}
	repo, root := packageRepository(t, false)
	runTestGit(t, repo, "config", "core.precomposeunicode", "true")
	runTestGit(t, repo, "config", "core.ignorecase", "true")
	// Both the declared skill root and the explicit file use NFD. Git performs
	// selection and returns its own NFC spelling; the application never folds it.
	skillRoot := "skills/cafe\u0301[1]"
	rule := "re\u0301sume\u0301[1].md"
	body := strings.ReplaceAll(skillManifestYAML(rule), "skills/advocate", skillRoot)
	writeHostileFile(t, root, manifest.Filename, body)
	writeHostileFile(t, root, rule, "# Rule\n")
	writeHostileFile(t, root, skillRoot+"/SKILL.md", "---\nname: cafe\ndescription: Cafe.\n---\n# Cafe\n")
	writeHostileFile(t, root, skillRoot+"/nai\u0308ve.py", "unicode\n")
	writeHostileFile(t, root, skillRoot+"/Mixed.py", "case rename\n")
	tagPackage(t, repo)
	if err := os.Rename(filepath.Join(root, skillRoot, "Mixed.py"), filepath.Join(root, skillRoot, "mixed.py")); err != nil {
		t.Fatal(err)
	}
	if status := gitBytes(t, root, "status", "--porcelain"); len(status) != 0 {
		t.Fatalf("case-only rename must be Git-clean: %s", status)
	}
	// Inventory from Git's literal selectors is the oracle for exact spelling.
	raw := gitBytes(t, root, "--literal-pathspecs", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", manifest.Filename, rule, skillRoot)
	want := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	if len(want) != 5 {
		t.Fatalf("native literal selection lost files: %+q", want)
	}
	assertInventoryArchive(t, repo, root, want)
}
