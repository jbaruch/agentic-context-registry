package realize

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const (
	transactionDirectory    = ".agents/.acr-transactions"
	transactionLockPath     = transactionDirectory + "/.lock"
	journalSchemaVersion    = 1
	journalManifestFilename = "manifest.json"
)

var (
	transactionFlock                             = syscall.Flock
	transactionID                                = randomTransactionID
	transactionRenameHook                        = func(string) {}
	journalFileWriter                            = writeSyncedFile
	journalDirectorySync                         = syncDirectory
	journalRename                                = os.Rename
	transactionWriter     operationWriter        = writeOperation
	transactionParents    parentDirectoryCreator = ensureParentDirectories
)

// PendingTransactionError prevents read-only commands from planning against
// a half-applied tree.
type PendingTransactionError struct {
	ID            string
	SchemaVersion int
}

func (err *PendingTransactionError) Error() string {
	return fmt.Sprintf("pending_transaction: transaction %s (schemaVersion %d) requires a mutating command to recover", err.ID, err.SchemaVersion)
}

// RecoveryConflictError means recovery could not prove a safe before/after
// state. The journal and current target are intentionally preserved.
type RecoveryConflictError struct {
	ID     string
	Detail string
}

func (err *RecoveryConflictError) Error() string {
	id := err.ID
	if id == "" {
		id = "<id>"
	}
	journal := path.Join(transactionDirectory, id)
	return fmt.Sprintf("recovery_conflict: %s; reconcile %s and affected targets to either the journal before-state or the journal after-state, then retry", err.Detail, journal)
}

// UnsupportedJournalVersionError fails closed for in-flight formats this
// binary cannot safely interpret.
type UnsupportedJournalVersionError struct{ Version int }

func (err *UnsupportedJournalVersionError) Error() string {
	return fmt.Sprintf("unsupported_journal_version: schemaVersion %d is not supported; run the matching ACR version", err.Version)
}

// TransactionBusyError reports a live mutating process holding the claim.
type TransactionBusyError struct{ Path string }

func (err *TransactionBusyError) Error() string {
	return fmt.Sprintf("transaction_busy: another ACR mutation holds %s", err.Path)
}

// TransactionLockUnavailableError reports a filesystem without working flock.
type TransactionLockUnavailableError struct {
	Path string
	Err  error
}

func (err *TransactionLockUnavailableError) Error() string {
	return fmt.Sprintf("transaction_lock_unavailable: lock %s: %v; use a filesystem mount with working flock", err.Path, err.Err)
}

func (err *TransactionLockUnavailableError) Unwrap() error { return err.Err }

type journalManifest struct {
	SchemaVersion int                `json:"schemaVersion"`
	ID            string             `json:"id"`
	Entries       []journalEntry     `json:"entries"`
	Directories   []journalDirectory `json:"directories,omitempty"`
}

type journalDirectory struct {
	Path         string `json:"path"`
	GitExclusion bool   `json:"gitExclusion,omitempty"`
	PhysicalRoot string `json:"physicalRoot,omitempty"`
	PhysicalPath string `json:"physicalPath,omitempty"`
	Device       uint64 `json:"device"`
	Inode        uint64 `json:"inode"`
}

type journalEntry struct {
	Path         string `json:"path"`
	BeforeExists bool   `json:"beforeExists"`
	BeforeHash   string `json:"beforeHash,omitempty"`
	BeforeSize   int64  `json:"beforeSize,omitempty"`
	BeforeMode   uint32 `json:"beforeMode,omitempty"`
	BeforeImage  string `json:"beforeImage,omitempty"`
	AfterExists  bool   `json:"afterExists"`
	AfterHash    string `json:"afterHash,omitempty"`
	AfterMode    uint32 `json:"afterMode,omitempty"`
	GitExclusion bool   `json:"gitExclusion,omitempty"`
	PhysicalRoot string `json:"physicalRoot,omitempty"`
	PhysicalPath string `json:"physicalPath,omitempty"`
}

