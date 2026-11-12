package dependency

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

const (
	reportLatest  = "github:owner/plugin"
	reportPinned  = "github:owner/pinned"
	reportWritten = "Dependency state updated in agents.yaml and .agents/registry.lock; run 'acr realize' to materialize locked artifacts."
	reportCurrent = "Dependency state is already current."
)

var (
	reportV1Commit     = strings.Repeat("1", 40)
	reportV2Commit     = strings.Repeat("2", 40)
	reportPinnedCommit = strings.Repeat("c", 40)
)

// reportProject installs a latest dependency at v1.0.0 and a commit pin
// through the ordinary service, so every lock row is one a real install wrote.
func reportProject(t *testing.T) (string, *perSourceGitHub) {
	t.Helper()
	root := t.TempDir()
	remote := &perSourceGitHub{
		latest: map[string]Release{reportLatest: {ID: 1, Tag: "v1.0.0"}},
		commits: map[string]string{
			reportLatest + "@v1.0.0":                     reportV1Commit,
			reportPinned + "@" + reportPinnedCommit[:12]: reportPinnedCommit,
		},
		archives: map[string][]byte{
			reportV1Commit:     packageArchiveFor(t, "owner/plugin", "1.0.0", "one\n"),
			reportPinnedCommit: packageArchiveFor(t, "owner/pinned", "3.0.0", "pinned\n"),
		},
	}
	service := NewService(NewResolver(remote))
	for _, request := range []struct{ source, requested string }{{reportLatest, "latest"}, {reportPinned, reportPinnedCommit[:12]}} {
		if _, err := service.Install(context.Background(), root, request.source, request.requested, DowngradeUnset, false); err != nil {
			t.Fatalf("Install(%s@%s) error = %v", request.source, request.requested, err)
		}
	}
	return root, remote
}

// publishReportRelease makes v2.0.0 the newest release of the latest
// dependency, the way a producer's next release reaches a consumer.
func publishReportRelease(t *testing.T, _ string, remote *perSourceGitHub) {
	t.Helper()
	remote.latest[reportLatest] = Release{ID: 2, Tag: "v2.0.0"}
	remote.commits[reportLatest+"@v2.0.0"] = reportV2Commit
	remote.archives[reportV2Commit] = packageArchiveFor(t, "owner/plugin", "2.0.0", "two\n")
}

// dropReportLock leaves the latest dependency declared but not locked, the
// state a hand-added declaration leaves behind.
func dropReportLock(t *testing.T, root string, _ *perSourceGitHub) {
	t.Helper()
	state, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	index, locked := findLock(state.Lock.Dependencies, reportLatest)
	if !locked {
		t.Fatalf("lock = %#v, want a row for %s", state.Lock.Dependencies, reportLatest)
	}
	state.Lock.Dependencies = append(state.Lock.Dependencies[:index], state.Lock.Dependencies[index+1:]...)
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
}

func runReportUpdate(t *testing.T, root string, remote GitHub, arguments ...string) string {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	invocation := append(append([]string{"update"}, arguments...), "--project", root)

	exitCode := cli.New(&stdout, &stderr, NewApplication(remote), cli.Build{Version: "test"}).Run(context.Background(), invocation)

	if exitCode != cli.ExitSuccess || stderr.Len() != 0 {
		t.Fatalf("Run(%v) exit = %d, stderr = %q", invocation, exitCode, stderr.String())
	}
	return stdout.String()
}

func TestUpdateTextReportsEachLockedVersion(t *testing.T) {
	t.Parallel()

	pinnedUnchanged := reportPinned + " " + reportPinnedCommit + " unchanged"
	tests := []struct {
		name      string
		prepare   func(*testing.T, string, *perSourceGitHub)
		arguments []string
		want      []string
	}{
		{
			name:    "new release",
			prepare: publishReportRelease,
			want:    []string{reportWritten, pinnedUnchanged, reportLatest + " v1.0.0 -> v2.0.0"},
		},
		{
			name:      "new release dry run",
			prepare:   publishReportRelease,
			arguments: []string{"--dry-run"},
			want: []string{
				"update would update dependency state; rerun without --dry-run to write agents.yaml and .agents/registry.lock.",
				pinnedUnchanged, reportLatest + " v1.0.0 -> v2.0.0",
			},
		},
		{
			name:      "new release targeted",
			prepare:   publishReportRelease,
			arguments: []string{reportLatest},
			want:      []string{reportWritten, reportLatest + " v1.0.0 -> v2.0.0"},
		},
		{
			name: "nothing moved",
			want: []string{reportCurrent, pinnedUnchanged, reportLatest + " v1.0.0 unchanged"},
		},
		{
			name:      "pin targeted",
			arguments: []string{reportPinned},
			want:      []string{reportCurrent, pinnedUnchanged},
		},
		{
			name:    "declared but not locked",
			prepare: dropReportLock,
			want:    []string{reportWritten, pinnedUnchanged, reportLatest + " none -> v1.0.0"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root, remote := reportProject(t)
			if test.prepare != nil {
				test.prepare(t, root, remote)
			}

			stdout := runReportUpdate(t, root, remote, test.arguments...)

			if want := strings.Join(test.want, "\n") + "\n"; stdout != want {
				t.Fatalf("acr update %v stdout =\n%s\nwant\n%s", test.arguments, stdout, want)
			}
		})
	}
}

func TestUpdateJSONCarriesEachLockedVersion(t *testing.T) {
	t.Parallel()

	pinnedUnchanged := map[string]any{"source": reportPinned, "from": reportPinnedCommit, "to": reportPinnedCommit, "changed": false}
	tests := []struct {
		name    string
		prepare func(*testing.T, string, *perSourceGitHub)
		want    []map[string]any
	}{
		{
			name:    "new release",
			prepare: publishReportRelease,
			want:    []map[string]any{pinnedUnchanged, {"source": reportLatest, "from": "v1.0.0", "to": "v2.0.0", "changed": true}},
		},
		{
			name: "nothing moved",
			want: []map[string]any{pinnedUnchanged, {"source": reportLatest, "from": "v1.0.0", "to": "v1.0.0", "changed": false}},
		},
		{
			name:    "declared but not locked",
			prepare: dropReportLock,
			want:    []map[string]any{pinnedUnchanged, {"source": reportLatest, "to": "v1.0.0", "changed": true}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root, remote := reportProject(t)
			if test.prepare != nil {
				test.prepare(t, root, remote)
			}

			stdout := runReportUpdate(t, root, remote, "--json")

			var envelope struct {
				OK      bool   `json:"ok"`
				Command string `json:"command"`
				Result  struct {
					Changed  bool             `json:"changed"`
					Versions []map[string]any `json:"versions"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatalf("decode %q: %v", stdout, err)
			}
			if !envelope.OK || envelope.Command != "update" {
				t.Fatalf("update envelope = %#v", envelope)
			}
			wantChanged := false
			for _, row := range test.want {
				wantChanged = wantChanged || row["changed"] == true
			}
			if envelope.Result.Changed != wantChanged {
				t.Fatalf("result.changed = %t, want %t", envelope.Result.Changed, wantChanged)
			}
			if !reflect.DeepEqual(envelope.Result.Versions, test.want) {
				t.Fatalf("result.versions = %#v, want %#v", envelope.Result.Versions, test.want)
			}
		})
	}
}
