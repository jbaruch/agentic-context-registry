package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

// consumerJourneys covers the commands a project runs against packages it
// consumes. Every remote step runs the shipped composition against the
// stateful GitHub fixture through the production client.
func consumerJourneys() []journeyCase {
	return []journeyCase{
		{leaf: "init", name: "detection-override-repeat", kind: journeySuccess, run: journeyInitSuccess},
		{leaf: "init", name: "no-agent-and-bad-policy", kind: journeyRefusal, run: journeyInitRefusals},
		{leaf: "install", name: "latest-tag-and-commit", kind: journeySuccess, run: journeyInstallSuccess},
		{leaf: "install", name: "reconcile-and-repeat", kind: journeySuccess, run: journeyInstallReconcile},
		{leaf: "install", name: "unknown-source-and-empty-argument", kind: journeyRefusal, run: journeyInstallRefusals},
		{leaf: "realize", name: "native-layouts-and-subset", kind: journeySuccess, run: journeyRealizeSuccess},
		{leaf: "realize", name: "unselected-agent", kind: journeyRefusal, run: journeyRealizeRefusals},
		{leaf: "check", name: "current-drift-and-repair", kind: journeySuccess, run: journeyCheckSuccess},
		{leaf: "check", name: "tampered-source", kind: journeyRefusal, run: journeyCheckRefusals},
	}
}

// journeyConsumer is an initialized consumer project with alpha installed and
// realized for the named agents. It is the starting point most consumer
// journeys share, and it is built by running commands, never by writing state.
func journeyConsumer(t *testing.T, github *journeyGitHub, pkg *journeyPackage, agents ...string) *journeyProject {
	t.Helper()
	project := newJourneyProject(t, github)
	args := []string{"init"}
	for _, agent := range agents {
		args = append(args, "--agent", agent)
	}
	args = append(args, "--freshness", "none", "--non-interactive")
	project.run(0, args...)
	project.run(0, "install", pkg.source, "--non-interactive")
	project.run(0, "realize")
	return project
}

func assertProjectFile(t *testing.T, project *journeyProject, path, body string, mode os.FileMode) {
	t.Helper()
	ffaAssertFile(t, project.root, path, body, mode)
}

func assertProjectAbsent(t *testing.T, project *journeyProject, path string) {
	t.Helper()
	if _, err := os.Lstat(project.path(path)); err == nil {
		t.Errorf("%s still exists", path)
	} else if !os.IsNotExist(err) {
		t.Errorf("stat %s: %v", path, err)
	}
}

func loadJourneyState(t *testing.T, project *journeyProject) dependency.State {
	t.Helper()
	state, err := dependency.LoadState(project.root)
	if err != nil {
		t.Fatalf("load project state: %v", err)
	}
	return state
}

// journeyInitSuccess proves init persists a real selection: explicit repeated
// flags deduplicate and sort, detection contributes a nonempty selection on
// its own, a dry run writes nothing, and a repeat changes no byte.
func journeyInitSuccess(t *testing.T) int {
	project := newJourneyProject(t, nil)
	before := project.snapshot()

	dry := project.run(0, "init", "--agent", "codex", "--freshness", "none", "--non-interactive", "--dry-run")
	if !strings.Contains(dry.stdout, "codex") {
		t.Fatalf("init --dry-run = %q, want the selection it would write", dry.stdout)
	}
	project.assertUnchanged(before, "acr init --dry-run")

	project.run(0, "init", "--agent", "codex", "--agent", "claude-code", "--agent", "codex", "--freshness", "none", "--non-interactive")
	state := loadJourneyState(t, project)
	if !reflect.DeepEqual(state.Project.Agents, []string{"claude-code", "codex"}) {
		t.Fatalf("agents = %v, want the deduplicated sorted selection", state.Project.Agents)
	}
	if state.Project.Freshness != "none" {
		t.Fatalf("freshness = %q, want none", state.Project.Freshness)
	}
	settled := project.snapshot()
	project.run(0, "init", "--agent", "codex", "--agent", "claude-code", "--freshness", "none", "--non-interactive")
	project.assertUnchanged(settled, "a repeated acr init")

	// Detection alone selects a nonempty agent set and the default policy.
	detected := newJourneyProject(t, nil)
	reverify2Put(t, detected.root, ".cursor/rules/user.mdc", "---\nalwaysApply: true\n---\n# User rule\n", 0o644)
	reverify2Put(t, detected.root, "CLAUDE.md", "# User guidance\n", 0o644)
	detected.run(0, "init", "--non-interactive")
	detectedState := loadJourneyState(t, detected)
	if !reflect.DeepEqual(detectedState.Project.Agents, []string{"claude-code", "cursor"}) {
		t.Fatalf("detected agents = %v, want claude-code and cursor", detectedState.Project.Agents)
	}
	if detectedState.Project.Freshness != "outdated" {
		t.Fatalf("detected freshness = %q, want the outdated default", detectedState.Project.Freshness)
	}
	assertProjectFile(t, detected, "CLAUDE.md", "# User guidance\n", 0o644)
	return len(state.Project.Agents) + len(detectedState.Project.Agents)
}

