package migrateapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// reverifyFullConsumerGitHub serves both fixture packages so a full-consumer
// migration resolves without a network call.
func reverifyFullConsumerGitHub(t *testing.T) *multiRepoGitHub {
	t.Helper()
	return newMultiRepoGitHub(map[string][]byte{
		"example/alpha": hostileArchive(t, "example/alpha", map[string]string{
			"rules/always-rule.md":          "---\nalwaysApply: true\n---\n# Always\n",
			"skills/review-change/SKILL.md": "# Review\n",
		}),
		"example/beta": hostileArchive(t, "example/beta", map[string]string{
			"rules/legacy-rule.md":          "---\nalwaysApply: true\n---\n# Legacy\n",
			"skills/legacy-skill/SKILL.md":  "# Legacy\n",
			"skills/review-change/SKILL.md": "# Review\n",
		}),
	})
}

func reverifyMapFlags() []string {
	return []string{
		"--map", "example/alpha=github:example/alpha@v1.0.0",
		"--map", "example/beta=github:example/beta@v1.0.0",
	}
}

// reverifyTesslOwnedSurface hashes every path Tessl owns by manifest evidence:
// everything under .tessl/, the manifest itself, and every path carrying a
// tessl__ component anywhere -- including a top-level one, which the earlier
// "/tessl__" substring filter would miss.
func reverifyTesslOwnedSurface(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	for path, digest := range hashTreeWithModes(t, root) {
		if reverifyTesslOwnedPath(path) {
			result[path] = digest
		}
	}
	return result
}

func reverifyTesslOwnedPath(filename string) bool {
	if filename == "tessl.json" || filename == ".tessl" || strings.HasPrefix(filename, ".tessl/") {
		return true
	}
	for _, component := range strings.Split(filename, "/") {
		if strings.HasPrefix(component, "tessl__") {
			return true
		}
	}
	return false
}

func reverifyAssertNoTesslOwnedOperation(t *testing.T, label string, plan realize.Plan) {
	t.Helper()
	for _, operation := range plan.Operations {
		if reverifyTesslOwnedPath(operation.Path) {
			t.Errorf("%s planned %s against the Tessl-owned path %s", label, operation.Kind, operation.Path)
		}
	}
}

func reverifyReadFile(t *testing.T, root, relative string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	return content
}

// TestReverifyFullConsumerHostsEveryAgentOutsideTessl applies a migration to the
// full consumer fixture -- Codex, Claude Code and Cursor trees, both Tessl
// manifest shapes, a Tessl .gitignore block and a tessl__ native -- and holds
// the round-2 host-selection fix to the acceptance criterion: every Tessl-owned
// byte and mode survives, no managed span lands under .tessl/, and each of the
// three agents still receives ACR's rules.
//
// The two sub-cases are the two shapes a Tessl consumer's instruction files
// take. Chained is what Tessl writes today (CLAUDE.md includes AGENTS.md), so
// the two roots share one host and the block lands in AGENTS.md, which CLAUDE.md
// still reaches through its preserved include. Sibling is the shape where each
// agent root includes .tessl/RULES.md independently: after the fix excludes the
// only common node, the roots no longer share a host and each receives its own
// block instead of collapsing onto Tessl's file.
func TestReverifyFullConsumerHostsEveryAgentOutsideTessl(t *testing.T) {
	for _, test := range []struct {
		name          string
		claude        []byte
		claudeHosting bool
	}{
		{
			name:   "chained CLAUDE.md",
			claude: []byte("# Claude notes\n\n@AGENTS.md\n"),
		},
		{
			name:          "sibling CLAUDE.md",
			claude:        []byte("# Claude notes\n\n@.tessl/RULES.md\n"),
			claudeHosting: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := seedDualManifestConsumer(t)
			writeFile(t, project, "CLAUDE.md", test.claude, 0o644)
			claudeBefore := reverifyReadFile(t, project, "CLAUDE.md")
			tesslBefore := reverifyTesslOwnedSurface(t, project)
			if len(tesslBefore) < 20 {
				t.Fatalf("fixture froze only %d Tessl-owned paths, want the full consumer surface", len(tesslBefore))
			}

			application := &Application{service: newService(reverifyFullConsumerGitHub(t)), fallback: cli.UnavailableApplication{}}
			args := append([]string{"migrate", "tessl", "--json", "--project", project}, reverifyMapFlags()...)
			stdout, stderr, exitCode := runCLI(t, application, args...)
			if exitCode != cli.ExitSuccess || stderr != "" {
				t.Fatalf("apply exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
			}
			if !strings.Contains(stdout, `"wrote":true`) {
				t.Fatalf("apply report = %s", stdout)
			}

			if after := reverifyTesslOwnedSurface(t, project); !mapsEqual(tesslBefore, after) {
				t.Fatalf("apply changed Tessl-owned bytes or modes\nbefore=%v\nafter=%v", tesslBefore, after)
			}
			hostileAssertNoACRSpanUnderTessl(t, project)

			agents := reverifyReadFile(t, project, "AGENTS.md")
			if !bytes.Contains(agents, []byte("acr:begin")) {
				t.Errorf("AGENTS.md carries no ACR managed block:\n%s", agents)
			}
			if !bytes.Contains(agents, []byte("## Agent Rules <!-- tessl-managed -->\n\n@.tessl/RULES.md")) {
				t.Errorf("Tessl-managed span was not preserved in AGENTS.md:\n%s", agents)
			}

			claudeAfter := reverifyReadFile(t, project, "CLAUDE.md")
			if test.claudeHosting {
				if !bytes.Contains(claudeAfter, []byte("acr:begin")) {
					t.Errorf("CLAUDE.md is its own host but carries no ACR managed block:\n%s", claudeAfter)
				}
			} else {
				// The chained root reaches the block through AGENTS.md, so the
				// contract is that the include survives untouched.
				if !bytes.Equal(claudeAfter, claudeBefore) {
					t.Errorf("CLAUDE.md changed\nbefore=%q\nafter=%q", claudeBefore, claudeAfter)
				}
				if !bytes.Contains(claudeAfter, []byte("@AGENTS.md")) {
					t.Errorf("CLAUDE.md no longer reaches the ACR block host:\n%s", claudeAfter)
				}
			}

			cursorRule := reverifyReadFile(t, project, ".cursor/rules/acr__example__alpha__always-rule.mdc")
			if !bytes.Contains(cursorRule, []byte("# Always")) {
				t.Errorf("Cursor host rule = %q, want the migrated rule body", cursorRule)
			}
			tree := hashTreeWithModes(t, project)
			for _, native := range []string{
				".claude/skills/acr__example__alpha__review-change/SKILL.md",
				".codex/skills/acr__example__alpha__review-change/SKILL.md",
				".cursor/skills/acr__example__alpha__review-change/SKILL.md",
			} {
				if _, ok := tree[native]; !ok {
					t.Errorf("agent tree lost its ACR native %s", native)
				}
			}
		})
	}
}

