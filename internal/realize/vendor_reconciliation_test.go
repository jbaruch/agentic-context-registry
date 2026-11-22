package realize_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const alphaRule = vendorRoot + "alpha/rules/rule-alpha.md"
const operatorEdit = "operator edit\n"

func TestVendorPlanIsValidatedAfterRecoveryUnderClaim(t *testing.T) {
	for _, point := range []string{vendorRoot + "bravo", vendorRoot + "charlie"} {
		t.Run(point, func(t *testing.T) {
			root := vendorMigrationFixture(t)
			// Apply reads a private legacy candidate after its first recovery and
			// preview, immediately before it applies. Kill a rival apply there.
			const candidate = ".agents/.acr-vendor-1234"
			if err := os.Chmod(filepath.Join(root, candidate), 0o700); err != nil {
				t.Fatal(err)
			}
			interrupted := false
			realize.OnVendorTreeRead(t, func(target string) {
				if target != candidate || interrupted {
					return
				}
				interrupted = true
				killVendorMigration(t, root, point)
			})
			report, err := runVendorMigration(root, false)
			if err != nil || !interrupted || !report.Wrote {
				t.Fatalf("apply after interleaved crash wrote=%t, interrupted=%t, error=%v", report.Wrote, interrupted, err)
			}
			assertVendorMigrationConverged(t, root)
		})
	}
}

func TestVendorPromotionReconciliationConverges(t *testing.T) {
	remove := func(t *testing.T, root string, names ...string) {
		t.Helper()
		for _, name := range names {
			if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Run("edited tree moved aside", func(t *testing.T) {
		root := vendorMigrationFixture(t)
		killVendorMigration(t, root, vendorRoot+"alpha")
		writeVendorFixture(t, root, alphaRule, operatorEdit, 0o644)
		assertVendorRecoveryConflict(t, root)
		if err := os.Rename(filepath.Join(root, vendorRoot+"alpha"), filepath.Join(root, "kept")); err != nil {
			t.Fatal(err)
		}
		retryVendorMigration(t, root)
		assertVendorContents(t, root, "kept/rules/rule-alpha.md", operatorEdit)
	})
	for _, point := range []string{vendorRoot + "alpha", vendorRoot + "charlie", "agents.yaml"} {
		t.Run("promoted trees deleted after "+point, func(t *testing.T) {
			root := vendorMigrationFixture(t)
			killVendorMigration(t, root, point)
			for _, name := range vendorPackages {
				remove(t, root, vendorRoot+name)
			}
			if err := realize.RecoverTransactions(root); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{transactionRoot, "agents.yaml", ".agents/registry.lock"} {
				assertVendorPathExists(t, root, name, false)
			}
			retryVendorMigration(t, root)
		})
	}
	t.Run("identical bytes restored", func(t *testing.T) {
		root := vendorMigrationFixture(t)
		killVendorMigration(t, root, vendorRoot+"alpha")
		// Keep the promoted inode allocated so the copy cannot reuse it.
		if err := os.Rename(filepath.Join(root, vendorRoot+"alpha"), filepath.Join(root, "kept")); err != nil {
			t.Fatal(err)
		}
		if err := os.CopyFS(filepath.Join(root, vendorRoot+"alpha"), os.DirFS(filepath.Join(root, "kept"))); err != nil {
			t.Fatal(err)
		}
		assertVendorRecoveryConflict(t, root)
		remove(t, root, vendorRoot+"alpha")
		retryVendorMigration(t, root)
	})
	t.Run("copied project", func(t *testing.T) {
		original := vendorMigrationFixture(t)
		killVendorMigration(t, original, vendorRoot+"alpha")
		root := filepath.Join(t.TempDir(), "copy")
		// The original stays allocated, so every copied tree has a new identity.
		if output, err := exec.Command("cp", "-a", original, root).CombinedOutput(); err != nil {
			t.Fatalf("copy project: %v\n%s", err, output)
		}
		assertVendorRecoveryConflict(t, root)
		remove(t, root, vendorRoot+"alpha")
		stages, err := filepath.Glob(filepath.Join(root, transactionRoot, "*", "vendor-*"))
		if err != nil || len(stages) != 2 {
			t.Fatalf("staged copies = %v, %v", stages, err)
		}
		for _, stage := range stages {
			assertVendorRecoveryConflict(t, root)
			if err := os.RemoveAll(stage); err != nil {
				t.Fatal(err)
			}
		}
		retryVendorMigration(t, root)
	})
	for _, moveAway := range []bool{false, true} {
		name := "edit in preserved copy"
		if moveAway {
			name = "edited preserved copy moved away"
		}
		t.Run(name, func(t *testing.T) {
			root := vendorMigrationFixture(t)
			killVendorMigration(t, root, vendorRoot+"bravo")
			killVendorMigration(t, root, "rollback-vendor")
			kept := preservedVendorTree(t, root, "vendor-000000")
			writeVendorFixture(t, root, kept+"/rules/rule-alpha.md", operatorEdit, 0o644)
			if moveAway {
				if err := os.Rename(filepath.Join(root, kept), filepath.Join(root, "kept")); err != nil {
					t.Fatal(err)
				}
				kept = "kept"
			}
			retryVendorMigration(t, root)
			assertVendorContents(t, root, kept+"/rules/rule-alpha.md", operatorEdit)
		})
	}
	t.Run("legacy staging deleted", func(t *testing.T) {
		root := vendorMigrationFixture(t)
		const staged = ".agents/.acr-vendor-3158729610"
		if err := os.CopyFS(filepath.Join(root, staged), os.DirFS(filepath.Join(root, ".tessl/plugins/zeta/bravo"))); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, staged), 0o700); err != nil {
			t.Fatal(err)
		}
		killVendorMigration(t, root, vendorRoot+"charlie")
		remove(t, root, staged)
		retryVendorMigration(t, root)
	})
}

