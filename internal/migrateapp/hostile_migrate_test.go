package migrateapp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// multiRepoGitHub serves a distinct archive per repository so an end-to-end run
// over a consumer holding both Tessl manifest shapes resolves each package to
// its own ACR release without a network call.
type multiRepoGitHub struct {
	archives map[string][]byte
	calls    map[string]int
}

func newMultiRepoGitHub(archives map[string][]byte) *multiRepoGitHub {
	return &multiRepoGitHub{archives: archives, calls: map[string]int{}}
}

func (github *multiRepoGitHub) key(repository dependency.Repository) string {
	return repository.Owner + "/" + repository.Name
}

func (github *multiRepoGitHub) LatestRelease(_ context.Context, repository dependency.Repository) (dependency.Release, error) {
	github.calls["latest"]++
	if _, ok := github.archives[github.key(repository)]; !ok {
		return dependency.Release{}, &dependency.RemoteError{StatusCode: 404, Err: fmt.Errorf("no release for %s", github.key(repository))}
	}
	return dependency.Release{ID: 1, Tag: "v1.0.0"}, nil
}

func (github *multiRepoGitHub) ReleaseByTag(_ context.Context, repository dependency.Repository, tag string) (dependency.Release, error) {
	github.calls["byTag"]++
	if _, ok := github.archives[github.key(repository)]; ok && tag == "v1.0.0" {
		return dependency.Release{ID: 1, Tag: "v1.0.0"}, nil
	}
	return dependency.Release{}, &dependency.RemoteError{StatusCode: 404, Err: fmt.Errorf("tag %s not found", tag)}
}

func (github *multiRepoGitHub) ResolveCommit(context.Context, dependency.Repository, string) (string, error) {
	github.calls["resolve"]++
	return strings.Repeat("a", 40), nil
}

func (github *multiRepoGitHub) DownloadArchive(_ context.Context, repository dependency.Repository, _ string) ([]byte, error) {
	github.calls["download"]++
	archive, ok := github.archives[github.key(repository)]
	if !ok {
		return nil, fmt.Errorf("no archive for %s", github.key(repository))
	}
	return append([]byte(nil), archive...), nil
}

func (github *multiRepoGitHub) DownloadReleaseAsset(context.Context, dependency.Repository, dependency.ReleaseAsset) ([]byte, error) {
	return nil, errors.New("unexpected release asset call")
}