// TestReverifyUserRuleBehindATesslOwnedFileStaysAHostCandidate proves the
// exclusion is scoped to Tessl-owned paths rather than to everything reachable
// through one. A tessl__ instruction file outside .tessl/ is Tessl-owned and is
// skipped, but the user rule it includes is deeper, unowned, and must still win
// host selection -- otherwise the fix would push ACR's block back up to AGENTS.md
// and silently ignore a legitimate candidate.
//
// The include lives on a tessl__ native rather than on .tessl/RULES.md because a
// file inside .tessl/ cannot reference anything outside it: the include parser
// rejects a leading ../ (see TestReverifyTesslRulesEscapingIncludeWritesNothing).
func TestReverifyUserRuleBehindATesslOwnedFileStaysAHostCandidate(t *testing.T) {
	project := seedDualManifestConsumer(t)
	writeFile(t, project, "AGENTS.md",
		[]byte("# User title\n\nUser prose lives here.\n\n## Agent Rules <!-- tessl-managed -->\n\n@.tessl/RULES.md\n\n@instructions/tessl__example__alpha.md\n"), 0o644)
	writeFile(t, project, "instructions/tessl__example__alpha.md", []byte("# Tessl native\n\n@user-rules.md\n"), 0o644)
	writeFile(t, project, "instructions/user-rules.md", []byte("# User rules\n"), 0o644)
	tesslBefore := reverifyTesslOwnedSurface(t, project)
	if _, ok := tesslBefore["instructions/tessl__example__alpha.md"]; !ok {
		t.Fatal("the tessl__ native outside .tessl/ was not frozen")
	}

	application := &Application{service: newService(reverifyFullConsumerGitHub(t)), fallback: cli.UnavailableApplication{}}
	args := append([]string{"migrate", "tessl", "--json", "--project", project}, reverifyMapFlags()...)
	if _, stderr, exitCode := runCLI(t, application, args...); exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("apply exit = %d, stderr = %q", exitCode, stderr)
	}

	if after := reverifyTesslOwnedSurface(t, project); !mapsEqual(tesslBefore, after) {
		t.Fatalf("apply changed Tessl-owned bytes or modes\nbefore=%v\nafter=%v", tesslBefore, after)
	}
	hostileAssertNoACRSpanUnderTessl(t, project)
	if userRule := reverifyReadFile(t, project, "instructions/user-rules.md"); !bytes.Contains(userRule, []byte("acr:begin")) {
		t.Fatalf("the user rule behind the Tessl-owned include was dropped as a host candidate:\n%s", userRule)
	}
	if agents := reverifyReadFile(t, project, "AGENTS.md"); bytes.Contains(agents, []byte("acr:begin")) {
		t.Errorf("host selection stopped at AGENTS.md instead of the deeper user rule:\n%s", agents)
	}
}

