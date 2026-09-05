package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// journeyGit runs one git command in a repository with a fixed identity, fixed
// author and committer dates, and no global configuration, so a fixture
// repository has the same commit on every machine and every run.
func journeyGit(t *testing.T, repository string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = repository
	command.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=ACR Journey", "GIT_AUTHOR_EMAIL=journey@example.invalid",
		"GIT_COMMITTER_NAME=ACR Journey", "GIT_COMMITTER_EMAIL=journey@example.invalid",
		"GIT_AUTHOR_DATE=2001-02-03T04:05:06Z", "GIT_COMMITTER_DATE=2001-02-03T04:05:06Z",
		"GIT_TERMINAL_PROMPT=0",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// journeyPublisher writes one package into a real Git repository and tags the
// commit its version names. The returned commit is Git's, not the fixture's,
// so every later assertion compares the publisher's own evidence.
func journeyPublisher(t *testing.T, pkg *journeyPackage) (root, commit string) {
	t.Helper()
	root = t.TempDir()
	for _, file := range pkg.files {
		mode := os.FileMode(0o644)
		if file.executable {
			mode = 0o755
		}
		reverify2Put(t, root, file.path, file.body, mode)
	}
	journeyGit(t, root, "init", "-q", "-b", "main")
	journeyGit(t, root, "add", "-A")
	journeyGit(t, root, "commit", "-qm", "Publish "+pkg.fullName+" "+pkg.version)
	journeyGit(t, root, "tag", pkg.tag)
	return root, journeyGit(t, root, "rev-parse", "HEAD")
}

// journeyGitSourceArchive builds the tarball codeload serves for one tagged
// commit, from that commit's own blobs. Nothing re-states the package content:
// what the publisher committed is what the consumer downloads.
func journeyGitSourceArchive(t *testing.T, repository, tag, commit string) []byte {
	t.Helper()
	command := exec.Command("git", "archive", "--format=tar", tag)
	command.Dir = repository
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("git archive %s: %v\n%s", tag, err, stderr.String())
	}
	var files []journeyFile
	reader := tar.NewReader(bytes.NewReader(stdout.Bytes()))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read git archive: %v", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		contents, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read %s from the git archive: %v", header.Name, err)
		}
		files = append(files, journeyFile{
			path:       filepath.ToSlash(header.Name),
			body:       string(contents),
			executable: header.FileInfo().Mode().Perm()&0o111 != 0,
		})
	}
	if len(files) == 0 {
		t.Fatalf("git archive %s produced no files", tag)
	}
	return journeyGitHubArchive(t, journeyArchiveRoot(repositoryFullName(t, repository), commit), commit, files)
}

// repositoryFullName reads the package identity a repository publishes under.
func repositoryFullName(t *testing.T, repository string) string {
	t.Helper()
	manifest, err := os.ReadFile(filepath.Join(repository, "agent-plugin.yaml"))
	if err != nil {
		t.Fatalf("read the package manifest: %v", err)
	}
	for _, line := range strings.Split(string(manifest), "\n") {
		if name, found := strings.CutPrefix(line, "name: "); found {
			return strings.TrimSpace(name)
		}
	}
	t.Fatalf("package manifest declares no name: %s", manifest)
	return ""
}
