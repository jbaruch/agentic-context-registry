package dependency

import (
	"context"
	"errors"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func localRecoveryTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		out[rel] = info.Mode().String()
		if info.Mode().IsRegular() {
			b, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			out[rel] += "\n" + string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLocalUnauthorizedInterruptedPathChange(t *testing.T) {
	if os.Getenv("ACR_LOCAL_PARTIAL_CRASH") == "1" {
		project, source := os.Getenv("ACR_LOCAL_PARTIAL_PROJECT"), os.Getenv("ACR_LOCAL_PARTIAL_SOURCE")
		before, e := LoadState(project)
		if e != nil {
			t.Fatal(e)
		}
		desired := cloneState(before)
		desired.Project.Dependencies[0].Path = source
		desired.Lock.Dependencies[0].Path = source
		pd, ld, e := MarshalState(desired)
		if e != nil {
			t.Fatal(e)
		}
		var edits []realize.FileTransactionEdit
		for i, name := range []string{ProjectFilename, LockFilename} {
			b, e := os.ReadFile(filepath.Join(project, name))
			if e != nil {
				t.Fatal(e)
			}
			a := pd
			if i == 1 {
				a = ld
			}
			edits = append(edits, realize.FileTransactionEdit{Path: name, Operation: "splice", Before: b, BeforeMode: 0644, After: a, AfterMode: 0644})
		}
		e = changeLocalAuthorization(project, desired.Project.Dependencies[0].Source, &localAuthorization{SchemaVersion: 1, Path: source, SourceRoot: source}, func() error {
			return realize.ApplyFileTransactionWithHooks(project, edits, nil, realize.FileTransactionHooks{TransactionID: func() (string, error) { return "tx-local-path-switch", nil }, AfterEdit: func(i int, _ realize.FileTransactionEdit) error {
				if i == 0 {
					os.Exit(77)
				}
				return nil
			}})
		})
		t.Fatalf("child returned %v", e)
	}
	binary := filepath.Join(t.TempDir(), "acr")
	if output, err := exec.Command("go", "build", "-o", binary, "../../cmd/acr").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %s %v", output, err)
	}
	for _, authorized := range []bool{false, true} {
		t.Run(map[bool]string{false: "implicit-no-authorization", true: "explicit-recovery-control"}[authorized], func(t *testing.T) {
			project, source, service, _ := localFixture(t)
			if _, e := service.InstallLocal(context.Background(), project, source, false); e != nil {
				t.Fatal(e)
			}
			state, e := LoadState(project)
			if e != nil {
				t.Fatal(e)
			}
			state.Project.Agents = []string{"codex"}
			state.Project.Freshness = "none"
			if e = WriteState(project, state); e != nil {
				t.Fatal(e)
			}
			next := t.TempDir()
			if e = ExtractPackageArchive(packageArchive(t, "1.0.0", "original\n"), next); e != nil {
				t.Fatal(e)
			}
			child := exec.Command(os.Args[0], "-test.run=^TestLocalUnauthorizedInterruptedPathChange$")
			child.Env = append(os.Environ(), "ACR_LOCAL_PARTIAL_CRASH=1", "ACR_LOCAL_PARTIAL_PROJECT="+project, "ACR_LOCAL_PARTIAL_SOURCE="+next)
			out, e := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(e, &exit) || exit.ExitCode() != 77 {
				t.Fatalf("child %s %v", out, e)
			}
			_, e = LoadState(project)
			if e == nil || !strings.Contains(e.Error(), "locked path") {
				t.Fatalf("missing partial-state setup: %v", e)
			}
			t.Setenv("ACR_STATE_HOME", t.TempDir())
			before := localRecoveryTree(t, project)
			sourceBefore := localRecoveryTree(t, next)
			originalSourceBefore := localRecoveryTree(t, source)
			args := []string{"realize", "--project", project}
			if authorized {
				args = []string{"install", "file:" + next, "--project", project}
			}
			cmd := exec.Command(binary, args...)
			out, e = cmd.CombinedOutput()
			t.Logf("command=%v err=%v output=%s", args, e, out)
			if !reflect.DeepEqual(sourceBefore, localRecoveryTree(t, next)) {
				t.Fatal("source modified")
			}
			if authorized {
				if e != nil {
					t.Fatal(e)
				}
				for _, verb := range []string{"realize", "check"} {
					out, e := exec.Command(binary, verb, "--project", project).CombinedOutput()
					if e != nil {
						t.Fatalf("%s %s %v", verb, out, e)
					}
				}
			} else {
				if e == nil || !strings.Contains(string(out), "local_source_unauthorized") || !strings.Contains(string(out), "acr install file:") {
					t.Fatalf("missing authorization refusal/explicit remedy: %s %v", out, e)
				}
				after := localRecoveryTree(t, project)
				var changed []string
				for p, b := range before {
					if after[p] != b {
						changed = append(changed, p)
					}
				}
				for p := range after {
					if _, ok := before[p]; !ok {
						changed = append(changed, p)
					}
				}
				t.Logf("changed project paths=%v", changed)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("unauthorized implicit realization modified interrupted project before refusing")
				}
				// The refused tree, including its retained journal, is repairable.
				out, e = exec.Command(binary, "install", "file:"+next, "--project", project).CombinedOutput()
				if e != nil {
					t.Fatalf("explicit repair after refusal: %s %v", out, e)
				}
				for _, verb := range []string{"realize", "check"} {
					out, e = exec.Command(binary, verb, "--project", project).CombinedOutput()
					if e != nil {
						t.Fatalf("%s after repair: %s %v", verb, out, e)
					}
				}
			}
			if !reflect.DeepEqual(originalSourceBefore, localRecoveryTree(t, source)) || !reflect.DeepEqual(sourceBefore, localRecoveryTree(t, next)) {
				t.Fatal("repair mutated a local source")
			}
		})
	}
}

