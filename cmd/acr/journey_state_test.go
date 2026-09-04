package main

import (
	"os"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

// stateJourneys covers the commands that read or move an installed project's
// dependency state: list, outdated, update, the rollback choices, resume,
// uninstall and the session-start freshness policy.
func stateJourneys() []journeyCase {
	return []journeyCase{
		{leaf: "list", name: "mixed-dependencies", kind: journeySuccess, run: journeyListSuccess},
		{leaf: "list", name: "malformed-state", kind: journeyRefusal, run: journeyListRefusals},
		{leaf: "outdated", name: "current-newer-and-pinned", kind: journeySuccess, run: journeyOutdatedSuccess},
		{leaf: "outdated", name: "malformed-state", kind: journeyRefusal, run: journeyOutdatedRefusals},
		{leaf: "update", name: "target-all-and-dry-run", kind: journeySuccess, run: journeyUpdateSuccess},
		{leaf: "update", name: "undeclared-source", kind: journeyRefusal, run: journeyUpdateRefusals},
		{leaf: "resume", name: "barrier-lifecycle", kind: journeySuccess, run: journeyResumeSuccess},
		{leaf: "resume", name: "unheld-and-undeclared", kind: journeyRefusal, run: journeyResumeRefusals},
		{leaf: "uninstall", name: "sibling-last-and-repeat", kind: journeySuccess, run: journeyUninstallSuccess},
		{leaf: "uninstall", name: "repeat-and-modified-output", kind: journeyRefusal, run: journeyUninstallRefusals},
		{leaf: "freshness run", name: "policies-and-throttle", kind: journeySuccess, run: journeyFreshnessSuccess},
		{leaf: "freshness run", name: "unknown-policy", kind: journeyRefusal, run: journeyFreshnessRefusals},
	}
}

// journeyMixedProject installs one latest dependency, one tag pin, and one
// held rollback, each through the ordinary commands.
func journeyMixedProject(t *testing.T, github *journeyGitHub) (*journeyProject, *journeyPackage, *journeyPackage, *journeyPackage) {
	t.Helper()
	alpha := newJourneyPackage(t, "example/alpha", "1.0.0")
	beta := newJourneySmallPackage(t, "example/beta", "1.0.0")
	gammaV1 := newJourneySmallPackage(t, "example/gamma", "1.0.0")
	gammaV2 := newJourneySmallPackage(t, "example/gamma", "2.0.0")
	github.SeedRelease(alpha.fullName, alpha.tag, alpha.commit, alpha.archive)
	github.SeedRelease(beta.fullName, beta.tag, beta.commit, beta.archive)
	github.SeedRelease(gammaV1.fullName, gammaV1.tag, gammaV1.commit, gammaV1.archive)
	github.SeedRelease(gammaV2.fullName, gammaV2.tag, gammaV2.commit, gammaV2.archive)

	project := newJourneyProject(t, github)
	project.run(0, "init", "--agent", "codex", "--freshness", "none", "--non-interactive")
	project.run(0, "install", alpha.source, "--non-interactive")
	project.run(0, "install", beta.source+"@"+beta.tag, "--non-interactive")
	project.run(0, "install", gammaV2.source, "--non-interactive")
	project.run(0, "install", gammaV1.source+"@"+gammaV1.tag, "--hold", "--non-interactive")
	return project, alpha, beta, gammaV1
}

// journeyListSuccess proves list reports the state on disk, in both formats,
// without reaching the network.
func journeyListSuccess(t *testing.T) int {
	github := newJourneyGitHub(t)
	project, alpha, beta, gamma := journeyMixedProject(t, github)
	settled := project.snapshot()
	github.ResetRequests()

	text := project.run(0, "list")
	for _, source := range []string{alpha.source, beta.source, gamma.source} {
		if !strings.Contains(text.stdout, source) {
			t.Errorf("acr list = %q, want it to name %s", text.stdout, source)
		}
	}
	if !strings.Contains(text.stdout, "held") {
		t.Errorf("acr list = %q, want the held row marked", text.stdout)
	}
	structured := project.run(0, "list", "--json")
	result := journeyResult(t, structured.stdout)
	rows, ok := result["dependencies"].([]any)
	if !ok || len(rows) != 3 {
		t.Fatalf("acr list --json dependencies = %#v, want three rows", result["dependencies"])
	}
	state := loadJourneyState(t, project)
	if len(state.Lock.Dependencies) != len(rows) {
		t.Fatalf("acr list --json reported %d rows for %d locked dependencies", len(rows), len(state.Lock.Dependencies))
	}
	if requests := github.Requests(); len(requests) != 0 {
		t.Fatalf("acr list reached the network: %v", requests)
	}
	project.assertUnchanged(settled, "acr list")

	empty := newJourneyProject(t, github)
	emptyRun := empty.run(0, "list")
	if !strings.Contains(strings.ToLower(emptyRun.stdout), "no dependencies") {
		t.Fatalf("acr list on an empty project = %q", emptyRun.stdout)
	}
	return len(rows)
}

func journeyListRefusals(t *testing.T) int {
	project := newJourneyProject(t, nil)
	reverify2Put(t, project.root, "agents.yaml", "schemaVersion: [broken\n", 0o644)
	before := project.snapshot()

	run := project.run(1, "list")
	if strings.Contains(strings.ToLower(run.stdout), "no dependencies") {
		t.Fatalf("acr list certified a malformed project as empty: %q", run.output())
	}
	if !strings.Contains(run.stderr, "agents.yaml") {
		t.Fatalf("acr list refusal = %q, want it to name the invalid file", run.stderr)
	}
	project.assertUnchanged(before, "acr list on a malformed project")
	return 1
}

// journeyOutdatedSuccess proves outdated distinguishes a checked-current row
// from a genuinely newer one, and never asks about a pin.
func journeyOutdatedSuccess(t *testing.T) int {
	github := newJourneyGitHub(t)
	alphaV1 := newJourneyPackage(t, "example/alpha", "1.0.0")
	alphaV2 := newJourneyPackage(t, "example/alpha", "2.0.0")
	beta := newJourneySmallPackage(t, "example/beta", "1.0.0")
	github.SeedRelease(alphaV1.fullName, alphaV1.tag, alphaV1.commit, alphaV1.archive)
	github.SeedRelease(beta.fullName, beta.tag, beta.commit, beta.archive)

	project := newJourneyProject(t, github)
	project.run(0, "init", "--agent", "codex", "--freshness", "none", "--non-interactive")
	project.run(0, "install", alphaV1.source, "--non-interactive")
	project.run(0, "install", beta.source+"@"+beta.tag, "--non-interactive")
	settled := project.snapshot()

	github.ResetRequests()
	current := project.run(0, "outdated")
	if !strings.Contains(strings.ToLower(current.stdout), "current") {
		t.Fatalf("acr outdated with nothing newer = %q", current.stdout)
	}
	if github.RequestCount("/repos/example/alpha/releases/latest") != 1 {
		t.Fatalf("acr outdated did not check latest exactly once: %v", github.Requests())
	}
	if github.RequestCount("/repos/example/beta/releases/latest") != 0 {
		t.Fatalf("acr outdated checked latest for a pinned dependency: %v", github.Requests())
	}

	github.SeedRelease(alphaV2.fullName, alphaV2.tag, alphaV2.commit, alphaV2.archive)
	newer := project.run(0, "outdated")
	if !strings.Contains(newer.stdout, "1 latest") || !strings.Contains(newer.stdout, "outdated") {
		t.Fatalf("acr outdated = %q, want it to report the actionable row", newer.stdout)
	}
	structured := project.run(0, "outdated", "--json")
	result := journeyResult(t, structured.stdout)
	rows, ok := result["outdated"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("acr outdated --json outdated = %#v, want one actionable row", result["outdated"])
	}
	row, ok := rows[0].(map[string]any)
	if !ok || row["source"] != alphaV1.source || row["latestTag"] != alphaV2.tag || row["currentTag"] != alphaV1.tag {
		t.Fatalf("acr outdated --json row = %#v, want %s moving %s -> %s", rows[0], alphaV1.source, alphaV1.tag, alphaV2.tag)
	}
	project.assertUnchanged(settled, "acr outdated")

	// An all-pinned project says so instead of certifying currency.
	pinned := newJourneyProject(t, github)
	pinned.run(0, "init", "--agent", "codex", "--freshness", "none", "--non-interactive")
	pinned.run(0, "install", beta.source+"@"+beta.tag, "--non-interactive")
	pinnedRun := pinned.run(0, "outdated")
	if !strings.Contains(strings.ToLower(pinnedRun.stdout), "no latest") {
		t.Fatalf("acr outdated on an all-pinned project = %q", pinnedRun.stdout)
	}
	return len(rows) + 1
}

func journeyOutdatedRefusals(t *testing.T) int {
	project := newJourneyProject(t, nil)
	reverify2Put(t, project.root, "agents.yaml", "schemaVersion: [broken\n", 0o644)
	before := project.snapshot()

	run := project.run(1, "outdated")
	if strings.Contains(run.output(), "are current") {
		t.Fatalf("acr outdated certified a malformed project: %q", run.output())
	}
	project.assertUnchanged(before, "acr outdated on a malformed project")
	return 1
}

// journeyUpdateSuccess proves update moves exactly the rows it was asked to
// move and predicts the same move without writing anything.
func journeyUpdateSuccess(t *testing.T) int {
	github := newJourneyGitHub(t)
	alphaV1 := newJourneyPackage(t, "example/alpha", "1.0.0")
	alphaV2 := newJourneyPackage(t, "example/alpha", "2.0.0")
	betaV1 := newJourneySmallPackage(t, "example/beta", "1.0.0")
	betaV2 := newJourneySmallPackage(t, "example/beta", "2.0.0")
	gamma := newJourneySmallPackage(t, "example/gamma", "1.0.0")
	github.SeedRelease(alphaV1.fullName, alphaV1.tag, alphaV1.commit, alphaV1.archive)
	github.SeedRelease(betaV1.fullName, betaV1.tag, betaV1.commit, betaV1.archive)
	github.SeedRelease(gamma.fullName, gamma.tag, gamma.commit, gamma.archive)

	project := newJourneyProject(t, github)
	project.run(0, "init", "--agent", "codex", "--freshness", "none", "--non-interactive")
	project.run(0, "install", alphaV1.source, "--non-interactive")
	project.run(0, "install", betaV1.source, "--non-interactive")
	project.run(0, "install", gamma.source+"@"+gamma.tag, "--non-interactive")
	project.run(0, "realize")

	github.SeedRelease(alphaV2.fullName, alphaV2.tag, alphaV2.commit, alphaV2.archive)
	github.SeedRelease(betaV2.fullName, betaV2.tag, betaV2.commit, betaV2.archive)

	settled := project.snapshot()
	dry := project.run(0, "update", alphaV1.source, "--dry-run", "--json")
	dryResult := journeyResult(t, dry.stdout)
	if dryResult["changed"] != true {
		t.Fatalf("acr update --dry-run result = %#v, want the change it would make", dryResult)
	}
	project.assertUnchanged(settled, "acr update --dry-run")

	project.run(0, "update", alphaV1.source)
	moved := lockedCommits(t, project)
	if moved[alphaV2.source] != alphaV2.commit {
		t.Fatalf("targeted update did not advance alpha: %v", moved)
	}
	if moved[betaV1.source] != betaV1.commit {
		t.Fatalf("targeted update moved beta: %v", moved)
	}

	project.run(0, "update")
	all := lockedCommits(t, project)
	if all[betaV2.source] != betaV2.commit {
		t.Fatalf("acr update did not advance every eligible row: %v", all)
	}
	if all[gamma.source] != gamma.commit {
		t.Fatalf("acr update moved a pinned dependency: %v", all)
	}
	project.run(0, "realize")
	assertProjectFile(t, project, nativeSkillDirectory(".codex", alphaV2.fullName, "advocate")+"/references/guide.md",
		alphaV2.body(t, "skills/advocate/references/guide.md"), 0o644)
	assertProjectFile(t, project, nativeHookExecutable(".codex", betaV2.fullName, "session-start", "session-start.sh"),
		betaV2.body(t, "hooks/session-start.sh"), 0o755)

	stable := project.snapshot()
	project.run(0, "update")
	project.assertUnchanged(stable, "a repeated acr update")
	return 2
}

func journeyUpdateRefusals(t *testing.T) int {
	github := newJourneyGitHub(t)
	alpha := newJourneyPackage(t, "example/alpha", "1.0.0")
	github.SeedRelease(alpha.fullName, alpha.tag, alpha.commit, alpha.archive)
	project := newJourneyProject(t, github)
	project.run(0, "init", "--agent", "codex", "--freshness", "none", "--non-interactive")
	project.run(0, "install", alpha.source, "--non-interactive")
	before := project.snapshot()

	run := project.run(2, "update", "github:example/absent", "--json")
	failure := journeyError(t, run.stderr)
	if failure["code"] != cli.CodeDependencyNotDeclared {
		t.Fatalf("acr update on an undeclared source = %#v, want %s", failure, cli.CodeDependencyNotDeclared)
	}
	project.assertUnchanged(before, "an update refusal")
	return 1
}

// journeyResumeSuccess walks a rollback hold from the choice that created it
// to the explicit resume that ends it.
func journeyResumeSuccess(t *testing.T) int {
	github := newJourneyGitHub(t)
	v1 := newJourneyPackage(t, "example/alpha", "1.0.0")
	v2 := newJourneyPackage(t, "example/alpha", "2.0.0")
	v3 := newJourneyPackage(t, "example/alpha", "3.0.0")
	beta := newJourneySmallPackage(t, "example/beta", "1.0.0")
	github.SeedRelease(v1.fullName, v1.tag, v1.commit, v1.archive)
	github.SeedRelease(v2.fullName, v2.tag, v2.commit, v2.archive)
	github.SeedRelease(beta.fullName, beta.tag, beta.commit, beta.archive)

	project := newJourneyProject(t, github)
	project.run(0, "init", "--agent", "codex", "--freshness", "none", "--non-interactive")
	project.run(0, "install", v1.source, "--non-interactive")
	project.run(0, "install", beta.source+"@"+beta.tag, "--non-interactive")

	// The rollback needs an explicit choice; without one it refuses.
	unchosen := project.run(2, "install", v1.source+"@"+v1.tag, "--non-interactive", "--json")
	if journeyError(t, unchosen.stderr)["code"] != "downgrade_choice_required" {
		t.Fatalf("a rollback without a choice = %q", unchosen.stderr)
	}

	project.run(0, "install", v1.source+"@"+v1.tag, "--hold", "--non-interactive")
	held := loadJourneyState(t, project)
	declaration := findDeclaration(t, held, v1.source)
	if declaration.Requested != "latest" || declaration.Hold == nil {
		t.Fatalf("hold declaration = %#v, want latest behind a hold", declaration)
	}
	if declaration.Hold.Pin != v1.tag || declaration.Hold.Rejected != v2.tag {
		t.Fatalf("hold = %#v, want pin %s behind barrier %s", declaration.Hold, v1.tag, v2.tag)
	}
	if commit := lockedCommits(t, project)[v1.source]; commit != v1.commit {
		t.Fatalf("held lock = %s, want the known-good %s", commit, v1.commit)
	}
	project.run(0, "realize")
	assertProjectFile(t, project, nativeSkillDirectory(".codex", v1.fullName, "advocate")+"/references/guide.md",
		v1.body(t, "skills/advocate/references/guide.md"), 0o644)

	// A hold survives reconcile and update, and a release beyond the barrier
	// names the only command that can cross it.
	settled := project.snapshot()
	project.run(0, "install", "--non-interactive")
	project.run(0, "update")
	project.assertUnchanged(settled, "a reconcile and update against a held dependency")

	github.SeedRelease(v3.fullName, v3.tag, v3.commit, v3.archive)
	beyond := project.run(0, "outdated")
	if !strings.Contains(beyond.stdout, "acr resume "+v1.source) {
		t.Fatalf("acr outdated beyond the barrier = %q, want it to name the resume command", beyond.stdout)
	}
	project.run(0, "install", "--non-interactive")
	if commit := lockedCommits(t, project)[v1.source]; commit != v1.commit {
		t.Fatalf("a reconcile crossed the barrier to %s", commit)
	}

	dry := project.run(0, "resume", v1.source, "--dry-run")
	if !strings.Contains(dry.stdout, v1.source) {
		t.Fatalf("acr resume --dry-run = %q", dry.stdout)
	}
	project.assertUnchanged(settled, "acr resume --dry-run")

	project.run(0, "resume", v1.source)
	resumed := loadJourneyState(t, project)
	if resumedDeclaration := findDeclaration(t, resumed, v1.source); resumedDeclaration.Hold != nil {
		t.Fatalf("resume left a hold: %#v", resumedDeclaration.Hold)
	}
	if commit := lockedCommits(t, project)[v1.source]; commit != v3.commit {
		t.Fatalf("resume locked %s, want the newest %s", commit, v3.commit)
	}
	if commit := lockedCommits(t, project)[beta.source]; commit != beta.commit {
		t.Fatalf("resume moved the pinned sibling to %s", commit)
	}
	project.run(0, "realize")
	assertProjectFile(t, project, nativeSkillDirectory(".codex", v3.fullName, "advocate")+"/references/guide.md",
		v3.body(t, "skills/advocate/references/guide.md"), 0o644)
	project.run(0, "check")
	return 3
}

func journeyResumeRefusals(t *testing.T) int {
	github := newJourneyGitHub(t)
	alpha := newJourneyPackage(t, "example/alpha", "1.0.0")
	github.SeedRelease(alpha.fullName, alpha.tag, alpha.commit, alpha.archive)
	project := newJourneyProject(t, github)
	project.run(0, "init", "--agent", "codex", "--freshness", "none", "--non-interactive")
	project.run(0, "install", alpha.source, "--non-interactive")
	before := project.snapshot()

	undeclared := project.run(2, "resume", "github:example/absent", "--json")
	if journeyError(t, undeclared.stderr)["code"] != cli.CodeDependencyNotDeclared {
		t.Fatalf("resume of an undeclared source = %q", undeclared.stderr)
	}
	unheld := project.run(1, "resume", alpha.source, "--json")
	if journeyError(t, unheld.stderr)["code"] == "" {
		t.Fatalf("resume of an unheld dependency = %q", unheld.stderr)
	}
	project.assertUnchanged(before, "a resume refusal")
	return 2
}

// journeyUninstallSuccess proves removal prunes exactly one package: its own
// outputs disappear, the sibling and the operator's own files survive, and the
// last removal works with no remote at all.
func journeyUninstallSuccess(t *testing.T) int {
	github := newJourneyGitHub(t)
	alpha := newJourneyPackage(t, "example/alpha", "1.0.0")
	beta := newJourneySmallPackage(t, "example/beta", "1.0.0")
	github.SeedRelease(alpha.fullName, alpha.tag, alpha.commit, alpha.archive)
	github.SeedRelease(beta.fullName, beta.tag, beta.commit, beta.archive)

	project := newJourneyProject(t, github)
	reverify2Put(t, project.root, "notes.md", "# Operator notes\n", 0o644)
	verify8GitCommit(t, project.root)
	project.run(0, "init", "--agent", "claude-code", "--freshness", "none", "--non-interactive")
	project.run(0, "install", alpha.source, "--non-interactive")
	project.run(0, "install", beta.source, "--non-interactive")
	project.run(0, "realize")

	owned := []string{
		nativeSkillDirectory(".claude", alpha.fullName, "advocate") + "/SKILL.md",
		nativeSkillDirectory(".claude", alpha.fullName, "advocate") + "/references/guide.md",
		nativeSkillDirectory(".claude", alpha.fullName, "advocate") + "/scripts/check.sh",
		nativeHookExecutable(".claude", alpha.fullName, "session-start", "session-start.sh"),
	}
	survivors := []string{nativeHookExecutable(".claude", beta.fullName, "session-start", "session-start.sh")}
	for _, path := range append(append([]string(nil), owned...), survivors...) {
		if _, err := os.Lstat(project.path(path)); err != nil {
			t.Fatalf("fixture is incomplete: %v", err)
		}
	}

	dry := project.run(0, "uninstall", alpha.source, "--dry-run")
	if !strings.Contains(dry.stdout, alpha.source) {
		t.Fatalf("acr uninstall --dry-run = %q", dry.stdout)
	}
	settled := project.snapshot()
	project.run(0, "uninstall", alpha.source, "--dry-run")
	project.assertUnchanged(settled, "acr uninstall --dry-run")

	project.run(0, "uninstall", alpha.source)
	removed := 0
	for _, path := range owned {
		assertProjectAbsent(t, project, path)
		removed++
	}
	for _, path := range survivors {
		assertProjectFile(t, project, path, beta.body(t, "hooks/session-start.sh"), 0o755)
	}
	assertProjectFile(t, project, "notes.md", "# Operator notes\n", 0o644)
	state := loadJourneyState(t, project)
	if len(state.Project.Dependencies) != 1 || state.Project.Dependencies[0].Source != beta.source {
		t.Fatalf("uninstall left %#v, want beta alone", state.Project.Dependencies)
	}

	// The last dependency comes out with no remote reachable at all.
	offline := &journeyProject{t: t, root: project.root, stateHome: project.stateHome}
	offline.run(0, "uninstall", beta.source)
	final := loadJourneyState(t, offline)
	if len(final.Project.Dependencies) != 0 || len(final.Lock.Dependencies) != 0 {
		t.Fatalf("the last uninstall left %#v", final)
	}
	assertProjectFile(t, project, "notes.md", "# Operator notes\n", 0o644)
	exclude := readProjectFile(t, project, gitExcludePath)
	if strings.Contains(exclude, nativeArtifactName(alpha.fullName, "advocate")) {
		t.Fatalf(".git/info/exclude still lists a removed generated path: %s", exclude)
	}
	return removed
}

func journeyUninstallRefusals(t *testing.T) int {
	github := newJourneyGitHub(t)
	alpha := newJourneyPackage(t, "example/alpha", "1.0.0")
	github.SeedRelease(alpha.fullName, alpha.tag, alpha.commit, alpha.archive)
	project := journeyConsumer(t, github, alpha, "claude-code")

	undeclared := project.run(2, "uninstall", "github:example/absent", "--json")
	if journeyError(t, undeclared.stderr)["code"] != cli.CodeDependencyNotDeclared {
		t.Fatalf("uninstall of an undeclared source = %q", undeclared.stderr)
	}

	// A hand-edited package output is a conflict, not something to overwrite.
	tampered := nativeSkillDirectory(".claude", alpha.fullName, "advocate") + "/SKILL.md"
	reverify2Put(t, project.root, tampered, "# Edited by the operator\n", 0o644)
	before := project.snapshot()
	conflict := project.run(4, "uninstall", alpha.source, "--json")
	if journeyError(t, conflict.stderr)["code"] != "realization_conflict" {
		t.Fatalf("uninstall over an edited output = %q, want realization_conflict", conflict.stderr)
	}
	project.assertUnchanged(before, "a refused uninstall")
	assertProjectFile(t, project, tampered, "# Edited by the operator\n", 0o644)
	return 2
}

// journeyFreshnessSuccess proves each session-start policy does what it
// promises, that the second attempt inside one window is throttled, and that
// the generated wrapper is a real executable hook.
func journeyFreshnessSuccess(t *testing.T) int {
	github := newJourneyGitHub(t)
	v1 := newJourneyPackage(t, "example/alpha", "1.0.0")
	v2 := newJourneyPackage(t, "example/alpha", "2.0.0")
	github.SeedRelease(v1.fullName, v1.tag, v1.commit, v1.archive)

	project := newJourneyProject(t, github)
	project.run(0, "init", "--agent", "codex", "--freshness", "outdated", "--non-interactive")
	project.run(0, "install", v1.source, "--non-interactive")
	project.run(0, "realize")

	// The realized project carries the ACR session-start wrapper.
	wrapper := ".codex/hooks/" + nativeArtifactName("jbaruch/agentic-context-registry", "freshness-session-start") + "/session-start.sh"
	body := readProjectFile(t, project, wrapper)
	if !strings.Contains(body, "freshness run") {
		t.Fatalf("generated wrapper = %q, want the throttled freshness invocation", body)
	}
	if info, err := os.Stat(project.path(wrapper)); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("generated wrapper mode = %v, %v", info, err)
	}
	// The selected policy reaches the wrapper as the registered argument
	// vector, not as a literal inside the generated script.
	registration := readProjectFile(t, project, ".codex/config.toml")
	if !strings.Contains(registration, "--policy") || !strings.Contains(registration, "outdated") {
		t.Fatalf(".codex/config.toml = %q, want the policy in the hook arguments", registration)
	}

	// policy none reaches no network and writes no machine state.
	github.ResetRequests()
	settled := project.snapshot()
	none := project.run(0, "freshness", "run", "--policy", "none", "--json")
	if journeyResult(t, none.stdout)["policy"] != "none" {
		t.Fatalf("freshness run --policy none = %q", none.stdout)
	}
	if requests := github.Requests(); len(requests) != 0 {
		t.Fatalf("policy none reached the network: %v", requests)
	}
	project.assertUnchanged(settled, "acr freshness run --policy none")

	// policy outdated reports the newer release and changes nothing.
	github.SeedRelease(v2.fullName, v2.tag, v2.commit, v2.archive)
	outdated := project.run(0, "freshness", "run", "--policy", "outdated", "--json")
	if !strings.Contains(outdated.output(), v2.tag) {
		t.Fatalf("freshness run --policy outdated = %q, want it to name %s", outdated.output(), v2.tag)
	}
	project.assertUnchanged(settled, "acr freshness run --policy outdated")

	// The same policy inside the same window is throttled, without a second
	// remote check, and without any sleep or clock assertion.
	github.ResetRequests()
	throttled := project.run(0, "freshness", "run", "--policy", "outdated", "--json")
	if journeyResult(t, throttled.stdout)["throttled"] != true {
		t.Fatalf("a second attempt in one window = %q, want it throttled", throttled.stdout)
	}
	if requests := github.Requests(); len(requests) != 0 {
		t.Fatalf("a throttled attempt reached the network: %v", requests)
	}

	// A policy change is not throttled, and install reconciles for real.
	install := project.run(0, "freshness", "run", "--policy", "install", "--json")
	assertNoCredentialLeak(t, install)
	if commit := lockedCommits(t, project)[v1.source]; commit != v2.commit {
		t.Fatalf("freshness install left the lock at %s, want %s", commit, v2.commit)
	}
	assertProjectFile(t, project, nativeSkillDirectory(".codex", v2.fullName, "advocate")+"/references/guide.md",
		v2.body(t, "skills/advocate/references/guide.md"), 0o644)
	project.run(0, "check")
	return 3
}

