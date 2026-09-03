package realizeapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// reverifyGuardProject seeds a Tessl consumer whose ownership is established by
// a regular tessl.json: a plugin tree and a tessl__ native outside it.
func reverifyGuardProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "tessl.json"), []byte(`{"name":"consumer"}`+"\n"), 0o644)
	writeFixture(t, filepath.Join(root, ".tessl", "plugins", "example", "alpha", "rules", "always-rule.md"),
		[]byte("---\nalwaysApply: true\n---\n# Always\n"), 0o644)
	writeFixture(t, filepath.Join(root, ".cursor", "rules", "tessl__rule__example__alpha.mdc"),
		[]byte("---\nalwaysApply: true\n---\n# Always\n"), 0o644)
	writeFixture(t, filepath.Join(root, "AGENTS.md"), []byte("# User\n\n@.tessl/RULES.md\n"), 0o644)
	return root
}

func reverifyGuardIntent(targetPath string) realize.Intent {
	content := []byte("managed\n")
	digest := sha256.Sum256(content)
	return realize.Intent{
		Action: realize.ActionEnsure, Path: targetPath, Content: content, Mode: 0o644,
		Ownership: realize.OwnershipGenerated,
		Entries: []realize.Entry{{
			Source: "github:owner/plugin", ArtifactID: "artifact", ArtifactKind: realize.ArtifactFile,
			SourcePath: "rules/source.md", Adapter: "test", AdapterVersion: "1.0.0",
			ManagedHash: "sha256:" + hex.EncodeToString(digest[:]),
		}},
	}
}

// reverifyGuardTree hashes every project path with its mode and symlink target.
func reverifyGuardTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(filename)
			if err != nil {
				return err
			}
			result[relative] = "link→" + filepath.ToSlash(target)
		case entry.IsDir():
			result[relative] = "dir " + info.Mode().Perm().String()
		default:
			content, err := os.ReadFile(filename)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(content)
			result[relative] = info.Mode().Perm().String() + " " + hex.EncodeToString(sum[:])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func reverifyTreesEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

// TestReverifyTesslOwnedTargetRefusesInEveryModeAndWritesNothing drives the real
// realization engine with a hand-built plan whose single operation targets a
// Tessl-owned prefix. Every mode must fail closed with the typed error naming
// the path, and the project tree must be byte- and mode-identical afterwards.
func TestReverifyTesslOwnedTargetRefusesInEveryModeAndWritesNothing(t *testing.T) {
	for _, target := range []string{
		".tessl/plugins/example/alpha/rules/injected-rule.md",
		".cursor/rules/tessl__rule__injected.mdc",
	} {
		for _, mode := range []realize.Mode{realize.ModeDryRun, realize.ModeCheck, realize.ModeApply} {
			t.Run(target+" "+string(mode), func(t *testing.T) {
				t.Parallel()
				project := reverifyGuardProject(t)
				before := reverifyGuardTree(t, project)

				saved := false
				_, err := realize.NewEngine().Run(project,
					realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion},
					[]realize.Intent{reverifyGuardIntent(target)}, mode,
					func(realize.Ledger) error { saved = true; return nil })

				var targetErr *realize.TesslOwnedTargetError
				if !errors.As(err, &targetErr) {
					t.Fatalf("Run(%s) error = %v, want TesslOwnedTargetError", mode, err)
				}
				if targetErr.Path != target {
					t.Fatalf("TesslOwnedTargetError.Path = %q, want %q", targetErr.Path, target)
				}
				if !strings.Contains(targetErr.Error(), target) {
					t.Fatalf("error message does not name the path: %q", targetErr.Error())
				}
				if saved {
					t.Fatal("a refused plan persisted an ownership ledger")
				}
				if after := reverifyGuardTree(t, project); !reverifyTreesEqual(before, after) {
					t.Fatalf("refusal mutated the tree\nbefore=%v\nafter=%v", before, after)
				}
				if _, err := os.Stat(filepath.Join(project, ".agents")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refusal created ACR state: %v", err)
				}
			})
		}
	}
}

// TestReverifyTesslOwnedTargetExitsFourThroughTheCLI carries the same hand-built
// plan across the shipped boundary: the engine's typed error, the production
// error mapper, and the real CLI renderer. The contract is exit 4 with
// tessl_owned_target, one JSON error line on stderr, empty stdout, no writes.
func TestReverifyTesslOwnedTargetExitsFourThroughTheCLI(t *testing.T) {
	t.Parallel()
	project := reverifyGuardProject(t)
	before := reverifyGuardTree(t, project)

	application := cli.ApplicationFunc(func(_ context.Context, invocation cli.Invocation) (cli.Result, error) {
		_, err := realize.NewEngine().Run(invocation.ProjectDirectory,
			realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion},
			[]realize.Intent{reverifyGuardIntent(".tessl/plugins/example/alpha/rules/injected-rule.md")},
			realize.ModeApply, func(realize.Ledger) error { return nil })
		if err != nil {
			return cli.Result{}, realizationError(err)
		}
		return cli.Result{}, errors.New("the guard let a Tessl-owned target through")
	})

	stdout, stderr, exitCode := runCLI(t, application, "realize", "--project", project, "--json")
	if cli.ExitConflict != 4 {
		t.Fatalf("cli.ExitConflict = %d, want the documented conflict exit 4", cli.ExitConflict)
	}
	if exitCode != cli.ExitConflict {
		t.Fatalf("exit = %d, want %d (ExitConflict)", exitCode, cli.ExitConflict)
	}
	if stdout != "" {
		t.Fatalf("refusal wrote to stdout: %q", stdout)
	}
	if strings.Count(strings.TrimSpace(stderr), "\n") != 0 {
		t.Fatalf("refusal wrote more than one stderr line: %q", stderr)
	}
	for _, want := range []string{`"ok":false`, `"command":"realize"`, `"code":"tessl_owned_target"`, ".tessl/plugins/example/alpha/rules/injected-rule.md"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want %q", stderr, want)
		}
	}
	if after := reverifyGuardTree(t, project); !reverifyTreesEqual(before, after) {
		t.Fatalf("refusal mutated the tree\nbefore=%v\nafter=%v", before, after)
	}
}

// TestReverifyTesslOwnedGuardNeedsALiveManifest pins the gate the guard is
// scoped by: without a regular tessl.json nothing establishes Tessl ownership,
// so the same path is an ordinary target and realization proceeds. A blanket
// path ban would break projects that merely happen to use those names.
func TestReverifyTesslOwnedGuardNeedsALiveManifest(t *testing.T) {
	t.Parallel()
	project := reverifyGuardProject(t)
	if err := os.Remove(filepath.Join(project, "tessl.json")); err != nil {
		t.Fatal(err)
	}
	target := ".tessl/plugins/example/alpha/rules/injected-rule.md"
	plan, err := realize.NewEngine().Run(project,
		realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion},
		[]realize.Intent{reverifyGuardIntent(target)}, realize.ModeApply,
		func(realize.Ledger) error { return nil })
	if err != nil {
		t.Fatalf("Run() without a Tessl manifest error = %v, want the ordinary path", err)
	}
	if !plan.HasChanges() {
		t.Fatalf("Run() without a Tessl manifest planned nothing: %#v", plan)
	}
	content, err := os.ReadFile(filepath.Join(project, filepath.FromSlash(target)))
	if err != nil || string(content) != "managed\n" {
		t.Fatalf("target = %q, %v, want the realized content", content, err)
	}
}