// TestReverifyTesslRulesEscapingIncludeWritesNothing records why the user rule
// above hangs off a tessl__ native: an @../ token inside .tessl/RULES.md is
// rejected by the include parser, so migration refuses before any write rather
// than reaching host selection at all.
func TestReverifyTesslRulesEscapingIncludeWritesNothing(t *testing.T) {
	project := seedDualManifestConsumer(t)
	writeFile(t, project, ".tessl/RULES.md",
		[]byte("# Agent Rules\n\n@plugins/example/alpha/rules/always-rule.md\n\n@../USER_RULES.md\n"), 0o644)
	writeFile(t, project, "USER_RULES.md", []byte("# User rules\n"), 0o644)
	before := hostileProjectTree(t, project)

	application := &Application{service: newService(reverifyFullConsumerGitHub(t)), fallback: cli.UnavailableApplication{}}
	args := append([]string{"migrate", "tessl", "--json", "--project", project}, reverifyMapFlags()...)
	stdout, stderr, exitCode := runCLI(t, application, args...)
	if exitCode != cli.ExitOperational || stdout != "" {
		t.Fatalf("escaping-include exit = %d, stdout = %q", exitCode, stdout)
	}
	if !strings.Contains(stderr, "invalid_include") || !strings.Contains(stderr, "../USER_RULES.md") {
		t.Fatalf("escaping-include stderr = %q", stderr)
	}
	if after := hostileProjectTree(t, project); !mapsEqual(before, after) {
		t.Fatalf("refusal mutated the tree\nbefore=%v\nafter=%v", before, after)
	}
}

// TestReverifyRealizeOnMigratedConsumerPlansNothingUnderTessl closes the second
// half of the round-1 finding: the host-selection defect was reachable from
// `acr realize` on an already-migrated tree, not only from migration. The
// migrated consumer is re-planned through the realization service in every
// read-only mode, and no operation may name a Tessl-owned path.
func TestReverifyRealizeOnMigratedConsumerPlansNothingUnderTessl(t *testing.T) {
	project := seedDualManifestConsumer(t)
	service := newService(reverifyFullConsumerGitHub(t))
	application := &Application{service: service, fallback: cli.UnavailableApplication{}}
	args := append([]string{"migrate", "tessl", "--json", "--project", project}, reverifyMapFlags()...)
	if _, stderr, exitCode := runCLI(t, application, args...); exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("apply exit = %d, stderr = %q", exitCode, stderr)
	}
	tesslBefore := reverifyTesslOwnedSurface(t, project)

	converged, err := service.realizer.Run(context.Background(), project, nil, realize.ModeDryRun)
	if err != nil {
		t.Fatalf("realize dry-run on the migrated consumer: %v", err)
	}
	reverifyAssertNoTesslOwnedOperation(t, "converged realize", converged.Plan)
	if converged.Plan.HasChanges() {
		t.Errorf("realize on the migrated consumer is not converged: %#v", converged.Plan.Operations)
	}
	if _, err := service.realizer.Run(context.Background(), project, nil, realize.ModeCheck); err != nil {
		t.Fatalf("realize check on the migrated consumer: %v", err)
	}

	if after := reverifyTesslOwnedSurface(t, project); !mapsEqual(tesslBefore, after) {
		t.Fatalf("realize changed Tessl-owned bytes or modes\nbefore=%v\nafter=%v", tesslBefore, after)
	}
	hostileAssertNoACRSpanUnderTessl(t, project)

	// A converged plan is empty, so on its own it proves nothing about host
	// selection. The second phase runs the realization planner over an
	// unrealized Tessl consumer -- ACR dependencies resolved, nothing written
	// yet -- which is where realize derives its own hosts and where the round-1
	// finding said the defect was reachable without migration.
	reverifyAssertUnrealizedPlanAvoidsTessl(t)
}

func reverifyAssertUnrealizedPlanAvoidsTessl(t *testing.T) {
	t.Helper()
	project := seedDualManifestConsumer(t)
	service := newService(reverifyFullConsumerGitHub(t))
	mappings, err := migrate.ParseInlineMappings([]string{
		"example/alpha=github:example/alpha@v1.0.0",
		"example/beta=github:example/beta@v1.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	desired, _, err := service.resolveState(context.Background(), dependency.State{}, mappings, nil)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := service.Inventory(project)
	if err != nil {
		t.Fatal(err)
	}
	desired.Project.Agents = selectedAgents(inventory)

	before := hostileProjectTree(t, project)
	result, err := service.realizer.RunState(context.Background(), project, desired, desired.Project.Agents, realize.ModeDryRun)
	if err != nil {
		t.Fatalf("realize dry-run on an unrealized Tessl consumer: %v", err)
	}
	if !result.Plan.HasChanges() {
		t.Fatal("the unrealized consumer produced an empty plan, so the row proves nothing")
	}
	reverifyAssertNoTesslOwnedOperation(t, "unrealized realize", result.Plan)
	if after := hostileProjectTree(t, project); !mapsEqual(before, after) {
		t.Fatalf("realize dry-run mutated the tree\nbefore=%v\nafter=%v", before, after)
	}
}
