package realize_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/migrateapp"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const vendorRoot = ".agents/vendor/zeta/"
const transactionRoot = ".agents/.acr-transactions"

var vendorPackages = []string{"alpha", "bravo", "charlie"}

func TestVendoredMigrationRecoversFromSIGKILL(t *testing.T) {
	if point := os.Getenv("ACR_TEST_VENDOR_CRASH_POINT"); point != "" {
		realize.ArmVendorMigrationCrash(point)
		if _, err := runVendorMigration(os.Getenv("ACR_TEST_VENDOR_CRASH_ROOT"), false); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("migration returned before reaching %s", point)
	}
	for _, test := range []struct {
		point     string
		promoted  int
		lock      bool
		project   bool
		receipt   bool
		committed bool
	}{
		{point: "stage"},
		{point: "publish"},
		{point: vendorRoot + "alpha", promoted: 1},
		{point: vendorRoot + "bravo", promoted: 2},
		{point: vendorRoot + "charlie", promoted: 3},
		{point: ".agents/registry.lock", promoted: 3, lock: true},
		{point: "agents.yaml", promoted: 3, lock: true, project: true},
		{point: ".agents/.acr-applied.json", promoted: 3, lock: true, project: true, receipt: true},
		{point: "retire", promoted: 3, lock: true, project: true, receipt: true, committed: true},
	} {
		t.Run(test.point, func(t *testing.T) {
			root := vendorMigrationFixture(t) + string(filepath.Separator)
			killVendorMigration(t, root, test.point)
			assertVendorPathExists(t, root, transactionRoot, true)
			assertVendorPathExists(t, root, ".agents/registry.lock", test.lock)
			assertVendorPathExists(t, root, "agents.yaml", test.project)
			assertVendorPathExists(t, root, ".agents/.acr-applied.json", test.receipt)
			for index, name := range vendorPackages {
				assertVendorPathExists(t, root, vendorRoot+name, index < test.promoted)
			}
			if test.point != "stage" && !test.committed {
				_, err := runVendorMigration(root, true)
				var pending *realize.PendingTransactionError
				// Preview can reject the incomplete state pair before it reaches
				// the journal check when only the lock has been written.
				if err == nil || (test.lock == test.project && !errors.As(err, &pending)) {
					t.Fatalf("dry run with in-flight transaction: %v", err)
				}
			}
			var promoted os.FileInfo
			if test.promoted != 0 {
				// Keep the first promoted directory's inode allocated after
				// rollback so the retry cannot obtain it by inode reuse.
				tree, err := os.Open(filepath.Join(root, vendorRoot+"alpha"))
				if err != nil {
					t.Fatal(err)
				}
				defer tree.Close()
				promoted, err = tree.Stat()
				if err != nil {
					t.Fatal(err)
				}
			}
			report, err := runVendorMigration(root, false)
			if err != nil || report.Wrote != !test.committed {
				t.Fatalf("retry wrote=%t, error=%v", report.Wrote, err)
			}
			if promoted != nil {
				live, err := os.Stat(filepath.Join(root, vendorRoot+"alpha"))
				if err != nil || os.SameFile(live, promoted) != test.committed {
					t.Fatalf("retry kept interrupted vendor inode, committed=%t, stat error=%v", test.committed, err)
				}
			}
			assertVendorMigrationConverged(t, root)
		})
	}
}

