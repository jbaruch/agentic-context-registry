package publishapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

func TestPublishJSONStdoutUncontaminated(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{tagCommit: prepared.Identity.Commit, tagExists: true}
	application := newApplication(NewService(fakePreparer{prepared: prepared}, remote), cli.UnavailableApplication{})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := cli.New(&stdout, &stderr, application, cli.Build{Version: "test"}).Run(context.Background(), []string{"publish", "--dry-run", "--json"})
	if exitCode != cli.ExitSuccess || stderr.Len() != 0 {
		t.Fatalf("exit = %d, stderr = %q", exitCode, stderr.String())
	}
	var document map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("stdout is not one JSON document: %v: %q", err, stdout.String())
	}
	if document["ok"] != true || document["command"] != "publish" {
		t.Fatalf("JSON envelope = %#v", document)
	}
}

func TestPublishExistingReleaseUsesOperationalExit(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{existing: dependency.Release{ID: 1, Tag: prepared.Identity.Tag}, exists: true, tagCommit: prepared.Identity.Commit, tagExists: true}
	application := newApplication(NewService(fakePreparer{prepared: prepared}, remote), cli.UnavailableApplication{})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := cli.New(&stdout, &stderr, application, cli.Build{Version: "test"}).Run(context.Background(), []string{"publish", "--json"})
	if exitCode != cli.ExitOperational || stdout.Len() != 0 || !bytes.Contains(stderr.Bytes(), []byte(`"code":"release_already_exists"`)) {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
	}
}

func TestValidationProjectPathPrecedence(t *testing.T) {
	root := t.TempDir()
	for _, sub := range []string{"cwd", "project", "project/nested", "absolute"} {
		dir := filepath.Join(root, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		name := strings.ReplaceAll(sub, "/", "-")
		manifest := "schemaVersion: 1\nname: example/" + name + "\nversion: 1.2.3\nsource:\n  repository: https://github.com/example/" + name + "\nartifacts:\n  skills:\n    - id: check\n      path: check\n"
		if err := os.WriteFile(filepath.Join(dir, "agent-plugin.yaml"), []byte(manifest), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, "check"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "check/SKILL.md"), []byte("# Check\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(filepath.Join(root, "cwd"))
	before := validationInventory(t, root)
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"default", nil, "cwd"},
		{"relative-path", []string{"../project"}, "project"},
		{"absolute-path", []string{filepath.Join(root, "absolute")}, "absolute"},
		{"relative-project", []string{"--project", "../project"}, "project"},
		{"absolute-project", []string{"--project", filepath.Join(root, "project")}, "project"},
		{"project-relative-path", []string{"nested", "--project", "../project"}, "project-nested"},
		{"project-absolute-path", []string{filepath.Join(root, "absolute"), "--project", "../project"}, "absolute"},
		{"invalid", []string{"missing", "--project", "../project"}, ""},
	} {
		for _, jsonOutput := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/json=%t", c.name, jsonOutput), func(t *testing.T) {
				args := append([]string{"validate"}, c.args...)
				if jsonOutput {
					args = append(args, "--json")
				}
				var stdout, stderr bytes.Buffer
				// No service, remote or fallback is supplied: any such access fails the test.
				app := newApplication(nil, nil)
				exit := cli.New(&stdout, &stderr, app, cli.Build{Version: "test"}).Run(context.Background(), args)
				if c.want == "" {
					if exit != cli.ExitOperational {
						t.Fatalf("invalid target exit=%d", exit)
					}
				} else {
					if exit != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "example/"+c.want) {
						t.Fatalf("selected package: exit=%d out=%s err=%s", exit, &stdout, &stderr)
					}
					if jsonOutput {
						var doc map[string]any
						if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
							t.Fatal(err)
						}
					}
				}
			})
		}
	}
	if !reflect.DeepEqual(before, validationInventory(t, root)) {
		t.Fatal("validation changed files or modes")
	}
}

func validationInventory(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		body := []byte(nil)
		if !entry.IsDir() {
			body, err = os.ReadFile(name)
			if err != nil {
				return err
			}
		}
		result[name] = fmt.Sprintf("%o:%s", info.Mode(), body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