type transactionClaim struct {
	file          *os.File
	lockName      string
	txRoot        string
	agentsRoot    string
	lockCreated   bool
	txCreated     bool
	agentsCreated bool
}

// RecoverTransactions acquires the project mutation claim and restores a
// pending journal. Application services call it before loading dependency
// state so a half-written registry.lock never becomes planning input.
func RecoverTransactions(projectDirectory string) (err error) {
	claim, err := claimTransactions(projectDirectory)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, claim.Close()) }()
	if err := cleanStagingTransactions(projectDirectory); err != nil {
		return err
	}
	return recoverPendingTransaction(projectDirectory)
}

func (claim *transactionClaim) Close() error {
	if claim == nil || claim.file == nil {
		return nil
	}
	unlockErr := transactionFlock(int(claim.file.Fd()), syscall.LOCK_UN)
	closeErr := claim.file.Close()
	if claim.agentsCreated {
		cleanupClaimPaths(claim.lockName, claim.txRoot, claim.agentsRoot, claim.lockCreated, claim.txCreated, claim.agentsCreated)
	}
	return errors.Join(unlockErr, closeErr)
}

func claimTransactions(projectDirectory string) (*transactionClaim, error) {
	txRoot := filepath.Join(projectDirectory, filepath.FromSlash(transactionDirectory))
	agentsRoot := filepath.Join(projectDirectory, ".agents")
	_, agentsErr := os.Lstat(agentsRoot)
	agentsCreated := errors.Is(agentsErr, os.ErrNotExist)
	if agentsErr != nil && !agentsCreated {
		return nil, fmt.Errorf("inspect transaction parent .agents: %w", agentsErr)
	}
	if !agentsCreated {
		info, _ := os.Lstat(agentsRoot)
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, errors.New("transaction parent .agents must be a directory, not a symlink or special file")
		}
	} else if err := os.Mkdir(agentsRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create transaction parent .agents: %w", err)
	}
	_, txErr := os.Lstat(txRoot)
	txCreated := errors.Is(txErr, os.ErrNotExist)
	if txErr != nil && !txCreated {
		cleanupClaimPaths("", txRoot, agentsRoot, false, false, agentsCreated)
		return nil, fmt.Errorf("inspect transaction claim directory: %w", txErr)
	}
	if !txCreated {
		info, _ := os.Lstat(txRoot)
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			cleanupClaimPaths("", txRoot, agentsRoot, false, false, agentsCreated)
			return nil, errors.New("transaction claim directory must be a directory, not a symlink or special file")
		}
	} else if err := os.Mkdir(txRoot, 0o700); err != nil {
		cleanupClaimPaths("", txRoot, agentsRoot, false, false, agentsCreated)
		return nil, fmt.Errorf("create transaction claim directory: %w", err)
	}
	lockName := filepath.Join(projectDirectory, filepath.FromSlash(transactionLockPath))
	_, lockErr := os.Lstat(lockName)
	lockCreated := errors.Is(lockErr, os.ErrNotExist)
	if lockErr != nil && !lockCreated {
		cleanupClaimPaths(lockName, txRoot, agentsRoot, false, txCreated, agentsCreated)
		return nil, fmt.Errorf("inspect transaction claim %s: %w", transactionLockPath, lockErr)
	}
	if !lockCreated {
		info, _ := os.Lstat(lockName)
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			cleanupClaimPaths(lockName, txRoot, agentsRoot, false, txCreated, agentsCreated)
			return nil, fmt.Errorf("transaction claim %s must be a regular file, not a symlink or special file", transactionLockPath)
		}
	}
	file, err := os.OpenFile(lockName, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		cleanupClaimPaths(lockName, txRoot, agentsRoot, lockCreated, txCreated, agentsCreated)
		return nil, fmt.Errorf("open transaction claim %s: %w", transactionLockPath, err)
	}
	opened, statErr := file.Stat()
	current, lstatErr := os.Lstat(lockName)
	if statErr != nil || lstatErr != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		file.Close()
		cleanupClaimPaths(lockName, txRoot, agentsRoot, lockCreated, txCreated, agentsCreated)
		return nil, fmt.Errorf("transaction claim %s changed while being opened", transactionLockPath)
	}
	if err := transactionFlock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		cleanupClaimPaths(lockName, txRoot, agentsRoot, lockCreated, txCreated, agentsCreated)
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, &TransactionBusyError{Path: transactionLockPath}
		}
		return nil, &TransactionLockUnavailableError{Path: transactionLockPath, Err: err}
	}
	return &transactionClaim{
		file: file, lockName: lockName, txRoot: txRoot, agentsRoot: agentsRoot,
		lockCreated: lockCreated, txCreated: txCreated, agentsCreated: agentsCreated,
	}, nil
}

