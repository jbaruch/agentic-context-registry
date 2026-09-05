package adaptertest

import (
	"bytes"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const skillReferenceFixture = "skill-reference-boundary"

// nativeSkillsRoots is the skills root each shipped adapter realizes into.
// The test states them rather than deriving them, so a renderer that starts
// writing somewhere else fails here instead of following the test.
var nativeSkillsRoots = map[string]string{
	"claude-code": ".claude/skills",
	"codex":       ".codex/skills",
	"cursor":      ".cursor/skills",
}

// unsupportedSkillReferences are the fixture's Step 4 bullets plus its two
// program-syntax blocks. None of them addresses this package's installed
// tree, so realization must copy every one of them byte for byte.
var unsupportedSkillReferences = []string{
	"`https://example.com/skills/advocate/scripts/check.sh`",
	"`https://example.com/?next=skills/advocate/scripts/check.sh`",
	"`https://example.com/archive#skills/advocate/scripts/check.sh`",
	"`archive#skills/advocate/scripts/check.sh`",
	"`https://example.com/(skills/advocate/scripts/check.sh)`",
	"`https://example.com/?next=[skills/advocate/scripts/check.sh]`",
	"`archive(skills/advocate/scripts/check.sh)`",
	"`archive{skills/advocate/scripts/check.sh}`",
	"`archive](skills/advocate/scripts/check.sh)`",
	"`https://example.com/a](skills/advocate/scripts/check.sh)`",
	"`https://example.com/a'skills/advocate/scripts/check.sh`",
	"`archive'skills/advocate/scripts/check.sh`",
	"`note[label](skills/advocate/scripts/check.sh)`",
	"    \"archive\n     skills/advocate/scripts/check.sh\"\n",
	"`caféskills/advocate/scripts/check.sh`",
	"`vendor/skills/advocate/scripts/check.sh`",
	"`myskills/advocate/scripts/check.sh`",
	"`skills/advocate-archive/scripts/check.sh`",
	"`/skills/advocate/scripts/check.sh`",
	"`./skills/advocate/scripts/check.sh`",
	"`.tessl/plugins/other-workspace/other-plugin/skills/advocate/scripts/check.sh`",
	"`.tessl/plugins/other-workspace/other-plugin/skills/unrelated/check.sh`",
	"`.tessl/plugins/legacy-workspace/skills/advocate/scripts/check.sh`",
	"`.tessl/plugins/legacy-workspace bad/advocate-plugin/skills/advocate/scripts/check.sh`",
	"`skills/advocate`",
	"    mount = (\".tessl/plugins/legacy-workspace/advocate-plugin\"\n             \"/skills/advocate/scripts/check.sh\")\n",
	"    .tessl/plugins/\n    KEEP THIS PROSE\n    /advocate-plugin/skills/advocate/scripts/check.sh\n",
}

// TestRealizedSkillCommandsExecute is the issue #92 regression. It realizes a
// package whose skills address one helper through both supported reference
// forms, then runs every command the realized skill files instruct an agent
// to run, from the project directory an agent runs it in. The defect it holds
// against produced a path that resolves nowhere, which no assertion over the
// rewriting rule itself would have caught: the rewriting was self-consistent
// and the file it named did not exist.
func TestRealizedSkillCommandsExecute(t *testing.T) {
	t.Parallel()
	for _, native := range []adapter.Adapter{claudecode.New(), codex.New(), cursor.New()} {
		native := native
		t.Run(native.Descriptor().ID, func(t *testing.T) {
			t.Parallel()
			project := realizeSkillReferenceFixture(t, native)
			skillsRoot, ok := nativeSkillsRoots[native.Descriptor().ID]
			if !ok {
				t.Fatalf("no skills root recorded for adapter %q", native.Descriptor().ID)
			}

			var commands []string
			for _, skill := range []string{"advocate", "router"} {
				commands = append(commands, instructedCommands(t, filepath.Join(project,
					filepath.FromSlash(path.Join(skillsRoot, "acr__example__coexist__"+skill, "SKILL.md"))))...)
			}
			if len(commands) != 4 {
				t.Fatalf("instructed commands = %d (%v), want the fixture's four helper invocations", len(commands), commands)
			}
			for _, command := range commands {
				assertHelperRuns(t, project, skillsRoot, command)
			}
		})
	}
}

// TestRealizedRuleHelperExecutes covers R4: a rule that requires a bundled
// helper is operational, so the path it names has to resolve from the project
// directory for every host an adapter selection produces — Cursor's own
// `.mdc`, a Claude-only host, a Codex-only host, two separate hosts, and one
// host Claude reaches through an import.
func TestRealizedRuleHelperExecutes(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name     string
		natives  []adapter.Adapter
		existing map[string]string
		hosts    []string
	}{
		{name: "claude only", natives: []adapter.Adapter{claudecode.New()}, hosts: []string{"CLAUDE.md"}},
		{name: "codex only", natives: []adapter.Adapter{codex.New()}, hosts: []string{"AGENTS.md"}},
		{name: "cursor only", natives: []adapter.Adapter{cursor.New()}, hosts: []string{".cursor/rules/acr__example__coexist__measurement.mdc"}},
		{
			name:     "separate hosts",
			natives:  []adapter.Adapter{claudecode.New(), codex.New()},
			existing: map[string]string{"CLAUDE.md": "# Claude guidance\n", "AGENTS.md": "# Agents guidance\n"},
			hosts:    []string{"CLAUDE.md", "AGENTS.md"},
		},
		{
			name:     "imported shared host",
			natives:  []adapter.Adapter{claudecode.New(), codex.New()},
			existing: map[string]string{"CLAUDE.md": "# Claude guidance\n@AGENTS.md\n", "AGENTS.md": "# Agents guidance\n"},
			hosts:    []string{"AGENTS.md"},
		},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			project := t.TempDir()
			for name, body := range testCase.existing {
				writeProjectFile(t, project, name, body)
			}
			realizeSkillReferenceFixtureInto(t, project, testCase.natives...)
			for _, host := range testCase.hosts {
				commands := instructedCommands(t, filepath.Join(project, filepath.FromSlash(host)))
				if len(commands) != 1 {
					t.Fatalf("%s carries %d rule commands (%v), want the fixture's one", host, len(commands), commands)
				}
				assertHelperRuns(t, project, "", commands[0])
			}
			assertRuleBundleAppearsOnce(t, project, testCase.hosts)
		})
	}
}

