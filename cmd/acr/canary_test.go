package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// canaryPackageRoot is the fixture the manual native-agent check installs. The
// operator copies this directory; this test drives the same bytes through the
// supported CLI, so the recipe in docs/manual-conformance.md is a procedure that
// has been executed rather than one that has been written down.
const canaryPackageRoot = "../../docs/canary-package"

// Sentinels the canary fixture carries. A human looks for exactly these strings
// in an agent session, so a fixture that stopped carrying one would make the
// manual observation unfalsifiable.
const (
	canaryAlwaysSentinel  = "ACR-CANARY-ALWAYS-7f3a"
	canaryScopedSentinel  = "ACR-CANARY-SCOPED-7f3a"
	canarySkillSentinel   = "ACR-CANARY-SKILL-7f3a"
	canaryHookMarker      = "ACR-CANARY-SESSION-START-7f3a"
	canaryPackageName     = "acr/canary"
	canaryScopedGlob      = "docs/**"
	canaryHumanRuleBody   = "# Operator rules\nKeep these bytes exactly.\n"
	canaryHumanRuleMode   = fs.FileMode(0o640)
	canaryVendoredSource  = "vendor:acr/canary"
	canaryRealizedSkillID = "acr-canary"

	// The two files the Cursor scoped observation attaches, one on each side of
	// the rule's docs/** glob.
	canaryInGlobProbe    = "docs/canary-in-scope.md"
	canaryOutOfGlobProbe = "canary-out-of-scope.md"
)