func cleanupClaimPaths(lockName, txRoot, agentsRoot string, lockCreated, txCreated, agentsCreated bool) {
	if lockCreated {
		// A failed unlink leaves only inert lock residue and must not replace the claim outcome.
		_ = os.Remove(lockName)
	}
	if txCreated {
		// A nonempty or busy transaction directory must remain and must not replace the claim outcome.
		_ = os.Remove(txRoot)
	}
	if agentsCreated {
		// A nonempty agent-state parent must remain and must not replace the claim outcome.
		_ = os.Remove(agentsRoot)
	}
}

func inspectTransactions(projectDirectory string) ([]TransactionNote, error) {
	agentsRoot := filepath.Join(projectDirectory, ".agents")
	if info, err := os.Lstat(agentsRoot); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("inspect transaction parent .agents: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("transaction parent .agents must be a directory, not a symlink or special file")
	}
	txRoot := filepath.Join(projectDirectory, filepath.FromSlash(transactionDirectory))
	if info, err := os.Lstat(txRoot); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("inspect transaction directory: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("transaction directory must be a directory, not a symlink or special file")
	}
	entries, err := os.ReadDir(txRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect transaction directory: %w", err)
	}
	var notes []TransactionNote
	var pending []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".staging-") {
			notes = append(notes, TransactionNote{Code: "stale_transaction_staging", Path: path.Join(transactionDirectory, entry.Name())})
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		pending = append(pending, entry.Name())
	}
	sort.Strings(pending)
	if len(pending) > 1 {
		return nil, &RecoveryConflictError{Detail: "multiple canonical journals exist: " + strings.Join(pending, ", ")}
	}
	if len(pending) == 1 {
		manifest, err := loadJournal(projectDirectory, pending[0])
		if err != nil {
			return nil, err
		}
		return nil, &PendingTransactionError{ID: pending[0], SchemaVersion: manifest.SchemaVersion}
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].Path < notes[j].Path })
	return notes, nil
}

func cleanStagingTransactions(projectDirectory string) error {
	root := filepath.Join(projectDirectory, filepath.FromSlash(transactionDirectory))
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect transaction staging: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".staging-") {
			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return fmt.Errorf("remove stale transaction staging %q: %w", entry.Name(), err)
			}
		}
	}
	return nil
}