func TestNonlocalInterruptedRecoveryStillWorks(t *testing.T) {
	if mode := os.Getenv("ACR_NONLOCAL_RECOVERY_CHILD"); mode != "" {
		project := os.Getenv("ACR_NONLOCAL_PROJECT")
		before := readTestFile(t, filepath.Join(project, ProjectFilename))
		after := "broken: [\n"
		if mode == "schema" {
			after = "schemaVersion: 99\nagents: [codex]\n"
		}
		err := realize.ApplyFileTransactionWithHooks(project, []realize.FileTransactionEdit{{Path: ProjectFilename, Operation: "splice", Before: []byte(before), BeforeMode: 0o644, After: []byte(after), AfterMode: 0o644}}, nil, realize.FileTransactionHooks{TransactionID: func() (string, error) { return "tx-nonlocal", nil }, AfterEdit: func(int, realize.FileTransactionEdit) error { os.Exit(77); return nil }})
		t.Fatalf("child returned: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "acr")
	if output, err := exec.Command("go", "build", "-o", binary, "../../cmd/acr").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", output, err)
	}
	for _, mode := range []string{"yaml", "schema"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("ACR_STATE_HOME", t.TempDir())
			project := t.TempDir()
			state := State{Project: Project{SchemaVersion: BaselineSchemaVersion, Agents: []string{"codex"}, Freshness: "none"}, Lock: Lockfile{SchemaVersion: BaselineSchemaVersion}}
			if err := WriteState(project, state); err != nil {
				t.Fatal(err)
			}
			child := exec.Command(os.Args[0], "-test.run=^TestNonlocalInterruptedRecoveryStillWorks$")
			child.Env = append(os.Environ(), "ACR_NONLOCAL_RECOVERY_CHILD="+mode, "ACR_NONLOCAL_PROJECT="+project)
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 77 {
				t.Fatalf("child: %s %v", output, err)
			}
			if _, err := LoadState(project); err == nil {
				t.Fatal("control state was readable")
			}
			for _, verb := range []string{"realize", "check"} {
				output, err := exec.Command(binary, verb, "--project", project).CombinedOutput()
				if err != nil {
					t.Fatalf("nonlocal %s: %s %v", verb, output, err)
				}
			}
		})
	}
}

func TestLocalRecoveryChecksBeforeImagesBehindNonlocalAfterState(t *testing.T) {
	project, source, service, _ := localFixture(t)
	if _, err := service.InstallLocal(context.Background(), project, source, false); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(project)
	if err != nil {
		t.Fatal(err)
	}
	state.Project.Dependencies = nil
	state.Lock.Dependencies = nil
	projectData, lockData, err := MarshalState(state)
	if err != nil {
		t.Fatal(err)
	}
	edits := []realize.FileTransactionEdit{}
	for index, name := range []string{ProjectFilename, LockFilename} {
		after := projectData
		if index == 1 {
			after = lockData
		}
		edits = append(edits, realize.FileTransactionEdit{Path: name, Operation: "splice", Before: []byte(readTestFile(t, filepath.Join(project, name))), BeforeMode: 0o644, After: after, AfterMode: 0o644})
	}
	t.Setenv("ACR_STATE_HOME", t.TempDir())
	inspected := false
	err = realize.ApplyFileTransactionWithHooks(project, edits, nil, realize.FileTransactionHooks{TransactionID: func() (string, error) { return "tx-local-removal", nil }, AfterEdit: func(index int, _ realize.FileTransactionEdit) error {
		if index != 1 {
			return nil
		}
		after, err := LoadState(project)
		if err != nil || hasLocalDeclarations(after) {
			t.Fatalf("nonlocal after-state: %v", err)
		}
		before := localRecoveryTree(t, project)
		err = AuthorizeLocalRecovery(project)
		if err == nil || !strings.Contains(err.Error(), "local_source_unauthorized") {
			t.Fatalf("recovered local row escaped authorization: %v", err)
		}
		if !reflect.DeepEqual(before, localRecoveryTree(t, project)) {
			t.Fatal("authorization inspection wrote state")
		}
		inspected = true
		return nil
	}})
	if err != nil || !inspected {
		t.Fatalf("transaction: %v", err)
	}
}
