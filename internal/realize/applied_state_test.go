package realize

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func deferredAppliedFinalizer(project string, fail bool) StateFinalizer {
	return bindStateFinalizer(project, func(Ledger) ([]StateFile, error) {
		return []StateFile{
			{Path: ".agents/.gitignore", Content: []byte("/.acr-applied.json\n"), Mode: 0o644},
			{Path: ".agents/.acr-applied.json", Content: []byte("placeholder\n"), Mode: 0o644, AfterApply: func() ([]byte, error) {
				data, err := os.ReadFile(filepath.Join(project, "generated/rule.md"))
				if err != nil {
					return nil, err
				}
				if string(data) != "managed\n" {
					return nil, fmt.Errorf("receipt rendered before native output")
				}
				if fail {
					return nil, fmt.Errorf("injected evidence capture failure")
				}
				return []byte("bound receipt\n"), nil
			}},
		}, nil
	})
}

func TestDeferredAppliedStateRollsBack(t *testing.T) {
	for _, failure := range []string{"capture", "manifest", "receipt"} {
		t.Run(failure, func(t *testing.T) {
			project := t.TempDir()
			writeFile(t, project, ".agents/.acr-applied.json", "old receipt\n")
			originalWriter := journalFileWriter
			t.Cleanup(func() { journalFileWriter = originalWriter })
			journalFileWriter = func(filename string, content []byte, mode os.FileMode) error {
				if failure == "manifest" && filepath.Base(filename) == journalManifestFilename && filepath.Base(filepath.Dir(filename)) == "deferred-state" {
					return fmt.Errorf("injected bound manifest failure")
				}
				return originalWriter(filename, content, mode)
			}
			originalID := transactionID
			transactionID = func() (string, error) { return "deferred-state", nil }
			t.Cleanup(func() { transactionID = originalID })
			setTransactionWriter(t, func(root *os.Root, operation Operation) (bool, error) {
				changed, err := writeOperation(root, operation)
				if err == nil && failure == "receipt" && operation.Path == ".agents/.acr-applied.json" {
					return changed, fmt.Errorf("injected receipt write failure")
				}
				return changed, err
			})
			_, err := NewEngine().RunStateFiles(project, testLedger(), Ledger{}, []Intent{
				testIntent("generated/rule.md", "managed\n", OwnershipGenerated),
			}, ModeApply, deferredAppliedFinalizer(project, failure == "capture"))
			if err == nil {
				t.Fatal("injected failure succeeded")
			}
			if got := readFile(t, project, ".agents/.acr-applied.json"); got != "old receipt\n" {
				t.Fatalf("failure changed receipt: %q", got)
			}
			for _, name := range []string{"generated/rule.md", ".agents/.gitignore"} {
				if _, err := os.Stat(filepath.Join(project, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("rollback left %s: %v", name, err)
				}
			}
		})
	}
}

func TestInterruptedDeferredAppliedStateRecovers(t *testing.T) {
	if os.Getenv("ACR_TEST_DEFERRED_RECEIPT") == "1" {
		project := os.Getenv("ACR_TEST_PROJECT")
		transactionRenameHook = func(name string) {
			if name == ".agents/.acr-applied.json" {
				os.Exit(86)
			}
		}
		_, _ = NewEngine().RunStateFiles(project, testLedger(), Ledger{}, []Intent{
			testIntent("generated/rule.md", "managed\n", OwnershipGenerated),
		}, ModeApply, deferredAppliedFinalizer(project, false))
		os.Exit(87)
	}
	project := t.TempDir()
	writeFile(t, project, ".agents/.acr-applied.json", "old receipt\n")
	command := exec.Command(os.Args[0], "-test.run=^TestInterruptedDeferredAppliedStateRecovers$")
	command.Env = append(os.Environ(), "ACR_TEST_DEFERRED_RECEIPT=1", "ACR_TEST_PROJECT="+project)
	err := command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("child error = %v", err)
	}
	if got := readFile(t, project, ".agents/.acr-applied.json"); got != "bound receipt\n" {
		t.Fatalf("child did not reach bound receipt write: %q", got)
	}
	if err := RecoverTransactions(project); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, project, ".agents/.acr-applied.json"); got != "old receipt\n" {
		t.Fatalf("recovery changed old receipt: %q", got)
	}
	if _, err := os.Stat(filepath.Join(project, "generated/rule.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery left native output: %v", err)
	}
}