func TestVendorRetirementEditConverges(t *testing.T) {
	// The rename hook fires after bravo's promotion, before alpha has moved, and
	// again immediately after alpha has moved to its preserved location.
	for _, point := range []string{vendorRoot + "bravo", vendorRoot + "alpha"} {
		t.Run("during apply at "+point, func(t *testing.T) {
			root := supersededVendorFixture(t)
			before, err := os.ReadFile(filepath.Join(root, ".agents/registry.lock"))
			if err != nil {
				t.Fatal(err)
			}
			edited := false
			realize.OnVendorMigrationRename(t, func(target string) {
				if target != point || edited {
					return
				}
				edited = true
				name := alphaRule
				if point == vendorRoot+"alpha" {
					name = preservedVendorTree(t, root, "vendor-000002") + "/rules/rule-alpha.md"
				}
				writeVendorFixture(t, root, name, operatorEdit, 0o644)
			})
			_, err = runMixedVendorMigration(t, root)
			var incomplete *realize.IncompleteRecoveryError
			if !edited || err == nil || errors.As(err, &incomplete) {
				t.Fatalf("apply with edited retirement: edited=%t, error=%v", edited, err)
			}
			assertVendorContents(t, root, alphaRule, operatorEdit)
			assertVendorContents(t, root, ".agents/registry.lock", string(before))
			assertVendorPathExists(t, root, vendorRoot+"bravo", false)
			assertVendorPathExists(t, root, transactionRoot, false)
			assertEditedRetirementConverges(t, root)
		})
		t.Run("after SIGKILL at "+point, func(t *testing.T) {
			root := supersededVendorFixture(t)
			killVendorMigration(t, root, "mixed:"+point)
			name := alphaRule
			if point == vendorRoot+"alpha" {
				name = preservedVendorTree(t, root, "vendor-000002") + "/rules/rule-alpha.md"
			}
			writeVendorFixture(t, root, name, operatorEdit, 0o644)
			assertEditedRetirementConverges(t, root)
		})
	}
	t.Run("edited tree moved aside before recovery", func(t *testing.T) {
		root := supersededVendorFixture(t)
		killVendorMigration(t, root, "mixed:"+vendorRoot+"bravo")
		writeVendorFixture(t, root, alphaRule, operatorEdit, 0o644)
		if err := os.Rename(filepath.Join(root, vendorRoot+"alpha"), filepath.Join(root, "aside")); err != nil {
			t.Fatal(err)
		}
		// The lock still declares alpha, so absence is not its before-state.
		assertVendorRecoveryConflict(t, root)
		if err := os.Rename(filepath.Join(root, "aside"), filepath.Join(root, vendorRoot+"alpha")); err != nil {
			t.Fatal(err)
		}
		assertEditedRetirementConverges(t, root)
	})
	t.Run("workspace deleted after retirement moved", func(t *testing.T) {
		root := supersededVendorFixture(t)
		killVendorMigration(t, root, "mixed:"+vendorRoot+"alpha")
		if err := os.RemoveAll(filepath.Join(root, vendorRoot)); err != nil {
			t.Fatal(err)
		}
		if err := realize.RecoverTransactions(root); err != nil {
			t.Fatal(err)
		}
		assertVendorPathExists(t, root, vendorRoot+"alpha", true)
		if report, err := runMixedVendorMigration(t, root); err != nil || !report.Wrote {
			t.Fatalf("retry wrote=%t, error=%v", report.Wrote, err)
		}
		assertMixedVendorMigrationConverged(t, root)
	})
}

