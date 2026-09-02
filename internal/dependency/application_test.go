package dependency

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

func TestApplicationInstallDefaultsToLatestAndWritesLock(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	commit := strings.Repeat("a", 40)
	remote := &fakeGitHub{
		latest:   Release{ID: 10, Tag: "v1.0.0"},
		commits:  map[string]string{"v1.0.0": commit},
		archives: map[string][]byte{commit: packageArchive(t, "1.0.0", "installed\n")},
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	runner := cli.New(&stdout, &stderr, NewApplication(remote), "test")

	exitCode := runner.Run(context.Background(), []string{"install", "github:owner/plugin", "--project", root, "--json"})

	if exitCode != cli.ExitSuccess || stderr.Len() != 0 {
		t.Fatalf("Run(install) exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			Changed bool `json:"changed"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || !envelope.OK || !envelope.Result.Changed {
		t.Fatalf("decode install output %q: envelope = %#v, err = %v", stdout.String(), envelope, err)
	}
	state, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Project.Dependencies) != 1 || state.Project.Dependencies[0].Requested != "latest" {
		t.Fatalf("project dependencies = %#v, want latest declaration", state.Project.Dependencies)
	}
	if len(state.Lock.Dependencies) != 1 || state.Lock.Dependencies[0].Commit != commit || state.Lock.Dependencies[0].ReleaseID != 10 {
		t.Fatalf("locked dependencies = %#v", state.Lock.Dependencies)
	}
}

func TestApplicationMalformedArchiveLeavesProjectUnchanged(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	commit := strings.Repeat("b", 40)
	remote := &fakeGitHub{
		latest:   Release{ID: 20, Tag: "v2.0.0"},
		commits:  map[string]string{"v2.0.0": commit},
		archives: map[string][]byte{commit: []byte("not an archive")},
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	runner := cli.New(&stdout, &stderr, NewApplication(remote), "test")

	exitCode := runner.Run(context.Background(), []string{"install", "github:owner/plugin", "--project", root, "--json"})

	if exitCode != cli.ExitOperational || stdout.Len() != 0 {
		t.Fatalf("Run(install malformed) exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `"code":"dependency_operation_failed"`) || !strings.Contains(stderr.String(), "valid GitHub tarball") {
		t.Fatalf("Run(install malformed) stderr = %q, want structured actionable error", stderr.String())
	}
	for _, relative := range []string{ProjectFilename, LockFilename} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative))); !os.IsNotExist(err) {
			t.Fatalf("failed install created %s: %v", relative, err)
		}
	}
}

func TestApplicationPersistsFreshnessAfterSuccessfulInstall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		flag   string
		policy string
	}{
		{name: "default", policy: "outdated"},
		{name: "outdated", flag: "outdated", policy: "outdated"},
		{name: "install", flag: "install", policy: "install"},
		{name: "none", flag: "none", policy: "none"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			args := []string{"install", "--project", root, "--non-interactive"}
			if test.flag != "" {
				args = append(args, "--freshness", test.flag)
			}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := cli.New(&stdout, &stderr, NewApplication(&fakeGitHub{}), "test").Run(context.Background(), args)
			if exitCode != cli.ExitSuccess || stderr.Len() != 0 {
				t.Fatalf("Run(install) exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
			}
			state, err := LoadState(root)
			if err != nil {
				t.Fatal(err)
			}
			if state.Project.Freshness != test.policy {
				t.Fatalf("freshness = %q, want %q", state.Project.Freshness, test.policy)
			}
		})
	}
}
