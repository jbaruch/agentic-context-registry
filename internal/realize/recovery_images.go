package realize

import (
	"errors"
	"fmt"
	"os"
	"path"
)

// RecoveryBeforeImages reads selected project-file before-images without
// claiming, cleaning, or recovering a journal. A present nil value means the
// file was absent. Callers can check trust requirements in the state recovery
// would restore before allowing any project mutation.
func RecoveryBeforeImages(projectDirectory string, filenames ...string) (images map[string][]byte, err error) {
	_, err = inspectTransactions(projectDirectory)
	var pending *PendingTransactionError
	if err == nil {
		return nil, nil
	}
	if !errors.As(err, &pending) {
		return nil, err
	}
	manifest, err := loadJournal(projectDirectory, pending.ID)
	if err != nil {
		return nil, err
	}
	projectRoot, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, projectRoot.Close()) }()
	journalPath := path.Join(transactionDirectory, pending.ID)
	if err := ValidateParentDirectories(projectRoot, path.Join(journalPath, journalManifestFilename)); err != nil {
		return nil, err
	}
	journalRoot, err := projectRoot.OpenRoot(journalPath)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, journalRoot.Close()) }()
	wanted := make(map[string]bool, len(filenames))
	for _, name := range filenames {
		wanted[name] = true
	}
	images = make(map[string][]byte)
	for _, entry := range manifest.Entries {
		if !wanted[entry.Path] || entry.GitExclusion {
			continue
		}
		if _, duplicate := images[entry.Path]; duplicate {
			return nil, &RecoveryConflictError{ID: pending.ID, Detail: "duplicate state-file entry"}
		}
		current, err := snapshotJournalFile(projectRoot, entry.Path)
		if err != nil {
			return nil, err
		}
		matchesBefore := current.exists == entry.BeforeExists && (!current.exists || current.hash == entry.BeforeHash && uint32(current.mode.Perm()) == entry.BeforeMode && current.symlinkTarget == entry.SymlinkTarget)
		matchesAfter := current.exists == entry.AfterExists && (!current.exists || current.hash == entry.AfterHash && uint32(current.mode.Perm()) == entry.AfterMode && current.symlinkTarget == "")
		if !matchesBefore && !matchesAfter {
			return nil, &RecoveryConflictError{ID: pending.ID, Detail: fmt.Sprintf("%s matches neither journal before-state nor after-state", entry.Path)}
		}
		if !entry.BeforeExists {
			images[entry.Path] = nil
			continue
		}
		before, err := snapshotFile(journalRoot, entry.BeforeImage)
		if err != nil {
			return nil, err
		}
		if !before.exists || int64(len(before.content)) != entry.BeforeSize || before.hash != entry.BeforeHash {
			return nil, &RecoveryConflictError{ID: pending.ID, Detail: fmt.Sprintf("before-image for %s is missing or corrupt", entry.Path)}
		}
		images[entry.Path] = before.content
	}
	return images, nil
}