func TestVendoredMigrationRecoveryPreservesUserContent(t *testing.T) {
	t.Run("identical package arrives before its promotion", func(t *testing.T) {
		root := vendorMigrationFixture(t)
		killVendorMigration(t, root, vendorRoot+"alpha")
		source := filepath.Join(root, ".tessl/plugins/zeta/bravo")
		if err := os.CopyFS(filepath.Join(root, vendorRoot+"bravo"), os.DirFS(source)); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(root, vendorRoot+"bravo")
		before, err := os.Stat(destination)
		if err != nil {
			t.Fatal(err)
		}
		if report, err := runVendorMigration(root, false); err != nil || !report.Wrote {
			t.Fatalf("retry wrote=%t, error=%v", report.Wrote, err)
		}
		after, err := os.Stat(destination)
		if err != nil || !os.SameFile(before, after) {
			t.Fatalf("pre-existing package replaced: %v", err)
		}
		assertVendorMigrationConverged(t, root)
	})
	t.Run("edited package blocks automatic rollback", func(t *testing.T) {
		root := vendorMigrationFixture(t)
		killVendorMigration(t, root, vendorRoot+"bravo")
		const edited = vendorRoot + "alpha/rules/rule-alpha.md"
		writeVendorFixture(t, root, edited, "operator edit\n", 0o644)
		_, err := runVendorMigration(root, false)
		var conflict *realize.RecoveryConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("retry with changed vendor: %v", err)
		}
		assertVendorContents(t, root, edited, "operator edit\n")
		assertVendorPathExists(t, root, vendorRoot+"bravo", true)
		assertVendorPathExists(t, root, "agents.yaml", false)
		assertVendorPathExists(t, root, transactionRoot, true)
		writeVendorFixture(t, root, edited, "Rule for alpha.\n", 0o644)
		if report, err := runVendorMigration(root, false); err != nil || !report.Wrote {
			t.Fatalf("retry after reconciliation wrote=%t, error=%v", report.Wrote, err)
		}
		assertVendorMigrationConverged(t, root)
	})
}

func killVendorMigration(t *testing.T, root, point string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestVendoredMigrationRecoversFromSIGKILL$")
	cmd.Env = append(os.Environ(), "ACR_TEST_VENDOR_CRASH_POINT="+point, "ACR_TEST_VENDOR_CRASH_ROOT="+root)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("child did not reach %s: %v\n%s", point, err, output)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || status.Signal() != syscall.SIGKILL {
		t.Fatalf("child did not receive SIGKILL at %s: %v\n%s", point, err, output)
	}
}

type unusedVendorRemote struct{ dependency.Remote }

func runVendorMigration(root string, dryRun bool) (migrate.MigrationReport, error) {
	app := migrateapp.NewApplication(unusedVendorRemote{}, "test")
	result, err := app.Execute(context.Background(), cli.Invocation{
		Command: cli.CommandMigrate, Subcommand: "tessl", ProjectDirectory: root,
		VendorUnmapped: true, DryRun: dryRun, NonInteractive: true,
	})
	report, _ := result.Value.(migrate.MigrationReport)
	return report, err
}

func vendorMigrationFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	declarations := make(map[string]map[string]string)
	for _, name := range vendorPackages {
		id := "zeta/" + name
		prefix := ".tessl/plugins/" + id + "/"
		declarations[id] = map[string]string{"version": "legacy"}
		writeVendorFixture(t, root, prefix+".tessl-plugin/plugin.json", fmt.Sprintf(`{"name":%q,"version":"legacy","rules":["rules"],"skills":["skills"]}`, id), 0o644)
		writeVendorFixture(t, root, prefix+"rules/rule-"+name+".md", "Rule for "+name+".\n", 0o644)
		writeVendorFixture(t, root, prefix+"skills/"+name+"/SKILL.md", "# "+name+"\n", 0o644)
		writeVendorFixture(t, root, prefix+"script.sh", "#!/bin/sh\nexit 0\n", 0o755)
	}
	data, err := json.Marshal(map[string]any{"name": "consumer", "dependencies": declarations})
	if err != nil {
		t.Fatal(err)
	}
	writeVendorFixture(t, root, "tessl.json", string(data), 0o644)
	writeVendorFixture(t, root, "CLAUDE.md", "Operator notes.\n", 0o644)
	writeVendorFixture(t, root, vendorRoot+"operator/notes.md", "User vendor content.\n", 0o644)
	writeVendorFixture(t, root, ".agents/.acr-vendor-1234/notes.md", "Pre-existing staging contents.\n", 0o644)
	if err := os.Symlink("../CLAUDE.md", filepath.Join(root, ".agents/.acr-vendor-5678")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".claude/skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../.tessl/plugins/zeta/alpha/skills/alpha", filepath.Join(root, ".claude/skills/tessl__alpha")); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeVendorFixture(t *testing.T, root, name, content string, mode os.FileMode) {
	t.Helper()
	filename := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func assertVendorMigrationConverged(t *testing.T, root string) {
	t.Helper()
	state, err := dependency.LoadState(root)
	if err != nil || len(state.Lock.Dependencies) != len(vendorPackages) {
		t.Fatalf("migration lock has %d dependencies: %v", len(state.Lock.Dependencies), err)
	}
	for index, name := range vendorPackages {
		locked := state.Lock.Dependencies[index]
		destination := filepath.Join(root, vendorRoot+name)
		hash, err := dependency.HashVendorTree(destination)
		if err != nil || locked.Source != "vendor:zeta/"+name || locked.ContentHash != hash {
			t.Fatalf("vendor %s lock %v does not match live hash %q: %v", name, locked, hash, err)
		}
		script, err := os.Stat(filepath.Join(destination, "script.sh"))
		if err != nil || script.Mode().Perm() != 0o755 {
			t.Fatalf("vendor %s lost its executable mode: %v", name, err)
		}
	}
	ledger, err := realize.DecodeLedger(state.Lock.Realization)
	if err != nil || len(ledger.Targets) == 0 {
		t.Fatalf("native realization ledger: %v, %v", ledger, err)
	}
	for _, target := range ledger.Targets {
		content, err := os.ReadFile(filepath.Join(root, target.Path))
		if err != nil || fmt.Sprintf("sha256:%x", sha256.Sum256(content)) != target.OutputHash {
			t.Fatalf("native output %s does not match its ledger: %v", target.Path, err)
		}
	}
	claude, err := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if err != nil || !strings.Contains(string(claude), "Operator notes.\n") {
		t.Fatalf("user-owned CLAUDE.md content: %q, %v", claude, err)
	}
	assertVendorPathExists(t, root, ".agents/.acr-applied.json", true)
	assertVendorPathExists(t, root, transactionRoot, false)
	assertVendorContents(t, root, vendorRoot+"operator/notes.md", "User vendor content.\n")
	assertVendorContents(t, root, ".agents/.acr-vendor-1234/notes.md", "Pre-existing staging contents.\n")
	if link, err := os.Readlink(filepath.Join(root, ".agents/.acr-vendor-5678")); err != nil || link != "../CLAUDE.md" {
		t.Fatalf("pre-existing staging link: %q, %v", link, err)
	}
	legacy, err := filepath.Glob(filepath.Join(root, ".agents/.acr-vendor-*"))
	if err != nil || !reflect.DeepEqual(legacy, []string{filepath.Join(root, ".agents/.acr-vendor-1234"), filepath.Join(root, ".agents/.acr-vendor-5678")}) {
		t.Fatalf("unexpected vendor staging: %v, %v", legacy, err)
	}
	if report, err := runVendorMigration(root, false); err != nil || report.Wrote {
		t.Fatalf("converged retry wrote=%t, error=%v", report.Wrote, err)
	}
	assertVendorPathExists(t, root, transactionRoot, false)
}

func assertVendorContents(t *testing.T, root, name, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil || string(got) != want {
		t.Fatalf("%s content = %q, error = %v", name, got, err)
	}
}

func assertVendorPathExists(t *testing.T, root, name string, want bool) {
	t.Helper()
	_, err := os.Lstat(filepath.Join(root, name))
	if (want && err != nil) || (!want && !errors.Is(err, os.ErrNotExist)) {
		t.Fatalf("%s exists, want %t: %v", name, want, err)
	}
}