func journeyInitRefusals(t *testing.T) int {
	project := newJourneyProject(t, nil)
	before := project.snapshot()

	empty := project.run(2, "init", "--non-interactive", "--json")
	if journeyError(t, empty.stderr)["code"] != "no_agent_selected" {
		t.Fatalf("undetected init refusal = %q, want no_agent_selected", empty.stderr)
	}
	badPolicy := project.run(2, "init", "--agent", "codex", "--freshness", "sometimes", "--non-interactive")
	if !strings.Contains(badPolicy.stderr, "sometimes") {
		t.Fatalf("invalid policy refusal = %q", badPolicy.stderr)
	}
	badAgent := project.run(2, "init", "--agent", "emacs", "--non-interactive")
	if !strings.Contains(badAgent.stderr, "emacs") {
		t.Fatalf("invalid agent refusal = %q", badAgent.stderr)
	}
	project.assertUnchanged(before, "an init refusal")
	assertProjectAbsent(t, project, "agents.yaml")
	return 3
}

// journeyInstallSuccess installs the same package by every selectable policy
// and proves each one locks a complete immutable identity.
func journeyInstallSuccess(t *testing.T) int {
	github := newJourneyGitHub(t)
	alpha := newJourneyPackage(t, "example/alpha", "1.0.0")
	github.SeedRelease(alpha.fullName, alpha.tag, alpha.commit, alpha.archive)

	proven := 0
	for _, policy := range []struct {
		name      string
		requested string
		kind      dependency.ResolutionKind
		release   bool
	}{
		{name: "latest", requested: "", kind: dependency.ResolutionRelease, release: true},
		{name: "tag", requested: "@" + alpha.tag, kind: dependency.ResolutionRelease, release: true},
		{name: "abbreviated commit", requested: "@" + alpha.commit[:7], kind: dependency.ResolutionCommit},
	} {
		project := newJourneyProject(t, github)
		project.run(0, "init", "--agent", "codex", "--freshness", "none", "--non-interactive")
		github.ResetRequests()

		run := project.run(0, "install", alpha.source+policy.requested, "--non-interactive", "--json")
		assertNoCredentialLeak(t, run)
		result := journeyResult(t, run.stdout)
		if result["changed"] != true {
			t.Fatalf("%s install result = %#v, want a change", policy.name, result)
		}
		state := loadJourneyState(t, project)
		if len(state.Project.Dependencies) != 1 || len(state.Lock.Dependencies) != 1 {
			t.Fatalf("%s install state = %#v, want one declaration and one lock row", policy.name, state)
		}
		declaration := state.Project.Dependencies[0]
		locked := state.Lock.Dependencies[0]
		wantRequested := "latest"
		if policy.requested != "" {
			wantRequested = strings.TrimPrefix(policy.requested, "@")
		}
		if declaration.Source != alpha.source || declaration.Requested != wantRequested {
			t.Fatalf("%s declaration = %#v, want %s@%s", policy.name, declaration, alpha.source, wantRequested)
		}
		if locked.Kind != policy.kind || locked.Commit != alpha.commit || locked.PackageVersion != alpha.version {
			t.Fatalf("%s lock = %#v, want %s at %s", policy.name, locked, policy.kind, alpha.commit)
		}
		if !strings.HasPrefix(locked.ContentHash, "sha256:") || len(locked.ContentHash) != 71 {
			t.Fatalf("%s lock content hash = %q", policy.name, locked.ContentHash)
		}
		if policy.release && (locked.Tag != alpha.tag || locked.ReleaseID <= 0) {
			t.Fatalf("%s lock lost its release identity: %#v", policy.name, locked)
		}
		if !policy.release && (locked.Tag != "" || locked.ReleaseID != 0) {
			t.Fatalf("%s lock invented a release: %#v", policy.name, locked)
		}
		// Every policy downloads the immutable commit archive, and a commit
		// request asks for no release at all.
		if github.RequestCount("/tar.gz/"+alpha.commit) == 0 {
			t.Fatalf("%s install never downloaded the immutable commit archive: %v", policy.name, github.Requests())
		}
		if !policy.release && github.RequestCount("/releases") != 0 {
			t.Fatalf("commit install consulted releases: %v", github.Requests())
		}
		// The archive redirect stays inside the trusted origin and carries the
		// credential there, which is what makes a private package installable.
		if !github.AuthorizationSeenOn(journeyAPIHost) || !github.AuthorizationSeenOn(journeyCodeloadHost) {
			t.Fatalf("%s install did not authenticate through the archive redirect", policy.name)
		}
		proven++
	}
	return proven
}

