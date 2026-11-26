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
	"github.com/jbaruch/agentic-context-registry/internal/dependencytest"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/migrateapp"
	"github.com/jbaruch/agentic-context-registry/internal/publish"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const vendorRoot = ".agents/vendor/zeta/"
const transactionRoot = ".agents/.acr-transactions"

var vendorPackages = []string{"alpha", "bravo", "charlie"}

func TestVendoredMigrationRecoversFromSIGKILL(t *testing.T) {
	if point := os.Getenv("ACR_TEST_VENDOR_CRASH_POINT"); point != "" {
		boundary, mixed := strings.CutPrefix(point, "mixed:")
		realize.ArmVendorMigrationCrash(boundary)
		root := os.Getenv("ACR_TEST_VENDOR_CRASH_ROOT")
		var err error
		if mixed {
			_, err = runMixedVendorMigration(t, root)
		} else {
			_, err = runVendorMigration(root, false)
		}
		if err != nil {
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
				// Vendor entries need the journal version that defines them.
				if pending != nil && pending.SchemaVersion != 2 {
					t.Fatalf("pending vendor journal schemaVersion = %d, want 2", pending.SchemaVersion)
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

func TestVendoredMigrationRecoversAfterSecondSIGKILL(t *testing.T) {
	root := vendorMigrationFixture(t)
	killVendorMigration(t, root, vendorRoot+"bravo")
	first, err := os.Stat(filepath.Join(root, vendorRoot+"alpha"))
	if err != nil {
		t.Fatal(err)
	}
	killVendorMigration(t, root, "rollback-vendor")
	assertVendorPathExists(t, root, vendorRoot+"alpha", false)
	assertVendorPathExists(t, root, vendorRoot+"bravo", true)
	assertVendorPathExists(t, root, transactionRoot, true)
	if report, err := runVendorMigration(root, false); err != nil || !report.Wrote {
		t.Fatalf("retry wrote=%t, error=%v", report.Wrote, err)
	}
	preserved, err := filepath.Glob(filepath.Join(root, ".agents/.acr-recovered-vendors-*/vendor-000000"))
	if err != nil || len(preserved) != 1 {
		t.Fatalf("preserved rollback tree = %v, error=%v", preserved, err)
	}
	info, err := os.Stat(preserved[0])
	if err != nil || !os.SameFile(first, info) {
		t.Fatalf("recovery changed the preserved first tree's identity: %v", err)
	}
	assertVendorMigrationConverged(t, root)
}

func TestLegacyVendorStagingConvergesAfterSIGKILL(t *testing.T) {
	root := vendorMigrationFixture(t)
	// The previous writer renamed the first .acr-vendor-* directory into
	// place and left the next complete stage behind when killed.
	const staged = ".agents/.acr-vendor-3158729610"
	const lookalike = ".agents/.acr-vendor-3158729611"
	for _, copy := range []struct{ pkg, destination string }{
		{"alpha", vendorRoot + "alpha"}, {"bravo", staged}, {"bravo", lookalike},
	} {
		location := filepath.Join(root, copy.destination)
		copyVendorFixture(t, filepath.Join(root, ".tessl/plugins/zeta", copy.pkg), location)
		if err := os.Chmod(location, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The normalized package hash ignores extra empty directories. Ownership
	// proof must account for them before retiring a journal-free tree.
	if err := os.Mkdir(filepath.Join(root, lookalike, "operator"), 0o755); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(filepath.Join(root, staged))
	if err != nil {
		t.Fatal(err)
	}
	preview, err := runVendorMigration(root, true)
	if err != nil || preview.Wrote {
		t.Fatalf("dry-run wrote=%t, error=%v", preview.Wrote, err)
	}
	// Dry-run names the stage that apply moves, as well as the one it keeps.
	if !hasVendorNote(preview, legacyRetirementNote, staged) || !hasVendorNote(preview, "ambiguous_legacy_vendor_staging", lookalike) {
		t.Fatalf("dry-run notes omit legacy staging: %+v", preview.Notes)
	}
	assertVendorPathExists(t, root, staged, true)
	killVendorMigration(t, root, staged)
	assertVendorPathExists(t, root, staged, false)
	assertVendorPathExists(t, root, lookalike+"/operator", true)
	if err := realize.RecoverTransactions(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	restored, err := os.Stat(filepath.Join(root, staged))
	if err != nil || !os.SameFile(original, restored) {
		t.Fatalf("rollback did not restore the legacy tree's identity: %v", err)
	}
	assertVendorPathExists(t, root, "agents.yaml", false)
	report, err := runVendorMigration(root, false)
	if err != nil || !report.Wrote {
		t.Fatalf("retry wrote=%t, error=%v", report.Wrote, err)
	}
	assertVendorPathExists(t, root, staged, false)
	assertVendorPathExists(t, root, lookalike+"/operator", true)
	archived, err := filepath.Glob(filepath.Join(root, ".agents/.acr-recovered-vendors-*/vendor-*"))
	if err != nil {
		t.Fatal(err)
	}
	var preserved bool
	for _, filename := range archived {
		info, err := os.Stat(filename)
		if err != nil {
			t.Fatal(err)
		}
		preserved = preserved || os.SameFile(original, info)
	}
	if !preserved {
		t.Fatal("legacy stage inode did not survive retirement")
	}
	if !hasVendorNote(report, "ambiguous_legacy_vendor_staging", lookalike) {
		t.Fatalf("unverified legacy path %s was not reported", lookalike)
	}
	if !hasVendorNote(report, legacyRetirementNote, staged) || hasVendorNote(report, legacyRetirementNote, lookalike) {
		t.Fatalf("apply notes misreport legacy retirement: %+v", report.Notes)
	}
	if err := os.Remove(filepath.Join(root, lookalike, "operator")); err != nil {
		t.Fatal(err)
	}
	// An otherwise matching legacy stage with user-edited bytes must remain.
	writeVendorFixture(t, root, lookalike+"/rules/rule-bravo.md", "User for bravo.\n", 0o644)
	if report, err := runVendorMigration(root, false); err != nil || report.Wrote {
		t.Fatalf("converged retry wrote=%t, error=%v", report.Wrote, err)
	}
	assertVendorContents(t, root, lookalike+"/rules/rule-bravo.md", "User for bravo.\n")
	assertVendorPathExists(t, root, transactionRoot, false)
	assertVendorContents(t, root, ".agents/.acr-vendor-1234/notes.md", "Pre-existing staging contents.\n")
	if link, err := os.Readlink(filepath.Join(root, ".agents/.acr-vendor-5678")); err != nil || link != "../CLAUDE.md" {
		t.Fatalf("pre-existing staging link = %q, %v", link, err)
	}
	copyVendorFixture(t, filepath.Join(root, ".tessl/plugins/zeta/bravo"), filepath.Join(root, staged))
	if err := os.Chmod(filepath.Join(root, staged), 0o700); err != nil {
		t.Fatal(err)
	}
	if report, err := runVendorMigration(root, false); err != nil || !report.Wrote {
		t.Fatalf("retirement-only apply wrote=%t, error=%v", report.Wrote, err)
	}
	assertVendorPathExists(t, root, staged, false)
	assertVendorPathExists(t, root, transactionRoot, false)
}

// macOS does not inherit setgid, so these tests set the modes Linux would create.
func TestVendorMigrationAcceptsInheritedSetgid(t *testing.T) {
	t.Run("rollback", func(t *testing.T) {
		root := vendorMigrationFixture(t)
		chmodSetgid(t, filepath.Join(root, ".agents"), 0o755)
		killVendorMigration(t, root, vendorRoot+"alpha")
		directory := setgidRecoveryDirectory(t, root)
		chmodSetgid(t, directory, 0o750)
		if err := realize.RecoverTransactions(t.Context(), root); err == nil || !strings.Contains(err.Error(), "private directory with mode 0700") {
			t.Fatalf("recovery into group-readable directory: %v", err)
		}
		assertVendorPathExists(t, root, vendorRoot+"alpha", true)
		chmodSetgid(t, directory, 0o700)
		retryVendorMigration(t, root)
		preservedVendorTree(t, root, "vendor-000000")
	})
	t.Run("mixed retirement", func(t *testing.T) {
		root := supersededVendorFixture(t)
		chmodSetgid(t, filepath.Join(root, ".agents"), 0o755)
		realize.OnVendorMigrationRename(t, func(target string) {
			if target == vendorRoot+"bravo" {
				setgidRecoveryDirectory(t, root)
			}
		})
		if report, err := runMixedVendorMigration(t, root); err != nil || !report.Wrote {
			t.Fatalf("mixed apply wrote=%t, error=%v", report.Wrote, err)
		}
		assertMixedVendorMigrationConverged(t, root)
		preservedVendorTree(t, root, "vendor-000002")
	})
	t.Run("legacy staging", func(t *testing.T) {
		root := vendorMigrationFixture(t)
		chmodSetgid(t, filepath.Join(root, ".agents"), 0o755)
		const staged = ".agents/.acr-vendor-3158729610"
		const openRoot = ".agents/.acr-vendor-3158729611"
		const openChild = ".agents/.acr-vendor-3158729612"
		for _, stage := range []struct {
			name        string
			root, child os.FileMode
		}{{staged, 0o700, 0o755}, {openRoot, 0o750, 0o755}, {openChild, 0o700, 0o775}} {
			location := filepath.Join(root, stage.name)
			copyVendorFixture(t, filepath.Join(root, ".tessl/plugins/zeta/bravo"), location)
			chmodSetgid(t, location, stage.root)
			for _, child := range []string{".tessl-plugin", "rules", "skills", "skills/bravo"} {
				chmodSetgid(t, filepath.Join(location, child), stage.child)
			}
		}
		if report, err := runVendorMigration(root, false); err != nil || !report.Wrote {
			t.Fatalf("apply wrote=%t, error=%v", report.Wrote, err)
		}
		assertVendorPathExists(t, root, staged, false)
		for _, ambiguous := range []string{openRoot, openChild} {
			assertVendorPathExists(t, root, ambiguous, true)
			if err := os.RemoveAll(filepath.Join(root, ambiguous)); err != nil {
				t.Fatal(err)
			}
		}
		assertVendorMigrationConverged(t, root)
	})
}

func TestCombinedVendorMigrationRecoversSupersededTree(t *testing.T) {
	for _, point := range []string{vendorRoot + "alpha", "retire"} {
		t.Run(point, func(t *testing.T) {
			root := supersededVendorFixture(t)
			original, err := os.Stat(filepath.Join(root, vendorRoot+"alpha"))
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(root, ".agents/registry.lock"))
			if err != nil {
				t.Fatal(err)
			}
			killVendorMigration(t, root, "mixed:"+point)
			assertVendorPathExists(t, root, vendorRoot+"alpha", false)
			assertVendorPathExists(t, root, vendorRoot+"bravo", true)
			if point != "retire" {
				if err := realize.RecoverTransactions(t.Context(), root); err != nil {
					t.Fatal(err)
				}
				assertVendorContents(t, root, ".agents/registry.lock", string(before))
				assertVendorPathExists(t, root, vendorRoot+"bravo", false)
				restored, err := os.Stat(filepath.Join(root, vendorRoot+"alpha"))
				if err != nil || !os.SameFile(original, restored) {
					t.Fatalf("superseded vendor inode was not restored: %v", err)
				}
			}
			report, err := runMixedVendorMigration(t, root)
			if err != nil || report.Wrote != (point != "retire") || len(report.Lock.Dependencies) != 3 || report.Lock.Dependencies[0].Source != "github:zeta/alpha" {
				t.Fatalf("combined retry wrote=%t, locks=%v, error=%v", report.Wrote, report.Lock.Dependencies, err)
			}
			assertVendorPathExists(t, root, vendorRoot+"alpha", false)
			assertVendorPathExists(t, root, transactionRoot, false)
			for _, name := range []string{"bravo", "charlie"} {
				assertVendorPathExists(t, root, vendorRoot+name, true)
			}
			if report, err := runMixedVendorMigration(t, root); err != nil || report.Wrote {
				t.Fatalf("converged combined retry wrote=%t, error=%v", report.Wrote, err)
			}
		})
	}
}

// supersededVendorFixture has alpha committed as a vendor package, while bravo
// and charlie wait to be vendored by the apply that maps alpha upstream.
func supersededVendorFixture(t *testing.T) string {
	t.Helper()
	root := vendorMigrationFixture(t)
	pendingPackages := t.TempDir()
	for _, name := range []string{"bravo", "charlie"} {
		if err := os.Rename(filepath.Join(root, ".tessl/plugins/zeta", name), filepath.Join(pendingPackages, name)); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := os.ReadFile(filepath.Join(root, "tessl.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeVendorFixture(t, root, "tessl.json", `{"name":"consumer","dependencies":{"zeta/alpha":{"version":"legacy"}}}`, 0o644)
	if report, err := runVendorMigration(root, false); err != nil || !report.Wrote {
		t.Fatalf("initial migration wrote=%t, error=%v", report.Wrote, err)
	}
	for _, name := range []string{"bravo", "charlie"} {
		if err := os.Rename(filepath.Join(pendingPackages, name), filepath.Join(root, ".tessl/plugins/zeta", name)); err != nil {
			t.Fatal(err)
		}
	}
	writeVendorFixture(t, root, "tessl.json", string(manifest), 0o644)
	return root
}

func runMixedVendorMigration(t *testing.T, root string) (migrate.MigrationReport, error) {
	t.Helper()
	const repo = "github:zeta/alpha"
	const tag = "v1.0.0"
	commit := strings.Repeat("a", 40)
	archive, err := publish.BuildArchive("alpha", tag, []publish.File{
		{Path: "agent-plugin.yaml", Mode: 0o644, Content: []byte("schemaVersion: 1\nname: zeta/alpha\nversion: 1.0.0\nsource:\n  repository: https://github.com/zeta/alpha\nartifacts:\n  rules:\n    - id: rule-alpha\n      path: rules/rule-alpha.md\n      activation:\n        mode: always\n  skills:\n    - id: alpha\n      path: skills/alpha\n")},
		{Path: "rules/rule-alpha.md", Mode: 0o644, Content: []byte("Rule for alpha.\n")},
		{Path: "skills/alpha/SKILL.md", Mode: 0o644, Content: []byte("# alpha\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	remote := dependencytest.NewRemote()
	remote.Latest[repo] = dependency.Release{ID: 7, Tag: tag}
	remote.Commits[repo+"@"+tag] = commit
	remote.Archives[repo+"@"+commit] = archive.Bytes
	app := migrateapp.NewApplication(remote, "test")
	result, err := app.Execute(context.Background(), cli.Invocation{
		Command: cli.CommandMigrate, Subcommand: "tessl", ProjectDirectory: root,
		VendorUnmapped: true, Mappings: []string{"zeta/alpha=" + repo + "@latest"}, NonInteractive: true,
	})
	report, _ := result.Value.(migrate.MigrationReport)
	return report, err
}

func TestVendoredMigrationRecoveryPreservesUserContent(t *testing.T) {
	t.Run("identical package arrives before its promotion", func(t *testing.T) {
		root := vendorMigrationFixture(t)
		killVendorMigration(t, root, vendorRoot+"alpha")
		source := filepath.Join(root, ".tessl/plugins/zeta/bravo")
		copyVendorFixture(t, source, filepath.Join(root, vendorRoot+"bravo"))
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

// os.CopyFS applies the process umask, so restore the modes the vendor writer sets.
func copyVendorFixture(t *testing.T, source, destination string) {
	t.Helper()
	if err := os.CopyFS(destination, os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(destination, func(filename string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.IsDir() || info.Mode()&0o100 != 0 {
			mode = 0o755
		}
		return os.Chmod(filename, mode)
	}); err != nil {
		t.Fatal(err)
	}
}

func chmodSetgid(t *testing.T, directory string, permissions os.FileMode) {
	t.Helper()
	// An unprivileged chmod drops setgid unless the caller is in the directory's group.
	if err := os.Chown(directory, -1, os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, os.ModeSetgid|permissions); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(directory); err != nil || info.Mode() != os.ModeDir|os.ModeSetgid|permissions {
		t.Fatalf("setgid fixture %s was not created: %v", directory, err)
	}
}

// setgidRecoveryDirectory stands in for ACR's Mkdir under a setgid parent.
// The pending transaction must still hold a staged promotion at index 1.
func setgidRecoveryDirectory(t *testing.T, root string) string {
	t.Helper()
	stages, err := filepath.Glob(filepath.Join(root, transactionRoot, "*", "vendor-000001"))
	if err != nil || len(stages) != 1 {
		t.Fatalf("pending vendor transaction = %v, %v", stages, err)
	}
	directory := filepath.Join(root, ".agents/.acr-recovered-vendors-"+filepath.Base(filepath.Dir(stages[0])))
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	chmodSetgid(t, directory, 0o700)
	return directory
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

const legacyRetirementNote = "legacy_vendor_staging_retirement"

func hasVendorNote(report migrate.MigrationReport, code, path string) bool {
	for _, note := range report.Notes {
		if note.Code == code && note.Path == path {
			return true
		}
	}
	return false
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