// TestRealizedSkillFilesPreserveUnsupportedReferences holds the other half of
// the contract: rebasing rewrites the supported forms and leaves every other
// byte where it was.
func TestRealizedSkillFilesPreserveUnsupportedReferences(t *testing.T) {
	t.Parallel()
	for _, native := range []adapter.Adapter{claudecode.New(), codex.New(), cursor.New()} {
		native := native
		t.Run(native.Descriptor().ID, func(t *testing.T) {
			t.Parallel()
			project := realizeSkillReferenceFixture(t, native)
			skillsRoot := nativeSkillsRoots[native.Descriptor().ID]
			advocate := filepath.Join(project, filepath.FromSlash(path.Join(skillsRoot, "acr__example__coexist__advocate", "SKILL.md")))
			realized, err := os.ReadFile(advocate)
			if err != nil {
				t.Fatal(err)
			}
			for _, reference := range unsupportedSkillReferences {
				if !bytes.Contains(realized, []byte(reference)) {
					t.Fatalf("realized %s dropped the unsupported reference %q:\n%s", advocate, reference, realized)
				}
			}
			if bytes.Contains(realized, []byte(".tessl/plugins/legacy-workspace/advocate-plugin/skills/advocate/")) {
				t.Fatalf("realized %s still addresses the Tessl-installed tree:\n%s", advocate, realized)
			}
			if bytes.Contains(realized, []byte("advocate-plugin/"+skillsRoot)) {
				t.Fatalf("realized %s spliced the native root into the Tessl-installed path:\n%s", advocate, realized)
			}
			for _, supported := range []string{
				"[the helper](" + skillsRoot + "/acr__example__coexist__advocate/scripts/check.sh)",
				"form (" + skillsRoot + "/acr__example__coexist__advocate/scripts/check.sh)",
				"argument \"" + skillsRoot + "/acr__example__coexist__advocate/scripts/check.sh\"",
				"command `python3 " + skillsRoot + "/acr__example__coexist__advocate/scripts/check.sh --quoted`",
				"\\" + skillsRoot + "/acr__example__coexist__advocate/scripts/check.sh",
				"--helper=" + skillsRoot + "/acr__example__coexist__advocate/scripts/check.sh",
				"HELPER=" + skillsRoot + "/acr__example__coexist__advocate/scripts/check.sh",
			} {
				if !bytes.Contains(realized, []byte(supported)) {
					t.Fatalf("realized %s did not rebase %q:\n%s", advocate, supported, realized)
				}
			}

			source, err := os.ReadFile(filepath.Join("testdata", skillReferenceFixture, "package", "skills", "router", "references", "notes.md"))
			if err != nil {
				t.Fatal(err)
			}
			companion := filepath.Join(project, filepath.FromSlash(path.Join(skillsRoot, "acr__example__coexist__router", "references", "notes.md")))
			realizedCompanion, err := os.ReadFile(companion)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(realizedCompanion, source) {
				t.Fatalf("realized %s differs from its package bytes\n--- got ---\n%s--- want ---\n%s", companion, realizedCompanion, source)
			}
		})
	}
}