// A retry refuses to retire the edit. Moving it aside and restoring the locked
// bytes, at any inode, then lets the migration finish.
func assertEditedRetirementConverges(t *testing.T, root string) {
	t.Helper()
	_, err := runMixedVendorMigration(t, root)
	if err == nil || !strings.Contains(err.Error(), "content hash mismatch for vendor:zeta/alpha") {
		t.Fatalf("retry over edited superseded tree: %v", err)
	}
	assertVendorContents(t, root, alphaRule, operatorEdit)
	assertVendorPathExists(t, root, transactionRoot, false)
	if err := os.Rename(filepath.Join(root, vendorRoot+"alpha"), filepath.Join(root, "kept")); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(root, vendorRoot+"alpha"), os.DirFS(filepath.Join(root, "kept"))); err != nil {
		t.Fatal(err)
	}
	writeVendorFixture(t, root, alphaRule, "Rule for alpha.\n", 0o644)
	if report, err := runMixedVendorMigration(t, root); err != nil || !report.Wrote {
		t.Fatalf("retry after reconciliation wrote=%t, error=%v", report.Wrote, err)
	}
	assertMixedVendorMigrationConverged(t, root)
	assertVendorContents(t, root, "kept/rules/rule-alpha.md", operatorEdit)
}

func assertMixedVendorMigrationConverged(t *testing.T, root string) {
	t.Helper()
	state, err := dependency.LoadState(root)
	if err != nil || len(state.Lock.Dependencies) != len(vendorPackages) || state.Lock.Dependencies[0].Source != "github:zeta/alpha" {
		t.Fatalf("mixed migration lock = %v, %v", state.Lock.Dependencies, err)
	}
	assertVendorPathExists(t, root, vendorRoot+"alpha", false)
	for index, name := range []string{"bravo", "charlie"} {
		locked := state.Lock.Dependencies[index+1]
		hash, err := dependency.HashVendorTree(filepath.Join(root, vendorRoot+name))
		if err != nil || locked.Source != "vendor:zeta/"+name || locked.ContentHash != hash {
			t.Fatalf("vendor %s lock %v does not match live hash %q: %v", name, locked, hash, err)
		}
	}
	if report, err := runMixedVendorMigration(t, root); err != nil || report.Wrote {
		t.Fatalf("converged retry wrote=%t, error=%v", report.Wrote, err)
	}
	assertVendorPathExists(t, root, transactionRoot, false)
}

func retryVendorMigration(t *testing.T, root string) {
	t.Helper()
	if report, err := runVendorMigration(root, false); err != nil || !report.Wrote {
		t.Fatalf("retry wrote=%t, error=%v", report.Wrote, err)
	}
	assertVendorMigrationConverged(t, root)
}

func assertVendorRecoveryConflict(t *testing.T, root string) {
	t.Helper()
	var conflict *realize.RecoveryConflictError
	if err := realize.RecoverTransactions(root); !errors.As(err, &conflict) {
		t.Fatalf("recovery = %v, want recovery conflict", err)
	}
	assertVendorPathExists(t, root, transactionRoot, true)
}

func preservedVendorTree(t *testing.T, root, slot string) string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(root, ".agents/.acr-recovered-vendors-*", slot))
	if err != nil || len(found) != 1 {
		t.Fatalf("preserved vendor tree = %v, %v", found, err)
	}
	relative, err := filepath.Rel(root, found[0])
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(relative)
}
