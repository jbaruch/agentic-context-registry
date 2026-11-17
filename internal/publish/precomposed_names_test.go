package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
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
			} else {
				writeHostileFile(t, root, ".gitignore", decomposedNaive+"\n")
			}
			tagPackage(t, repo)
			if !bothTracked {
				if err := os.Symlink("absent", filepath.Join(root, decomposedNaive)); err != nil {
					t.Fatal(err)
				}
				assertInventoryArchive(t, repo, root, want)
				return
			}
			list, err := PackageListing(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			_, _, loadErr := manifest.LoadListed(root, list)
			_, prepareErr := NewBuilder("test").Prepare(context.Background(), root)
			wantMessage := fmt.Sprintf("distribution path %q occupies %q, which conflicts with %q under NFC normalization and Unicode simple folding (including file/directory occupancy); rename the conflicting or unsafe source path and update its declarations before retrying", composedNaive, composedNaive, decomposedNaive)
			for _, err := range []error{loadErr, prepareErr} {
				var failure *manifest.ValidationErrors
				if !errors.As(err, &failure) || len(failure.Issues) != 1 || failure.Issues[0].Code != manifest.CodeInvalidPath || failure.Issues[0].Field != composedNaive || failure.Issues[0].Message != wantMessage {
					t.Fatalf("tracked Unicode collision refusal = %v; want invalid_path at %q: %s", err, composedNaive, wantMessage)
				}
			}
		})
	}
}

func TestMacOSRefusesMismatchedDeclarations(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires macOS filesystem spelling behavior")
	}
	for _, kind := range []string{"rule", "skill"} {
		t.Run(kind, func(t *testing.T) {
			repo, root := packageRepository(t, false)
			runTestGit(t, repo, "config", "core.precomposeunicode", "true")
			rule, skill := "r\u00e9sum\u00e9[1].md", "skills/caf\u00e9[1]"
			declared, stored := "re\u0301sume\u0301[1].md", rule
			if kind == "rule" {
				rule = declared
			} else {
				declared, stored = "skills/cafe\u0301[1]", skill
				skill = declared
			}
			body := strings.ReplaceAll(skillManifestYAML(rule), "skills/advocate", skill)
			writeHostileFile(t, root, manifest.Filename, body)
			writeHostileFile(t, root, rule, "# Rule\n")
			writeHostileFile(t, root, skill+"/SKILL.md", "---\nname: cafe\ndescription: Cafe.\n---\n# Cafe\n")
			tagPackage(t, repo)
			listedPackage(t, root) // Local aliases can validate despite incompatible declarations.
			_, err := NewBuilder("test").Prepare(context.Background(), root)
			assertSpellingRefusal(t, err, declared, stored)

			body = strings.ReplaceAll(body, declared, stored)
			writeHostileFile(t, root, manifest.Filename, body)
			tagPackage(t, repo)
			prepared, err := NewBuilder("test").Prepare(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if got := tarFileContent(t, prepared.Assets.Archive.Bytes, "plugin-1.2.3/"+manifest.Filename); !bytes.Equal(got, []byte(body)) {
				t.Fatal("publication changed the corrected committed manifest")
			}
		})
	}
}

func TestCorrectUnicodeDeclarationsExtractAndValidate(t *testing.T) {
	for _, nfd := range []bool{false, true} {
		t.Run(fmt.Sprintf("nfd-%t", nfd), func(t *testing.T) {
			if nfd && runtime.GOOS != "linux" {
				t.Skip("requires Git to retain distinct NFD bytes on Linux")
			}
			repo, root := packageRepository(t, false)
			rule, skill := "r\u00e9sum\u00e9[1].md", "skills/caf\u00e9[1]"
			if nfd {
				rule, skill = "re\u0301sume\u0301[1].md", "skills/cafe\u0301[1]"
			}
			body := strings.ReplaceAll(skillManifestYAML(rule), "skills/advocate", skill)
			writeHostileFile(t, root, manifest.Filename, body)
			writeHostileFile(t, root, rule, "# Rule\n")
			writeHostileFile(t, root, skill+"/SKILL.md", "---\nname: cafe\ndescription: Cafe.\n---\n# Cafe\n")
			tagPackage(t, repo)
			prepared, err := NewBuilder("test").Prepare(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			destination := t.TempDir()
			if err := dependency.ExtractPackageArchive(prepared.Assets.Archive.Bytes, destination); err != nil {
				t.Fatal(err)
			}
			if _, err := manifest.Load(destination); err != nil {
				t.Fatalf("consumer validation: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(destination, manifest.Filename))
			if err != nil || !bytes.Equal(got, []byte(body)) {
				t.Fatalf("committed manifest changed: %+q, %v", got, err)
			}
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
	// Declarations match Git's NFC records; disk aliases and support files may
	// use NFD. The application preserves Git's names without folding them.
	skillRoot := "skills/caf\u00e9[1]"
	rule := "r\u00e9sum\u00e9[1].md"
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

// spellingSource models Git's native selector aliases on every test host.
type spellingSource struct {
	fakeGitSource
	t                *testing.T
	declared, stored string
	mode             string
}

func (source spellingSource) Tree(ctx context.Context, root, commit, relative string, recursive bool) ([]treeEntry, error) {
	if relative == source.declared {
		if recursive {
			source.t.Fatal("recursed before refusing the declaration spelling")
		}
		return []treeEntry{{Path: source.stored, Mode: source.mode, OID: "unused"}}, nil
	}
	return source.fakeGitSource.Tree(ctx, root, commit, relative, recursive)
}

func (source spellingSource) Blob(context.Context, string, string) ([]byte, error) {
	source.t.Fatal("read a blob before refusing the declaration spelling")
	return nil, nil
}

func TestCommittedDeclarationsRequireExactSpelling(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"rule", "script", "hook", "skill", "SKILL.md"} {
		t.Run(kind, func(t *testing.T) {
			declared, stored := "cafe\u0301", "caf\u00e9"
			value := manifest.Manifest{}
			switch kind {
			case "rule":
				value.Artifacts.Rules = []manifest.RuleArtifact{{Path: declared}}
			case "script":
				value.Artifacts.Scripts = []manifest.ScriptArtifact{{Path: declared}}
			case "hook":
				value.Artifacts.Hooks = []manifest.HookArtifact{{Path: declared}}
			case "skill":
				value.Artifacts.Skills = []manifest.SkillArtifact{{Path: declared}}
			case "SKILL.md":
				value.Artifacts.Skills = []manifest.SkillArtifact{{Path: "skills/cafe"}}
				declared, stored = "skills/cafe/SKILL.md", "skills/cafe/skill.md"
			}
			source := spellingSource{t: t, declared: declared, stored: stored, mode: "100644", fakeGitSource: fakeGitSource{files: map[string]File{
				manifest.Filename:      {Path: manifest.Filename},
				"skills/cafe/SKILL.md": {Path: "skills/cafe/SKILL.md"},
			}}}
			if kind == "skill" {
				source.mode = "040000"
			}
			_, err := readTreeFiles(context.Background(), ".", Identity{}, value, &source)
			assertSpellingRefusal(t, err, declared, stored)
		})
	}
}

func assertSpellingRefusal(t *testing.T, err error, declared, stored string) {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != CodeUnpublishable {
		t.Fatalf("spelling mismatch = %v, want unpublishable_path", err)
	}
	for _, want := range []string{fmt.Sprintf("%+q", declared), fmt.Sprintf("%+q", stored), "correct the declaration", "agent-plugin.yaml", "commit and retag"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("spelling refusal %q lacks %q", err, want)
		}
	}
}
