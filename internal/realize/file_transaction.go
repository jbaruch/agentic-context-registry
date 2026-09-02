package realize

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// FileTransactionEdit is a fingerprint-bound whole-file mutation used when a
// service must journal files that are not adapter targets. Operation is
// "splice" or "remove". A symlink removal records its target in LinkTarget;
// Before then remains empty.
type FileTransactionEdit struct {
	Path       string
	Operation  string
	Before     []byte
	After      []byte
	BeforeMode uint32
	AfterMode  uint32
	LinkTarget string
}

var fileTransactionRename = func(root *os.Root, oldname, newname string) error {
	return root.Rename(oldname, newname)
}

// FileTransactionHooks exposes deterministic boundaries to internal callers
// that need to verify precondition and interruption behavior.
type FileTransactionHooks struct {
	BeforeEdit func(int, FileTransactionEdit) error
	AfterEdit  func(int, FileTransactionEdit) error
}

// ApplyFileTransaction applies precomputed edits through the shared durable
// journal. All before-images are synced before the first live path changes.
func ApplyFileTransaction(projectDirectory string, edits []FileTransactionEdit) (err error) {
	return ApplyFileTransactionWithFinalizer(projectDirectory, edits, nil)
}

// ApplyFileTransactionWithFinalizer runs a final filesystem cleanup before
// committing the journal. If it fails, every file edit is recovered first.
func ApplyFileTransactionWithFinalizer(projectDirectory string, edits []FileTransactionEdit, finalize func() error) (err error) {
	return ApplyFileTransactionWithHooks(projectDirectory, edits, finalize, FileTransactionHooks{})
}

// ApplyFileTransactionWithHooks applies a transaction with deterministic
// callbacks around each live edit. Callbacks are part of the internal test and
// composition boundary; returned errors use the normal journal recovery path.
func ApplyFileTransactionWithHooks(projectDirectory string, edits []FileTransactionEdit, finalize func() error, hooks FileTransactionHooks) (err error) {
	claim, err := claimTransactions(projectDirectory)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, claim.Close()) }()
	if err := cleanStagingTransactions(projectDirectory); err != nil {
		return err
	}
	if err := recoverPendingTransaction(projectDirectory); err != nil {
		return err
	}
	root, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return fmt.Errorf("open project directory %q: %w", projectDirectory, err)
	}
	defer root.Close()
	for index := range edits {
		edit := &edits[index]
		if err := validateFileTransactionEdit(*edit); err != nil {
			return err
		}
		current, err := snapshotJournalFile(root, edit.Path)
		if err != nil {
			return &FileTransactionConflictError{Path: edit.Path, Err: err}
		}
		if edit.LinkTarget != "" && edit.BeforeMode == 0 {
			edit.BeforeMode = uint32(current.mode.Perm())
		}
		if !matchesFileTransactionBefore(*edit, current) {
			return &FileTransactionConflictError{Path: edit.Path}
		}
	}
	if len(edits) == 0 {
		return nil
	}
	_, journalDir, err := createFileTransactionJournal(projectDirectory, edits)
	if err != nil {
		return err
	}
	// Recheck the complete fingerprint after the staging barrier and before the
	// first rename/write.
	for _, edit := range edits {
		current, err := snapshotJournalFile(root, edit.Path)
		if err != nil || !matchesFileTransactionBefore(edit, current) {
			return recoverApplyFailure(projectDirectory, journalDir, &FileTransactionConflictError{Path: edit.Path, Err: err})
		}
	}
	journalRelative, err := filepath.Rel(projectDirectory, journalDir)
	if err != nil {
		return recoverApplyFailure(projectDirectory, journalDir, err)
	}
	journalRelative = filepath.ToSlash(journalRelative)
	for index, edit := range edits {
		if hooks.BeforeEdit != nil {
			if err := hooks.BeforeEdit(index, edit); err != nil {
				return recoverApplyFailure(projectDirectory, journalDir, err)
			}
		}
		current, err := snapshotJournalFile(root, edit.Path)
		if err != nil || !matchesFileTransactionBefore(edit, current) {
			return recoverApplyFailure(projectDirectory, journalDir, &FileTransactionConflictError{Path: edit.Path, Err: err})
		}
		switch edit.Operation {
		case "remove":
			removed := path.Join(journalRelative, "removed", fmt.Sprintf("%06d", index))
			if err := root.MkdirAll(path.Dir(removed), 0o700); err != nil {
				return recoverApplyFailure(projectDirectory, journalDir, err)
			}
			err := fileTransactionRename(root, edit.Path, removed)
			if errors.Is(err, syscall.EXDEV) {
				if verifyErr := verifyFileTransactionBeforeImage(journalDir, index, edit); verifyErr != nil {
					return recoverApplyFailure(projectDirectory, journalDir, verifyErr)
				}
				err = root.Remove(edit.Path)
			}
			if err != nil {
				return recoverApplyFailure(projectDirectory, journalDir, fmt.Errorf("remove %s: %w", edit.Path, err))
			}
			if err := syncDirectory(filepath.Join(journalDir, "removed")); err != nil {
				return recoverApplyFailure(projectDirectory, journalDir, err)
			}
		case "splice":
			if err := writeFileAtomic(root, edit.Path, edit.After, fs.FileMode(edit.AfterMode)); err != nil {
				return recoverApplyFailure(projectDirectory, journalDir, err)
			}
		}
		parent := filepath.Join(projectDirectory, filepath.FromSlash(path.Dir(edit.Path)))
		if err := syncDirectory(parent); err != nil {
			return recoverApplyFailure(projectDirectory, journalDir, err)
		}
		transactionRenameHook(edit.Path)
		if hooks.AfterEdit != nil {
			if err := hooks.AfterEdit(index, edit); err != nil {
				return recoverApplyFailure(projectDirectory, journalDir, err)
			}
		}
	}
	if finalize != nil {
		if err := finalize(); err != nil {
			return recoverApplyFailure(projectDirectory, journalDir, err)
		}
	}
	if err := retireJournal(journalDir); err != nil {
		return fmt.Errorf("remove completed transaction journal: %w", err)
	}
	return nil
}

