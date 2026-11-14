package publish

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"

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

func TestPathFilterMatchesDecomposedWalkNamesToGitsPrecomposedPaths(t *testing.T) {
	t.Parallel()

	filter := newPathFilter([]string{composedNaive, composedResume, "skills/advocate/SKILL.md"})
	for _, test := range []struct {
		name  string
		isDir bool
		want  string
		keep  bool
	}{
		{decomposedNaive, false, composedNaive, true},
		{composedNaive, false, composedNaive, true},
		{decomposedResume, false, composedResume, true},
		{decomposedDirectory, true, composedDirectory, true},
		{"skills/advocate/naïve.pyc", false, "", false},
		{decomposedDirectory + "/résumé.pyc", false, "", false},
		{"skills/advocate/caché", true, "", false},
	} {
		got, keep := filter(test.name, test.isDir)
		if got != test.want || keep != test.keep {
			t.Errorf("filter(%+q, dir=%t) = %+q, %t; want %+q, %t", test.name, test.isDir, got, keep, test.want, test.keep)
		}
	}
}

func TestPathFilterKeepsNamesApartWhereOnlyNormalizationDiffers(t *testing.T) {
	t.Parallel()

	// A file system that keeps both spellings (Linux) lists both; each is kept
	// under its own spelling.
	filter := newPathFilter([]string{composedNaive, decomposedNaive})
	for _, name := range []string{composedNaive, decomposedNaive} {
		if got, keep := filter(name, false); !keep || got != name {
			t.Errorf("filter(%+q) = %+q, %t; want the name unchanged", name, got, keep)
		}
	}
}

func TestSamePathIgnoresUnicodeNormalization(t *testing.T) {
	t.Parallel()

	if !samePath(composedNaive, decomposedNaive) || !samePath(composedNaive, composedNaive) {
		t.Fatal("samePath rejected two spellings of one name")
	}
	if samePath(composedNaive, "skills/advocate/naive.py") || samePath(composedNaive, composedResume) {
		t.Fatal("samePath accepted different names")
	}
}

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

func TestPackageFilesFollowGitsPrecomposedPathsForDecomposedDiskNames(t *testing.T) {
	t.Parallel()

	root := decomposedPackage(t, true)
	source := &fakeGitSource{inside: true, visible: []string{
		"agent-plugin.yaml", "guidance.md", "skills/advocate/SKILL.md", composedNaive, composedResume,
	}}
	filter, err := packageFilter(context.Background(), root, source)
	if err != nil {
		t.Fatal(err)
	}
	value, err := manifest.LoadFiltered(root, filter)
	if err != nil {
		t.Fatal(err)
	}
	files, err := manifest.PackageFilesFiltered(root, value, filter)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"agent-plugin.yaml", "guidance.md", "skills/advocate/SKILL.md", composedResume, composedNaive}
	slices.Sort(want)
	if !slices.Equal(files, want) {
		t.Fatalf("files = %+q, want %+q", files, want)
	}
}

// strictTree is a tagged tree that answers only for the exact spelling Git
// stores, as the real tree does for the paths the filter returns.
func strictTree(manifestContent string) map[string]File {
	skill := "---\nname: advocate\ndescription: Advocate.\n---\n# Skill\n"
	entries := map[string]string{
		"agent-plugin.yaml":        manifestContent,
		"guidance.md":              "# Guidance\n",
		"skills/advocate/SKILL.md": skill,
		composedNaive:              "print('naive')\n",
		composedResume:             "print('resume')\n",
	}
	tree := map[string]File{}
	for name, content := range entries {
		tree[name] = File{Path: name, Mode: 0o644, Content: []byte(content)}
	}
	return tree
}

func TestBuilderPublishesPrecomposedPathsForDecomposedDiskNames(t *testing.T) {
	t.Parallel()

	root := decomposedPackage(t, true)
	source := &fakeGitSource{
		clean: true, head: strings.Repeat("a", 40), tags: []string{"v1.2.3"}, inside: true,
		files:   strictTree(skillManifestYAML("guidance.md")),
		visible: []string{"agent-plugin.yaml", "guidance.md", "skills/advocate/SKILL.md", composedNaive, composedResume},
	}
	prepared, err := newBuilder(source, NewGate(), "acr test").Prepare(context.Background(), root)
	if err != nil {
		t.Fatalf("tracked precomposed files were refused or dropped: %v", err)
	}
	names := tarEntryNames(t, prepared.Assets.Archive.Bytes)
	for _, want := range []string{composedNaive, composedResume} {
		if !slices.ContainsFunc(names, func(name string) bool { return strings.HasSuffix(name, "/"+want) }) {
			t.Errorf("archive entries %+q lack %+q", names, want)
		}
	}
	for _, name := range names {
		if strings.HasSuffix(name, ".pyc") {
			t.Errorf("archive holds ignored bytecode %q", name)
		}
	}
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

func TestCommandGitSourceKeepsPrecomposedSkillFilesThroughValidateAndTagTree(t *testing.T) {
	root := precomposedRepository(t)
	filter, err := PackageFilter(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	value, err := manifest.LoadFiltered(root, filter)
	if err != nil {
		t.Fatal(err)
	}
	files, err := manifest.PackageFilesFiltered(root, value, filter)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, name := range files {
		got = append(got, norm.NFC.String(name))
	}
	want := []string{"agent-plugin.yaml", "guidance.md", "skills/advocate/SKILL.md", composedResume, composedNaive}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("files = %+q, want %+q up to normalization", files, want)
	}

	source := commandGitSource{}
	for _, name := range files {
		file, err := source.FileAt(context.Background(), root, "v1.2.3", name)
		if err != nil {
			t.Fatalf("tag tree refused %+q: %v", name, err)
		}
		if file.Path != name {
			t.Fatalf("FileAt(%+q).Path = %+q", name, file.Path)
		}
	}
	// The decomposed spelling the walk finds reaches the same tree entry.
	file, err := source.FileAt(context.Background(), root, "v1.2.3", decomposedNaive)
	if err != nil {
		t.Fatalf("FileAt(decomposed) refused a tracked file: %v", err)
	}
	var gitSpelling string
	for _, name := range files {
		if samePath(name, decomposedNaive) {
			gitSpelling = name
		}
	}
	if file.Path != gitSpelling || string(file.Content) != "print('naive')\n" {
		t.Fatalf("FileAt(decomposed) = %+v, want Git path %+q and the tracked content", file, gitSpelling)
	}

	prepared, err := NewBuilder("test").Prepare(context.Background(), root)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	names := tarEntryNames(t, prepared.Assets.Archive.Bytes)
	for _, want := range []string{composedNaive, composedResume} {
		if !slices.ContainsFunc(names, func(name string) bool { return strings.HasSuffix(norm.NFC.String(name), "/"+want) }) {
			t.Errorf("archive entries %+q lack %+q", names, want)
		}
	}
	for _, name := range names {
		if strings.HasSuffix(name, ".pyc") {
			t.Errorf("archive holds ignored bytecode %q", name)
		}
	}
}
