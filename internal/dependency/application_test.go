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
	runner := cli.New(&stdout, &stderr, NewApplication(remote), cli.Build{Version: "test"})

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
	runner := cli.New(&stdout, &stderr, NewApplication(remote), cli.Build{Version: "test"})

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

func TestApplicationDowngradeRequiresAnExplicitChoice(t *testing.T) {
	t.Parallel()

	rejectedCommit := strings.Repeat("d", 40)
	root := latestProject(t, rejectedTag, rejectedCommit)
	remote := rollbackRemote(t, strings.Repeat("7", 40), rejectedCommit)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := cli.New(&stdout, &stderr, NewApplication(remote), "test").Run(context.Background(), []string{"install", heldSource + "@" + heldTag, "--project", root, "--json"})

	if exitCode != cli.ExitUsage || stdout.Len() != 0 {
		t.Fatalf("Run(downgrade) exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
	}
	var envelope struct {
		OK    bool `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %q: %v", stderr.String(), err)
	}
	if envelope.OK || envelope.Error.Code != "downgrade_choice_required" {
		t.Fatalf("error envelope = %#v", envelope)
	}
	if !strings.Contains(envelope.Error.Message, "--hold") || !strings.Contains(envelope.Error.Message, "--pin") {
		t.Fatalf("error message %q does not name both non-interactive choices", envelope.Error.Message)
	}
}

// At the CLI boundary an equal reference exits 2 with the choice code and
// writes nothing, and --pin is the flag that converts it into a permanent pin.
func TestApplicationEqualReferenceExitsWithTheChoiceCode(t *testing.T) {
	t.Parallel()

	lockedCommit := strings.Repeat("d", 40)
	for _, requested := range []string{rejectedTag, lockedCommit} {
		t.Run(requested, func(t *testing.T) {
			t.Parallel()
			root := latestProject(t, rejectedTag, lockedCommit)
			projectBefore, lockBefore := readStateFiles(t, root)
			remote := equalReferenceRemote(t, lockedCommit)
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := cli.New(&stdout, &stderr, NewApplication(remote), "test").Run(context.Background(), []string{"install", heldSource + "@" + requested, "--project", root, "--json"})

			if exitCode != cli.ExitUsage || stdout.Len() != 0 {
				t.Fatalf("Run(install @%s) exit = %d, stdout = %q, stderr = %q", requested, exitCode, stdout.String(), stderr.String())
			}
			var envelope struct {
				OK    bool `json:"ok"`
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(stderr.Bytes(), &envelope); err != nil {
				t.Fatalf("decode %q: %v", stderr.String(), err)
			}
			if envelope.OK || envelope.Error.Code != "downgrade_choice_required" {
				t.Fatalf("error envelope = %#v", envelope)
			}
			projectAfter, lockAfter := readStateFiles(t, root)
			if projectAfter != projectBefore || lockAfter != lockBefore {
				t.Fatal("an unanswered equal-reference install wrote state")
			}
		})
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
			exitCode := cli.New(&stdout, &stderr, NewApplication(&fakeGitHub{}), cli.Build{Version: "test"}).Run(context.Background(), args)
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
