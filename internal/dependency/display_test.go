package dependency

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

// mixedOutdatedProject declares three latest dependencies: one ordinary update,
// one held at its barrier, and one whose candidate is beyond the barrier.
func mixedOutdatedProject(t *testing.T) (string, *perSourceGitHub) {
	t.Helper()
	root := t.TempDir()
	const beyondSource = "github:owner/beyond"
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
			{Source: siblingSourc, Requested: "latest"},
			{Source: heldSource, Requested: "latest", Hold: &Hold{Pin: heldTag, Rejected: rejectedTag}},
			{Source: beyondSource, Requested: "latest", Hold: &Hold{Pin: heldTag, Rejected: rejectedTag}},
		}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{
			{Source: siblingSourc, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 5, Tag: "v2.0.0",
				Commit: strings.Repeat("1", 40), PackageVersion: "2.0.0", ContentHash: "sha256:" + strings.Repeat("3", 64)},
			{Source: heldSource, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 987, Tag: heldTag,
				Commit: strings.Repeat("a", 40), PackageVersion: "1.3.2", ContentHash: "sha256:" + strings.Repeat("b", 64),
				Hold: &LockHold{RejectedTag: rejectedTag, RejectedReleaseID: 1024}},
			{Source: beyondSource, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 987, Tag: heldTag,
				Commit: strings.Repeat("a", 40), PackageVersion: "1.3.2", ContentHash: "sha256:" + strings.Repeat("b", 64),
				Hold: &LockHold{RejectedTag: rejectedTag, RejectedReleaseID: 1024}},
		}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	remote := &perSourceGitHub{
		latest: map[string]Release{
			siblingSourc: {ID: 6, Tag: "v2.1.0"},
			heldSource:   {ID: 1024, Tag: rejectedTag},
			beyondSource: {ID: 2048, Tag: "v1.4.1"},
		},
		commits: map[string]string{
			siblingSourc + "@v2.1.0":       strings.Repeat("2", 40),
			heldSource + "@" + rejectedTag: strings.Repeat("d", 40),
			beyondSource + "@v1.4.1":       strings.Repeat("e", 40),
		},
	}
	return root, remote
}

func TestOutdatedClassifiesUpdateHeldAndBeyondBarrier(t *testing.T) {
	t.Parallel()

	root, remote := mixedOutdatedProject(t)
	projectBefore, lockBefore := readStateFiles(t, root)

	outdated, err := NewService(NewResolver(remote)).Outdated(context.Background(), root)
	if err != nil {
		t.Fatalf("Outdated() error = %v", err)
	}
	if len(outdated) != 3 {
		t.Fatalf("Outdated() = %#v, want three classified rows", outdated)
	}
	byStatus := make(map[OutdatedStatus]OutdatedDependency, len(outdated))
	for _, item := range outdated {
		byStatus[item.Status] = item
	}
	update, ok := byStatus[OutdatedUpdate]
	if !ok || update.Source != siblingSourc || update.LatestTag != "v2.1.0" || update.Hold != nil || update.ResumeCommand != "" {
		t.Fatalf("update row = %#v", update)
	}
	held, ok := byStatus[OutdatedHeld]
	if !ok || held.Source != heldSource || held.Hold == nil || held.Hold.Rejected != rejectedTag || held.ResumeCommand != "" || held.Actionable() {
		t.Fatalf("held row = %#v", held)
	}
	beyond, ok := byStatus[OutdatedBeyondBarrier]
	if !ok || beyond.ResumeCommand != "acr resume github:owner/beyond" || beyond.LatestTag != "v1.4.1" || beyond.Notice == "" || !beyond.Actionable() {
		t.Fatalf("beyond-barrier row = %#v", beyond)
	}
	if remote.downloadCalls != 0 {
		t.Fatalf("Outdated() downloaded content: %#v", remote)
	}
	projectAfter, lockAfter := readStateFiles(t, root)
	if projectAfter != projectBefore || lockAfter != lockBefore {
		t.Fatal("Outdated() modified project state")
	}
}

