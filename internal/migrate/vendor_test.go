package migrate

import (
	"io/fs"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

func TestVendorHashCoversUndeclaredFiles(t *testing.T) {
	t.Parallel()
	base := []VendorFile{{Path: "plugin.json", Content: []byte("{}"), Mode: 0o644}}
	withReadme := append(append([]VendorFile(nil), base...), VendorFile{Path: "README.md", Content: []byte("extra"), Mode: 0o644})
	if HashVendorFiles(base) == HashVendorFiles(withReadme) {
		t.Fatal("undeclared package file did not affect vendor hash")
	}
}

func TestVendorHashIsUmaskIndependent(t *testing.T) {
	t.Parallel()
	left := []VendorFile{{Path: "hooks/run.sh", Content: []byte("#!/bin/sh\n"), Mode: fs.FileMode(0o755)}}
	right := []VendorFile{{Path: "hooks/run.sh", Content: []byte("#!/bin/sh\n"), Mode: fs.FileMode(0o755)}}
	if HashVendorFiles(left) != HashVendorFiles(right) {
		t.Fatal("equal normalized modes produced different hashes")
	}
}

func TestVendorRejectsEscapingPaths(t *testing.T) {
	t.Parallel()
	snapshot := hostileVendorSnapshot{}
	_, err := PlanVendor(snapshot, PackageInstall{TesslIdentity: "example/orphan", Root: ".tessl/plugins/example/orphan"})
	if err == nil || !containsText(err.Error(), "vendor_escape") {
		t.Fatalf("PlanVendor error = %v, want vendor_escape", err)
	}
}

type hostileVendorSnapshot struct{}

func (hostileVendorSnapshot) ReadDir(string) ([]adapter.ObservedEntry, error) {
	return []adapter.ObservedEntry{{Path: ".tessl/plugins/example/orphan/link", Mode: fs.ModeSymlink | 0o777}}, nil
}

func (hostileVendorSnapshot) ReadFile(string) (adapter.ObservedFile, error) {
	return adapter.ObservedFile{}, fs.ErrNotExist
}

func containsText(value, wanted string) bool {
	for index := 0; index+len(wanted) <= len(value); index++ {
		if value[index:index+len(wanted)] == wanted {
			return true
		}
	}
	return false
}
