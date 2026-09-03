package realizeapp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// twoAgentProject realizes the shared fixture for claude-code and codex and
// returns the project root, the package root, and the application under test.
func twoAgentProject(t *testing.T) (string, string, *Application) {
	t.Helper()
	projectRoot, packageRoot, state, value := realizationFixture(t)
	state.Project.Agents = []string{"claude-code", "codex"}
	if err := dependency.WriteState(projectRoot, state); err != nil {
		t.Fatal(err)
	}
	application := &Application{service: NewService(fixtureLoader{root: packageRoot, manifest: value}), fallback: cli.UnavailableApplication{}}
	stdout, stderr, exitCode := runCLI(t, application, "realize", "--project", projectRoot, "--json")
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("two-agent realize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	return projectRoot, packageRoot, application
}

// hashProjectTree digests every regular file under root, dotfiles included, so
// a test can assert that an invocation left the project byte-identical.
func hashProjectTree(t *testing.T, root string) map[string]string {
	t.Helper()
	digests := make(map[string]string)
	if err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		content, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(content)
		digests[filepath.ToSlash(relative)] = fmt.Sprintf("%04o:%s", info.Mode().Perm(), hex.EncodeToString(digest[:]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return digests
}

func claudeCodeTargets(t *testing.T, digests map[string]string) map[string]string {
	t.Helper()
	owned := make(map[string]string)
	for name, digest := range digests {
		if name == "CLAUDE.md" || strings.HasPrefix(name, ".claude/") {
			owned[name] = digest
		}
	}
	if len(owned) == 0 {
		t.Fatalf("fixture realized no claude-code outputs: %#v", digests)
	}
	return owned
}

func projectLedger(t *testing.T, root string) realize.Ledger {
	t.Helper()
	state, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := realize.DecodeLedger(state.Lock.Realization)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func adapterTargets(ledger realize.Ledger, agentID string) map[string]realize.Target {
	targets := make(map[string]realize.Target)
	for _, target := range ledger.Targets {
		for _, entry := range target.Entries {
			if entry.Adapter == agentID {
				targets[target.Path] = target
				break
			}
		}
	}
	return targets
}

func TestRealizeWithAgentSubsetLeavesOmittedAgentOutputsUntouched(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := twoAgentProject(t)
	before := hashProjectTree(t, projectRoot)
	claudeBefore := claudeCodeTargets(t, before)

	stdout, stderr, exitCode := runCLI(t, application, "realize", "--project", projectRoot, "--agent", "codex", "--json")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"agents":["codex"]`) {
		t.Fatalf("subset realize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}

	after := hashProjectTree(t, projectRoot)
	for name, digest := range claudeBefore {
		if after[name] != digest {
			t.Fatalf("omitted agent output %q = %q, want %q", name, after[name], digest)
		}
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("subset realize rewrote the project:\n before %#v\n after  %#v", before, after)
	}
}

func TestRealizeWithAgentSubsetKeepsOmittedAgentLedgerEntries(t *testing.T) {
	t.Parallel()

	projectRoot, packageRoot, application := twoAgentProject(t)
	claudeBefore := adapterTargets(projectLedger(t, projectRoot), "claude-code")
	if len(claudeBefore) == 0 {
		t.Fatal("fixture recorded no claude-code ledger targets")
	}
	outputsBefore := claudeCodeTargets(t, hashProjectTree(t, projectRoot))
	writeFixture(t, filepath.Join(packageRoot, "rules", "guidance.md"), []byte("# Guidance\n\nRevised.\n"), 0o644)

	stdout, stderr, exitCode := runCLI(t, application, "realize", "--project", projectRoot, "--agent", "codex", "--json")
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("subset realize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}

	ledger := projectLedger(t, projectRoot)
	if got := adapterTargets(ledger, "claude-code"); !reflect.DeepEqual(got, claudeBefore) {
		t.Fatalf("claude-code ledger targets = %#v, want %#v", got, claudeBefore)
	}
	if len(adapterTargets(ledger, "codex")) == 0 {
		t.Fatalf("codex ledger targets were dropped: %#v", ledger)
	}
	for name, digest := range outputsBefore {
		if got := hashProjectTree(t, projectRoot)[name]; got != digest {
			t.Fatalf("omitted agent output %q = %q, want %q", name, got, digest)
		}
	}
}

func TestCheckWithAgentSubsetReportsNoChanges(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := twoAgentProject(t)

	stdout, stderr, exitCode := runCLI(t, application, "check", "--project", projectRoot, "--agent", "codex")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, "current for codex") {
		t.Fatalf("subset check exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
}

func TestRealizeWithAgentSubsetConflictsOnAMixedTarget(t *testing.T) {
	t.Parallel()

	projectRoot, packageRoot, state, value := realizationFixture(t)
	state.Project.Agents = []string{"claude-code", "codex"}
	entry := realize.Entry{
		Source: "github:example/all-agents", ArtifactID: "guidance", ArtifactKind: realize.ArtifactManagedBlock,
		SourcePath: "rules/guidance.md", AdapterVersion: "1.0.0", ManagedHash: "sha256:" + strings.Repeat("1", 64),
	}
	claude, codex := entry, entry
	claude.Adapter, codex.Adapter = "claude-code", "codex"
	encoded, err := realize.EncodeLedger(realize.Ledger{
		SchemaVersion: realize.CurrentLedgerSchemaVersion,
		Targets: []realize.Target{{
			Path: "MIXED.md", Mode: 0o644, Ownership: realize.OwnershipGenerated,
			OutputHash: "sha256:" + strings.Repeat("2", 64), Entries: []realize.Entry{claude, codex},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	state.Lock.Realization = encoded
	if err := dependency.WriteState(projectRoot, state); err != nil {
		t.Fatal(err)
	}
	// Existing projects may retain the empty, gitignored transaction claim
	// after a converged run. Keep that accepted residue in the before-image so
	// this assertion remains focused on user and dependency state.
	writeFixture(t, filepath.Join(projectRoot, ".agents", ".acr-transactions", ".lock"), nil, 0o600)
	application := &Application{service: NewService(fixtureLoader{root: packageRoot, manifest: value}), fallback: cli.UnavailableApplication{}}
	before := hashProjectTree(t, projectRoot)

	stdout, stderr, exitCode := runCLI(t, application, "realize", "--project", projectRoot, "--agent", "codex", "--json")

	if exitCode != cli.ExitConflict || stdout != "" || !strings.Contains(stderr, `"code":"realization_conflict"`) {
		t.Fatalf("mixed-target realize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stderr, "MIXED.md") || !strings.Contains(stderr, "re-run without --agent") {
		t.Fatalf("mixed-target diagnostic = %q, want the path and the recovery command", stderr)
	}
	if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(before, after) {
		t.Fatalf("mixed-target realize wrote files:\n before %#v\n after  %#v", before, after)
	}
}