// TestHostileEndToEndCoexistenceApplyAndFinalizeOnBothManifestShapes drives the
// real CLI over a consumer built from plugin.json and tile.json packages: apply
// writes ACR state and natives, every Tessl-owned byte and mode is frozen, the
// second apply is inert, and --finalize writes nothing at all.
func TestHostileEndToEndCoexistenceApplyAndFinalizeOnBothManifestShapes(t *testing.T) {
	project := seedDualManifestConsumer(t)
	github := newMultiRepoGitHub(map[string][]byte{
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
	application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
	mapFlags := []string{
		"--map", "example/alpha=github:example/alpha@v1.0.0",
		"--map", "example/beta=github:example/beta@v1.0.0",
	}

	tesslBefore := hostileTesslSurface(t, project)
	args := append([]string{"migrate", "tessl", "--json", "--project", project}, mapFlags...)
	stdout, stderr, exitCode := runCLI(t, application, args...)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("apply exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, `"wrote":true`) || !strings.Contains(stdout, `"mode":"coexistence"`) {
		t.Fatalf("apply report = %s", stdout)
	}
	for _, state := range []string{dependency.ProjectFilename, dependency.LockFilename} {
		if _, err := os.Stat(filepath.Join(project, filepath.FromSlash(state))); err != nil {
			t.Fatalf("apply did not write %s: %v", state, err)
		}
	}
	if !hostileHasACRNative(t, project) {
		t.Fatal("apply wrote no acr__ native output")
	}
	// Recorded, not fatal: the remaining end-to-end contract is still worth
	// exercising when the freeze breaks.
	if after := hostileTesslSurface(t, project); !mapsEqual(tesslBefore, after) {
		t.Errorf("apply changed Tessl-owned bytes or modes\nbefore=%v\nafter=%v", tesslBefore, after)
	}
	hostileAssertNoACRSpanUnderTessl(t, project)

	// Second apply on the converged tree writes nothing new.
	appliedTree := hostileProjectTree(t, project)
	stdout, stderr, exitCode = runCLI(t, application, args...)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("second apply exit = %d, stderr = %q", exitCode, stderr)
	}
	if !strings.Contains(stdout, `"wrote":false`) {
		t.Fatalf("second apply claimed a write: %s", stdout)
	}
	if after := hostileProjectTree(t, project); !mapsEqual(appliedTree, after) {
		t.Fatalf("second apply mutated the tree\nbefore=%v\nafter=%v", appliedTree, after)
	}

	// --finalize is parsed and gated; issue #2 never deletes anything.
	finalizeArgs := append([]string{"migrate", "tessl", "--finalize", "--json", "--project", project}, mapFlags...)
	stdout, stderr, exitCode = runCLI(t, application, finalizeArgs...)
	if exitCode != cli.ExitConflict && exitCode != cli.ExitOperational {
		t.Fatalf("finalize exit = %d, want 4 (blocked) or 1 (not_implemented); stdout = %q stderr = %q", exitCode, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("finalize refusal wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, `"code":"finalization_blocked"`) && !strings.Contains(stderr, `"code":"not_implemented"`) {
		t.Fatalf("finalize stderr = %q", stderr)
	}
	if after := hostileProjectTree(t, project); !mapsEqual(appliedTree, after) {
		t.Fatalf("finalize mutated the tree\nbefore=%v\nafter=%v", appliedTree, after)
	}
	if _, err := os.Stat(filepath.Join(project, "tessl.json")); err != nil {
		t.Fatalf("finalize removed tessl.json: %v", err)
	}
}

// TestHostileRefusalJSONIsPureAndWritesNothing walks every #2 refusal code the
// migrate surface can reach and holds each to the same bar: one JSON error line
// on stderr, empty stdout, and a byte/mode-identical tree.
func TestHostileRefusalJSONIsPureAndWritesNothing(t *testing.T) {
	for _, test := range []struct {
		name    string
		code    string
		exit    int
		arrange func(*testing.T) (string, cli.Application, []string)
	}{
		{
			name: "unmapped_package",
			code: "unmapped_package",
			exit: cli.ExitOperational,
			arrange: func(t *testing.T) (string, cli.Application, []string) {
				project := seedConsumer(t)
				return project, &Application{service: newService(&integrationGitHub{}), fallback: cli.UnavailableApplication{}}, nil
			},
		},
		{
			name: "mapping_conflict",
			code: "mapping_conflict",
			exit: cli.ExitOperational,
			arrange: func(t *testing.T) (string, cli.Application, []string) {
				project := seedConsumer(t)
				writeFile(t, project, "mapping.yaml", []byte("schemaVersion: 1\npackages:\n  - from: example/alpha\n    source: github:one/alpha\n  - from: example/alpha\n    source: github:two/alpha\n"), 0o644)
				return project, &Application{service: newService(&integrationGitHub{}), fallback: cli.UnavailableApplication{}}, []string{"--mapping-file", "mapping.yaml"}
			},
		},
		{
			name: "tessl_manifest_absent",
			code: "tessl_manifest_absent",
			exit: cli.ExitOperational,
			arrange: func(t *testing.T) (string, cli.Application, []string) {
				project := seedConsumer(t)
				if err := os.Remove(filepath.Join(project, "tessl.json")); err != nil {
					t.Fatal(err)
				}
				return project, &Application{service: newService(&integrationGitHub{}), fallback: cli.UnavailableApplication{}}, nil
			},
		},
		{
			name: "pending_transaction",
			code: "pending_transaction",
			exit: cli.ExitOperational,
			arrange: func(t *testing.T) (string, cli.Application, []string) {
				project := seedConsumer(t)
				hostileWriteJournal(t, project, "pending", 1)
				return project, hostileMigrateApplication(t), []string{"--dry-run", "--map", "example/alpha=github:example/alpha@latest"}
			},
		},
		{
			name: "unsupported_journal_version",
			code: "unsupported_journal_version",
			exit: cli.ExitOperational,
			arrange: func(t *testing.T) (string, cli.Application, []string) {
				project := seedConsumer(t)
				hostileWriteJournal(t, project, "old", 999)
				return project, hostileMigrateApplication(t), []string{"--dry-run", "--map", "example/alpha=github:example/alpha@latest"}
			},
		},
		{
			name: "recovery_conflict",
			code: "recovery_conflict",
			exit: cli.ExitOperational,
			arrange: func(t *testing.T) (string, cli.Application, []string) {
				project := seedConsumer(t)
				hostileSeedNeitherHashJournal(t, project)
				return project, hostileMigrateApplication(t), []string{"--map", "example/alpha=github:example/alpha@latest"}
			},
		},
		{
			name: "transaction_busy",
			code: "transaction_busy",
			exit: cli.ExitOperational,
			arrange: func(t *testing.T) (string, cli.Application, []string) {
				project := seedConsumer(t)
				hostileHoldClaim(t, project)
				return project, hostileMigrateApplication(t), []string{"--map", "example/alpha=github:example/alpha@latest"}
			},
		},
		{
			name: "finalization_blocked",
			code: "finalization_blocked",
			exit: cli.ExitConflict,
			arrange: func(t *testing.T) (string, cli.Application, []string) {
				project := seedConsumer(t)
				github := &integrationGitHub{
					release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("e", 40),
					archive: migrationPackageArchiveWithRule(t, "# Different\n"),
				}
				application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
				return project, application, []string{"--finalize", "--map", "example/alpha=github:example/alpha@latest"}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			project, application, extra := test.arrange(t)
			before := hashTreeWithModes(t, project)

			args := append([]string{"migrate", "tessl", "--json", "--project", project}, extra...)
			stdout, stderr, exitCode := runCLI(t, application, args...)

			if exitCode != test.exit {
				t.Fatalf("exit = %d, want %d; stdout = %q stderr = %q", exitCode, test.exit, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("refusal wrote to stdout: %q", stdout)
			}
			if strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") {
				t.Fatalf("refusal stderr must be exactly one JSON line, got %q", stderr)
			}
			var envelope struct {
				OK      bool   `json:"ok"`
				Command string `json:"command"`
				Error   struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
				t.Fatalf("refusal stderr is not a JSON envelope: %v: %q", err, stderr)
			}
			if envelope.OK || envelope.Command != "migrate" || envelope.Error.Code != test.code {
				t.Fatalf("envelope = %+v, want ok=false command=migrate code=%s", envelope, test.code)
			}
			if envelope.Error.Message == "" {
				t.Fatal("refusal carries no message")
			}
			if strings.Contains(stderr, project) {
				t.Fatalf("refusal leaked the host project path: %q", stderr)
			}

			after := hashTreeWithModes(t, project)
			// A mutating command legitimately creates the claim file before it
			// can classify the refusal; nothing else may move.
			delete(before, ".agents/.acr-transactions/.lock")
			delete(after, ".agents/.acr-transactions/.lock")
			delete(before, ".agents")
			delete(after, ".agents")
			delete(before, ".agents/.acr-transactions")
			delete(after, ".agents/.acr-transactions")
			if !mapsEqual(before, after) {
				t.Fatalf("%s refusal mutated the tree\nbefore=%v\nafter=%v", test.code, before, after)
			}
			for _, state := range []string{dependency.ProjectFilename, dependency.LockFilename} {
				if _, err := os.Stat(filepath.Join(project, filepath.FromSlash(state))); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s refusal wrote %s: %v", test.code, state, err)
				}
			}
		})
	}
}

// TestHostileHandEditedNativeBetweenInventoryAndApplySurvivesApply covers the
// window the plan names: the operator edits a Tessl native after the dry-run
// inventory and before apply. Apply must succeed, freeze the edit, and refuse
// finalization; restoring the native from the plugin tree clears the gate.
func TestHostileHandEditedNativeBetweenInventoryAndApplySurvivesApply(t *testing.T) {
	project := seedConsumer(t)
	native := ".cursor/rules/tessl__rule__example__alpha__always-rule.mdc"
	source, err := os.ReadFile(filepath.Join(project, filepath.FromSlash(".tessl/plugins/example/alpha/rules/always-rule.md")))
	if err != nil {
		t.Fatal(err)
	}
	clean := append([]byte("---\nalwaysApply: true\n---\n\n"), source...)
	writeFile(t, project, native, clean, 0o644)

	github := &integrationGitHub{
		release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("a", 40), archive: migrationPackageArchive(t),
	}
	application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
	args := []string{"migrate", "tessl", "--json", "--project", project, "--map", "example/alpha=github:example/alpha@latest"}

	// Inventory first, then the operator edits the native, then apply.
	if _, _, exitCode := runCLI(t, application, append(append([]string{}, args...), "--dry-run")...); exitCode != cli.ExitSuccess {
		t.Fatalf("inventory exit = %d", exitCode)
	}
	edited := append(append([]byte(nil), clean...), []byte("hand-edited after inventory\n")...)
	writeFile(t, project, native, edited, 0o644)

	stdout, stderr, exitCode := runCLI(t, application, args...)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("apply exit = %d, stderr = %q", exitCode, stderr)
	}
	if !strings.Contains(stdout, `"wrote":true`) || !strings.Contains(stdout, `"finalizationReady":false`) {
		t.Fatalf("apply report = %s", stdout)
	}
	assertFileBytes(t, filepath.Join(project, filepath.FromSlash(native)), edited)

	applied := hostileProjectTree(t, project)
	_, stderr, exitCode = runCLI(t, application, append(append([]string{}, args...), "--finalize")...)
	if exitCode != cli.ExitConflict || !strings.Contains(stderr, `"code":"finalization_blocked"`) {
		t.Fatalf("finalize exit = %d, stderr = %q", exitCode, stderr)
	}
	if after := hostileProjectTree(t, project); !mapsEqual(applied, after) {
		t.Fatalf("blocked finalize mutated the tree\nbefore=%v\nafter=%v", applied, after)
	}
	assertFileBytes(t, filepath.Join(project, filepath.FromSlash(native)), edited)

	// Restoring the native from the plugin tree clears the gate on a fresh run.
	writeFile(t, project, native, clean, 0o644)
	stdout, stderr, exitCode = runCLI(t, application, append(append([]string{}, args...), "--dry-run")...)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("restored dry-run exit = %d, stderr = %q", exitCode, stderr)
	}
	if !strings.Contains(stdout, `"finalizationReady":true`) {
		t.Fatalf("restored fixture is still blocked: %s", stdout)
	}
}