// FileTransactionConflictError means a target no longer matches the
// fingerprint captured by the caller.
type FileTransactionConflictError struct {
	Path string
	Err  error
}

func (err *FileTransactionConflictError) Error() string {
	if err.Err != nil {
		return fmt.Sprintf("finalization target %s changed after inventory: %v", err.Path, err.Err)
	}
	return fmt.Sprintf("finalization target %s changed after inventory", err.Path)
}

func (err *FileTransactionConflictError) Unwrap() error { return err.Err }

func validateFileTransactionEdit(edit FileTransactionEdit) error {
	if err := validateFileTransactionPath(edit.Path); err != nil {
		return err
	}
	if edit.Operation != "remove" && edit.Operation != "splice" {
		return fmt.Errorf("unsupported file transaction operation %q", edit.Operation)
	}
	if edit.BeforeMode == 0 && edit.LinkTarget == "" {
		return fmt.Errorf("file transaction target %q has no before mode", edit.Path)
	}
	if edit.Operation == "splice" {
		if edit.LinkTarget != "" {
			return fmt.Errorf("file transaction cannot splice symbolic link %q", edit.Path)
		}
		if edit.AfterMode == 0 {
			return fmt.Errorf("file transaction target %q has no after mode", edit.Path)
		}
	}
	return nil
}

func validateFileTransactionPath(filename string) error {
	if err := validateRelativePath(filename); err != nil {
		return err
	}
	if filename == ".git" || strings.HasPrefix(filename, ".git/") || filename == transactionDirectory || strings.HasPrefix(filename, transactionDirectory+"/") || filename == ".agents/vendor" || strings.HasPrefix(filename, ".agents/vendor/") {
		return fmt.Errorf("reserved path %q cannot be changed by a file transaction", filename)
	}
	return nil
}

func matchesFileTransactionBefore(edit FileTransactionEdit, current journalFileSnapshot) bool {
	if !current.exists || current.hash != fileTransactionBeforeHash(edit) || uint32(current.mode.Perm()) != edit.BeforeMode {
		return false
	}
	return current.symlinkTarget == edit.LinkTarget
}

func fileTransactionBeforeHash(edit FileTransactionEdit) string {
	if edit.LinkTarget != "" {
		return contentHash([]byte(edit.LinkTarget))
	}
	return contentHash(edit.Before)
}

