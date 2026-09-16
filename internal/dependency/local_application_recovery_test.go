package dependency

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// Exit after the two dependency edits, leaving real durable before-images.
// The authorized control leaves completed grants intact; the interrupted
// removal stages an inactive grant exactly as the production remover does.
func TestLocalApplicationRecoveryCrash(t *testing.T) {
	if os.Getenv("ACR_APPLICATION_CRASH") == "" {
		return
	}
	p := os.Getenv("ACR_APPLICATION_PROJECT")
	before, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	desired, _, err := PruneDependency(before, "github:owner/plugin")
	if err != nil {
		t.Fatal(err)
	}
	pd, ld, err := MarshalState(desired)
	if err != nil {
		t.Fatal(err)
	}
	var edits []realize.FileTransactionEdit
	for i, name := range []string{ProjectFilename, LockFilename} {
		b, err := os.ReadFile(filepath.Join(p, name))
		if err != nil {
			t.Fatal(err)
		}
		after := pd
		if i == 1 {
			after = ld
		}
		edits = append(edits, realize.FileTransactionEdit{Path: name, Operation: "splice", Before: b, BeforeMode: 0644, After: after, AfterMode: 0644})
	}
	operation := func() error {
		return realize.ApplyFileTransactionWithHooks(p, edits, nil, realize.FileTransactionHooks{
			TransactionID: func() (string, error) { return "application-local-recovery", nil },
			AfterEdit: func(i int, _ realize.FileTransactionEdit) error {
				if i == 1 {
					os.Exit(77)
				}
				return nil
			},
		})
	}
	if os.Getenv("ACR_APPLICATION_CRASH") == "authorized" {
		err = operation()
	} else {
		err = ChangeLocalRemoval(p, "github:owner/plugin", operation)
	}
	t.Fatalf("crash child returned: %v", err)
}

func TestLocalApplicationRecoveryBoundaries(t *testing.T) {
	binary := localCallerBinary(t)
	for _, mode := range []string{"migrate-pending", "migrate-finalize-pending", "migrate-dry-run", "migrate-authorized", "migrate-live-unauthorized-no-journal", "explicit-path-repair", "uninstall-agents-pending", "uninstall-dry-run", "uninstall-vendor-no-agents-pending", "uninstall-vendor-no-journal"} {
		t.Run(mode, func(t *testing.T) {
			p, s, svc, _ := localFixture(t)
			ctx := context.Background()
			if _, err := svc.InstallLocal(ctx, p, s, false); err != nil {
				t.Fatal(err)
			}
			other := ""
			if strings.HasPrefix(mode, "uninstall-") && !strings.HasPrefix(mode, "uninstall-vendor") {
				other = t.TempDir()
				if err := ExtractPackageArchive(packageArchiveFor(t, "owner/other", "1.0.0", "other\n"), other); err != nil {
					t.Fatal(err)
				}
				if _, err := svc.InstallLocal(ctx, p, other, false); err != nil {
					t.Fatal(err)
				}
			}
			state, err := LoadState(p)
			if err != nil {
				t.Fatal(err)
			}
			state.Project.Freshness = "none"
			vendor := strings.HasPrefix(mode, "uninstall-vendor")
			if vendor {
				root := filepath.Join(p, ".agents/vendor/owner/vendor")
				if err := os.MkdirAll(root, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "tile.json"), []byte(`{"name":"owner/vendor","version":"1.0.0"}`), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "guidance.md"), []byte("vendor content\n"), 0644); err != nil {
					t.Fatal(err)
				}
				declaration := Declaration{Source: "vendor:owner/vendor", Requested: "vendored"}
				locked, err := rebuildVendorLock(p, declaration)
				if err != nil {
					t.Fatal(err)
				}
				state.Project.Dependencies = append(state.Project.Dependencies, declaration)
				state.Lock.Dependencies = append(state.Lock.Dependencies, locked)
			} else if strings.HasPrefix(mode, "uninstall-") {
				state.Project.Agents = []string{"codex"}
			}
			if err := WriteState(p, state); err != nil {
				t.Fatal(err)
			}
			noJournal := strings.HasSuffix(mode, "no-journal")
			if noJournal {
				file, _, err := localAuthorizationPath(p, "github:owner/plugin")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			} else {
				crash := "unauthorized"
				if mode == "migrate-authorized" {
					crash = "authorized"
				}
				child := exec.Command(os.Args[0], "-test.run=^TestLocalApplicationRecoveryCrash$")
				child.Env = append(os.Environ(), "ACR_APPLICATION_CRASH="+crash, "ACR_APPLICATION_PROJECT="+p)
				out, err := child.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 77 {
					t.Fatalf("child: %s %v", out, err)
				}
				images, err := realize.RecoveryBeforeImages(p, ProjectFilename, LockFilename)
				if err != nil || len(images) != 2 {
					t.Fatalf("missing journal before-images: %v %v", images, err)
				}
			}
			// The local row is live only without a journal; pending fixtures retain it
			// solely in the checked before-images. This discriminates pending-only gates.
			before, sourceBefore, storeBefore := localRecoveryTree(t, p), localRecoveryTree(t, s), localRecoveryTree(t, os.Getenv("ACR_STATE_HOME"))
			var otherBefore map[string]string
			if other != "" {
				otherBefore = localRecoveryTree(t, other)
			}
			args := []string{"migrate", "tessl", "--project", p}
			switch mode {
			case "migrate-finalize-pending":
				args = append(args, "--finalize")
			case "migrate-dry-run":
				args = append(args, "--dry-run")
			case "explicit-path-repair":
				args = []string{"install", "file:" + s, "--project", p}
			case "uninstall-agents-pending", "uninstall-dry-run":
				args = []string{"uninstall", "github:owner/other", "--project", p}
			case "uninstall-vendor-no-agents-pending", "uninstall-vendor-no-journal":
				args = []string{"uninstall", "vendor:owner/vendor", "--project", p}
			}
			if mode == "uninstall-dry-run" {
				args = append(args, "--dry-run")
			}
			out, runErr := exec.Command(binary, args...).CombinedOutput()
			changed := !reflect.DeepEqual(before, localRecoveryTree(t, p))
			t.Logf("mode=%s exit=%v changed=%v output=%s", mode, runErr, changed, out)
			if !reflect.DeepEqual(sourceBefore, localRecoveryTree(t, s)) {
				t.Fatal("source changed")
			}
			if other != "" && !reflect.DeepEqual(otherBefore, localRecoveryTree(t, other)) {
				t.Fatal("other source changed")
			}
			switch mode {
			case "migrate-authorized", "explicit-path-repair", "uninstall-vendor-no-journal":
				if runErr != nil || !changed {
					t.Fatalf("authorized recovery/repair/removal failed: %s %v", out, runErr)
				}
				live, err := LoadState(p)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "uninstall-vendor-no-journal" {
					if len(live.Project.Dependencies) != 1 || live.Project.Dependencies[0].Source != "github:owner/plugin" {
						t.Fatal("vendor prune did not preserve the local row")
					}
					if _, err := os.Lstat(filepath.Join(p, ".agents/vendor/owner/vendor")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("vendor tree retained: %v", err)
					}
				} else {
					if !reflect.DeepEqual(state, live) {
						t.Fatalf("recovery did not restore dependency state: %+v", live)
					}
					images, err := realize.RecoveryBeforeImages(p, ProjectFilename, LockFilename)
					if err != nil || images != nil {
						t.Fatalf("recovery left a journal: %v %v", images, err)
					}
				}
			case "migrate-dry-run", "migrate-live-unauthorized-no-journal":
				if runErr != nil || changed {
					t.Fatalf("no-op/dry-run failed: %s %v", out, runErr)
				}
			case "uninstall-dry-run":
				if runErr == nil || !strings.Contains(string(out), "pending_transaction") || changed {
					t.Fatalf("dry-run: %s %v", out, runErr)
				}
			default:
				if runErr == nil || !strings.Contains(string(out), "local_source_unauthorized") || changed {
					t.Errorf("must refuse unauthorized recovery before any mutation: %s %v", out, runErr)
				}
			}
			if mode != "explicit-path-repair" && !reflect.DeepEqual(storeBefore, localRecoveryTree(t, os.Getenv("ACR_STATE_HOME"))) {
				t.Fatal("recovery/refusal changed authorization store")
			}
		})
	}
}

