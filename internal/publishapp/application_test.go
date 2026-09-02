package publishapp

import (
	"bytes"
	"context"
	"encoding/json"
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