// Mutate after the finalizer has bound every before-image, immediately before
// appendStateOperations takes its own snapshot. No journal may adopt the edit.
func TestStateFilesRejectChangesAfterFinalization(t *testing.T) {
	paths := []string{"agents.yaml", ".agents/registry.lock", ".agents/.acr-applied.json", ".agents/.gitignore"}
	for _, target := range paths {
		for _, change := range []string{"bytes", "mode", "created", "removed", "no-op"} {
			t.Run(target+"/"+change, func(t *testing.T) {
				project := t.TempDir()
				for _, name := range paths {
					if name != target || change != "created" {
						writeFile(t, project, name, "before\n")
					}
				}
				writeFile(t, project, "untouched.md", "untouched\n")
				readState := func() map[string]StateFile {
					result := map[string]StateFile{}
					for _, name := range append(append([]string(nil), paths...), "untouched.md", "generated/rule.md") {
						file, exists, err := ReadStateFile(project, name)
						if err != nil {
							t.Fatal(err)
						}
						if exists {
							result[name] = file
						}
					}
					return result
				}
				var injected map[string]StateFile
				finalize := func(Ledger) ([]StateFile, error) {
					var files []StateFile
					for _, name := range paths {
						file, _, err := ReadStateFile(project, name)
						if err != nil {
							return nil, err
						}
						file.Content, file.Mode = []byte("after\n"), 0o644
						if change == "no-op" {
							file.Content = []byte("before\n")
						}
						files = append(files, file)
					}
					switch change {
					case "bytes", "created":
						writeFile(t, project, target, "concurrent\n")
					case "mode":
						if err := os.Chmod(filepath.Join(project, target), 0o600); err != nil {
							t.Fatal(err)
						}
					case "removed":
						if err := os.Remove(filepath.Join(project, target)); err != nil {
							t.Fatal(err)
						}
					case "no-op":
						// The live snapshot equals the desired bytes, but differs from the
						// preparation image. The no-op shortcut must still refuse it.
						writeFile(t, project, target, "after\n")
						for i := range files {
							if files[i].Path == target {
								files[i].Content = []byte("after\n")
							}
						}
					}
					injected = readState()
					return files, nil
				}
				_, err := NewEngine().RunStateFiles(project, testLedger(), Ledger{}, []Intent{testIntent("generated/rule.md", "managed\n", OwnershipGenerated)}, ModeApply, finalize)
				if err == nil || !strings.Contains(err.Error(), target+" changed") || !strings.Contains(err.Error(), "retry") {
					t.Fatalf("stale state error = %v", err)
				}
				if !reflect.DeepEqual(injected, readState()) {
					t.Fatal("refusal changed state or outputs")
				}
				entries, err := os.ReadDir(filepath.Join(project, transactionDirectory))
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if entry.IsDir() {
						t.Fatalf("refusal created journal %s", entry.Name())
					}
				}
			})
		}
	}
}

func TestStateFilesRefuseZeroModeBeforeMutation(t *testing.T) {
	for _, name := range []string{"agents.yaml", ".agents/registry.lock", ".agents/.acr-applied.json", ".agents/.gitignore"} {
		t.Run(name, func(t *testing.T) {
			project := t.TempDir()
			writeFile(t, project, name, "before\n")
			if err := os.Chmod(filepath.Join(project, name), 0); err != nil {
				t.Fatal(err)
			}
			// Supply known readable bytes at the snapshot seam. Refusal must not
			// depend on EACCES: privileged processes can read mode-000 files.
			finalize := func(Ledger) ([]StateFile, error) {
				return []StateFile{{Path: name, Content: []byte("after\n"), Mode: 0o644, BeforeExists: true, BeforeContent: []byte("before\n"), BeforeMode: 0}}, nil
			}
			_, err := NewEngine().RunStateFiles(project, testLedger(), Ledger{}, []Intent{testIntent("generated/rule.md", "managed\n", OwnershipGenerated)}, ModeApply, finalize)
			if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "restore readable permissions") {
				t.Fatalf("zero-mode error = %v", err)
			}
			info, err := os.Stat(filepath.Join(project, name))
			if err != nil || info.Mode().Perm() != 0 {
				t.Fatalf("permissions changed: %v, %v", info, err)
			}
			if err := os.Chmod(filepath.Join(project, name), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, project, name); got != "before\n" {
				t.Fatalf("content changed: %q", got)
			}
			if _, err := os.Stat(filepath.Join(project, "generated/rule.md")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("native output written: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(project, transactionDirectory))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.IsDir() {
					t.Fatalf("refusal created journal %s", entry.Name())
				}
			}
		})
	}
}

func TestConvergedAppliedStateAttemptsNoWrites(t *testing.T) {
	project := t.TempDir()
	intents := []Intent{testIntent("generated/rule.md", "managed\n", OwnershipGenerated)}
	engine := NewEngine()
	first, err := engine.RunStateFiles(project, testLedger(), Ledger{}, intents, ModeApply, deferredAppliedFinalizer(project, false))
	if err != nil {
		t.Fatal(err)
	}
	originalID, originalWriter, originalRename, originalHook := transactionID, journalFileWriter, journalRename, transactionRenameHook
	t.Cleanup(func() {
		transactionID, journalFileWriter, journalRename, transactionRenameHook = originalID, originalWriter, originalRename, originalHook
	})
	transactionID = func() (string, error) {
		t.Error("converged apply attempted journal creation")
		return "", errors.New("unexpected journal creation")
	}
	journalFileWriter = func(string, []byte, os.FileMode) error {
		t.Error("converged apply attempted journal write")
		return errors.New("unexpected journal write")
	}
	journalRename = func(string, string) error {
		t.Error("converged apply attempted journal rename")
		return errors.New("unexpected journal rename")
	}
	transactionRenameHook = func(name string) { t.Errorf("converged apply renamed %s", name) }
	setTransactionWriter(t, func(_ *os.Root, operation Operation) (bool, error) {
		t.Errorf("converged apply attempted write of %s", operation.Path)
		return false, errors.New("unexpected state or native write")
	})
	plan, err := engine.RunStateFiles(project, first.NextLedger, Ledger{}, intents, ModeApply, deferredAppliedFinalizer(project, false))
	if err != nil || plan.HasChanges() {
		t.Fatalf("converged apply = %v, changes = %t", err, plan.HasChanges())
	}
}