// TestHostilePluginPathStopHookNeedsALiveManifest drives the ownership claim
// end to end rather than through the private predicate: the same Stop hook is
// Tessl-owned with a live tessl.json and unreachable without one.
func TestHostilePluginPathStopHookNeedsALiveManifest(t *testing.T) {
	project := seedConsumer(t)
	writeFile(t, project, ".tessl/plugins/example/alpha/hooks/stop-handoff-hygiene.sh", []byte("#!/bin/sh\necho stop\n"), 0o755)
	writeJSON(t, project, ".claude/settings.json", map[string]any{
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{"hooks": []any{map[string]any{
					"type": "command", "command": "bash",
					"args": []string{"${CLAUDE_PROJECT_DIR}/.tessl/plugins/example/alpha/hooks/stop-handoff-hygiene.sh"},
				}}},
				map[string]any{"hooks": []any{map[string]any{
					"type": "command", "command": "./scripts/notify.sh --state .tessl/state.json",
				}}},
			},
		},
	})

	github := &integrationGitHub{
		release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("a", 40), archive: migrationPackageArchive(t),
	}
	report, err := newService(github).Migrate(context.Background(), project, Options{
		DryRun: true, CLIMappings: hostileMappings(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hostileOwns(report.TesslOwned, ".claude/settings.json") {
		t.Fatalf("live plugin-path Stop hook is not tesslOwned: %#v", report.TesslOwned)
	}
	for _, record := range report.Unmanaged {
		if record.Path == ".claude/settings.json" && strings.Contains(record.ID, "stop-handoff-hygiene") {
			t.Fatalf("live plugin-path Stop hook landed in unmanaged: %#v", record)
		}
	}

	// The same tree with tessl.json removed refuses before enumerating plugins.
	if err := os.Remove(filepath.Join(project, "tessl.json")); err != nil {
		t.Fatal(err)
	}
	_, err = newService(github).Migrate(context.Background(), project, Options{DryRun: true, CLIMappings: hostileMappings(t)})
	var migrationErr *Error
	if !errors.As(err, &migrationErr) || migrationErr.Code != "tessl_manifest_absent" {
		t.Fatalf("error = %#v, want tessl_manifest_absent", err)
	}
}

// TestHostileFinalizationRequiresCurrentCoexistenceState proves #8 refuses to
// remove Tessl until the host-selection-safe ACR realization has been applied.
func TestHostileFinalizationRequiresCurrentCoexistenceState(t *testing.T) {
	project := seedConsumer(t)
	github := &integrationGitHub{
		release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("a", 40), archive: migrationPackageArchive(t),
	}
	application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
	args := []string{"migrate", "tessl", "--json", "--project", project, "--map", "example/alpha=github:example/alpha@latest"}
	_, stderr, exitCode := runCLI(t, application, append(append([]string{}, args...), "--finalize")...)
	if strings.Contains(stderr, "finalization_conflict") {
		t.Fatalf("current-state gate emitted finalization_conflict: %q", stderr)
	}
	if exitCode != cli.ExitConflict || !strings.Contains(stderr, `"code":"finalization_blocked"`) {
		t.Fatalf("pre-apply finalize exit = %d, stderr = %q, want finalization_blocked", exitCode, stderr)
	}
	if _, err := os.Stat(filepath.Join(project, "tessl.json")); err != nil {
		t.Fatalf("blocked finalize removed tessl.json: %v", err)
	}
	if _, stderr, exitCode = runCLI(t, application, args...); exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("coexistence apply exit = %d, stderr = %q", exitCode, stderr)
	}
	stdout, stderr, exitCode := runCLI(t, application, append(append([]string{}, args...), "--finalize")...)
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"mode":"finalized"`) {
		t.Fatalf("finalize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(project, "tessl.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("finalize retained tessl.json: %v", err)
	}
}

// TestHostileTransactionLockUnavailableMapsToItsCLICode pins the one refusal
// class the end-to-end matrix cannot reach without injecting into another
// package: the CLI must translate the realize error, not fall back to
// migrate_failed.
func TestHostileTransactionLockUnavailableMapsToItsCLICode(t *testing.T) {
	err := migrateCLIError(&realize.TransactionLockUnavailableError{
		Path: ".agents/.acr-transactions/.lock", Err: syscall.ENOLCK,
	})
	var cliErr *cli.Error
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %#v, want *cli.Error", err)
	}
	if cliErr.Code != "transaction_lock_unavailable" || cliErr.ExitCode != cli.ExitOperational {
		t.Fatalf("cli error = %+v, want transaction_lock_unavailable at exit 1", cliErr)
	}
	if !strings.Contains(cliErr.Message, ".agents/.acr-transactions/.lock") {
		t.Fatalf("message %q does not name the claim path", cliErr.Message)
	}
}

// TestHostileCodexHostSelectionNeverLandsInsideTheTesslPluginTree isolates the
// coexistence break the end-to-end run surfaces. Tessl's `.tessl/RULES.md`
// includes each plugin's rule source, and `AGENTS.md` includes that file, so
// Codex's include graph makes a file under `.tessl/plugins/**` the deepest
// shared host. Apply then splices an ACR managed block into Tessl's own package
// source, which the first acceptance criterion forbids. Claude Code and Cursor
// do not select that host, so the trigger is a consumer with a Codex tree --
// exactly the ground-truth consumer shape the issue targets.
func TestHostileCodexHostSelectionNeverLandsInsideTheTesslPluginTree(t *testing.T) {
	for _, test := range []struct {
		name  string
		agent func(*testing.T, string)
	}{
		{
			name: "codex",
			agent: func(t *testing.T, project string) {
				writeFile(t, project, ".codex/config.toml", []byte("[[hooks.SessionStart]]\n[[hooks.SessionStart.hooks]]\ntype = \"command\"\ncommand = \"tessl hook run --plugin-path='.tessl/plugins/example/alpha' --event='SessionStart' --agent=codex --schema-version=1\"\n"), 0o644)
			},
		},
		{
			name: "claude-code",
			agent: func(t *testing.T, project string) {
				writeJSON(t, project, ".claude/settings.json", map[string]any{"hooks": map[string]any{"SessionStart": []any{
					map[string]any{"hooks": []any{map[string]any{"type": "command", "command": `tessl hook run --plugin-path=".tessl/plugins/example/alpha" --event="SessionStart" --agent=claude-code --schema-version=1`}}},
				}}})
			},
		},
		{
			name: "cursor",
			agent: func(t *testing.T, project string) {
				writeJSON(t, project, ".cursor/hooks.json", map[string]any{"version": 1, "hooks": map[string]any{"sessionStart": []any{
					map[string]any{"command": `tessl hook run --plugin-path=".tessl/plugins/example/alpha" --event="sessionStart" --agent=cursor --schema-version=1`},
				}}})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := seedConsumer(t)
			test.agent(t, project)
			before := hostileTesslSurface(t, project)

			github := &integrationGitHub{
				release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("a", 40), archive: migrationPackageArchive(t),
			}
			application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
			_, stderr, exitCode := runCLI(t, application, "migrate", "tessl", "--json", "--project", project,
				"--map", "example/alpha=github:example/alpha@latest")
			if exitCode != cli.ExitSuccess || stderr != "" {
				t.Fatalf("apply exit = %d, stderr = %q", exitCode, stderr)
			}

			hostileAssertNoACRSpanUnderTessl(t, project)
			if after := hostileTesslSurface(t, project); !mapsEqual(before, after) {
				t.Fatalf("apply mutated the Tessl surface\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

// hostileAssertNoACRSpanUnderTessl fails when any file under a Tessl-owned path
// carries an ACR managed span.
func hostileAssertNoACRSpanUnderTessl(t *testing.T, project string) {
	t.Helper()
	err := filepath.WalkDir(filepath.Join(project, ".tessl"), func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		content, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		if bytes.Contains(content, []byte("acr:begin")) {
			relative, relErr := filepath.Rel(project, filename)
			if relErr != nil {
				return relErr
			}
			t.Errorf("ACR wrote a managed span into the Tessl-owned path %s:\n%s", filepath.ToSlash(relative), content)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	tesslManifest := filepath.Join(project, "tessl.json")
	content, err := os.ReadFile(tesslManifest)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte("acr:begin")) {
		t.Errorf("ACR wrote a managed span into tessl.json:\n%s", content)
	}
}

func hostileMappings(t *testing.T) []migrate.Mapping {
	t.Helper()
	mappings, err := migrate.ParseInlineMappings([]string{"example/alpha=github:example/alpha@latest"})
	if err != nil {
		t.Fatal(err)
	}
	return mappings
}

func hostileMigrateApplication(t *testing.T) cli.Application {
	t.Helper()
	github := &integrationGitHub{
		release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("a", 40), archive: migrationPackageArchive(t),
	}
	return &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
}

func hostileOwns(records []migrate.OwnershipRecord, path string) bool {
	for _, record := range records {
		if record.Path == path {
			return true
		}
	}
	return false
}

// hostileHoldClaim takes a real exclusive flock on the project's claim file and
// holds it for the rest of the test, so the contender's refusal is proved by
// LOCK_NB rather than by any elapsed time.
func hostileHoldClaim(t *testing.T, project string) {
	t.Helper()
	lock := filepath.Join(project, filepath.FromSlash(".agents/.acr-transactions/.lock"))
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
			t.Error(err)
		}
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
}

// hostileWriteJournal drops a canonical, complete journal with no entries at the
// requested schema version.
func hostileWriteJournal(t *testing.T, project, id string, version int) {
	t.Helper()
	journal := filepath.Join(project, filepath.FromSlash(".agents/.acr-transactions"), id)
	if err := os.MkdirAll(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{"schemaVersion":%d,"id":%q,"entries":[]}`+"\n", version, id)
	if err := os.WriteFile(filepath.Join(journal, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hostileSeedNeitherHashJournal writes a journal whose single target matches
// neither the before-image nor the recorded after hash, which is the concurrent
// operator edit recovery must refuse.
func hostileSeedNeitherHashJournal(t *testing.T, project string) {
	t.Helper()
	journal := filepath.Join(project, filepath.FromSlash(".agents/.acr-transactions"), "conflicted")
	if err := os.MkdirAll(filepath.Join(journal, "before"), 0o700); err != nil {
		t.Fatal(err)
	}
	before := []byte("before\n")
	if err := os.WriteFile(filepath.Join(journal, "before", "000000"), before, 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, project, "AGENTS.md", []byte("operator rewrote this\n"), 0o644)
	manifest := map[string]any{
		"schemaVersion": 1,
		"id":            "conflicted",
		"entries": []map[string]any{{
			"path":         "AGENTS.md",
			"beforeExists": true,
			"beforeHash":   hostileHash(before),
			"beforeSize":   len(before),
			"beforeMode":   0o644,
			"beforeImage":  "before/000000",
			"afterExists":  true,
			"afterHash":    hostileHash([]byte("after\n")),
			"afterMode":    0o644,
		}},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(journal, "manifest.json"), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func hostileHash(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func hostileHasACRNative(t *testing.T, root string) bool {
	t.Helper()
	found := false
	for path := range hashTreeWithModes(t, root) {
		if strings.Contains(path, "acr__") {
			found = true
		}
	}
	return found
}

// hostileProjectTree hashes every project path with its mode and symlink target
// but drops the transaction claim, which a converged apply legitimately leaves
// in place once .agents/ already exists.
func hostileProjectTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := hashTreeWithModes(t, root)
	delete(tree, ".agents/.acr-transactions")
	delete(tree, ".agents/.acr-transactions/.lock")
	return tree
}

func hostileTesslSurface(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	for path, digest := range hashTreeWithModes(t, root) {
		if path == "tessl.json" || path == ".gitignore" || strings.HasPrefix(path, ".tessl/") ||
			strings.Contains(path, "/tessl__") || strings.HasSuffix(path, "/mcp.json") ||
			path == ".claude/settings.local.json" {
			result[path] = digest
		}
	}
	return result
}

func hostileArchive(t *testing.T, name string, files map[string]string) []byte {
	t.Helper()
	manifest := "schemaVersion: 1\nname: " + name + "\nversion: 1.0.0\nsource:\n  repository: https://github.com/" + name + "\nartifacts:\n"
	var rules, skills []string
	for path := range files {
		switch {
		case strings.HasPrefix(path, "rules/"):
			rules = append(rules, path)
		case strings.HasPrefix(path, "skills/"):
			skills = append(skills, path)
		}
	}
	sortStrings(rules)
	sortStrings(skills)
	if len(rules) > 0 {
		manifest += "  rules:\n"
		for _, path := range rules {
			id := strings.TrimSuffix(strings.TrimPrefix(path, "rules/"), ".md")
			manifest += "    - id: " + id + "\n      path: " + path + "\n      activation:\n        mode: always\n"
		}
	}
	if len(skills) > 0 {
		manifest += "  skills:\n"
		for _, path := range skills {
			id := strings.TrimSuffix(strings.TrimPrefix(path, "skills/"), "/SKILL.md")
			manifest += "    - id: " + id + "\n      path: skills/" + id + "\n"
		}
	}
	payload := map[string]string{"agent-plugin.yaml": manifest}
	for path, content := range files {
		payload[path] = content
	}
	var encoded bytes.Buffer
	gzipWriter := gzip.NewWriter(&encoded)
	tarWriter := tar.NewWriter(gzipWriter)
	names := make([]string, 0, len(payload))
	for path := range payload {
		names = append(names, path)
	}
	sortStrings(names)
	for _, path := range names {
		data := []byte(payload[path])
		header := &tar.Header{Name: "archive-root/" + path, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func sortStrings(values []string) {
	for outer := 1; outer < len(values); outer++ {
		for inner := outer; inner > 0 && values[inner] < values[inner-1]; inner-- {
			values[inner], values[inner-1] = values[inner-1], values[inner]
		}
	}
}

// TestHostileUnmappedPackageBlocksItsMappedSiblings is the claim the plan
// amendment replaced the lone-orphan test to prove: a single unmapped package
// must leave every explicitly mapped sibling unwritten and unresolved too.
func TestHostileUnmappedPackageBlocksItsMappedSiblings(t *testing.T) {
	project := seedConsumer(t)
	dependencies := map[string]any{"example/alpha": map[string]string{"version": "1.0.0"}}
	mapFlags := []string{"--map", "example/alpha=github:example/alpha@latest"}
	for _, name := range []string{"beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota"} {
		identity := "example/" + name
		dependencies[identity] = map[string]string{"version": "1.0.0"}
		writeJSON(t, project, ".tessl/plugins/"+identity+"/.tessl-plugin/plugin.json", map[string]any{
			"name": identity, "version": "1.0.0", "rules": []string{"rules/always-rule.md"},
		})
		writeFile(t, project, ".tessl/plugins/"+identity+"/rules/always-rule.md",
			[]byte("---\nalwaysApply: true\n---\n# Always\n"), 0o644)
		mapFlags = append(mapFlags, "--map", identity+"=github:"+identity+"@latest")
	}
	// The tenth package is the blocker: declared by Tessl, mapped by nobody.
	dependencies["example/orphan"] = map[string]string{"version": "1.0.0"}
	writeJSON(t, project, ".tessl/plugins/example/orphan/.tessl-plugin/plugin.json", map[string]any{
		"name": "example/orphan", "version": "1.0.0", "rules": []string{"rules/always-rule.md"},
	})
	writeFile(t, project, ".tessl/plugins/example/orphan/rules/always-rule.md",
		[]byte("---\nalwaysApply: true\n---\n# Always\n"), 0o644)
	writeJSON(t, project, "tessl.json", map[string]any{
		"name": "consumer", "mode": "vendored", "dependencies": dependencies,
	})

	before := hostileProjectTree(t, project)
	github := &integrationGitHub{
		release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("a", 40), archive: migrationPackageArchive(t),
	}
	application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
	args := append([]string{"migrate", "tessl", "--json", "--non-interactive", "--project", project}, mapFlags...)
	stdout, stderr, exitCode := runCLI(t, application, args...)

	if exitCode != cli.ExitOperational || stdout != "" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stderr, `"code":"unmapped_package"`) || !strings.Contains(stderr, "example/orphan") {
		t.Fatalf("stderr = %q, want unmapped_package naming example/orphan", stderr)
	}
	if github.latestCalls != 0 || github.resolveCalls != 0 || github.downloadCalls != 0 {
		t.Fatalf("nine mapped siblings still reached GitHub: latest=%d resolve=%d download=%d",
			github.latestCalls, github.resolveCalls, github.downloadCalls)
	}
	if after := hostileProjectTree(t, project); !mapsEqual(before, after) {
		t.Fatalf("one unmapped package still wrote mapped siblings\nbefore=%v\nafter=%v", before, after)
	}
}

// TestHostileDuplicateEffectIsDerivedSortedAndNegative drives the warning off a
// real seeded consumer instead of a hand-built report: one warning per event
// both dispatchers own, sorted, and none for an event only one of them owns.
func TestHostileDuplicateEffectIsDerivedSortedAndNegative(t *testing.T) {
	project := seedConsumer(t)
	// Tessl owns session-start and stop; the ACR archive supplies session-start
	// only, so stop must never warn.
	writeFile(t, project, ".tessl/plugins/example/alpha/hooks/stop.sh", []byte("#!/bin/sh\necho stop\n"), 0o755)
	writeJSON(t, project, ".tessl/plugins/example/alpha/.tessl-plugin/plugin.json", map[string]any{
		"name": "example/alpha", "version": "1.0.0",
		"rules":  []string{"rules/always-rule.md"},
		"skills": []string{"skills/review-change"},
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/session-start.sh"},
			}}}},
			"Stop": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"},
			}}}},
		},
	})

	github := &integrationGitHub{
		release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("a", 40), archive: migrationPackageArchive(t),
	}
	report, err := newService(github).Migrate(context.Background(), project, Options{DryRun: true, CLIMappings: hostileMappings(t)})
	if err != nil {
		t.Fatal(err)
	}

	var events []string
	for _, note := range report.Notes {
		if note.Code != "duplicate-effect" {
			continue
		}
		if note.Tessl == "" || note.ACR == "" {
			t.Errorf("duplicate-effect note names only one command: %#v", note)
		}
		events = append(events, note.Event)
	}
	if len(events) != 1 || events[0] != "session-start" {
		t.Fatalf("duplicate-effect events = %#v, want exactly [session-start]; stop has no ACR dispatcher", events)
	}
	for index := 1; index < len(events); index++ {
		if events[index] < events[index-1] {
			t.Fatalf("duplicate-effect notes are not sorted by event: %#v", events)
		}
	}
	text := migrate.FormatCoexistenceText(report)
	if !strings.Contains(text, "WARNING duplicate-effect session-start:") {
		t.Fatalf("text output carries no session-start warning:\n%s", text)
	}
	if strings.Contains(text, "WARNING duplicate-effect stop:") {
		t.Fatalf("text output warned for an event only Tessl dispatches:\n%s", text)
	}
}

// TestHostileSharedInstructionHostCoexists covers the plan row that has no
// shipped test: the user prefix and the Tessl-managed heading in AGENTS.md are
// preserved byte-for-byte, and ACR's block lands in a shared host rather than
// inside Tessl's own tree.
func TestHostileSharedInstructionHostCoexists(t *testing.T) {
	project := seedConsumer(t)
	writeFile(t, project, ".codex/config.toml", []byte("[[hooks.SessionStart]]\n[[hooks.SessionStart.hooks]]\ntype = \"command\"\ncommand = \"tessl hook run --event='SessionStart' --agent=codex\"\n"), 0o644)
	writeFile(t, project, "CLAUDE.md", []byte("# Claude notes\n\n@AGENTS.md\n"), 0o644)
	claudeBefore, err := os.ReadFile(filepath.Join(project, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}

	github := &integrationGitHub{
		release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("a", 40), archive: migrationPackageArchive(t),
	}
	application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
	if _, stderr, exitCode := runCLI(t, application, "migrate", "tessl", "--json", "--project", project,
		"--map", "example/alpha=github:example/alpha@latest"); exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("apply exit = %d, stderr = %q", exitCode, stderr)
	}

	agents, err := os.ReadFile(filepath.Join(project, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(agents, []byte("# User\n")) {
		t.Errorf("user prefix was not preserved at the head of AGENTS.md:\n%s", agents)
	}
	if !bytes.Contains(agents, []byte("## Agent Rules <!-- tessl-managed -->\n\n@.tessl/RULES.md")) {
		t.Errorf("Tessl-managed span was not preserved in AGENTS.md:\n%s", agents)
	}
	if !bytes.Contains(agents, []byte("acr:begin")) {
		t.Errorf("ACR wrote no managed block into the shared instruction host:\n%s", agents)
	}
	assertFileBytes(t, filepath.Join(project, "CLAUDE.md"), claudeBefore)
	hostileAssertNoACRSpanUnderTessl(t, project)
}