// TestCanaryFixturePreparesEveryAdapter is the preparation half of the manual
// native-agent check: it proves the canary installs and realizes for all three
// adapters from a purely local vendored source, with no publication and no
// network, and that everything the manual observations look for is on disk
// before any agent is started.
//
// It deliberately does not run an agent, dispatch the hook, or execute any
// canary artifact. Runtime loading is the manual half and stays manual.
func TestCanaryFixturePreparesEveryAdapter(t *testing.T) {
	project := newJourneyProject(t, nil)

	// Phase 1 — a human file with known bytes and mode, so the later teardown
	// has something whose preservation the operator can check, plus the two
	// files the scoped-rule observation attaches: one inside the rule's glob and
	// one outside it.
	reverify2Put(t, project.root, "AGENTS.md", canaryHumanRuleBody, canaryHumanRuleMode)
	reverify2Put(t, project.root, canaryInGlobProbe, "# In scope\nA file the scoped rule glob matches.\n", 0o644)
	reverify2Put(t, project.root, canaryOutOfGlobProbe, "# Out of scope\nA file the scoped rule glob does not match.\n", 0o644)

	// Phase 2 — the consumer already uses all three agents. Migration derives
	// its agent selection from what it detects, so a canary that has to reach
	// three adapters starts from a project the three adapters are visible in.
	for _, marker := range []string{".claude/skills", ".codex/skills", ".cursor/skills"} {
		if err := os.MkdirAll(project.path(marker), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Phase 3 — the canary arrives as a local Tessl package. Nothing is
	// published and nothing is downloaded.
	copyCanaryPackage(t, project.path(".tessl/plugins/"+canaryPackageName))
	reverify2Put(t, project.root, "tessl.json",
		`{"name":"canary-consumer","dependencies":{"`+canaryPackageName+`":{"version":"1.0.0"}}}`+"\n", 0o644)

	// Phase 4 — vendor the package, then realize. Migration derives its own
	// agent selection and refuses an agents.yaml that already disagrees with
	// it, so the selection is what migration detected and never one written
	// over the top of it.
	project.run(0, "migrate", "tessl", "--vendor-unmapped", "--non-interactive")
	state := loadJourneyState(t, project)
	if got := strings.Join(state.Project.Agents, ","); got != "claude-code,codex,cursor" {
		t.Fatalf("migration selected %q, want all three adapters", got)
	}
	project.run(0, "realize")

	if len(state.Lock.Dependencies) != 1 || state.Lock.Dependencies[0].Source != canaryVendoredSource {
		t.Fatalf("locked %#v, want the vendored canary", state.Lock.Dependencies)
	}

	// Phase 5 — everything the manual observations look for exists, per adapter.
	for _, agent := range []string{".claude", ".codex", ".cursor"} {
		skill := nativeSkillDirectory(agent, canaryPackageName, canaryRealizedSkillID)
		body := readProjectFile(t, project, skill+"/SKILL.md")
		if !strings.Contains(body, canarySkillSentinel) {
			t.Errorf("%s skill does not carry %s: %s", agent, canarySkillSentinel, body)
		}
		hook := nativeHookExecutable(agent, canaryPackageName, "session-start", "session-start.sh")
		script := readProjectFile(t, project, hook)
		if !strings.Contains(script, canaryHookMarker) {
			t.Errorf("%s hook does not emit %s: %s", agent, canaryHookMarker, script)
		}
		if info, err := os.Stat(project.path(hook)); err != nil || info.Mode().Perm() != 0o755 {
			t.Errorf("%s hook mode = %v, %v, want an executable hook", info, err, agent)
		}
	}

	// The always-on rule reaches each adapter's own rule host.
	for _, host := range []string{"AGENTS.md", "CLAUDE.md"} {
		if body := readProjectFile(t, project, host); !strings.Contains(body, canaryAlwaysSentinel) {
			t.Errorf("%s does not carry %s: %s", host, canaryAlwaysSentinel, body)
		}
	}
	// The operator's own bytes survive the block ACR spliced into AGENTS.md.
	if body := readProjectFile(t, project, "AGENTS.md"); !strings.Contains(body, "Keep these bytes exactly.") {
		t.Errorf("realization dropped the operator's own AGENTS.md content: %s", body)
	}

	// The scoped rule keeps its activation, which is the only observation that
	// distinguishes a scoped rule from an always-on one.
	cursorRule := readProjectFile(t, project, ".cursor/rules/"+nativeArtifactName(canaryPackageName, "scoped")+".mdc")
	if !strings.Contains(cursorRule, canaryScopedGlob) || !strings.Contains(cursorRule, "alwaysApply: false") {
		t.Errorf("cursor scoped rule lost its activation: %s", cursorRule)
	}
	if !strings.Contains(cursorRule, canaryScopedSentinel) {
		t.Errorf("cursor scoped rule does not carry %s: %s", canaryScopedSentinel, cursorRule)
	}
	// Cursor is the only adapter that realizes path scoping, and the scoped
	// observation is written for Cursor alone because of it. Claude Code and
	// Codex render both rules into the shared always-on host, so the scoped
	// sentinel is expected in every one of their sessions and an out-of-glob
	// absence would be testing a feature they do not implement. If that ever
	// changes, the observation table changes with it.
	for _, host := range []string{"AGENTS.md", "CLAUDE.md"} {
		if body := readProjectFile(t, project, host); !strings.Contains(body, canaryScopedSentinel) {
			t.Errorf("%s does not carry the scoped rule as always-on text: %s", host, body)
		}
	}

	// Both probe files survive realization untouched: an observation that
	// attaches one of them is attaching the operator's own file.
	assertProjectFile(t, project, canaryInGlobProbe, "# In scope\nA file the scoped rule glob matches.\n", 0o644)
	assertProjectFile(t, project, canaryOutOfGlobProbe, "# Out of scope\nA file the scoped rule glob does not match.\n", 0o644)

	// Each adapter registers the hook in its own configuration file, which is
	// what makes "the hook never fired" a finding rather than a setup mistake.
	for _, configuration := range []string{".claude/settings.json", ".codex/config.toml", ".cursor/hooks.json"} {
		if body := readProjectFile(t, project, configuration); !strings.Contains(body, nativeArtifactName(canaryPackageName, "session-start")) {
			t.Errorf("%s has no canary hook registration: %s", configuration, body)
		}
	}

	// Phase 6 — the project is current and stable, so an observation that fails
	// later is the agent's behaviour and not a half-prepared fixture.
	project.run(0, "check")
	settled := project.snapshot()
	project.run(0, "realize")
	project.assertUnchanged(settled, "a repeated realize of the canary")

	// Phase 7 — teardown is only correct after the observations, so the check
	// proves it removes exactly what the observations needed.
	project.run(0, "uninstall", canaryVendoredSource)
	assertProjectAbsent(t, project, nativeSkillDirectory(".cursor", canaryPackageName, canaryRealizedSkillID)+"/SKILL.md")
	if body := readProjectFile(t, project, "AGENTS.md"); strings.Contains(body, canaryAlwaysSentinel) {
		t.Errorf("uninstall left the canary rule behind: %s", body)
	}
	if body := readProjectFile(t, project, "AGENTS.md"); !strings.Contains(body, "Keep these bytes exactly.") {
		t.Errorf("uninstall took the operator's own AGENTS.md content with it: %s", body)
	}
}

// canaryShims are the programs a hook that reached beyond its scratch log would
// have to run. Each is shadowed on PATH so an attempt is recorded rather than
// performed, whether the hook names the program directly or reaches it through
// a variable.
var canaryShims = []string{
	"curl", "wget", "nc", "ssh", "scp", "git", "npm", "pip", "pip3",
	"python", "python3", "sudo", "rm", "chmod", "chown", "open",
	"osascript", "launchctl", "defaults", "crontab",
}

// TestCanaryHookIsLocalOnly holds the shipped hook to what the manual check
// promises an operator: it appends a marker to the log it was given and does
// nothing else. A hook that grew a network call or an operator-configuration
// write would make the canary unsafe to run in a real agent session.
//
// It runs the hook in a sandbox and decides the claim on what the process did —
// which programs it ran, which paths it wrote — rather than on how its source is
// spelled. A source scan reads a harmless comment as a reach and an indirect
// call as none.
func TestCanaryHookIsLocalOnly(t *testing.T) {
	t.Parallel()

	hook := canaryHookPath(t)
	home := t.TempDir()
	sandbox := t.TempDir()
	witness := filepath.Join(t.TempDir(), "invoked.log")
	shims := canaryShimPath(t, witness)

	// A log path carrying a space: an unquoted expansion would split it and
	// neither half would end up holding the marker.
	log := filepath.Join(sandbox, "evidence directory", "session start.log")

	// The hook is executed directly rather than through an interpreter, so its
	// shebang and its executable bit are part of what passes here.
	stderr, exit := runCanaryHookIsolated(t, hook, sandbox, log, home, shims)
	if exit != 0 || stderr != "" {
		t.Fatalf("the canary hook exited %d with stderr %q", exit, stderr)
	}
	if lines := canaryMarkerLines(t, log); lines != 1 {
		t.Fatalf("a log path containing a space holds %d marker lines, want 1", lines)
	}
	if _, err := os.Stat(witness); err == nil {
		t.Errorf("the canary hook ran %s", strings.TrimSpace(readFileForTest(t, witness)))
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}

	// The log and its parent are the only paths the hook created. Anything
	// else it wrote — beside the working directory, into the home it was
	// given — shows up here as an extra entry.
	want := []string{"evidence directory", "evidence directory/session start.log"}
	if got := canaryTree(t, sandbox); !slices.Equal(got, want) {
		t.Errorf("the canary hook left %v in its working directory, want %v", got, want)
	}
	if got := canaryTree(t, home); len(got) != 0 {
		t.Errorf("the canary hook wrote %v into the home directory it was given", got)
	}

	// Each dispatch appends, so a second session is a greater count rather
	// than a rewrite.
	if _, exit := runCanaryHookIsolated(t, hook, sandbox, log, home, shims); exit != 0 {
		t.Fatalf("the second dispatch exited %d", exit)
	}
	if lines := canaryMarkerLines(t, log); lines != 2 {
		t.Fatalf("a second dispatch left %d marker lines, want 2", lines)
	}
}

// TestCanaryHookReportsALogItCannotWrite holds the hook to failing visibly. An
// agent session must survive a hook that cannot write, and an operator must not
// read that session as a dispatch that happened.
func TestCanaryHookReportsALogItCannotWrite(t *testing.T) {
	t.Parallel()

	hook := canaryHookPath(t)
	home := t.TempDir()
	sandbox := t.TempDir()
	witness := filepath.Join(t.TempDir(), "invoked.log")
	shims := canaryShimPath(t, witness)

	// The log's parent is a regular file, so the directory it needs cannot be
	// created.
	occupied := filepath.Join(sandbox, "occupied")
	if err := os.WriteFile(occupied, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(occupied, "session-start.log")

	stderr, exit := runCanaryHookIsolated(t, hook, sandbox, log, home, shims)
	if exit != 0 {
		t.Errorf("the canary hook exited %d, want an exit that leaves the agent session running", exit)
	}
	if !strings.Contains(stderr, canaryHookMarker) {
		t.Errorf("the canary hook failed silently: stderr %q", stderr)
	}
	if got := canaryTree(t, sandbox); !slices.Equal(got, []string{"occupied"}) {
		t.Errorf("the canary hook left %v behind after a log it could not write", got)
	}
}

// canaryHookPath is the absolute path of the shipped fixture hook.
func canaryHookPath(t *testing.T) string {
	t.Helper()
	hook, err := filepath.Abs(filepath.Join(canaryPackageRoot, "hooks", "session-start.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return hook
}

// canaryShimPath returns a PATH entry shadowing every program in canaryShims
// with a stub that records its own name in witness and does nothing else.
//
// The stubs run under strict mode at a resolved absolute interpreter, so a
// witness the stub cannot append to fails the hook the stub was called from
// rather than reporting a call that was never recorded.
func canaryShimPath(t *testing.T, witness string) string {
	t.Helper()
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("resolve bash for the shims: %v", err)
	}
	directory := t.TempDir()
	for _, name := range canaryShims {
		stub := "#!" + shell + "\nset -euo pipefail\nprintf '%s\\n' " + name + " >>\"" + witness + "\"\n"
		if err := os.WriteFile(filepath.Join(directory, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

// runCanaryHookIsolated executes the hook with the shims ahead of the real PATH
// and a home of its own, and returns its stderr and exit status.
func runCanaryHookIsolated(t *testing.T, hook, workingDirectory, logPath, home, shims string) (string, int) {
	t.Helper()
	command := exec.Command(hook, "session-start")
	command.Dir = workingDirectory
	command.Env = []string{
		"PATH=" + shims + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home,
		"ACR_CANARY_LOG=" + logPath,
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	exit := 0
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		exit = exitErr.ExitCode()
	default:
		t.Fatalf("run the canary hook: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("the canary hook wrote %q to stdout", stdout.String())
	}
	return stderr.String(), exit
}

// canaryTree lists every path under root, relative and slash-separated, so a
// test can name the complete set of files a run is allowed to leave.
func canaryTree(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		if relative != "." {
			paths = append(paths, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	slices.Sort(paths)
	return paths
}

// readFileForTest reads one file a failure message needs.
func readFileForTest(t *testing.T, name string) string {
	t.Helper()
	contents, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

// TestCanaryHookLogIsAttributable holds the shipped hook to the two properties
// the manual per-agent evidence procedure rests on: an absolute ACR_CANARY_LOG
// decides where a marker lands regardless of where the agent started the hook,
// and each dispatch appends, so a log proven empty before a session and holding
// a line after it can only have been written by that session.
//
// It executes the fixture's own hook in a temporary directory. That is a local
// script invocation and is deliberately not evidence of native dispatch, which
// is exactly why the recipe forbids running it by hand to satisfy a row.
func TestCanaryHookLogIsAttributable(t *testing.T) {
	t.Parallel()

	hook, err := filepath.Abs(filepath.Join(canaryPackageRoot, "hooks", "session-start.sh"))
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	evidence := filepath.Join(t.TempDir(), "claude-code.session-start.log")

	// An absolute log path is unaffected by the working directory the agent
	// hands the hook.
	runCanaryHook(t, hook, elsewhere, evidence)
	if lines := canaryMarkerLines(t, evidence); lines != 1 {
		t.Fatalf("an absolute log holds %d marker lines after one dispatch, want 1", lines)
	}
	if entries, err := os.ReadDir(filepath.Join(elsewhere, ".acr-canary")); err == nil {
		t.Fatalf("the hook also wrote a relative log next to the working directory: %v", entries)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}

	// Each dispatch appends, so "empty before, non-empty after" is observable
	// and a second session is a strictly greater count rather than a rewrite.
	runCanaryHook(t, hook, elsewhere, evidence)
	if lines := canaryMarkerLines(t, evidence); lines != 2 {
		t.Fatalf("a second dispatch left %d marker lines, want 2", lines)
	}

	// Without the variable the log is relative to the working directory, which
	// is why the recipe requires the absolute form.
	working := t.TempDir()
	runCanaryHook(t, hook, working, "")
	if lines := canaryMarkerLines(t, filepath.Join(working, ".acr-canary", "session-start.log")); lines != 1 {
		t.Fatalf("the default log holds %d marker lines, want 1", lines)
	}
}

// runCanaryHook executes the fixture hook from one working directory, with an
// absolute log path when logPath is set.
func runCanaryHook(t *testing.T, hook, workingDirectory, logPath string) {
	t.Helper()
	command := exec.Command("bash", hook, "session-start")
	command.Dir = workingDirectory
	command.Env = append(os.Environ(), "ACR_CANARY_LOG="+logPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run the canary hook: %v\n%s", err, output)
	}
}

// canaryMarkerLines counts the marker lines one log holds.
func canaryMarkerLines(t *testing.T, logPath string) int {
	t.Helper()
	contents, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read the canary log: %v", err)
	}
	lines := 0
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.Contains(line, canaryHookMarker) {
			lines++
		}
	}
	return lines
}

// copyCanaryPackage copies the shipped fixture into a project, preserving the
// executable bit the hook needs.
func copyCanaryPackage(t *testing.T, destination string) {
	t.Helper()
	err := filepath.WalkDir(canaryPackageRoot, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(canaryPackageRoot, name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		contents, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		return os.WriteFile(target, contents, mode)
	})
	if err != nil {
		t.Fatalf("copy the canary package: %v", err)
	}
}