func TestOutdatedTextSummaryCountsActionableRowsSeparately(t *testing.T) {
	t.Parallel()

	root, remote := mixedOutdatedProject(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := cli.New(&stdout, &stderr, NewApplication(remote), cli.Build{Version: "test"}).Run(context.Background(), []string{"outdated", "--project", root})

	if exitCode != cli.ExitSuccess {
		t.Fatalf("Run(outdated) exit = %d, stderr = %q", exitCode, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "2 latest dependencies are outdated.") {
		t.Fatalf("Run(outdated) stdout = %q, want the held row excluded from the count", output)
	}
	heldSection, barrierSection, split := strings.Cut(output, "Beyond a rollback barrier:")
	if !split || !strings.Contains(barrierSection, "acr resume github:owner/beyond") {
		t.Fatalf("Run(outdated) stdout = %q, want the barrier listed separately", output)
	}
	if strings.Contains(barrierSection, heldSource) {
		t.Fatalf("Run(outdated) barrier list = %q, want the held row kept out of it", barrierSection)
	}
	if !strings.Contains(heldSection, "Held behind a rollback barrier:") || !strings.Contains(heldSection, heldSource) {
		t.Fatalf("Run(outdated) stdout = %q, want the held steady state reported in its own section", output)
	}
}

// An explicit acr outdated reports a standing hold even when nothing else is
// outdated: the "all current" line on its own hides the rollback from the
// operator who asked.
func TestOutdatedTextSummaryReportsAHeldOnlyProject(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
			{Source: heldSource, Requested: "latest", Hold: &Hold{Pin: heldTag, Rejected: rejectedTag}},
		}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{
			{Source: heldSource, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 987, Tag: heldTag,
				Commit: strings.Repeat("a", 40), PackageVersion: "1.3.2", ContentHash: "sha256:" + strings.Repeat("b", 64),
				Hold: &LockHold{RejectedTag: rejectedTag, RejectedReleaseID: 1024}},
		}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	remote := &perSourceGitHub{
		latest:  map[string]Release{heldSource: {ID: 1024, Tag: rejectedTag}},
		commits: map[string]string{heldSource + "@" + rejectedTag: strings.Repeat("d", 40)},
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := cli.New(&stdout, &stderr, NewApplication(remote), cli.Build{Version: "test"}).Run(context.Background(), []string{"outdated", "--project", root})

	if exitCode != cli.ExitSuccess {
		t.Fatalf("Run(outdated) exit = %d, stderr = %q", exitCode, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "All latest dependencies are current.") {
		t.Fatalf("Run(outdated) stdout = %q, want the held row excluded from the actionable count", output)
	}
	wantRow := "Held behind a rollback barrier:\n" + heldSource + " (pin " + heldTag + ", barrier " + rejectedTag + ")"
	if !strings.Contains(output, wantRow) {
		t.Fatalf("Run(outdated) stdout = %q, want %q", output, wantRow)
	}
	if strings.Contains(output, "Beyond a rollback barrier:") {
		t.Fatalf("Run(outdated) stdout = %q, want no barrier section for a standing hold", output)
	}
}

func TestHoldJSONStdoutDistinctFromTextNotices(t *testing.T) {
	t.Parallel()

	root, remote := mixedOutdatedProject(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := cli.New(&stdout, &stderr, NewApplication(remote), cli.Build{Version: "test"}).Run(context.Background(), []string{"outdated", "--project", root, "--json"})

	if exitCode != cli.ExitSuccess || stderr.Len() != 0 {
		t.Fatalf("Run(outdated --json) exit = %d, stderr = %q", exitCode, stderr.String())
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			Outdated []struct {
				Source        string `json:"source"`
				Status        string `json:"status"`
				ResumeCommand string `json:"resumeCommand"`
				Hold          *struct {
					Pin      string `json:"pin"`
					Rejected string `json:"rejected"`
				} `json:"hold"`
			} `json:"outdated"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	if !envelope.OK || len(envelope.Result.Outdated) != 3 {
		t.Fatalf("outdated envelope = %#v", envelope)
	}
	for _, row := range envelope.Result.Outdated {
		switch row.Status {
		case "update":
			if row.Hold != nil || row.ResumeCommand != "" {
				t.Fatalf("update row carries hold fields: %#v", row)
			}
		case "held":
			if row.Hold == nil || row.Hold.Rejected != rejectedTag || row.ResumeCommand != "" {
				t.Fatalf("held row = %#v", row)
			}
		case "beyond-barrier":
			if row.Hold == nil || row.ResumeCommand == "" {
				t.Fatalf("beyond-barrier row = %#v", row)
			}
		default:
			t.Fatalf("unclassified row %#v", row)
		}
	}
}

func TestListShowsHeldDependenciesDistinctly(t *testing.T) {
	t.Parallel()

	root := heldProject(t, strings.Repeat("a", 40))
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := cli.New(&stdout, &stderr, NewApplication(&fakeGitHub{}), cli.Build{Version: "test"}).Run(context.Background(), []string{"list", "--project", root})

	if exitCode != cli.ExitSuccess || stderr.Len() != 0 {
		t.Fatalf("Run(list) exit = %d, stderr = %q", exitCode, stderr.String())
	}
	want := heldSource + "@latest [held " + heldTag + ", barrier " + rejectedTag + "] -> " + strings.Repeat("a", 40)
	if strings.TrimSpace(stdout.String()) != want {
		t.Fatalf("Run(list) stdout = %q, want %q", stdout.String(), want)
	}
}

func TestListJSONCarriesTheTypedHoldOnBothSides(t *testing.T) {
	t.Parallel()

	root := heldProject(t, strings.Repeat("a", 40))
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := cli.New(&stdout, &stderr, NewApplication(&fakeGitHub{}), cli.Build{Version: "test"}).Run(context.Background(), []string{"list", "--project", root, "--json"})

	if exitCode != cli.ExitSuccess || stderr.Len() != 0 {
		t.Fatalf("Run(list --json) exit = %d, stderr = %q", exitCode, stderr.String())
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			Dependencies []struct {
				Declaration struct {
					Requested string `json:"requested"`
					Hold      *struct {
						Pin      string `json:"pin"`
						Rejected string `json:"rejected"`
						Reason   string `json:"reason"`
					} `json:"hold"`
				} `json:"declaration"`
				Locked *struct {
					Tag  string `json:"tag"`
					Hold *struct {
						RejectedTag       string `json:"rejectedTag"`
						RejectedReleaseID int64  `json:"rejectedReleaseId"`
					} `json:"hold"`
				} `json:"locked"`
			} `json:"dependencies"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	if !envelope.OK || len(envelope.Result.Dependencies) != 1 {
		t.Fatalf("list envelope = %#v", envelope)
	}
	row := envelope.Result.Dependencies[0]
	if row.Declaration.Requested != "latest" || row.Declaration.Hold == nil || row.Declaration.Hold.Pin != heldTag || row.Declaration.Hold.Rejected != rejectedTag {
		t.Fatalf("declaration = %#v", row.Declaration)
	}
	if row.Locked == nil || row.Locked.Tag != heldTag || row.Locked.Hold == nil || row.Locked.Hold.RejectedTag != rejectedTag || row.Locked.Hold.RejectedReleaseID != 1024 {
		t.Fatalf("locked = %#v", row.Locked)
	}
}

func TestChangeMessagesNameHeldAndResumedDependencies(t *testing.T) {
	t.Parallel()

	t.Run("held", func(t *testing.T) {
		root := heldProject(t, strings.Repeat("a", 40))
		remote := &fakeGitHub{latest: Release{ID: 1024, Tag: rejectedTag}, commits: map[string]string{rejectedTag: strings.Repeat("d", 40)}}
		var stdout bytes.Buffer
		var stderr bytes.Buffer

		exitCode := cli.New(&stdout, &stderr, NewApplication(remote), cli.Build{Version: "test"}).Run(context.Background(), []string{"install", "--project", root, "--dry-run"})

		if exitCode != cli.ExitSuccess {
			t.Fatalf("Run(install --dry-run) exit = %d, stderr = %q", exitCode, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Held behind a rollback barrier: "+heldSource) {
			t.Fatalf("Run(install --dry-run) stdout = %q", stdout.String())
		}
	})

	// A dry run writes nothing, so it must describe what it would do rather
	// than report a resume that has not happened.
	t.Run("resumed", func(t *testing.T) {
		tests := []struct {
			name      string
			arguments []string
			want      string
			unwanted  string
		}{
			{
				name:      "dry run",
				arguments: []string{"--dry-run"},
				want:      "Would resume latest for " + heldSource,
				unwanted:  "Resumed latest for " + heldSource,
			},
			{
				name:     "written",
				want:     "Resumed latest for " + heldSource,
				unwanted: "Would resume latest for " + heldSource,
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				root := heldProject(t, strings.Repeat("a", 40))
				remote := resumeRemote(t, "v1.4.1", strings.Repeat("e", 40))
				var stdout bytes.Buffer
				var stderr bytes.Buffer
				invocation := append([]string{"resume", heldSource, "--project", root}, test.arguments...)

				exitCode := cli.New(&stdout, &stderr, NewApplication(remote), cli.Build{Version: "test"}).Run(context.Background(), invocation)

				if exitCode != cli.ExitSuccess {
					t.Fatalf("Run(resume %v) exit = %d, stderr = %q", test.arguments, exitCode, stderr.String())
				}
				if !strings.Contains(stdout.String(), test.want) || strings.Contains(stdout.String(), test.unwanted) {
					t.Fatalf("Run(resume %v) stdout = %q, want %q and not %q", test.arguments, stdout.String(), test.want, test.unwanted)
				}
			})
		}
	})
}
