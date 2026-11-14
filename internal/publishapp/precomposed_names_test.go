package publishapp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/publish"
)

// The skill files are written decomposed, as macOS stores them, and committed
// with core.precomposeunicode=true so macOS Git lists them precomposed. On
// other systems Git keeps the bytes it was given; every assertion compares up
// to normalization so the scenario holds on both.
const (
	precomposedNaive  = "skills/worker/naïve.py"
	precomposedCafe   = "skills/worker/café/plain.py"
	precomposedResume = "skills/worker/café/résumé.py"
)

func precomposedNamesRepository(t *testing.T) (root, commit string) {
	t.Helper()
	root, commit = ignoredFilesRepository(t)
	put := func(name, body string) {
		t.Helper()
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	publicationGit(t, root, "config", "core.precomposeunicode", "true")
	put("skills/worker/naïve.py", "print('naive')\n")
	put("skills/worker/café/plain.py", "print('plain')\n")
	put("skills/worker/café/résumé.py", "print('resume')\n")
	publicationGit(t, root, "add", ".")
	publicationGit(t, root, "-c", "commit.gpgsign=false", "commit", "-qm", "Add non-ASCII skill files")
	publicationGit(t, root, "tag", "-d", "v1.0.0")
	publicationGit(t, root, "-c", "tag.gpgsign=false", "tag", "v1.0.0")
	commit = publicationGit(t, root, "rev-parse", "HEAD")
	// Ignored bytecode beside the tracked files, also decomposed.
	put("skills/worker/naïve.pyc", "bytecode")
	put("skills/worker/café/résumé.pyc", "bytecode")
	return root, commit
}

func normalized(names []string) []string {
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, norm.NFC.String(name))
	}
	return result
}

func TestValidateListsTrackedNonASCIISkillFilesGitReportsPrecomposed(t *testing.T) {
	root, _ := precomposedNamesRepository(t)
	got := normalized(validateFiles(t, root))
	want := []string{
		"agent-plugin.yaml", "guidance.md", "skills/worker/SKILL.md", "skills/worker/runtime/worker.py",
		precomposedNaive, precomposedCafe, precomposedResume,
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("validate files = %+q, want %+q", got, want)
	}
}

func TestPublishDryRunArchivesTrackedNonASCIISkillFiles(t *testing.T) {
	root, commit := precomposedNamesRepository(t)
	remote := &fakeReleases{tagCommit: commit, tagExists: true}
	builder := &recordPublication{builder: publish.NewBuilder("test")}
	app := newApplication(NewService(builder, remote), cli.UnavailableApplication{})
	var stdout, stderr bytes.Buffer
	exit := cli.New(&stdout, &stderr, app, cli.Build{Version: "test"}).Run(context.Background(), []string{"publish", root, "--dry-run", "--json"})
	if exit != cli.ExitSuccess || stderr.Len() != 0 {
		t.Fatalf("publish --dry-run exit = %d, stdout = %s, stderr = %s", exit, &stdout, &stderr)
	}
	var got []string
	reader, err := gzip.NewReader(bytes.NewReader(builder.prepared.Assets.Archive.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			got = append(got, norm.NFC.String(header.Name))
		}
	}
	for _, want := range []string{precomposedNaive, precomposedCafe, precomposedResume} {
		if !slices.ContainsFunc(got, func(name string) bool { return strings.HasSuffix(name, "/"+want) }) {
			t.Errorf("archive files %+q lack tracked %+q", got, want)
		}
	}
	for _, name := range got {
		if strings.HasSuffix(name, ".pyc") {
			t.Errorf("archive holds ignored bytecode %q", name)
		}
	}
}

func TestValidateOfABareRepositoryDirectoryListsEveryFile(t *testing.T) {
	root, _ := ignoredFilesRepository(t)
	bare := filepath.Join(t.TempDir(), "package.git")
	publicationGit(t, root, "clone", "-q", "--bare", ".", bare)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(bare))
	// A package authored inside a bare repository's directory has no work tree,
	// so Git has no view to apply and the directory is walked whole.
	for name, body := range map[string]string{
		"agent-plugin.yaml":               ignoredFilesManifest,
		"guidance.md":                     "# Guidance\n",
		"skills/worker/SKILL.md":          "---\nname: worker\ndescription: Runs the worker.\n---\n# Worker\n",
		"skills/worker/helper.pyc":        "bytecode",
		"skills/worker/runtime/worker.py": "print('worker')\n",
	} {
		target := filepath.Join(bare, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := strings.Join(validateFiles(t, bare), " ")
	if !strings.Contains(got, "skills/worker/helper.pyc") {
		t.Fatalf("validate files = %q, want the whole directory", got)
	}
}

func TestValidateSurfacesGitFailuresInsteadOfWalkingEverything(t *testing.T) {
	root, _ := ignoredFilesRepository(t)
	config, err := os.OpenFile(filepath.Join(root, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.WriteString("[core\n"); err != nil {
		t.Fatal(err)
	}
	if err := config.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = validatePackage(context.Background(), root)
	var failure *cli.Error
	if err == nil || !errors.As(err, &failure) || failure.Code != "operation_failed" {
		t.Fatalf("validate error = %v, want operation_failed from the Git failure", err)
	}
}