func realizeSkillReferenceFixture(t *testing.T, natives ...adapter.Adapter) string {
	t.Helper()
	return realizeSkillReferenceFixtureInto(t, t.TempDir(), natives...)
}

func realizeSkillReferenceFixtureInto(t *testing.T, project string, natives ...adapter.Adapter) string {
	t.Helper()
	root := filepath.Join("testdata", skillReferenceFixture, "package")
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	pkg := adapter.Package{Source: "github:" + loaded.Name, Root: os.DirFS(root), Manifest: loaded}
	applyNativePackages(t, project, []adapter.Package{pkg}, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, natives...)
	return project
}

func writeProjectFile(t *testing.T, project, relative, body string) {
	t.Helper()
	target := filepath.Join(project, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// instructedCommands returns the command a realized file tells an agent to
// run: the backquoted text on a line that begins with "Run `".
func instructedCommands(t *testing.T, filename string) []string {
	t.Helper()
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	for _, line := range strings.Split(string(content), "\n") {
		const marker = "Run `"
		if !strings.HasPrefix(line, marker) {
			continue
		}
		command, _, found := strings.Cut(line[len(marker):], "`")
		if !found {
			t.Fatalf("%s has an unterminated command on line %q", filename, line)
		}
		commands = append(commands, command)
	}
	return commands
}

// assertHelperRuns executes one instructed command from the project directory
// and holds its output to the argument the command carries. skillsRoot, when
// given, additionally requires the command to address that adapter's tree.
func assertHelperRuns(t *testing.T, project, skillsRoot, command string) {
	t.Helper()
	fields := strings.Fields(command)
	if len(fields) != 2 {
		t.Fatalf("instructed command %q does not carry a program and one argument", command)
	}
	if skillsRoot != "" && !strings.HasPrefix(fields[0], skillsRoot+"/") {
		t.Fatalf("instructed command %q does not address the installed skill tree under %s", command, skillsRoot)
	}
	// exec resolves a relative program against this process's directory, not
	// Dir, so the join is what "run it from the project" means here.
	executable := exec.Command(filepath.Join(project, filepath.FromSlash(fields[0])), fields[1:]...)
	executable.Dir = project
	var stdout, stderr bytes.Buffer
	executable.Stdout = &stdout
	executable.Stderr = &stderr
	if err := executable.Run(); err != nil {
		t.Fatalf("run %q from the project directory: %v\n%s%s", command, err, stdout.String(), stderr.String())
	}
	want := "{\"ok\":true,\"helper\":\"advocate-check\",\"argument\":\"" + fields[1] + "\"}\n"
	if stdout.String() != want {
		t.Fatalf("run %q stdout = %q, want %q", command, stdout.String(), want)
	}
}

// assertRuleBundleAppearsOnce keeps the R4 rebasing from multiplying rule
// bodies: the package contributes one block, to one host, whatever selection
// produced it.
func assertRuleBundleAppearsOnce(t *testing.T, project string, hosts []string) {
	t.Helper()
	total := 0
	for _, host := range []string{"CLAUDE.md", "AGENTS.md", ".cursor/rules/acr__example__coexist__measurement.mdc"} {
		content, err := os.ReadFile(filepath.Join(project, filepath.FromSlash(host)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		count := bytes.Count(content, []byte("No other count is acceptable."))
		if count > 1 {
			t.Fatalf("%s carries the rule body %d times", host, count)
		}
		total += count
	}
	if total != len(hosts) {
		t.Fatalf("rule body appears %d times across the project, want one per host %v", total, len(hosts))
	}
}

// TestOmittedGeneratedHostRelocatesWithoutRefusal is the issue #93
// regression. ACR creates CLAUDE.md as a generated-only host; another tool
// then adds an `@AGENTS.md` import plus its own prose. Host selection moves
// the rule contribution to AGENTS.md and omits CLAUDE.md, which used to reach
// the engine's plain generated-only delete path and refuse the whole
// realization as modified managed output.
func TestOmittedGeneratedHostRelocatesWithoutRefusal(t *testing.T) {
	t.Parallel()
	natives := []adapter.Adapter{claudecode.New(), codex.New()}
	root := filepath.Join("testdata", skillReferenceFixture, "package")
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	pkg := adapter.Package{Source: "github:" + loaded.Name, Root: os.DirFS(root), Manifest: loaded}
	packages := []adapter.Package{pkg}

	project := t.TempDir()
	_, ledger := applyNativePackages(t, project, packages, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, natives...)
	claude := filepath.Join(project, "CLAUDE.md")
	generated, err := os.ReadFile(claude)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(generated, []byte("No other count is acceptable.")) {
		t.Fatalf("CLAUDE.md did not receive the rule bundle:\n%s", generated)
	}

	const foreign = "# Foreign guidance nobody else may drop\n@AGENTS.md\n"
	if err := os.WriteFile(claude, append([]byte(foreign), generated...), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, next := applyNativePackages(t, project, packages, ledger, natives...)
	for _, operation := range plan.Operations {
		if operation.Kind == realize.OperationConflict {
			t.Fatalf("relocating the contribution conflicted on %s: %s", operation.Path, operation.Reason)
		}
	}
	relocated, err := os.ReadFile(claude)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(relocated, []byte(foreign)) {
		t.Fatalf("CLAUDE.md lost the foreign bytes:\n%s", relocated)
	}
	if bytes.Contains(relocated, []byte("No other count is acceptable.")) {
		t.Fatalf("CLAUDE.md kept the relocated rule body:\n%s", relocated)
	}
	agents, err := os.ReadFile(filepath.Join(project, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(agents, []byte("No other count is acceptable.")) != 1 {
		t.Fatalf("AGENTS.md does not carry exactly one relocated rule body:\n%s", agents)
	}
	for _, target := range next.Targets {
		if target.Path == "CLAUDE.md" {
			t.Fatalf("ledger still owns the relocated-away host: %#v", target)
		}
	}

	idempotent, _ := planNativePackages(t, project, packages, next, natives...)
	if idempotent.HasChanges() {
		t.Fatalf("re-realizing after the relocation has changes: %#v", idempotent)
	}
}

// TestModifiedManagedBlockStillRefuses keeps the #93 fix narrow: an omitted
// host whose managed block was actually edited is not a relocation, and the
// realization still refuses rather than overwriting the edit.
func TestModifiedManagedBlockStillRefuses(t *testing.T) {
	t.Parallel()
	natives := []adapter.Adapter{claudecode.New(), codex.New()}
	root := filepath.Join("testdata", skillReferenceFixture, "package")
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	packages := []adapter.Package{{Source: "github:" + loaded.Name, Root: os.DirFS(root), Manifest: loaded}}

	project := t.TempDir()
	_, ledger := applyNativePackages(t, project, packages, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, natives...)
	claude := filepath.Join(project, "CLAUDE.md")
	generated, err := os.ReadFile(claude)
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(generated, []byte("No other count is acceptable."), []byte("Any count will do."), 1)
	if bytes.Equal(edited, generated) {
		t.Fatal("fixture rule body not found in the generated host")
	}
	if err := os.WriteFile(claude, append([]byte("@AGENTS.md\n"), edited...), 0o644); err != nil {
		t.Fatal(err)
	}

	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), natives...)
	if err != nil {
		t.Fatal(err)
	}
	// The refusal may land in either half of the composition: the compiler
	// rejects an edited marker outright, and anything it does let through
	// still has to reach the planner as a conflict.
	intents, err := coordinator.Realize(t.Context(), adapter.NewFSSnapshot(os.DirFS(project)), packages, ledger)
	if err != nil {
		if !strings.Contains(err.Error(), "CLAUDE.md") {
			t.Fatalf("refusal does not name the edited host: %v", err)
		}
		return
	}
	plan, err := realize.NewEngine().Run(project, ledger, intents, realize.ModeDryRun, func(realize.Ledger) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range plan.Operations {
		if operation.Kind == realize.OperationConflict && operation.Path == "CLAUDE.md" {
			return
		}
	}
	t.Fatalf("an edited managed block did not refuse: %#v", plan.Operations)
}

// selectionCycle is the adapter selection an operator edits in agents.yaml,
// with the rule host each selection is expected to produce.
var selectionCycle = []struct {
	name    string
	natives []adapter.Adapter
	hosts   []string
}{
	{name: "all three", natives: []adapter.Adapter{claudecode.New(), codex.New(), cursor.New()}, hosts: []string{"AGENTS.md", cursorRuleHost}},
	{name: "claude only", natives: []adapter.Adapter{claudecode.New()}, hosts: []string{"AGENTS.md"}},
	{name: "codex only", natives: []adapter.Adapter{codex.New()}, hosts: []string{"AGENTS.md"}},
	{name: "cursor only", natives: []adapter.Adapter{cursor.New()}, hosts: []string{cursorRuleHost}},
	{name: "all three again", natives: []adapter.Adapter{claudecode.New(), codex.New(), cursor.New()}, hosts: []string{"AGENTS.md", cursorRuleHost}},
}

const cursorRuleHost = ".cursor/rules/acr__example__coexist__measurement.mdc"

// TestSelectionCycleKeepsForeignIncludeResolvable walks the whole deselect and
// reselect lifecycle over a host the user has added their own import and prose
// to.
//
// Deselecting every Markdown adapter removes the ACR-created AGENTS.md while
// the user's `@AGENTS.md` in CLAUDE.md survives. Reselecting then had to
// discover that host before it could regenerate it, so realization refused
// with `unresolved_include` and the only way out was for the user to delete
// their own import. Each step here realizes, re-plans to prove it settled, and
// executes the helper the rule names.
func TestSelectionCycleKeepsForeignIncludeResolvable(t *testing.T) {
	t.Parallel()
	root := filepath.Join("testdata", skillReferenceFixture, "package")
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	packages := []adapter.Package{{Source: "github:" + loaded.Name, Root: os.DirFS(root), Manifest: loaded}}

	project := t.TempDir()
	ledger := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}
	_, ledger = applyNativePackages(t, project, packages, ledger, selectionCycle[0].natives...)

	const foreign = "# Foreign guidance nobody else may drop\n@AGENTS.md\n"
	claude := filepath.Join(project, "CLAUDE.md")
	existing, err := os.ReadFile(claude)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claude, append([]byte(foreign), existing...), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, step := range selectionCycle {
		plan, next := applyNativePackages(t, project, packages, ledger, step.natives...)
		ledger = next
		for _, operation := range plan.Operations {
			if operation.Kind == realize.OperationConflict {
				t.Fatalf("selection %q conflicted on %s: %s", step.name, operation.Path, operation.Reason)
			}
		}
		settled, _ := planNativePackages(t, project, packages, ledger, step.natives...)
		if settled.HasChanges() {
			t.Fatalf("selection %q did not settle: %#v", step.name, settled)
		}
		for _, host := range step.hosts {
			commands := instructedCommands(t, filepath.Join(project, filepath.FromSlash(host)))
			if len(commands) != 1 {
				t.Fatalf("selection %q host %s carries %d rule commands (%v)", step.name, host, len(commands), commands)
			}
			assertHelperRuns(t, project, "", commands[0])
		}
		current, err := os.ReadFile(claude)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(current, []byte(foreign)) {
			t.Fatalf("selection %q dropped the foreign bytes:\n%s", step.name, current)
		}
	}
}

// TestUnresolvedForeignIncludeStillRefuses keeps the selection fix narrow: an
// import naming a file ACR is not going to write is still a refusal, so a typo
// in a user's own import is reported rather than silently ignored.
func TestUnresolvedForeignIncludeStillRefuses(t *testing.T) {
	t.Parallel()
	root := filepath.Join("testdata", skillReferenceFixture, "package")
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	packages := []adapter.Package{{Source: "github:" + loaded.Name, Root: os.DirFS(root), Manifest: loaded}}
	natives := []adapter.Adapter{claudecode.New(), codex.New()}

	project := t.TempDir()
	writeProjectFile(t, project, "CLAUDE.md", "# Guidance\n@notes/missing.md\n")
	writeProjectFile(t, project, "AGENTS.md", "# Agents guidance\n")

	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), natives...)
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Realize(t.Context(), adapter.NewFSSnapshot(os.DirFS(project)),
		packages, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion})
	if err == nil {
		t.Fatal("an include naming a file ACR does not write was accepted")
	}
	if !strings.Contains(err.Error(), "notes/missing.md") {
		t.Fatalf("refusal does not name the unresolved include: %v", err)
	}
}
