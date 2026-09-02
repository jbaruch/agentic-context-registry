package publish

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveIdentity(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)
	tests := []struct {
		name  string
		git   fakeGitSource
		code  string
		valid bool
	}{
		{name: "matching v tag", git: fakeGitSource{clean: true, head: commit, tags: []string{"v1.2.3"}}, valid: true},
		{name: "matching bare tag", git: fakeGitSource{clean: true, head: commit, tags: []string{"1.2.3"}}, valid: true},
		{name: "dirty", git: fakeGitSource{head: commit, tags: []string{"v1.2.3"}}, code: CodeDirtyWorktree},
		{name: "status failure", git: fakeGitSource{cleanErr: errors.New("status unavailable")}, code: CodeGitAccess},
		{name: "head failure", git: fakeGitSource{clean: true, headErr: errors.New("HEAD unavailable")}, code: CodeGitAccess},
		{name: "tag failure", git: fakeGitSource{clean: true, head: commit, tagsErr: errors.New("tags unavailable")}, code: CodeGitAccess},
		{name: "missing tag", git: fakeGitSource{clean: true, head: commit}, code: CodeNoPublishableTag},
		{name: "multiple tags", git: fakeGitSource{clean: true, head: commit, tags: []string{"1.2.3", "v1.2.3"}}, code: CodeAmbiguousTag},
		{name: "mismatch", git: fakeGitSource{clean: true, head: commit, tags: []string{"v1.2.4"}}, code: CodeTagVersion},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			identity, err := resolveIdentity(context.Background(), ".", "1.2.3", &test.git)
			if test.valid {
				if err != nil || identity.Commit != commit {
					t.Fatalf("resolveIdentity() = %#v, %v", identity, err)
				}
				return
			}
			publishErr, ok := err.(*Error)
			if !ok || publishErr.Code != test.code {
				t.Fatalf("resolveIdentity() error = %#v, want code %s", err, test.code)
			}
		})
	}
}

func TestCommandGitSourceReadsTaggedBlobAndMode(t *testing.T) {
	repository := t.TempDir()
	runTestGit(t, repository, "init")
	runTestGit(t, repository, "config", "user.name", "ACR Test")
	runTestGit(t, repository, "config", "user.email", "acr-test@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, ".gitattributes"), []byte("* text=auto\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "script.sh"), []byte("#!/bin/sh\r\nexit 0\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repository, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repository, "add", ".gitattributes", "script.sh")
	runTestGit(t, repository, "commit", "-m", "Add fixture")
	runTestGit(t, repository, "tag", "v1.0.0")

	source := commandGitSource{}
	file, err := source.FileAt(context.Background(), repository, "v1.0.0", "script.sh")
	if err != nil {
		t.Fatal(err)
	}
	if string(file.Content) != "#!/bin/sh\nexit 0\n" || file.Mode.Perm() != 0o755 {
		t.Fatalf("FileAt() = content %q mode %04o", file.Content, file.Mode.Perm())
	}
	clean, err := source.Clean(context.Background(), repository)
	if err != nil || !clean {
		t.Fatalf("Clean() = %t, %v", clean, err)
	}
}

type fakeGitSource struct {
	clean    bool
	head     string
	tags     []string
	files    map[string]File
	err      error
	cleanErr error
	headErr  error
	tagsErr  error
}

func (fake *fakeGitSource) Clean(context.Context, string) (bool, error) {
	if fake.cleanErr != nil {
		return false, fake.cleanErr
	}
	return fake.clean, fake.err
}
func (fake *fakeGitSource) Head(context.Context, string) (string, error) {
	if fake.headErr != nil {
		return "", fake.headErr
	}
	return fake.head, fake.err
}
func (fake *fakeGitSource) TagsAtHead(context.Context, string) ([]string, error) {
	if fake.tagsErr != nil {
		return nil, fake.tagsErr
	}
	return append([]string(nil), fake.tags...), fake.err
}
func (fake *fakeGitSource) FileAt(_ context.Context, _, _, name string) (File, error) {
	if fake.err != nil {
		return File{}, fake.err
	}
	file, ok := fake.files[name]
	if !ok {
		return File{}, os.ErrNotExist
	}
	return file, nil
}

func runTestGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	command.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}
