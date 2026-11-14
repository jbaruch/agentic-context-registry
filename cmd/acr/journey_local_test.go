package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestLocalBuiltCLIJourney(t *testing.T) {
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	pkg := newJourneyPackage(t, "local/example", "1.0.0")
	source := filepath.Join(t.TempDir(), "plugin@dev")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := dependency.ExtractPackageArchive(pkg.archive, source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("undeclared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	originalSource := snapshotProjectTree(t, source)
	pathFromProject, err := filepath.Rel(project.root, source)
	if err != nil {
		t.Fatal(err)
	}
	project.runBinary(binary, 0, "install", "file:"+pathFromProject, "--agent", "codex", "--agent", "claude-code", "--agent", "cursor", "--freshness", "none", "--non-interactive")
	state := loadJourneyState(t, project)
	if state.Project.SchemaVersion != 5 || state.Lock.Dependencies[0].Kind != dependency.ResolutionLocal || state.Lock.Dependencies[0].Path != filepath.ToSlash(pathFromProject) {
		t.Fatalf("state: %+v", state)
	}
	project.runBinary(binary, 0, "realize")
	project.runBinary(binary, 0, "check")
	list := project.runBinary(binary, 0, "list")
	if !strings.Contains(list.output(), "local") || !strings.Contains(list.output(), "authorized on this machine") {
		t.Fatalf("list missing local warning: %s", list.output())
	}
	if !reflect.DeepEqual(originalSource, snapshotProjectTree(t, source)) {
		t.Fatal("install/realize mutated source")
	}
	before := project.snapshot()
	project.runBinary(binary, 0, "install")
	project.assertUnchanged(before, "unchanged local refresh")
	if err := os.WriteFile(filepath.Join(source, "rules/boundaries.md"), []byte("# Edited local guidance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"realize"}, {"realize", "--dry-run"}, {"check"}} {
		result := project.runBinary(binary, 1, args...)
		if !strings.Contains(result.output(), "local_source_changed") {
			t.Fatalf("wrong drift refusal: %s", result.output())
		}
		project.assertUnchanged(before, "locked local drift")
	}
	project.runBinary(binary, 0, "install")
	project.runBinary(binary, 0, "realize")
	project.runBinary(binary, 0, "check")
	// A fresh clone has only committed state and no transaction claim paths.
	copied := &journeyProject{t: t, root: t.TempDir(), stateHome: t.TempDir()}
	if err := os.Mkdir(filepath.Join(copied.root, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agents.yaml", ".agents/registry.lock"} {
		data, err := os.ReadFile(filepath.Join(project.root, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(copied.root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	copiedBefore := copied.snapshot()
	copied.runBinary(binary, 1, "realize")
	copied.assertUnchanged(copiedBefore, "unauthorized clone before recovery")
	// A second machine has identical project/source bytes but no local grant.
	project.stateHome = t.TempDir()
	t.Setenv("ACR_STATE_HOME", project.stateHome)
	before = project.snapshot()
	for _, args := range [][]string{{"realize"}, {"check"}, {"install"}, {"freshness", "run", "--policy", "install"}} {
		result := project.runBinary(binary, 1, args...)
		if !strings.Contains(result.output(), "local_source_unauthorized") {
			t.Fatalf("implicit adoption: %s", result.output())
		}
		project.assertUnchanged(before, "unauthorized local source")
	}
	project.runBinary(binary, 0, "list")
	project.runBinary(binary, 0, "outdated")
	project.runBinary(binary, 0, "install", "file:"+pathFromProject)
	project.runBinary(binary, 0, "realize")
	// Freshness install refreshes edits only after explicit directory consent.
	if err := os.WriteFile(filepath.Join(source, "rules/boundaries.md"), []byte("# Freshness revision\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clock := newJourneyClock()
	project.useClock(clock)
	if err := (freshness.Store{BaseDirectory: project.stateHome}).Write(project.root, clock.Now().Add(-freshness.Window), freshness.PolicyInstall, freshness.OutcomeFailed); err != nil {
		t.Fatal(err)
	}
	project.run(0, "freshness", "run", "--policy", "install")
	project.runBinary(binary, 0, "check")
	// Uninstall needs neither the source nor the grant.
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	project.stateHome = t.TempDir()
	t.Setenv("ACR_STATE_HOME", project.stateHome)
	project.runBinary(binary, 0, "uninstall", pkg.source)
	state = loadJourneyState(t, project)
	ledger, err := realize.DecodeLedger(state.Lock.Realization)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Project.Dependencies) != 0 || len(state.Lock.Dependencies) != 0 || len(ledger.Targets) != 0 {
		t.Fatalf("uninstall left local ownership: %+v", state)
	}
	if state.Project.SchemaVersion != 2 || state.Lock.SchemaVersion != 2 {
		t.Fatalf("uninstall did not downgrade schema: %+v", state)
	}
}

func TestLocalMatchesReleaseNativeOutput(t *testing.T) {
	remote := newJourneyGitHub(t)
	pkg := newJourneyPackage(t, "local/parity", "1.0.0")
	remote.SeedRelease(pkg.fullName, pkg.tag, pkg.commit, pkg.archive)
	project := newJourneyProject(t, remote)
	source := t.TempDir()
	if err := dependency.ExtractPackageArchive(pkg.archive, source); err != nil {
		t.Fatal(err)
	}
	project.run(0, "init", "--agent", "codex", "--agent", "claude-code", "--agent", "cursor", "--freshness", "none", "--non-interactive")
	project.run(0, "install", "file:"+source)
	project.run(0, "realize")
	local := loadJourneyState(t, project)
	before := project.snapshot()
	project.run(0, "install", pkg.source)
	released := loadJourneyState(t, project)
	if released.Lock.Dependencies[0].ContentHash != local.Lock.Dependencies[0].ContentHash {
		t.Fatal("release/local inventory hashes differ")
	}
	if released.Project.SchemaVersion != 2 || released.Lock.SchemaVersion != 2 || released.Project.Dependencies[0].Path != "" {
		t.Fatalf("remote replacement retained local declaration: %+v", released)
	}
	project.run(0, "realize")
	after := project.snapshot()
	for _, statePath := range []string{"agents.yaml", ".agents/registry.lock"} {
		delete(before, statePath)
		delete(after, statePath)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("local/release native output differs\nbefore=%v\nafter=%v", before, after)
	}
	if !reflect.DeepEqual(local.Lock.Realization, loadJourneyState(t, project).Lock.Realization) {
		t.Fatal("local/release ledger differs")
	}
}

func TestLocalDotInstallUsesSelectedProject(t *testing.T) {
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	pkg := newJourneySmallPackage(t, "local/self", "1.0.0")
	if err := dependency.ExtractPackageArchive(pkg.archive, project.root); err != nil {
		t.Fatal(err)
	}
	project.runBinary(binary, 0, "install", ".", "--agent", "codex", "--freshness", "none", "--non-interactive")
	original := loadJourneyState(t, project).Lock.Dependencies[0]
	if original.Path != "." {
		t.Fatalf("dot path became %q", original.Path)
	}
	project.runBinary(binary, 0, "realize")
	project.runBinary(binary, 0, "install", "./")
	if got := loadJourneyState(t, project).Lock.Dependencies[0]; got.ContentHash != original.ContentHash {
		t.Fatalf("native outputs changed local inventory: %+v", got)
	}
	for _, file := range pkg.files {
		if body, err := os.ReadFile(filepath.Join(project.root, file.path)); err != nil || string(body) != file.body {
			t.Fatalf("source file changed: %s %v", file.path, err)
		}
	}
}

func TestLocalUninstallRevokesAuthorizationWithoutLockRow(t *testing.T) {
	project := newJourneyProject(t, nil)
	pkg := newJourneySmallPackage(t, "local/missing-lock", "1.0.0")
	source := t.TempDir()
	if err := dependency.ExtractPackageArchive(pkg.archive, source); err != nil {
		t.Fatal(err)
	}
	project.run(0, "install", "file:"+source, "--agent", "codex", "--freshness", "none", "--non-interactive")
	state := loadJourneyState(t, project)
	state.Lock.Dependencies = nil
	if err := dependency.WriteState(project.root, state); err != nil {
		t.Fatal(err)
	}
	project.run(0, "uninstall", pkg.source)
	records := 0
	if err := filepath.WalkDir(filepath.Join(project.stateHome, "local"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".json") {
			records++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if records != 0 {
		t.Fatal("uninstall retained directory authorization after missing lock")
	}
}

// A bare name remains a source even when a same-named directory exists.
// Only explicit path syntax grants permission to install that directory.
func TestLocalBareNameInstallHint(t *testing.T) {
	binary := journeyBuiltBinary(t)
	for _, directory := range []bool{false, true} {
		for _, format := range []string{"text", "json"} {
			name := "missing-directory/" + format
			if directory {
				name = "existing-directory/" + format
			}
			t.Run(name, func(t *testing.T) {
				project := &journeyProject{t: t, root: t.TempDir(), stateHome: t.TempDir()}
				project.runBinary(binary, 0, "init", "--agent", "codex", "--freshness", "none", "--non-interactive")
				if directory {
					source := project.path("my-plugin")
					if err := os.Mkdir(source, 0o755); err != nil {
						t.Fatal(err)
					}
					pkg := newJourneySmallPackage(t, "local/hint", "1.0.0")
					if err := dependency.ExtractPackageArchive(pkg.archive, source); err != nil {
						t.Fatal(err)
					}
				}
				before := project.snapshot()
				storeBefore := snapshotProjectTree(t, project.stateHome)
				args := []string{"install", "my-plugin"}
				if format == "json" {
					args = append(args, "--json")
				}
				result := project.runBinary(binary, 1, args...)
				project.assertUnchanged(before, "bare-name install refusal, including source")
				assertTreeUnchanged(t, storeBefore, project.stateHome, "bare-name install authorization store")
				if result.stdout != "" {
					t.Fatalf("refusal wrote stdout: %s", result.stdout)
				}
				message := result.stderr
				if format == "json" {
					var envelope struct {
						OK      bool   `json:"ok"`
						Command string `json:"command"`
						Error   struct {
							Code    string `json:"code"`
							Message string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal([]byte(result.stderr), &envelope); err != nil {
						t.Fatal(err)
					}
					if envelope.OK || envelope.Command != "install" || envelope.Error.Code != "dependency_operation_failed" {
						t.Fatalf("changed refusal contract: %s", result.stderr)
					}
					message = envelope.Error.Message
				} else if !strings.HasPrefix(message, "acr install: ") {
					t.Errorf("missing text diagnostic prefix: %s", message)
				}
				for _, want := range []string{`invalid source "my-plugin"`, "github:owner/repository", "./my-plugin", "local directory"} {
					if !strings.Contains(message, want) {
						t.Errorf("missing %q in actual CLI refusal: %s", want, message)
					}
				}
				if directory {
					// The suggested spelling must actually select and install the local package.
					sourceBefore := snapshotProjectTree(t, project.path("my-plugin"))
					project.runBinary(binary, 0, "install", "./my-plugin")
					state := loadJourneyState(t, project)
					if len(state.Lock.Dependencies) != 1 || state.Lock.Dependencies[0].Kind != dependency.ResolutionLocal || state.Lock.Dependencies[0].Source != "github:local/hint" || state.Lock.Dependencies[0].Path != "my-plugin" {
						t.Fatalf("explicit local install changed identity or dispatch: %+v", state)
					}
					assertTreeUnchanged(t, sourceBefore, project.path("my-plugin"), "explicit local install source")
				}
			})
		}
	}
}

func TestSymlinkedClaudeInstructionHost(t *testing.T) {
	t.Setenv("ACR_VERSION_CHECK", "off")
	pkg := minimalPluginArg(t)

	t.Run("auto-detect claude and codex", func(t *testing.T) {
		project := claudeAliasProject(t, "AGENTS.md")
		project.run(0, installArgs(pkg)...)
		assertAgents(t, project.root, "claude-code", "codex")
		realizeClaudeAlias(t, project, "codex")
	})

	t.Run("claude-code alone", func(t *testing.T) {
		project := claudeAliasProject(t, "AGENTS.md")
		project.run(0, installArgs(pkg, "claude-code")...)
		realizeClaudeAlias(t, project, "claude-code")
	})

	t.Run("codex alone leaves the symlink untouched", func(t *testing.T) {
		project := claudeAliasProject(t, "AGENTS.md")
		project.run(0, installArgs(pkg, "codex")...)
		realizeClaudeAlias(t, project, "codex")
	})

	t.Run("chain of symlinks", func(t *testing.T) {
		project := newGitJourneyProject(t)
		writeProjectText(t, project.root, "AGENTS.md", "# Agents\n")
		if err := os.Symlink("AGENTS.md", filepath.Join(project.root, "mid")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("mid", filepath.Join(project.root, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		project.run(0, installArgs(pkg)...)
		project.run(0, "realize")
		assertOneManagedBlock(t, project.root, "AGENTS.md", "codex")
		assertLink(t, project.root, "CLAUDE.md", "mid")
		assertLink(t, project.root, "mid", "AGENTS.md")
	})

	t.Run("dangling install writes no state", func(t *testing.T) {
		project := claudeAliasProject(t, "missing.md")
		result := project.run(1, installArgs(pkg)...)
		assertSymlinkRefusalText(t, result.stderr, "CLAUDE.md", "missing.md", "symlink target is missing")
		assertNoSetupState(t, project.root)
	})

	for _, spelling := range []string{"relative", "absolute", "chain", "absolute chain"} {
		for _, suffix := range []string{"/", "/.", "//", "/.."} {
			t.Run("regular file suffix/"+spelling+suffix, func(t *testing.T) {
				project := newGitJourneyProject(t)
				writeProjectText(t, project.root, "AGENTS.md", "# Agents\n")
				target := "AGENTS.md" + suffix
				if strings.Contains(spelling, "chain") {
					if err := os.Symlink("AGENTS.md", filepath.Join(project.root, "mid")); err != nil {
						t.Fatal(err)
					}
					target = "mid" + suffix
				}
				if strings.HasPrefix(spelling, "absolute") {
					// filepath.Join would erase the directory-traversal suffix.
					target = project.root + "/" + target
				}
				if err := os.Symlink(target, filepath.Join(project.root, "CLAUDE.md")); err != nil {
					t.Fatal(err)
				}
				if _, err := os.ReadFile(filepath.Join(project.root, "CLAUDE.md")); !errors.Is(err, syscall.ENOTDIR) {
					t.Fatalf("OS read error = %v, want ENOTDIR", err)
				}
				before := snapshotProjectTree(t, project.root)
				result := project.run(1, installArgs(pkg)...)
				assertSymlinkRefusalText(t, result.stderr, "CLAUDE.md", "AGENTS.md"+suffix, "symlink target is not a regular file")
				assertNoSetupState(t, project.root)
				assertTreeUnchanged(t, before, project.root, "refused auto-detected install")

				project.run(0, installArgs(pkg, "claude-code", "codex")...)
				before = snapshotProjectTree(t, project.root)
				result = project.run(1, "realize")
				assertSymlinkRefusalText(t, result.stderr, "CLAUDE.md", "AGENTS.md"+suffix, "symlink target is not a regular file")
				assertTreeUnchanged(t, before, project.root, "refused realization")
			})
		}
	}

	t.Run("dangling realize", func(t *testing.T) {
		project := claudeAliasProject(t, "missing.md")
		project.run(0, installArgs(pkg, "claude-code")...)
		before := readProjectText(t, project.root, "AGENTS.md")
		result := project.run(1, "realize")
		assertSymlinkRefusalText(t, result.stderr, "CLAUDE.md", "missing.md", "symlink target is missing")
		if got := readProjectText(t, project.root, "AGENTS.md"); got != before {
			t.Fatalf("realize wrote AGENTS.md:\n%s", got)
		}
		assertLink(t, project.root, "CLAUDE.md", "missing.md")
	})

	t.Run("relative escape", func(t *testing.T) {
		project := claudeAliasProject(t, "../secret.md")
		result := project.run(1, installArgs(pkg)...)
		assertSymlinkRefusalText(t, result.stderr, "CLAUDE.md", "../secret.md", "symlink escapes the project")
		assertNoSetupState(t, project.root)

		explicit := claudeAliasProject(t, "../secret.md")
		explicit.run(0, installArgs(pkg, "claude-code")...)
		before := readProjectText(t, explicit.root, "AGENTS.md")
		realized := explicit.run(1, "realize")
		assertSymlinkRefusalText(t, realized.stderr, "CLAUDE.md", "../secret.md", "symlink escapes the project")
		if got := readProjectText(t, explicit.root, "AGENTS.md"); got != before {
			t.Fatalf("realize wrote through an escaping symlink:\n%s", got)
		}
		assertLink(t, explicit.root, "CLAUDE.md", "../secret.md")
	})

	t.Run("absolute escape", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "secret.md")
		if err := os.WriteFile(outside, []byte("SECRET-BYTES"), 0o644); err != nil {
			t.Fatal(err)
		}
		project := claudeAliasProject(t, outside)
		result := project.run(1, installArgs(pkg)...)
		assertSymlinkRefusalText(t, result.stderr, "CLAUDE.md", outside, "symlink escapes the project")
		if strings.Contains(result.stderr, "SECRET-BYTES") {
			t.Fatalf("refusal included outside file contents: %s", result.stderr)
		}
		assertNoSetupState(t, project.root)
	})

	t.Run("absolute dotdot escape refuses install and realize", func(t *testing.T) {
		project := newGitJourneyProject(t)
		writeProjectText(t, project.root, "AGENTS.md", "# Agents\n")
		outside := t.TempDir()
		writeProjectText(t, outside, "sub/placeholder.md", "# Sub\n")
		writeProjectText(t, outside, "AGENTS.md", "Outside instructions\n")
		if err := os.Symlink(filepath.Join(outside, "sub"), filepath.Join(project.root, "sc")); err != nil {
			t.Fatal(err)
		}
		target := project.root + "/sc/../AGENTS.md"
		if err := os.Symlink(target, filepath.Join(project.root, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		// The kernel reads the outside file; ACR must not treat this as AGENTS.md.
		if got := readProjectText(t, project.root, "CLAUDE.md"); got != "Outside instructions\n" {
			t.Fatalf("kernel resolved CLAUDE.md to %q", got)
		}
		result := project.run(1, installArgs(pkg)...)
		assertSymlinkRefusalText(t, result.stderr, "CLAUDE.md", outside+"/sub/../AGENTS.md", "symlink escapes the project")
		assertNoSetupState(t, project.root)
		project.run(0, installArgs(pkg, "claude-code")...)
		realized := project.run(1, "realize")
		assertSymlinkRefusalText(t, realized.stderr, "CLAUDE.md", outside+"/sub/../AGENTS.md", "symlink escapes the project")
		assertLink(t, project.root, "CLAUDE.md", target)
		if got := readProjectText(t, project.root, "AGENTS.md"); got != "# Agents\n" {
			t.Fatalf("realize wrote in-project AGENTS.md: %q", got)
		}
		if got := readProjectText(t, outside, "AGENTS.md"); got != "Outside instructions\n" {
			t.Fatalf("realize wrote outside AGENTS.md: %q", got)
		}
	})

	t.Run("non-regular directory and chain", func(t *testing.T) {
		project := newGitJourneyProject(t)
		if err := os.Mkdir(filepath.Join(project.root, "subdir"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeProjectText(t, project.root, "AGENTS.md", "# Agents\n")
		if err := os.Symlink("subdir", filepath.Join(project.root, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		result := project.run(1, installArgs(pkg)...)
		assertSymlinkRefusalText(t, result.stderr, "CLAUDE.md", "subdir", "symlink target is not a regular file")
		assertNoSetupState(t, project.root)

		chain := newGitJourneyProject(t)
		if err := os.Mkdir(filepath.Join(chain.root, "subdir"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeProjectText(t, chain.root, "AGENTS.md", "# Agents\n")
		if err := os.Symlink("subdir", filepath.Join(chain.root, "mid")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("mid", filepath.Join(chain.root, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		chain.run(0, installArgs(pkg, "claude-code")...)
		realized := chain.run(1, "realize")
		assertSymlinkRefusalText(t, realized.stderr, "CLAUDE.md", "subdir", "symlink target is not a regular file")
		assertLink(t, chain.root, "CLAUDE.md", "mid")
		assertLink(t, chain.root, "mid", "subdir")
		if strings.Contains(readProjectText(t, chain.root, "AGENTS.md"), "<!-- acr:begin") {
			t.Fatal("realize wrote a block after a non-regular symlink chain")
		}
	})

	t.Run("symlinked AGENTS.md and nested claude host", func(t *testing.T) {
		project := newGitJourneyProject(t)
		writeProjectText(t, project.root, "notes.md", "# Notes\n")
		if err := os.Symlink("notes.md", filepath.Join(project.root, "AGENTS.md")); err != nil {
			t.Fatal(err)
		}
		result := project.run(1, installArgs(pkg)...)
		assertSymlinkRefusalText(t, result.stderr, "AGENTS.md", "notes.md", "")
		assertNoSetupState(t, project.root)

		nested := newGitJourneyProject(t)
		writeProjectText(t, nested.root, "AGENTS.md", "# Agents\n")
		if err := os.Mkdir(filepath.Join(nested.root, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../AGENTS.md", filepath.Join(nested.root, ".claude", "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		nestedResult := nested.run(1, installArgs(pkg)...)
		assertSymlinkRefusalText(t, nestedResult.stderr, ".claude/CLAUDE.md", "AGENTS.md", "")
		assertNoSetupState(t, nested.root)
	})

	t.Run("nested symlink is skipped", func(t *testing.T) {
		project := newGitJourneyProject(t)
		writeProjectText(t, project.root, "AGENTS.md", "# Agents\n")
		if err := os.Mkdir(filepath.Join(project.root, "docs"), 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "secret.md")
		if err := os.WriteFile(outside, []byte("SECRET-NESTED"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(project.root, "docs", "AGENTS.md")); err != nil {
			t.Fatal(err)
		}
		project.run(0, installArgs(pkg, "codex")...)
		project.run(0, "realize")
		body := readProjectText(t, project.root, "AGENTS.md")
		if strings.Count(body, "<!-- acr:begin") != 1 || strings.Contains(body, "SECRET-NESTED") {
			t.Fatalf("nested symlink changed realization:\n%s", body)
		}
		assertLink(t, project.root, "docs/AGENTS.md", outside)
	})

	t.Run("GEMINI.md is not a claude alias", func(t *testing.T) {
		alone := newGitJourneyProject(t)
		if err := os.Symlink("AGENTS.md", filepath.Join(alone.root, "GEMINI.md")); err != nil {
			t.Fatal(err)
		}
		result := alone.run(2, installArgs(pkg)...)
		if !strings.Contains(result.stderr, "no agent adapter selected") || strings.Contains(result.stderr, "must be a regular file") {
			t.Fatalf("GEMINI.md symlink error = %s", result.stderr)
		}
		assertNoSetupState(t, alone.root)

		withAgents := newGitJourneyProject(t)
		writeProjectText(t, withAgents.root, "AGENTS.md", "# Agents\n")
		if err := os.Symlink("/tmp/gemini-outside.md", filepath.Join(withAgents.root, "GEMINI.md")); err != nil {
			t.Fatal(err)
		}
		withAgents.run(0, installArgs(pkg)...)
		assertAgents(t, withAgents.root, "codex")
		recorded := readProjectText(t, withAgents.root, dependency.ProjectFilename)
		if strings.Contains(recorded, "claude-code") {
			t.Fatalf("GEMINI.md selected claude-code:\n%s", recorded)
		}
		withAgents.run(0, "realize")
		if strings.Contains(readProjectText(t, withAgents.root, "AGENTS.md"), "SECRET") {
			t.Fatal("GEMINI.md symlink was followed")
		}
	})
}

func minimalPluginArg(t *testing.T) string {
	t.Helper()
	absolute, err := filepath.Abs(filepath.Join("..", "..", "examples", "minimal"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(absolute, "agent-plugin.yaml"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("examples/minimal: %v", err)
	}
	return "file:" + absolute
}

func installArgs(pkg string, agents ...string) []string {
	args := []string{"install", pkg, "--freshness", "none", "--non-interactive"}
	for _, agent := range agents {
		args = append(args, "--agent", agent)
	}
	return args
}

func newGitJourneyProject(t *testing.T) *journeyProject {
	t.Helper()
	project := newJourneyProject(t, nil)
	command := exec.Command("git", "init", "--quiet")
	command.Dir = project.root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	return project
}

func claudeAliasProject(t *testing.T, linkTarget string) *journeyProject {
	t.Helper()
	project := newGitJourneyProject(t)
	writeProjectText(t, project.root, "AGENTS.md", "# Agents\n")
	if err := os.Symlink(linkTarget, filepath.Join(project.root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	return project
}

func realizeClaudeAlias(t *testing.T, project *journeyProject, adapterID string) {
	t.Helper()
	project.run(0, "realize")
	assertOneManagedBlock(t, project.root, "AGENTS.md", adapterID)
	assertLink(t, project.root, "CLAUDE.md", "AGENTS.md")
	if _, err := os.Lstat(filepath.Join(project.root, ".claude", "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatalf("realization created .claude/CLAUDE.md: %v", err)
	}
	project.run(0, "check")
	before := readProjectText(t, project.root, "AGENTS.md")
	project.run(0, "realize")
	if got := readProjectText(t, project.root, "AGENTS.md"); got != before {
		t.Fatalf("second realize changed AGENTS.md\nbefore:\n%s\nafter:\n%s", before, got)
	}
	assertLink(t, project.root, "CLAUDE.md", "AGENTS.md")
	project.run(0, "check")
	project.run(0, "uninstall", "github:example/minimal-plugin")
	if got := readProjectText(t, project.root, "AGENTS.md"); got != "# Agents\n" {
		t.Fatalf("uninstall left %q", got)
	}
	assertLink(t, project.root, "CLAUDE.md", "AGENTS.md")
	project.run(0, "check")
}

func assertOneManagedBlock(t *testing.T, root, name, adapterID string) {
	t.Helper()
	body := readProjectText(t, root, name)
	if strings.Count(body, "<!-- acr:begin") != 1 || !strings.Contains(body, "adapter="+adapterID) || !strings.HasPrefix(body, "# Agents\n") || !strings.Contains(body, "project-guidance") {
		t.Fatalf("%s block:\n%s", name, body)
	}
}

func assertAgents(t *testing.T, root string, agents ...string) {
	t.Helper()
	body := readProjectText(t, root, dependency.ProjectFilename)
	for _, agent := range agents {
		if !strings.Contains(body, agent) {
			t.Fatalf("agents.yaml missing %s:\n%s", agent, body)
		}
	}
}

func assertLink(t *testing.T, root, name, target string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(name))
	info, err := os.Lstat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s mode = %v, want a symlink", name, info.Mode())
	}
	got, err := os.Readlink(filename)
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Fatalf("%s -> %q, want %q", name, got, target)
	}
}

func assertNoSetupState(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{dependency.ProjectFilename, ".agents", dependency.LockFilename} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Fatalf("%s left behind after detection refusal: %v", name, err)
		}
	}
}

func assertSymlinkRefusalText(t *testing.T, message, path, target, reason string) {
	t.Helper()
	wants := []string{path, target, "must be a regular file, not a symlink or special file", "symlink target resolves to"}
	if reason != "" {
		wants = append(wants, reason)
	}
	for _, want := range wants {
		if !strings.Contains(message, want) {
			t.Fatalf("refusal %q, want %q", message, want)
		}
	}
}

func writeProjectText(t *testing.T, root, name, content string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readProjectText(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