func TestLocalLastPackageUninstallCLI(t *testing.T) {
	binary := localCallerBinary(t)
	for _, agents := range []bool{false, true} {
		for _, grant := range []string{"valid", "missing-record", "missing-store", "missing-source"} {
			name := grant + "/no-agents"
			if agents {
				name = grant + "/agents"
			}
			t.Run(name, func(t *testing.T) {
				p, s, svc, _ := localFixture(t)
				if _, err := svc.InstallLocal(context.Background(), p, s, false); err != nil {
					t.Fatal(err)
				}
				state, err := LoadState(p)
				if err != nil {
					t.Fatal(err)
				}
				state.Project.Freshness = "none"
				if agents {
					state.Project.Agents = []string{"codex"}
				}
				if err := WriteState(p, state); err != nil {
					t.Fatal(err)
				}
				if agents {
					if out, err := exec.Command(binary, "realize", "--project", p).CombinedOutput(); err != nil {
						t.Fatalf("setup realize: %s %v", out, err)
					}
				}
				file, _, err := localAuthorizationPath(p, "github:owner/plugin")
				if err != nil {
					t.Fatal(err)
				}
				switch grant {
				case "missing-record":
					if err := os.Remove(file); err != nil {
						t.Fatal(err)
					}
				case "missing-store", "missing-source":
					if err := os.RemoveAll(os.Getenv("ACR_STATE_HOME")); err != nil {
						t.Fatal(err)
					}
				}
				if grant == "missing-source" {
					if err := os.RemoveAll(s); err != nil {
						t.Fatal(err)
					}
				}
				var sourceBefore map[string]string
				if grant != "missing-source" {
					sourceBefore = localRecoveryTree(t, s)
				}
				out, err := exec.Command(binary, "uninstall", "github:owner/plugin", "--project", p).CombinedOutput()
				if err != nil {
					t.Fatalf("uninstall: %s %v", out, err)
				}
				after, err := LoadState(p)
				if err != nil {
					t.Fatal(err)
				}
				if len(after.Project.Dependencies) != 0 || len(after.Lock.Dependencies) != 0 {
					t.Fatal("last row retained")
				}
				if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("grant retained: %v", err)
				}
				if grant == "missing-store" || grant == "missing-source" {
					if _, err := os.Lstat(os.Getenv("ACR_STATE_HOME")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("created absent store: %v", err)
					}
				}
				if grant == "missing-source" {
					if _, err := os.Lstat(s); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("missing source recreated: %v", err)
					}
				} else if !reflect.DeepEqual(sourceBefore, localRecoveryTree(t, s)) {
					t.Fatal("source changed")
				}
			})
		}
	}
}