func recoverPendingTransaction(projectDirectory string) error {
	_, err := inspectTransactions(projectDirectory)
	var pending *PendingTransactionError
	if err == nil {
		return nil
	}
	if !errors.As(err, &pending) {
		return err
	}
	manifest, err := loadJournal(projectDirectory, pending.ID)
	if err != nil {
		return err
	}
	journalDir := filepath.Join(projectDirectory, filepath.FromSlash(path.Join(transactionDirectory, pending.ID)))
	type recovery struct {
		entry   journalEntry
		before  []byte
		root    *os.Root
		restore bool
	}
	projectRoot, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return err
	}
	defer projectRoot.Close()
	journalRoot, err := os.OpenRoot(journalDir)
	if err != nil {
		return &RecoveryConflictError{ID: pending.ID, Detail: fmt.Sprintf("open journal %s: %v", pending.ID, err)}
	}
	defer journalRoot.Close()
	exclusionRoot, exclusionPath, err := recoveryGitExclusion(projectDirectory, manifest)
	if err != nil {
		return &RecoveryConflictError{ID: pending.ID, Detail: err.Error()}
	}
	external := make(map[string]*os.Root)
	defer func() {
		for _, root := range external {
			root.Close()
		}
	}()
	var recoveries []recovery
	for _, entry := range manifest.Entries {
		root, target, err := journalLocation(projectRoot, external, exclusionRoot, exclusionPath, entry)
		if err != nil {
			return &RecoveryConflictError{ID: pending.ID, Detail: err.Error()}
		}
		current, err := snapshotFile(root, target)
		if err != nil {
			return &RecoveryConflictError{ID: pending.ID, Detail: err.Error()}
		}
		var before []byte
		if entry.BeforeExists {
			before, err = journalRoot.ReadFile(entry.BeforeImage)
			if err != nil || int64(len(before)) != entry.BeforeSize || contentHash(before) != entry.BeforeHash {
				return &RecoveryConflictError{ID: pending.ID, Detail: fmt.Sprintf("before-image for %s is missing or corrupt", entry.Path)}
			}
		}
		matchesBefore := current.exists == entry.BeforeExists && (!current.exists || current.hash == entry.BeforeHash && uint32(current.mode.Perm()) == entry.BeforeMode)
		matchesAfter := current.exists == entry.AfterExists && (!current.exists || current.hash == entry.AfterHash && uint32(current.mode.Perm()) == entry.AfterMode)
		if !matchesBefore && !matchesAfter {
			return &RecoveryConflictError{ID: pending.ID, Detail: fmt.Sprintf("%s matches neither journal before-state nor after-state", entry.Path)}
		}
		recoveries = append(recoveries, recovery{entry: entry, before: before, root: root, restore: matchesAfter})
	}
	for _, item := range recoveries {
		if !item.restore {
			continue
		}
		target := item.entry.Path
		if item.entry.GitExclusion {
			target = item.entry.PhysicalPath
		}
		if item.entry.BeforeExists {
			if err := writeFileAtomic(item.root, target, item.before, os.FileMode(item.entry.BeforeMode)); err != nil {
				return fmt.Errorf("restore %q from transaction journal: %w", item.entry.Path, err)
			}
		} else if err := item.root.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %q during transaction recovery: %w", item.entry.Path, err)
		}
	}
	directories := append([]journalDirectory(nil), manifest.Directories...)
	sort.Slice(directories, func(i, j int) bool {
		return strings.Count(directories[i].Path, "/") > strings.Count(directories[j].Path, "/")
	})
	for _, directory := range directories {
		entry := journalEntry{Path: directory.Path, GitExclusion: directory.GitExclusion, PhysicalRoot: directory.PhysicalRoot, PhysicalPath: directory.PhysicalPath}
		root, target, err := journalLocation(projectRoot, external, exclusionRoot, exclusionPath, entry)
		if err != nil {
			return &RecoveryConflictError{ID: pending.ID, Detail: err.Error()}
		}
		info, err := root.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect transaction-created directory %q: %w", directory.Path, err)
		}
		device, inode, ok := filesystemIdentity(info)
		if !ok || !info.IsDir() || device != directory.Device || inode != directory.Inode {
			continue
		}
		if err := root.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("remove transaction-created directory %q: %w", directory.Path, err)
		}
	}
	if err := retireJournal(journalDir); err != nil {
		return fmt.Errorf("remove recovered transaction journal: %w", err)
	}
	return nil
}