func createFileTransactionJournal(projectDirectory string, edits []FileTransactionEdit) (string, string, error) {
	id, err := transactionID()
	if err != nil {
		return "", "", err
	}
	if id == "" || strings.Contains(id, "/") || strings.HasPrefix(id, ".") {
		return "", "", fmt.Errorf("invalid generated transaction ID %q", id)
	}
	parent := filepath.Join(projectDirectory, filepath.FromSlash(transactionDirectory))
	staging := filepath.Join(parent, ".staging-"+id)
	canonical := filepath.Join(parent, id)
	if err := os.Mkdir(staging, 0o700); err != nil {
		return "", "", fmt.Errorf("create transaction staging: %w", err)
	}
	removeStaging := true
	defer func() {
		if removeStaging {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := os.Mkdir(filepath.Join(staging, "before"), 0o700); err != nil {
		return "", "", err
	}
	hasRemoval := false
	for _, edit := range edits {
		if edit.Operation == "remove" {
			hasRemoval = true
			break
		}
	}
	if hasRemoval {
		if err := os.Mkdir(filepath.Join(staging, "removed"), 0o700); err != nil {
			return "", "", err
		}
	}
	manifest := journalManifest{SchemaVersion: journalSchemaVersion, ID: id, Entries: make([]journalEntry, 0, len(edits))}
	for index, edit := range edits {
		before := append([]byte(nil), edit.Before...)
		if edit.LinkTarget != "" {
			before = []byte(edit.LinkTarget)
		}
		beforeImage := fmt.Sprintf("before/%06d", index)
		if err := journalFileWriter(filepath.Join(staging, filepath.FromSlash(beforeImage)), before, 0o600); err != nil {
			return "", "", err
		}
		entry := journalEntry{
			Path: edit.Path, Operation: edit.Operation, BeforeExists: true,
			BeforeHash: contentHash(before), BeforeSize: int64(len(before)), BeforeMode: edit.BeforeMode,
			SymlinkTarget: edit.LinkTarget, BeforeImage: beforeImage,
			AfterExists: edit.Operation == "splice", AfterMode: edit.AfterMode,
		}
		if edit.Operation == "splice" {
			entry.AfterHash = contentHash(edit.After)
		} else {
			entry.RemovedImage = fmt.Sprintf("removed/%06d", index)
		}
		manifest.Entries = append(manifest.Entries, entry)
	}
	encoded, err := marshalJournal(manifest)
	if err != nil {
		return "", "", err
	}
	if err := journalFileWriter(filepath.Join(staging, journalManifestFilename), encoded, 0o600); err != nil {
		return "", "", err
	}
	if err := journalDirectorySync(filepath.Join(staging, "before")); err != nil {
		return "", "", err
	}
	if hasRemoval {
		if err := journalDirectorySync(filepath.Join(staging, "removed")); err != nil {
			return "", "", err
		}
	}
	if err := journalDirectorySync(staging); err != nil {
		return "", "", err
	}
	if err := journalRename(staging, canonical); err != nil {
		return "", "", fmt.Errorf("commit transaction journal: %w", err)
	}
	removeStaging = false
	if err := journalDirectorySync(parent); err != nil {
		return "", "", err
	}
	return id, canonical, nil
}

func verifyFileTransactionBeforeImage(journalDir string, index int, edit FileTransactionEdit) error {
	filename := filepath.Join(journalDir, "before", fmt.Sprintf("%06d", index))
	content, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	want := edit.Before
	if edit.LinkTarget != "" {
		want = []byte(edit.LinkTarget)
	}
	if len(content) != len(want) || contentHash(content) != contentHash(want) {
		return fmt.Errorf("before-image for %s is missing or corrupt", edit.Path)
	}
	return nil
}

type journalFileSnapshot struct {
	exists        bool
	content       []byte
	mode          fs.FileMode
	hash          string
	symlinkTarget string
}

func snapshotJournalFile(root *os.Root, filename string) (journalFileSnapshot, error) {
	if err := ValidateParentDirectories(root, filename); err != nil {
		return journalFileSnapshot{}, err
	}
	info, err := root.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return journalFileSnapshot{}, nil
	}
	if err != nil {
		return journalFileSnapshot{}, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := root.Readlink(filename)
		if err != nil {
			return journalFileSnapshot{}, err
		}
		return journalFileSnapshot{exists: true, content: []byte(target), mode: info.Mode().Perm(), hash: contentHash([]byte(target)), symlinkTarget: target}, nil
	}
	if !info.Mode().IsRegular() {
		return journalFileSnapshot{}, fmt.Errorf("target %q must be a regular file or symbolic link", filename)
	}
	snapshot, err := snapshotFile(root, filename)
	if err != nil {
		return journalFileSnapshot{}, err
	}
	return journalFileSnapshot{exists: snapshot.exists, content: snapshot.content, mode: snapshot.mode, hash: snapshot.hash}, nil
}
