package dependency

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerify8VendorDriftIsRefusedInEveryShape drives the locked content hash
// against the four ways a vendored tree can drift on disk. Only an edited file
// is exercised elsewhere; a tree that gained a file, lost one, or had a mode
// flipped is equally not the package the lock describes, and materializing it
// would realize content nobody recorded.
func TestVerify8VendorDriftIsRefusedInEveryShape(t *testing.T) {
	t.Parallel()
	for name, drift := range map[string]func(*testing.T, string){
		"edited file": func(t *testing.T, packageRoot string) {
			if err := os.WriteFile(filepath.Join(packageRoot, "rules/always.md"), []byte("Tampered.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"added file": func(t *testing.T, packageRoot string) {
			if err := os.WriteFile(filepath.Join(packageRoot, "rules/extra.md"), []byte("Extra.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"removed file": func(t *testing.T, packageRoot string) {
			if err := os.Remove(filepath.Join(packageRoot, "rules/always.md")); err != nil {
				t.Fatal(err)
			}
		},
		"changed mode": func(t *testing.T, packageRoot string) {
			if err := os.Chmod(filepath.Join(packageRoot, "rules/always.md"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root, locked := writeVendorFixture(t)
			packageRoot := filepath.Join(root, ".agents/vendor/example/orphan")
			drift(t, packageRoot)

			_, _, err := NewResolver(vendorPanicGitHub{}).MaterializeLockedAt(context.Background(), root, locked)
			if err == nil {
				t.Fatalf("%s materialized against a stale lock", name)
			}
			for _, want := range []string{
				"content hash mismatch for vendor:example/orphan",
				locked.ContentHash,
				"restore the vendored tree from version control",
				"acr migrate tessl --vendor-unmapped",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("%s refusal = %q, want %q", name, err, want)
				}
			}
		})
	}
}

// TestVerify8VendorTreeRefusesSymlinksInsideThePackage covers the read half of
// the escape contract. A vendored tree is copied from a Tessl install and then
// lives in the consumer's repository, where anyone can drop a link into it
// between one realization and the next; following one would let a package
// realize bytes from outside its own directory.
func TestVerify8VendorTreeRefusesSymlinksInsideThePackage(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	target := filepath.Join(outside, "elsewhere.md")
	if err := os.WriteFile(target, []byte("Not part of the package.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, link := range map[string]string{
		"escaping absolute link": target,
		"in-tree relative link":  "always.md",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root, locked := writeVendorFixture(t)
			packageRoot := filepath.Join(root, ".agents/vendor/example/orphan")
			if err := os.Symlink(link, filepath.Join(packageRoot, "rules/linked.md")); err != nil {
				t.Fatal(err)
			}
			_, _, err := NewResolver(vendorPanicGitHub{}).MaterializeLockedAt(context.Background(), root, locked)
			if err == nil {
				t.Fatalf("%s materialized", name)
			}
			if !strings.Contains(err.Error(), "vendor_escape") {
				t.Fatalf("%s refusal = %q, want vendor_escape", name, err)
			}
		})
	}
}

// TestVerify8VendorRootMustBeARealDirectory pins the package root itself. A
// symlinked or missing .agents/vendor/<ws>/<pkg> is the cheapest way to point a
// declared vendor dependency somewhere else entirely, so it must refuse rather
// than resolve.
func TestVerify8VendorRootMustBeARealDirectory(t *testing.T) {
	t.Parallel()
	root, locked := writeVendorFixture(t)
	packageRoot := filepath.Join(root, ".agents/vendor/example/orphan")
	elsewhere := filepath.Join(root, "elsewhere")
	if err := os.Rename(packageRoot, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, packageRoot); err != nil {
		t.Fatal(err)
	}
	_, _, err := NewResolver(vendorPanicGitHub{}).MaterializeLockedAt(context.Background(), root, locked)
	if err == nil {
		t.Fatal("a symlinked vendor package root materialized")
	}
	if !strings.Contains(err.Error(), "vendor_escape") {
		t.Fatalf("refusal = %q, want vendor_escape", err)
	}
}
