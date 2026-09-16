package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/versioncheck"
)

// realizeOutput is the line a non-dry-run realization prints for one agent.
var realizeOutput = regexp.MustCompile(`^Applied \d+ realization change\(s\) for codex\.\n$`)

// attachNoticeHarness composes the release-notice inputs onto an existing
// journey project, sharing its isolated state directory, so one command can
// exercise the realization transaction and the notice together.
func attachNoticeHarness(t *testing.T, project *journeyProject) *noticeHarness {
	t.Helper()
	t.Setenv("ACR_VERSION_CHECK", "")
	originalVersion, originalCommit := version, commit
	version, commit = noticeRunningVersion, noticeRunningCommit
	t.Cleanup(func() { version, commit = originalVersion, originalCommit })
	return &noticeHarness{
		t:      t,
		store:  versioncheck.Store{BaseDirectory: project.stateHome},
		clock:  newJourneyClock(),
		source: &noticeSource{release: dependency.Release{ID: 99, Tag: noticeLatest}},
		probe:  func(io.Writer) bool { return true },
	}
}

// TestReleaseNoticeSurroundsARealizationTransaction integrates the two
// contracts this branch carries: the release notice and its post-command
// refresh around a real realization that takes and retires the transaction
// claim. The notice precedes the realization's own output, the realization
// writes its files and leaves no claim residue, the refresh runs only after
// the command and only under the machine state directory, a converged rerun
// behaves the same, and a --json realization is byte-identical to the run
// with the check switched off.
func TestReleaseNoticeSurroundsARealizationTransaction(t *testing.T) {
	github := newJourneyGitHub(t)
	alpha := newJourneyPackage(t, "example/alpha", "1.0.0")
	github.SeedRelease(alpha.fullName, alpha.tag, alpha.commit, alpha.archive)
	project := newJourneyProject(t, github)
	project.run(0, "init", "--agent", "codex", "--freshness", "none", "--non-interactive")
	project.run(0, "install", alpha.source, "--non-interactive")
	harness := attachNoticeHarness(t, project)
	harness.seedCache(noticeLatest)
	claimDirectory := project.path(".agents/.acr-transactions")

	realize := func(check bool, args ...string) (string, string, int) {
		t.Helper()
		if check {
			t.Setenv("ACR_VERSION_CHECK", "")
		} else {
			t.Setenv("ACR_VERSION_CHECK", "off")
		}
		var stdout, stderr bytes.Buffer
		full := append(append([]string{"realize"}, args...), "--project", project.root)
		exit := runComposed(project.composedClient(), strings.NewReader(""), &stdout, &stderr, full, project.freshness, harness.options())
		github.AssertNoUnknownRequests(t)
		return stdout.String(), stderr.String(), exit
	}

	// First realization: the notice, then the realization's own line.
	var combined bytes.Buffer
	exit := runComposed(project.composedClient(), strings.NewReader(""), &combined, &combined, []string{"realize", "--project", project.root}, project.freshness, harness.options())
	github.AssertNoUnknownRequests(t)
	if exit != 0 {
		t.Fatalf("realize exit = %d\n%s", exit, combined.String())
	}
	rest, found := strings.CutPrefix(combined.String(), noticeLine)
	if !found || !realizeOutput.MatchString(rest) {
		t.Fatalf("terminal stream = %q, want the notice followed by the realization line", combined.String())
	}
	if strings.HasPrefix(rest, "Applied 0 ") {
		t.Fatalf("the first realization applied nothing: %q", rest)
	}
	skill := nativeSkillDirectory(".codex", alpha.fullName, "advocate")
	assertProjectFile(t, project, skill+"/SKILL.md", alpha.body(t, "skills/advocate/SKILL.md"), 0o644)
	if _, err := os.Lstat(claimDirectory); !os.IsNotExist(err) {
		t.Fatalf("the transaction claim was not retired after a successful realization: %v", err)
	}
	if harness.source.count() != 1 {
		t.Fatalf("source called %d times after one terminal realization, want 1", harness.source.count())
	}
	if cache, usable, err := harness.store.ReadCache(); err != nil || !usable || cache.CheckedAt != harness.clock.Now() {
		t.Fatalf("the post-command refresh did not run after the realization: %#v, %t, %v", cache, usable, err)
	}
	for _, name := range []string{"version.lock", "latest.json", "attempt.json"} {
		if _, err := os.Lstat(project.path(".agents/" + name)); !os.IsNotExist(err) {
			t.Fatalf("version state %s leaked into the project's .agents: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(project.stateHome, "version", "version.lock")); err != nil {
		t.Fatalf("the version lock does not live under the machine state directory: %v", err)
	}

	// A converged rerun inside the window: notice again, the current-state
	// line, claim retired again, no second fetch.
	combined.Reset()
	if exit := runComposed(project.composedClient(), strings.NewReader(""), &combined, &combined, []string{"realize", "--project", project.root}, project.freshness, harness.options()); exit != 0 {
		t.Fatalf("converged realize exit = %d\n%s", exit, combined.String())
	}
	if got, want := combined.String(), noticeLine+"Realization is already current for codex.\n"; got != want {
		t.Fatalf("converged terminal stream = %q, want %q", got, want)
	}
	if _, err := os.Lstat(claimDirectory); !os.IsNotExist(err) {
		t.Fatalf("the claim was not retired after a converged realization: %v", err)
	}
	if harness.source.count() != 1 {
		t.Fatalf("source called %d times inside one window, want 1", harness.source.count())
	}

	// --json is machine-facing: byte parity with the switched-off run, no
	// notice, no refresh.
	checkedOut, checkedErr, checkedExit := realize(true, "--json")
	offOut, offErr, offExit := realize(false, "--json")
	if checkedExit != 0 || offExit != 0 || checkedOut != offOut || checkedErr != "" || offErr != "" {
		t.Fatalf("--json parity broken: %q/%q/%d with the check, %q/%q/%d without", checkedOut, checkedErr, checkedExit, offOut, offErr, offExit)
	}
	if !strings.Contains(checkedOut, `"command":"realize"`) {
		t.Fatalf("--json realization did not produce its envelope: %q", checkedOut)
	}
	if harness.source.count() != 1 {
		t.Fatalf("a --json realization reached the source: %d calls", harness.source.count())
	}

	// After the window closes, a terminal realization refreshes again and the
	// claim is still retired: the two locks never meet.
	t.Setenv("ACR_VERSION_CHECK", "")
	harness.clock.Advance(versioncheck.Window)
	harness.source.release.Tag = "v10.0.0"
	combined.Reset()
	if exit := runComposed(project.composedClient(), strings.NewReader(""), &combined, &combined, []string{"realize", "--project", project.root}, project.freshness, harness.options()); exit != 0 {
		t.Fatalf("realize after the window exit = %d\n%s", exit, combined.String())
	}
	if harness.source.count() != 2 {
		t.Fatalf("source called %d times after the window closed, want 2", harness.source.count())
	}
	if cache, _, _ := harness.store.ReadCache(); cache.LatestVersion != "v10.0.0" {
		t.Fatalf("cache after the second refresh = %#v", cache)
	}
	if _, err := os.Lstat(claimDirectory); !os.IsNotExist(err) {
		t.Fatalf("claim residue after the final realization: %v", err)
	}
}