// journeyInstallReconcile advances the remote, reconciles without a source,
// and proves the moved latest row changed while the pinned sibling did not.
func journeyInstallReconcile(t *testing.T) int {
	github := newJourneyGitHub(t)
	alphaV1 := newJourneyPackage(t, "example/alpha", "1.0.0")
	alphaV2 := newJourneyPackage(t, "example/alpha", "2.0.0")
	beta := newJourneySmallPackage(t, "example/beta", "1.0.0")
	github.SeedRelease(alphaV1.fullName, alphaV1.tag, alphaV1.commit, alphaV1.archive)
	github.SeedRelease(beta.fullName, beta.tag, beta.commit, beta.archive)

	project := newJourneyProject(t, github)
	project.run(0, "init", "--agent", "claude-code", "--freshness", "none", "--non-interactive")
	project.run(0, "install", alphaV1.source, "--non-interactive")
	project.run(0, "install", beta.source+"@"+beta.tag, "--non-interactive")
	project.run(0, "realize")
	assertProjectFile(t, project, nativeSkillDirectory(".claude", alphaV1.fullName, "advocate")+"/references/guide.md",
		alphaV1.body(t, "skills/advocate/references/guide.md"), 0o644)

	// A repeated reconcile against unmoved remote state changes nothing.
	settled := project.snapshot()
	project.run(0, "install", "--non-interactive")
	project.assertUnchanged(settled, "a reconcile with no remote change")

	// The publisher moves latest forward.
	github.SeedRelease(alphaV2.fullName, alphaV2.tag, alphaV2.commit, alphaV2.archive)
	github.ResetRequests()
	project.run(0, "install", "--non-interactive")
	state := loadJourneyState(t, project)
	changed := 0
	for _, locked := range state.Lock.Dependencies {
		switch locked.Source {
		case alphaV2.source:
			if locked.Commit != alphaV2.commit || locked.Tag != alphaV2.tag || locked.PackageVersion != alphaV2.version {
				t.Fatalf("reconcile did not advance alpha: %#v", locked)
			}
			changed++
		case beta.source:
			if locked.Commit != beta.commit || locked.Requested != beta.tag {
				t.Fatalf("reconcile moved the pinned sibling: %#v", locked)
			}
		}
	}
	if changed != 1 {
		t.Fatalf("reconcile changed %d rows, want alpha only", changed)
	}
	if github.RequestCount("/repos/example/beta/releases/latest") != 0 {
		t.Fatalf("reconcile looked up latest for a pinned dependency: %v", github.Requests())
	}
	project.run(0, "realize")
	assertProjectFile(t, project, nativeSkillDirectory(".claude", alphaV2.fullName, "advocate")+"/references/guide.md",
		alphaV2.body(t, "skills/advocate/references/guide.md"), 0o644)
	assertProjectFile(t, project, nativeHookExecutable(".claude", beta.fullName, "session-start", "session-start.sh"),
		beta.body(t, "hooks/session-start.sh"), 0o755)
	return changed + 1
}

