package realize

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// verifyAndSyncOwnerRollback derives its coverage from the journal rather
// than this invocation's writes. A retry can see a rename or unlink that
// survived in memory even though its directory was never synced.
func verifyAndSyncOwnerRollback(root *os.Root, journalName string, manifest journalManifest) error {
	dirs := map[string]bool{}
	addParents := func(target string) {
		for dir := path.Dir(target); !dirs[dir]; dir = path.Dir(dir) {
			dirs[dir] = true
		}
	}
	journal := path.Join(transactionDirectory, journalName)
	for index, entry := range manifest.Entries {
		if entry.GitExclusion {
			continue // The shared claim handles its own before-state.
		}
		addParents(entry.Path)
		switch entry.Operation {
		case vendorPromotion:
			preserved := recoveredVendorPath(manifest.ID, index)
			stage := path.Join(journal, vendorStageName(index))
			addParents(preserved)
			addParents(stage)
			location, err := vendorTreeLocation(root, preserved, stage, entry.Path)
			if err != nil {
				return err
			}
			switch location {
			case "", preserved: // Preserved trees may be edited, and live occupants can be foreign.
			case stage:
				if err := requireVendorTree(root, stage, entry.AfterHash); err != nil {
					return err
				}
			default:
				return fmt.Errorf("vendor promotion %q has not been rolled back", entry.Path)
			}
		case vendorRetirement:
			preserved := recoveredVendorPath(manifest.ID, index)
			addParents(preserved)
			location, err := vendorTreeLocation(root, preserved, entry.Path)
			if err != nil {
				return err
			}
			// As in rollback, edits travel with the tree. Legacy content can
			// also be missing because the restored state declares only the package.
			if location != entry.Path && !(location == "" && isLegacyVendorPath(entry.Path)) {
				return fmt.Errorf("recorded vendor tree %q is not restored", entry.Path)
			}
		default:
			if entry.RemovedImage != "" {
				addParents(path.Join(journal, entry.RemovedImage))
			}
			current, err := snapshotJournalFile(root, entry.Path)
			if err != nil {
				return err
			}
			if current.exists != entry.BeforeExists || current.exists &&
				(current.hash != entry.BeforeHash || int64(len(current.content)) != entry.BeforeSize ||
					uint32(current.mode.Perm()) != entry.BeforeMode || current.symlinkTarget != entry.SymlinkTarget) {
				return fmt.Errorf("%q does not match the journal before-state", entry.Path)
			}
			if current.exists && current.symlinkTarget == "" {
				if err := syncMatchedFile(root, entry.Path, current.mode, current.generation); err != nil {
					return err
				}
			}
		}
	}
	for _, dir := range manifest.Directories {
		if !dir.GitExclusion {
			addParents(dir.Path)
		}
	}
	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	depth := func(dir string) int {
		if dir == "." {
			return -1
		}
		return strings.Count(dir, "/")
	}
	slices.SortFunc(ordered, func(a, b string) int {
		if difference := depth(b) - depth(a); difference != 0 {
			return difference
		}
		return strings.Compare(a, b)
	})
	for _, dir := range ordered {
		if err := ValidateParentDirectories(root, dir); err != nil {
			return err
		}
		info, err := root.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue // Its surviving ancestors still need syncing.
		}
		if err != nil {
			return fmt.Errorf("inspect rollback directory %q: %w", dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("rollback directory %q is no longer a directory", dir)
		}
		if err := journalDirectorySync(filepath.Join(root.Name(), filepath.FromSlash(dir))); err != nil {
			return fmt.Errorf("sync rollback directory %q: %w", dir, err)
		}
	}
	return nil
}
