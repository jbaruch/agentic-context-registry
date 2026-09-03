package realize

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVendorTreeRemovalRecoversAfterInterruptedRemoval(t *testing.T) {
	project := t.TempDir()
	root := filepath.Join(project, ".agents", "vendor", "example", "orphan")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]struct {
		content []byte
		mode    os.FileMode
	}{
		"plugin.json":      {content: []byte("manifest\n"), mode: 0o644},
		"nested/hook.sh":   {content: []byte("#!/bin/sh\nexit 0\n"), mode: 0o755},
		"nested/edited.md": {content: []byte("operator changed this vendored file\n"), mode: 0o640},
	}
	for relative, file := range files {
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.WriteFile(filename, file.content, file.mode); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := PlanVendorTreeRemoval(project, ".agents/vendor/example/orphan")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Files != len(files) {
		t.Fatalf("planned files = %d, want %d", plan.Files, len(files))
	}

	setFileTransactionID(t, "tx-vendor-crash")
	originalRename := fileTransactionRename
	t.Cleanup(func() { fileTransactionRename = originalRename })
	removeCount := 0
	fileTransactionRename = func(root *os.Root, oldname, newname string) error {
		if err := originalRename(root, oldname, newname); err != nil {
			return err
		}
		if strings.HasPrefix(oldname, ".agents/vendor/example/orphan/") {
			removeCount++
			if removeCount == 1 {
				panic("simulated process interruption")
			}
		}
		return nil
	}
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("vendor removal returned instead of being interrupted")
			}
		}()
		_ = applyVendorTreeRemovalWithHooks(project, plan, FileTransactionHooks{})
	}()
	fileTransactionRename = originalRename

	journal := filepath.Join(project, filepath.FromSlash(transactionDirectory), "tx-vendor-crash", journalManifestFilename)
	if _, err := os.Stat(journal); err != nil {
		t.Fatalf("interrupted removal left no recovery journal: %v", err)
	}
	if err := RecoverTransactions(project); err != nil {
		t.Fatal(err)
	}
	for relative, file := range files {
		filename := filepath.Join(root, filepath.FromSlash(relative))
		content, err := os.ReadFile(filename)
		if err != nil || !bytes.Equal(content, file.content) {
			t.Fatalf("recovered %s = %q, %v", relative, content, err)
		}
		info, err := os.Stat(filename)
		if err != nil || info.Mode().Perm() != file.mode {
			t.Fatalf("recovered %s mode = %v, %v; want %v", relative, infoMode(info), err, file.mode)
		}
	}
	if _, err := os.Stat(filepath.Dir(journal)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery retained canonical journal: %v", err)
	}
}

func TestVendorTreeRemovalPrunesOnlyEmptyVendorParents(t *testing.T) {
	project := t.TempDir()
	removed := filepath.Join(project, ".agents", "vendor", "example", "orphan")
	sibling := filepath.Join(project, ".agents", "vendor", "example", "kept", "rule.md")
	if err := os.MkdirAll(removed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(removed, "rule.md"), []byte("remove\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(sibling), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanVendorTreeRemoval(project, ".agents/vendor/example/orphan")
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyVendorTreeRemoval(project, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(removed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed vendor tree still exists: %v", err)
	}
	if content, err := os.ReadFile(sibling); err != nil || string(content) != "keep\n" {
		t.Fatalf("sibling vendor file = %q, %v", content, err)
	}
}

func TestVendorTreeRemovalsRecoverTogether(t *testing.T) {
	project := t.TempDir()
	var plans []VendorTreeRemovalPlan
	for _, packageName := range []string{"alpha", "beta"} {
		root := filepath.Join(project, ".agents", "vendor", "example", packageName)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "rule.md"), []byte(packageName+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		plan, err := PlanVendorTreeRemoval(project, ".agents/vendor/example/"+packageName)
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, plan)
	}

	injected := errors.New("injected second-tree removal failure")
	err := applyVendorTreeRemovalsWithHooks(project, plans, FileTransactionHooks{BeforeEdit: func(index int, _ FileTransactionEdit) error {
		if index == 1 {
			return injected
		}
		return nil
	}})
	if !errors.Is(err, injected) {
		t.Fatalf("combined removal error = %v", err)
	}
	for _, packageName := range []string{"alpha", "beta"} {
		filename := filepath.Join(project, ".agents", "vendor", "example", packageName, "rule.md")
		content, readErr := os.ReadFile(filename)
		if readErr != nil || string(content) != packageName+"\n" {
			t.Fatalf("recovered %s = %q, %v", packageName, content, readErr)
		}
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}