func journeyInstallRefusals(t *testing.T) int {
	github := newJourneyGitHub(t)
	project := newJourneyProject(t, github)
	project.run(0, "init", "--agent", "codex", "--freshness", "none", "--non-interactive")
	before := project.snapshot()

	refusals := []struct {
		args []string
		exit int
		want string
	}{
		{args: []string{"install", "", "--non-interactive"}, exit: 2, want: "source must not be empty"},
		{args: []string{"install", "not-a-source", "--non-interactive"}, exit: 1, want: "github:owner/repository"},
		{args: []string{"install", "github:example/missing", "--non-interactive"}, exit: 1, want: "github:example/missing"},
		{args: []string{"install", "github:example/alpha", "--hold", "--non-interactive"}, exit: 2, want: "--hold"},
	}
	for _, refusal := range refusals {
		run := project.run(refusal.exit, refusal.args...)
		if !strings.Contains(run.stderr, refusal.want) {
			t.Errorf("acr %v refusal = %q, want it to name %q", refusal.args, run.stderr, refusal.want)
		}
		if run.stdout != "" {
			t.Errorf("acr %v wrote %q to stdout", refusal.args, run.stdout)
		}
	}
	// A tarball redirect that leaves the trusted origins is refused before the
	// request is made, so no credential and no request reach it.
	alpha := newJourneyPackage(t, "example/alpha", "1.0.0")
	github.SeedRelease(alpha.fullName, alpha.tag, alpha.commit, alpha.archive)
	github.RedirectArchivesToAnUntrustedOrigin()
	untrusted := project.run(1, "install", alpha.source, "--non-interactive")
	if !strings.Contains(untrusted.stderr, "untrusted origin") {
		t.Fatalf("an untrusted archive redirect = %q, want the allowlist refusal", untrusted.stderr)
	}
	if github.AuthorizationSeenOn("archives.example.invalid") {
		t.Fatal("the credential reached an untrusted origin")
	}
	project.assertUnchanged(before, "an install refusal")
	if state := loadJourneyState(t, project); len(state.Lock.Dependencies) != 0 {
		t.Fatalf("a refused install locked %#v", state.Lock.Dependencies)
	}
	return len(refusals) + 1
}

// journeyRealizeSuccess proves realization writes the complete native layout
// for three adapters and that an agent subset leaves the others alone.
func journeyRealizeSuccess(t *testing.T) int {
	github := newJourneyGitHub(t)
	alpha := newJourneyPackage(t, "example/alpha", "1.0.0")
	github.SeedRelease(alpha.fullName, alpha.tag, alpha.commit, alpha.archive)
	project := journeyConsumer(t, github, alpha, "claude-code", "codex", "cursor")

	written := 0
	for _, agent := range []string{".claude", ".codex", ".cursor"} {
		skill := nativeSkillDirectory(agent, alpha.fullName, "advocate")
		assertProjectFile(t, project, skill+"/SKILL.md", alpha.body(t, "skills/advocate/SKILL.md"), 0o644)
		assertProjectFile(t, project, skill+"/references/guide.md", alpha.body(t, "skills/advocate/references/guide.md"), 0o644)
		assertProjectFile(t, project, skill+"/scripts/check.sh", alpha.body(t, "skills/advocate/scripts/check.sh"), 0o755)
		assertProjectFile(t, project, nativeHookExecutable(agent, alpha.fullName, "session-start", "session-start.sh"),
			alpha.body(t, "hooks/session-start.sh"), 0o755)
		written += 4
	}
	// Rules reach each adapter in its own native shape.
	for _, host := range []string{"AGENTS.md", "CLAUDE.md"} {
		body := readProjectFile(t, project, host)
		if !strings.Contains(body, "Verified facts only, revision 1.0.0.") {
			t.Fatalf("%s does not carry the always-on rule: %s", host, body)
		}
		written++
	}
	cursorRule := readProjectFile(t, project, ".cursor/rules/"+nativeArtifactName(alpha.fullName, "scoped")+".mdc")
	if !strings.Contains(cursorRule, "docs/**") || !strings.Contains(cursorRule, "alwaysApply: false") {
		t.Fatalf("cursor path rule lost its activation: %s", cursorRule)
	}
	written++
	// Hook registration reaches every adapter's own configuration file.
	for _, configuration := range []string{".claude/settings.json", ".codex/config.toml", ".cursor/hooks.json"} {
		body := readProjectFile(t, project, configuration)
		if !strings.Contains(body, nativeArtifactName(alpha.fullName, "session-start")) {
			t.Fatalf("%s has no hook registration: %s", configuration, body)
		}
		written++
	}

	// Repeating realization is byte-identical, and a subset leaves the agents
	// it omits exactly as they were.
	settled := project.snapshot()
	project.run(0, "realize")
	project.assertUnchanged(settled, "a repeated acr realize")
	project.run(0, "realize", "--agent", "codex")
	project.assertUnchanged(settled, "acr realize --agent codex")

	dry := project.run(0, "realize", "--dry-run")
	if !strings.Contains(dry.stdout, "0 change") {
		t.Fatalf("acr realize --dry-run on a current project = %q", dry.stdout)
	}
	project.assertUnchanged(settled, "acr realize --dry-run")
	return written
}

