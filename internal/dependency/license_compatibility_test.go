package dependency

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Frozen using candidate 1894b7d6036a6f2e661c394ffdeb6fdc4c0bb045's v1
// hasher with an undeclared root LICENSE in the source archive. Do not derive
// this value using the current collector: it guards already-published locks.
const legacyLicenseHash = "sha256:9ad9979f91a1f15f020e7a47af71f1f4900f8424a3d49dca68f65f603060c351"

func legacyLicenseFiles() map[string]string {
	return map[string]string{
		"agent-plugin.yaml": "schemaVersion: 1\nname: owner/plugin\nversion: 1.2.3\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  rules:\n    - id: guidance\n      path: rules/guidance.md\n      activation:\n        mode: always\n",
		"rules/guidance.md": "Use deterministic tests.\n",
		"LICENSE":           "Legacy undeclared notice.\n",
	}
}

func TestFrozenV1HashWithUndeclaredRootLicense(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"source-tree", "old-archive", "new-archive", "changed-notice", "additional-notice"} {
		t.Run(variant, func(t *testing.T) {
			files := legacyLicenseFiles()
			switch variant {
			case "old-archive":
				delete(files, "LICENSE")
			case "changed-notice":
				files["LICENSE"] = "Changed notice bytes.\r\n"
			case "additional-notice":
				files["COPYING.txt"] = "Additional notice.\n"
			}
			verified, err := verifyPackageArchive(testArchive(t, "plugin-1.2.3", files), Repository{Owner: "owner", Name: "plugin"})
			if err != nil || verified.ContentHash != legacyLicenseHash {
				t.Fatalf("%s hash = %s, %v; want frozen %s", variant, verified.ContentHash, err, legacyLicenseHash)
			}
		})
	}
}

func TestLegacyLicenseLockAndReleaseMetadataRemainUsable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	commit := strings.Repeat("a", 40)
	locked := LockedDependency{Source: "github:owner/plugin", Requested: "v1.2.3", Kind: ResolutionRelease, ReleaseID: 5, Tag: "v1.2.3", Commit: commit, PackageVersion: "1.2.3", ContentHash: legacyLicenseHash}
	// Historical metadata and archive omit LICENSE even though the locked
	// source tree contains it. Consumers download the full source tree.
	legacy := legacyLicenseFiles()
	delete(legacy, "LICENSE")
	metadata := []byte(fmt.Sprintf(`{"metadataVersion":1,"commit":%q,"contentHash":%q,"files":[{"path":"agent-plugin.yaml"},{"path":"rules/guidance.md"}]}`, commit, legacyLicenseHash))
	remote := &fakeGitHub{
		releases: map[string]Release{"v1.2.3": {ID: 5, Tag: "v1.2.3", Assets: []ReleaseAsset{{ID: 9, Name: "acr-package.json"}, {ID: 10, Name: "plugin-1.2.3.tar.gz"}}}},
		commits:  map[string]string{"v1.2.3": commit},
		archives: map[string][]byte{commit: testArchive(t, "source-tree", legacyLicenseFiles())},
		assets:   map[int64][]byte{9: metadata, 10: testArchive(t, "plugin-1.2.3", legacy)},
	}
	resolver := NewResolver(remote)
	if err := resolver.VerifyLocked(ctx, locked); err != nil {
		t.Fatalf("VerifyLocked legacy lock: %v", err)
	}
	materialized, cleanup, err := resolver.MaterializeLocked(ctx, locked)
	if err != nil {
		t.Fatalf("MaterializeLocked legacy lock: %v", err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	}()
	if got, err := os.ReadFile(filepath.Join(materialized.Root, "LICENSE")); err != nil || string(got) != legacyLicenseFiles()["LICENSE"] {
		t.Fatalf("materialized notice = %q, %v", got, err)
	}
	fresh, err := resolver.Resolve(ctx, Declaration{Source: locked.Source, Requested: locked.Tag})
	if err != nil || fresh.ContentHash != legacyLicenseHash || remote.assetCalls != 1 {
		t.Fatalf("fresh release resolution = %#v, %v; metadata reads %d", fresh, err, remote.assetCalls)
	}
	changed := legacyLicenseFiles()
	changed["rules/guidance.md"] = "Changed declared content.\n"
	remote.archives[commit] = testArchive(t, "source-tree", changed)
	if err := resolver.VerifyLocked(ctx, locked); err == nil || !strings.Contains(err.Error(), "content hash mismatch") {
		t.Fatalf("declared change accepted: %v", err)
	}
	if _, err := resolver.Resolve(ctx, Declaration{Source: locked.Source, Requested: locked.Tag}); err == nil || !strings.Contains(err.Error(), "metadata content hash mismatch") {
		t.Fatalf("declared metadata mismatch accepted: %v", err)
	}
}

func TestExplicitLicenseStillChangesContentHash(t *testing.T) {
	files := legacyLicenseFiles()
	files["agent-plugin.yaml"] = strings.ReplaceAll(files["agent-plugin.yaml"], "rules/guidance.md", "LICENSE")
	before, err := verifyPackageArchive(testArchive(t, "root", files), Repository{Owner: "owner", Name: "plugin"})
	if err != nil {
		t.Fatal(err)
	}
	changed := maps.Clone(files)
	changed["LICENSE"] = "Changed explicit notice.\n"
	after, err := verifyPackageArchive(testArchive(t, "root", changed), Repository{Owner: "owner", Name: "plugin"})
	if err != nil || before.ContentHash == after.ContentHash {
		t.Fatalf("explicit license not hashed: %s, %s, %v", before.ContentHash, after.ContentHash, err)
	}
	commit := strings.Repeat("b", 40)
	remote := &fakeGitHub{archives: map[string][]byte{commit: testArchive(t, "root", changed)}}
	locked := LockedDependency{Source: "github:owner/plugin", Kind: ResolutionCommit, Commit: commit, PackageVersion: "1.2.3", ContentHash: before.ContentHash}
	if err := NewResolver(remote).VerifyLocked(context.Background(), locked); err == nil || !strings.Contains(err.Error(), "content hash mismatch") {
		t.Fatalf("explicit notice mismatch accepted: %v", err)
	}
}