func applyPlanJournaled(projectDirectory string, plan Plan, finalize Finalizer) error {
	projectRoot, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return fmt.Errorf("open project directory %q: %w", projectDirectory, err)
	}
	defer projectRoot.Close()
	externalRoots := make(map[string]*os.Root)
	defer func() {
		for _, root := range externalRoots {
			root.Close()
		}
	}()
	var mutations []preparedOperation
	for _, operation := range plan.Operations {
		if operation.Kind == OperationPreserve {
			continue
		}
		if !operation.GitExclusion && !operation.stateFile {
			if err := ValidateTargetPath(operation.Path); err != nil {
				return fmt.Errorf("planned operation path: %w", err)
			}
		}
		root, target, err := resolveOperationLocation(projectRoot, externalRoots, operation)
		if err != nil {
			return err
		}
		snapshot, err := snapshotFile(root, target)
		if err != nil {
			return err
		}
		if !matchesBeforeState(operation, snapshot) {
			return fmt.Errorf("target %q changed after planning; rerun realization to produce a fresh plan", operation.Path)
		}
		if !operation.remove && contentHash(operation.content) != operation.AfterHash {
			return fmt.Errorf("planned content hash for %q is inconsistent", operation.Path)
		}
		if (!operation.remove && needsWrite(snapshot, operation)) || (operation.remove && snapshot.exists) {
			mutations = append(mutations, preparedOperation{operation: operation, root: root, path: target, snapshot: snapshot})
		}
	}
	_, journalDir, err := createJournal(projectDirectory, mutations)
	if err != nil {
		return err
	}
	for _, prepared := range mutations {
		if !prepared.operation.remove {
			created, err := transactionParents(prepared.root, prepared.path)
			if err != nil {
				return recoverApplyFailure(projectDirectory, journalDir, fmt.Errorf("create parents for %q: %w", prepared.operation.Path, err))
			}
			if err := recordCreatedDirectories(projectDirectory, journalDir, prepared, created); err != nil {
				return recoverApplyFailure(projectDirectory, journalDir, fmt.Errorf("record parents for %q: %w", prepared.operation.Path, err))
			}
		}
		physical := prepared.operation
		physical.Path = prepared.path
		if _, err := transactionWriter(prepared.root, physical); err != nil {
			return recoverApplyFailure(projectDirectory, journalDir, fmt.Errorf("apply %s %q: %w", prepared.operation.Kind, prepared.operation.Path, err))
		}
		transactionRenameHook(prepared.operation.Path)
	}
	if finalize != nil {
		if err := finalize(plan.NextLedger); err != nil {
			return recoverApplyFailure(projectDirectory, journalDir, fmt.Errorf("persist realization ledger: %w", err))
		}
	}
	if err := retireJournal(journalDir); err != nil {
		return fmt.Errorf("remove completed transaction journal: %w", err)
	}
	return nil
}

func retireJournal(journalDir string) error {
	parent := filepath.Dir(journalDir)
	retired := filepath.Join(parent, ".staging-complete-"+filepath.Base(journalDir))
	if err := journalRename(journalDir, retired); err != nil {
		return err
	}
	if err := journalDirectorySync(parent); err != nil {
		return err
	}
	if err := os.RemoveAll(retired); err != nil {
		return err
	}
	return journalDirectorySync(parent)
}

func recoverApplyFailure(projectDirectory, journalDir string, applyErr error) error {
	if err := recoverPendingTransaction(projectDirectory); err != nil {
		return fmt.Errorf("%w; automatic recovery failed: %v; journal preserved at %s", applyErr, err, journalDir)
	}
	return fmt.Errorf("%w; all filesystem changes were rolled back", applyErr)
}

