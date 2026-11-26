package publishapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/publish"
)

const ignoredFilesManifest = "schemaVersion: 1\nname: example/ignored\nversion: 1.0.0\nsource:\n  repository: https://github.com/example/ignored\nartifacts:\n  skills:\n    - id: worker\n      path: skills/worker\n  rules:\n    - id: guidance\n      path: guidance.md\n      activation:\n        mode: always\n"

// ignoredFilesRepository is a tagged repository whose skill directory holds
// committed sources plus Python bytecode that .gitignore excludes.
func ignoredFilesRepository(t *testing.T) (root, commit string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "Fixture")
	t.Setenv("GIT_AUTHOR_EMAIL", "fixture@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "Fixture")
	t.Setenv("GIT_COMMITTER_EMAIL", "fixture@example.test")
	root = t.TempDir()
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
	put("agent-plugin.yaml", ignoredFilesManifest)
	put("guidance.md", "# Guidance\n")
	put(".gitignore", "__pycache__/\n*.pyc\n")
	put("skills/worker/SKILL.md", "---\nname: worker\ndescription: Runs the worker.\n---\n# Worker\n")
	put("skills/worker/runtime/worker.py", "print('worker')\n")
	publicationGit(t, root, "init", "--template=", "-q")
	publicationGit(t, root, "add", ".")
	publicationGit(t, root, "-c", "commit.gpgsign=false", "commit", "-qm", "Create package")
	publicationGit(t, root, "-c", "tag.gpgsign=false", "tag", "v1.0.0")
	commit = publicationGit(t, root, "rev-parse", "HEAD")
	// Local test runs leave bytecode behind; .gitignore keeps it out of Git.
	put("skills/worker/runtime/__pycache__/worker.cpython-311.pyc", "bytecode")
	put("skills/worker/runtime/helper.pyc", "bytecode")
	return root, commit
}

func validateFiles(t *testing.T, root string) []string {
	t.Helper()
	result, err := validatePackage(context.Background(), root)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return result.Value.(validationResult).Files
}

func TestValidateOmitsGitIgnoredSkillFiles(t *testing.T) {
	root, _ := ignoredFilesRepository(t)
	got := strings.Join(validateFiles(t, root), " ")
	want := "agent-plugin.yaml guidance.md skills/worker/SKILL.md skills/worker/runtime/worker.py"
	if got != want {
		t.Fatalf("validate files = %q, want %q", got, want)
	}
}

func TestPublishRefusesArchivesAntigravityCannotLoad(t *testing.T) {
	const skillPath = "skills/worker/SKILL.md"
	for _, tc := range []struct{ name, path, body, want string }{
		{"missing name", skillPath, "---\ndescription: Runs the worker.\n---\n", "set name"},
		{"invalid name", skillPath, "---\nname: []\ndescription: Runs the worker.\n---\n", "set name"},
		{"missing description", skillPath, "---\nname: worker\n---\n", "set description"},
		{"invalid description", skillPath, "---\nname: worker\ndescription: '  '\n---\n", "set description"},
		{"oversized rendered root", "guidance.md", strings.Repeat("x", 23999) + "\n", "limit 24000 bytes"},
		{"native include", "guidance.md", "See @[guide](guide.md).\n", "@[label](path)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := ignoredFilesRepository(t)
			if err := os.WriteFile(filepath.Join(root, tc.path), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			publicationGit(t, root, "-c", "commit.gpgsign=false", "commit", "-qam", "Change package content")
			publicationGit(t, root, "-c", "tag.gpgsign=false", "tag", "-f", "v1.0.0")
			remote := &fakeReleases{tagCommit: publicationGit(t, root, "rev-parse", "HEAD"), tagExists: true}
			_, err := NewService(publish.NewBuilder("test"), remote).Publish(context.Background(), root, false)
			assertPublishCode(t, err, publish.CodeAdapterRealization)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("gate error = %v, want %q", err, tc.want)
			}
			if remote.lookupCalls != 0 || remote.tagCalls != 0 || remote.writeCalls() != 0 {
				t.Fatalf("GitHub accessed for refused archive: %#v", remote)
			}
		})
	}
}