func journeyRealizeRefusals(t *testing.T) int {
	project := newJourneyProject(t, nil)
	before := project.snapshot()

	uninitialized := project.run(1, "realize")
	if !strings.Contains(uninitialized.stderr, "agent") {
		t.Fatalf("realize without a selection = %q, want adapter guidance", uninitialized.stderr)
	}
	unknown := project.run(1, "realize", "--agent", "emacs")
	if !strings.Contains(unknown.stderr, "emacs") {
		t.Fatalf("realize --agent emacs = %q", unknown.stderr)
	}
	project.assertUnchanged(before, "a realize refusal")
	return 2
}

// journeyCheckSuccess proves check reports the truth about a realized project
// in all three of its states: current, drifted, and repaired.
func journeyCheckSuccess(t *testing.T) int {
	github := newJourneyGitHub(t)
	alpha := newJourneyPackage(t, "example/alpha", "1.0.0")
	github.SeedRelease(alpha.fullName, alpha.tag, alpha.commit, alpha.archive)
	project := journeyConsumer(t, github, alpha, "claude-code")

	current := project.run(0, "check")
	if !strings.Contains(current.stdout, "current") {
		t.Fatalf("check on a realized project = %q", current.stdout)
	}

	removed := nativeSkillDirectory(".claude", alpha.fullName, "advocate") + "/references/guide.md"
	if err := os.Remove(project.path(removed)); err != nil {
		t.Fatal(err)
	}
	drifted := project.snapshot()
	changes := project.run(3, "check")
	if !strings.Contains(changes.output(), "1 unapplied change") {
		t.Fatalf("check after a deletion = %q, want it to report the unapplied change", changes.output())
	}
	if !strings.Contains(changes.stderr, removed) {
		t.Fatalf("check after a deletion = %q, want it to name %s", changes.stderr, removed)
	}
	structured := project.run(3, "check", "--json")
	failure := journeyError(t, structured.stderr)
	if failure["code"] != "realization_changes" || !strings.Contains(failure["message"].(string), removed) {
		t.Fatalf("check --json error = %#v, want realization_changes naming %s", failure, removed)
	}
	if structured.stdout != "" {
		t.Fatalf("check --json wrote %q to stdout while refusing", structured.stdout)
	}
	project.assertUnchanged(drifted, "acr check on a drifted project")
	assertProjectAbsent(t, project, removed)

	project.run(0, "realize")
	assertProjectFile(t, project, removed, alpha.body(t, "skills/advocate/references/guide.md"), 0o644)
	project.run(0, "check")
	return 3
}

func journeyCheckRefusals(t *testing.T) int {
	github := newJourneyGitHub(t)
	alpha := newJourneyPackage(t, "example/alpha", "1.0.0")
	github.SeedRelease(alpha.fullName, alpha.tag, alpha.commit, alpha.archive)
	project := journeyConsumer(t, github, alpha, "claude-code")
	before := project.snapshot()

	// The locked commit's archive is replaced with different content. The
	// package identity no longer verifies, so check refuses instead of
	// silently repairing the project from unverified bytes.
	tampered := newJourneyPackage(t, "example/alpha", "1.0.0")
	tampered.files[1].body = "# Tampered\n"
	github.PublishSource(alpha.fullName, alpha.tag, alpha.commit,
		journeyGitHubArchive(t, alpha.root, alpha.commit, tampered.files))

	run := project.run(1, "check")
	if !strings.Contains(strings.ToLower(run.output()), "hash") {
		t.Fatalf("check against a tampered archive = %q, want a content hash refusal", run.output())
	}
	project.assertUnchanged(before, "acr check against a tampered archive")
	return 1
}

func readProjectFile(t *testing.T, project *journeyProject, path string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(project.root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(contents)
}