func createJournal(projectDirectory string, mutations []preparedOperation) (string, string, error) {
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
			// A failed removal leaves inert staging for the next mutating run and must not replace the journal error.
			_ = os.RemoveAll(staging)
		}
	}()
	if err := os.Mkdir(filepath.Join(staging, "before"), 0o700); err != nil {
		return "", "", err
	}
	manifest := journalManifest{SchemaVersion: journalSchemaVersion, ID: id, Entries: make([]journalEntry, 0, len(mutations))}
	for index, mutation := range mutations {
		entry := journalEntry{
			Path: mutation.operation.Path, BeforeExists: mutation.snapshot.exists,
			BeforeHash: mutation.snapshot.hash, BeforeSize: int64(len(mutation.snapshot.content)), BeforeMode: uint32(mutation.snapshot.mode.Perm()),
			AfterExists: !mutation.operation.remove, AfterHash: mutation.operation.AfterHash, AfterMode: mutation.operation.Mode,
			GitExclusion: mutation.operation.GitExclusion, PhysicalRoot: mutation.operation.physicalRoot, PhysicalPath: mutation.operation.physicalPath,
		}
		if mutation.snapshot.exists {
			entry.BeforeImage = fmt.Sprintf("before/%06d", index)
			if err := journalFileWriter(filepath.Join(staging, filepath.FromSlash(entry.BeforeImage)), mutation.snapshot.content, 0o600); err != nil {
				return "", "", err
			}
		}
		manifest.Entries = append(manifest.Entries, entry)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", "", err
	}
	encoded = append(encoded, '\n')
	if err := journalFileWriter(filepath.Join(staging, journalManifestFilename), encoded, 0o600); err != nil {
		return "", "", err
	}
	if err := journalDirectorySync(filepath.Join(staging, "before")); err != nil {
		return "", "", err
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

func recordCreatedDirectories(projectDirectory, journalDir string, mutation preparedOperation, created []rootedDirectory) error {
	if len(created) == 0 {
		return nil
	}
	manifest, err := loadJournal(projectDirectory, filepath.Base(journalDir))
	if err != nil {
		return err
	}
	for _, directory := range created {
		device, inode, ok := filesystemIdentity(directory.info)
		if !ok {
			return fmt.Errorf("cannot identify created directory %q", directory.path)
		}
		logical := directory.path
		if mutation.operation.GitExclusion {
			logical = mutation.operation.Path + ":parent:" + directory.path
		}
		manifest.Directories = append(manifest.Directories, journalDirectory{
			Path: logical, GitExclusion: mutation.operation.GitExclusion,
			PhysicalRoot: mutation.operation.physicalRoot, PhysicalPath: directory.path,
			Device: device, Inode: inode,
		})
	}
	sort.Slice(manifest.Directories, func(i, j int) bool { return manifest.Directories[i].Path < manifest.Directories[j].Path })
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return journalFileWriter(filepath.Join(journalDir, journalManifestFilename), append(encoded, '\n'), 0o600)
}

func filesystemIdentity(info os.FileInfo) (uint64, uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(stat.Dev), uint64(stat.Ino), true
}

func loadJournal(projectDirectory, id string) (journalManifest, error) {
	filename := filepath.Join(projectDirectory, filepath.FromSlash(path.Join(transactionDirectory, id, journalManifestFilename)))
	data, err := os.ReadFile(filename)
	if err != nil {
		return journalManifest{}, &RecoveryConflictError{ID: id, Detail: fmt.Sprintf("canonical journal %s has no readable manifest", id)}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest journalManifest
	if err := decoder.Decode(&manifest); err != nil {
		return journalManifest{}, &RecoveryConflictError{ID: id, Detail: fmt.Sprintf("decode journal %s manifest: %v", id, err)}
	}
	if err := requireJSONEOF(decoder); err != nil {
		return journalManifest{}, &RecoveryConflictError{ID: id, Detail: fmt.Sprintf("decode journal %s manifest: %v", id, err)}
	}
	if manifest.SchemaVersion != journalSchemaVersion {
		return journalManifest{}, &UnsupportedJournalVersionError{Version: manifest.SchemaVersion}
	}
	if manifest.ID != id {
		return journalManifest{}, &RecoveryConflictError{ID: id, Detail: fmt.Sprintf("journal directory %s disagrees with manifest ID %s", id, manifest.ID)}
	}
	for _, entry := range manifest.Entries {
		if entry.Path == "" || (!entry.GitExclusion && entry.Path != "agents.yaml" && entry.Path != ".agents/registry.lock" && ValidateTargetPath(entry.Path) != nil) {
			return journalManifest{}, &RecoveryConflictError{ID: id, Detail: fmt.Sprintf("journal contains invalid target path %q", entry.Path)}
		}
		if entry.BeforeExists && (entry.BeforeImage == "" || entry.BeforeHash == "" || entry.BeforeMode == 0) {
			return journalManifest{}, &RecoveryConflictError{ID: id, Detail: fmt.Sprintf("journal entry %s has incomplete before-image metadata", entry.Path)}
		}
		if entry.BeforeImage != "" && (ValidateTargetPath(entry.BeforeImage) != nil || path.Dir(entry.BeforeImage) != "before") {
			return journalManifest{}, &RecoveryConflictError{ID: id, Detail: fmt.Sprintf("journal entry %s has invalid before-image path %q", entry.Path, entry.BeforeImage)}
		}
	}
	for _, directory := range manifest.Directories {
		if directory.Path == "" || (!directory.GitExclusion && ValidateTargetPath(directory.Path) != nil) {
			return journalManifest{}, &RecoveryConflictError{ID: id, Detail: fmt.Sprintf("journal contains invalid created directory %q", directory.Path)}
		}
		if directory.Device == 0 || directory.Inode == 0 {
			return journalManifest{}, &RecoveryConflictError{ID: id, Detail: fmt.Sprintf("journal created directory %q has no filesystem identity", directory.Path)}
		}
	}
	return manifest, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("unexpected trailing JSON value")
	}
	return err
}

func recoveryGitExclusion(projectDirectory string, manifest journalManifest) (string, string, error) {
	usesGitExclusion := false
	for _, entry := range manifest.Entries {
		usesGitExclusion = usesGitExclusion || entry.GitExclusion
	}
	for _, directory := range manifest.Directories {
		usesGitExclusion = usesGitExclusion || directory.GitExclusion
	}
	if !usesGitExclusion {
		return "", "", nil
	}
	root, target, err := resolveGitExclude(projectDirectory)
	if err != nil {
		return "", "", fmt.Errorf("recompute project Git exclusion location: %w", err)
	}
	return root, target, nil
}

func journalLocation(projectRoot *os.Root, external map[string]*os.Root, exclusionRoot, exclusionPath string, entry journalEntry) (*os.Root, string, error) {
	if !entry.GitExclusion {
		return projectRoot, entry.Path, nil
	}
	if !filepath.IsAbs(entry.PhysicalRoot) || filepath.Clean(entry.PhysicalRoot) != entry.PhysicalRoot {
		return nil, "", fmt.Errorf("invalid Git exclusion root for %s", entry.Path)
	}
	if err := ValidateTargetPath(entry.PhysicalPath); err != nil {
		return nil, "", fmt.Errorf("invalid Git exclusion path for %s", entry.Path)
	}
	if entry.PhysicalRoot != exclusionRoot || entry.PhysicalPath != exclusionPath {
		return nil, "", fmt.Errorf("journal Git exclusion location for %s does not match this project", entry.Path)
	}
	root := external[entry.PhysicalRoot]
	if root == nil {
		var err error
		root, err = os.OpenRoot(entry.PhysicalRoot)
		if err != nil {
			return nil, "", err
		}
		external[entry.PhysicalRoot] = root
	}
	return root, entry.PhysicalPath, nil
}

func writeSyncedFile(filename string, content []byte, mode os.FileMode) error {
	temporary := filename + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	written, writeErr := file.Write(content)
	if writeErr == nil && written != len(content) {
		writeErr = io.ErrShortWrite
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(temporary, filename); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(filename))
}

func syncDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func randomTransactionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate transaction ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