func journeyFreshnessRefusals(t *testing.T) int {
	project := newJourneyProject(t, nil)
	before := project.snapshot()

	run := project.run(2, "freshness", "run", "--policy", "sometimes")
	if !strings.Contains(run.stderr, "sometimes") {
		t.Fatalf("freshness run --policy sometimes = %q", run.stderr)
	}
	missingValue := project.run(2, "freshness", "run", "--policy")
	if !strings.Contains(missingValue.stderr, "--policy") {
		t.Fatalf("freshness run --policy without a value = %q", missingValue.stderr)
	}
	extra := project.run(2, "freshness", "run", "twice")
	if !strings.Contains(extra.stderr, "usage") {
		t.Fatalf("freshness run with an extra operand = %q", extra.stderr)
	}
	project.assertUnchanged(before, "a freshness refusal")
	return 3
}

func lockedCommits(t *testing.T, project *journeyProject) map[string]string {
	t.Helper()
	commits := map[string]string{}
	for _, locked := range loadJourneyState(t, project).Lock.Dependencies {
		commits[locked.Source] = locked.Commit
	}
	return commits
}

func findDeclaration(t *testing.T, state dependency.State, source string) dependency.Declaration {
	t.Helper()
	for _, declaration := range state.Project.Dependencies {
		if declaration.Source == source {
			return declaration
		}
	}
	t.Fatalf("project declares no %s", source)
	return dependency.Declaration{}
}