func TestValidateStillReportsUntrackedFilesGitDoesNotIgnore(t *testing.T) {
	root, _ := ignoredFilesRepository(t)
	if err := os.WriteFile(filepath.Join(root, "skills/worker/runtime/new.py"), []byte("print('new')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(validateFiles(t, root), " ")
	if !strings.Contains(got, "skills/worker/runtime/new.py") || strings.Contains(got, "pyc") {
		t.Fatalf("validate files = %q, want the untracked source and no bytecode", got)
	}
}

func TestValidateStillReportsDeclaredFilesThatAreMissing(t *testing.T) {
	root, _ := ignoredFilesRepository(t)
	if err := os.Remove(filepath.Join(root, "guidance.md")); err != nil {
		t.Fatal(err)
	}
	_, err := validatePackage(context.Background(), root)
	var failure *cli.Error
	if err == nil || !errors.As(err, &failure) || failure.Code != "path_not_found" {
		t.Fatalf("validate error = %v, want path_not_found", err)
	}
}

func TestValidateWithoutGitListsEveryFile(t *testing.T) {
	root, _ := ignoredFilesRepository(t)
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	got := strings.Join(validateFiles(t, root), " ")
	if !strings.Contains(got, "skills/worker/runtime/helper.pyc") {
		t.Fatalf("validate files = %q, want the whole tree outside Git", got)
	}
}

func TestPublishDryRunIgnoresGitIgnoredBytecode(t *testing.T) {
	root, commit := ignoredFilesRepository(t)
	remote := &fakeReleases{tagCommit: commit, tagExists: true}
	app := newApplication(NewService(publish.NewBuilder("test"), remote), cli.UnavailableApplication{})
	var stdout, stderr bytes.Buffer
	exit := cli.New(&stdout, &stderr, app, cli.Build{Version: "test"}).Run(context.Background(), []string{"publish", root, "--dry-run", "--json"})
	if exit != cli.ExitSuccess || stderr.Len() != 0 {
		t.Fatalf("publish --dry-run exit = %d, stdout = %s, stderr = %s", exit, &stdout, &stderr)
	}
	if remote.writeCalls() != 0 {
		t.Fatal("dry run wrote to the remote")
	}
}

func TestPublishRefusesDeclaredIgnoredFileWithoutAdvisingCommit(t *testing.T) {
	root, commit := ignoredFilesRepository(t)
	// A declared rule that .gitignore excludes is not in the tag's tree.
	manifestBody := strings.Replace(ignoredFilesManifest, "path: guidance.md", "path: notes.pyc", 1)
	if err := os.WriteFile(filepath.Join(root, "notes.pyc"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	publicationGit(t, root, "rm", "-q", "guidance.md")
	if err := os.WriteFile(filepath.Join(root, "agent-plugin.yaml"), []byte(manifestBody), 0o644); err != nil {
		t.Fatal(err)
	}
	publicationGit(t, root, "add", "agent-plugin.yaml")
	publicationGit(t, root, "-c", "commit.gpgsign=false", "commit", "-qm", "Declare an ignored file")
	publicationGit(t, root, "tag", "-d", "v1.0.0")
	publicationGit(t, root, "-c", "tag.gpgsign=false", "tag", "v1.0.0")
	commit = publicationGit(t, root, "rev-parse", "HEAD")

	remote := &fakeReleases{tagCommit: commit, tagExists: true}
	app := newApplication(NewService(publish.NewBuilder("test"), remote), cli.UnavailableApplication{})
	var stdout, stderr bytes.Buffer
	exit := cli.New(&stdout, &stderr, app, cli.Build{Version: "test"}).Run(context.Background(), []string{"publish", root, "--dry-run", "--json"})
	if exit != cli.ExitOperational || stdout.Len() != 0 {
		t.Fatalf("exit = %d, stdout = %s, stderr = %s", exit, &stdout, &stderr)
	}
	var envelope struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal(stderr.Bytes(), &envelope); err != nil {
		t.Fatalf("stderr is not JSON: %s", &stderr)
	}
	message := envelope.Error.Message
	if envelope.Error.Code != publish.CodeUnpublishable || !strings.Contains(message, `Git ignores "notes.pyc"`) {
		t.Fatalf("error = %#v", envelope.Error)
	}
	if strings.Contains(message, "commit every declared package file") || strings.Contains(message, "commit the declared file") {
		t.Fatalf("advice tells the operator to commit an ignored file: %s", message)
	}
}

func TestValidateDoesNotWalkIntoIgnoredDirectories(t *testing.T) {
	root, _ := ignoredFilesRepository(t)
	// A link inside an ignored directory would fail the skill tree check if the
	// walk reached it; Git's view hides it with the rest of the directory.
	if err := os.Symlink("/nonexistent", filepath.Join(root, "skills/worker/runtime/__pycache__/link")); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(validateFiles(t, root), " ")
	if strings.Contains(got, "__pycache__") {
		t.Fatalf("validate files = %q", got)
	}
}
