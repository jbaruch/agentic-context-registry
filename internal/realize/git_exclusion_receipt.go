package realize

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	gitExcludeReceipt         = "acr-exclusion-separator"
	gitExcludeReceiptPath     = ".git/info/" + gitExcludeReceipt
	gitExcludeReceiptMode     = 0o600
	gitExcludeReceiptVersion  = 1
	maxGitExcludeReceiptBytes = 1024
)

type gitExcludeMarkerError struct{ detail string }

func (err *gitExcludeMarkerError) Error() string {
	return gitExcludePath + " contains " + err.detail
}

type gitSeparatorReceipt struct {
	SchemaVersion int            `json:"schema_version"`
	Separator     int            `json:"separator"` // byte offset plus one
	Hash          string         `json:"hash"`
	Generation    fileGeneration `json:"generation"`
}

type gitExclusionChange struct {
	before fileSnapshot
	// nil means the companion's shape is not recognized and it is left alone.
	receipt                         *fileSnapshot
	beforeSeparator, afterSeparator int
}

// The companion is part of the exclusion journal entry, at a fixed path
// beside Git's resolved exclude. No journal data can redirect either write.
type gitExclusionJournalEdit struct {
	BeforeGeneration fileGeneration           `json:"beforeGeneration"`
	AfterGeneration  fileGeneration           `json:"afterGeneration,omitzero"`
	Receipt          *gitExclusionReceiptEdit `json:"receipt,omitempty"`
}

type gitExclusionReceiptEdit struct {
	Before       []byte `json:"before,omitempty"`
	BeforeMode   uint32 `json:"beforeMode,omitempty"`
	AfterReady   bool   `json:"afterReady,omitempty"` // an empty after hash means removal
	AfterHash    string `json:"afterHash,omitempty"`
	RollbackHash string `json:"rollbackHash,omitempty"`
}

func (state gitContext) planExcludeRewrite(root *os.Root, target string, targets []string) (fileSnapshot, []byte, *gitExclusionChange, []TransactionNote, error) {
	before, err := snapshotFile(root, target)
	if err != nil {
		return fileSnapshot{}, nil, nil, nil, err
	}
	receipt, notes, err := readGitSeparatorReceipt(root)
	if err != nil {
		return fileSnapshot{}, nil, nil, nil, err
	}
	var separator int
	if receipt != nil {
		separator = provenGitSeparator(receipt.content, before.content, before.hash, before.generation)
	}
	after, next, note, err := state.rewriteExclude(before.content, targets, separator)
	if note != nil {
		notes = append(notes, *note)
	}
	return before, after, &gitExclusionChange{before, receipt, separator, next}, notes, err
}

func readGitSeparatorReceipt(root *os.Root) (*fileSnapshot, []TransactionNote, error) {
	info, err := root.Lstat(gitExcludeReceipt)
	if errors.Is(err, os.ErrNotExist) {
		return &fileSnapshot{}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("inspect Git exclusion separator receipt: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxGitExcludeReceiptBytes {
		return nil, nil, nil
	}
	snapshot, err := snapshotFile(root, gitExcludeReceipt)
	if err != nil {
		return nil, nil, err
	}
	if snapshot.exists && decodeGitSeparatorReceipt(snapshot.content) == (gitSeparatorReceipt{}) {
		var header struct {
			SchemaVersion int `json:"schema_version"`
		}
		if len(snapshot.content) <= maxGitExcludeReceiptBytes && json.Unmarshal(snapshot.content, &header) == nil && header.SchemaVersion != gitExcludeReceiptVersion {
			return nil, []TransactionNote{{
				Code: "unsupported_git_exclusion_receipt_version", Path: gitExcludeReceiptPath,
				Message: fmt.Sprintf("ACR left the receipt and any unproven separator intact (schema_version %d; 0 also denotes an unversioned draft). Use a reader that supports the recorded version or follow the draft-state procedure in docs/realization.md before changing the receipt.", header.SchemaVersion),
			}}, nil
		}
		return nil, nil, nil
	}
	return &snapshot, nil, nil
}

func decodeGitSeparatorReceipt(content []byte) gitSeparatorReceipt {
	var receipt gitSeparatorReceipt
	if len(content) > maxGitExcludeReceiptBytes || json.Unmarshal(content, &receipt) != nil || receipt.SchemaVersion != gitExcludeReceiptVersion || receipt.Separator <= 0 ||
		!validHash(receipt.Hash) || receipt.Generation.Inode == 0 || receipt.Generation.Ctime == 0 {
		return gitSeparatorReceipt{}
	}
	// Reject partial, duplicate-key, or otherwise noncanonical records.
	canonical, err := json.Marshal(receipt)
	if err != nil || !bytes.Equal(content, canonical) {
		return gitSeparatorReceipt{}
	}
	return receipt
}

func provenGitSeparator(receipt, content []byte, hash string, generation fileGeneration) int {
	record := decodeGitSeparatorReceipt(receipt)
	if record.Separator > 0 && record.Generation == generation && record.Hash == hash &&
		record.Separator <= len(content) && content[record.Separator-1] == '\n' {
		return record.Separator
	}
	return 0
}

func (change *gitExclusionChange) journalEdit() *gitExclusionJournalEdit {
	if change == nil {
		return nil
	}
	edit := &gitExclusionJournalEdit{BeforeGeneration: change.before.generation}
	if change.receipt != nil && (change.beforeSeparator != 0 || change.afterSeparator != 0) {
		edit.Receipt = &gitExclusionReceiptEdit{Before: change.receipt.content, BeforeMode: uint32(change.receipt.mode.Perm())}
	}
	return edit
}

func matchesGitSnapshot(want, got fileSnapshot) bool {
	return want.exists == got.exists && want.hash == got.hash && want.mode == got.mode && want.generation == got.generation
}

func (change *gitExclusionChange) recheck(root *os.Root, target string) error {
	current, err := snapshotFile(root, target)
	if err != nil {
		return err
	}
	if !matchesGitSnapshot(change.before, current) {
		return fmt.Errorf("%s changed after planning; keep it stable and retry", gitExcludePath)
	}
	if change.receipt != nil {
		receipt, err := snapshotFile(root, gitExcludeReceipt)
		if err != nil {
			return err
		}
		if !matchesGitSnapshot(*change.receipt, receipt) {
			return fmt.Errorf("%s changed after planning; keep it stable and retry", gitExcludeReceiptPath)
		}
	}
	return nil
}

func applyGitExclusion(journalDir string, claim *gitExclusionClaim, target string, content []byte, mode os.FileMode, change *gitExclusionChange) error {
	root := claim.root
	edit := change.journalEdit()
	if err := replaceFileAtomic(root, target, content, mode, func() error { return change.recheck(root, target) }, &edit.AfterGeneration); err != nil {
		return err
	}
	hash := contentHash(content)
	var receipt []byte
	if edit.Receipt != nil && edit.AfterGeneration != (fileGeneration{}) {
		edit.Receipt.AfterReady = true
		if change.afterSeparator != 0 {
			var err error
			receipt, err = json.Marshal(gitSeparatorReceipt{gitExcludeReceiptVersion, change.afterSeparator, hash, edit.AfterGeneration})
			if err != nil {
				return err
			}
			edit.Receipt.AfterHash = contentHash(receipt)
		}
	}
	if err := updateGitExclusionJournal(journalDir, edit); err != nil {
		return err
	}
	if err := claim.sync(); err != nil {
		return err
	}
	transactionRenameHook(gitExcludePath)
	if edit.Receipt == nil || !edit.Receipt.AfterReady {
		return nil
	}
	check := func() error {
		current, err := snapshotFile(root, target)
		if err != nil {
			return err
		}
		if !current.exists || current.hash != hash || current.generation != edit.AfterGeneration || current.mode != mode {
			return fmt.Errorf("%s changed before writing its separator receipt; keep it stable and retry", gitExcludePath)
		}
		currentReceipt, err := snapshotFile(root, gitExcludeReceipt)
		if err != nil {
			return err
		}
		if !matchesGitSnapshot(*change.receipt, currentReceipt) {
			return fmt.Errorf("%s changed during exclusion write; keep it stable and retry", gitExcludeReceiptPath)
		}
		return nil
	}
	if receipt == nil {
		if err := check(); err != nil {
			return err
		}
		if err := root.Remove(gitExcludeReceipt); err != nil {
			return err
		}
	} else if err := replaceFileAtomic(root, gitExcludeReceipt, receipt, gitExcludeReceiptMode, check, nil); err != nil {
		return err
	}
	if err := claim.sync(); err != nil {
		return err
	}
	transactionRenameHook(gitExcludeReceiptPath)
	return nil
}

func updateGitExclusionJournal(journalDir string, edit *gitExclusionJournalEdit) error {
	project := filepath.Clean(filepath.Join(journalDir, "..", "..", ".."))
	manifest, err := loadJournal(project, filepath.Base(journalDir))
	if err != nil {
		return err
	}
	for i := range manifest.Entries {
		if manifest.Entries[i].GitExclusion && manifest.Entries[i].ExclusionEdit != nil {
			manifest.Entries[i].ExclusionEdit = edit
			data, err := marshalJournal(manifest)
			if err != nil {
				return err
			}
			return journalFileWriter(filepath.Join(journalDir, journalManifestFilename), data, 0o600)
		}
	}
	return fmt.Errorf("missing Git exclusion entry in transaction journal %q", journalDir)
}

func gitExclusionRecoveryDetail(filename, journalDir, reason string) string {
	return fmt.Sprintf("Git exclusion %q %s; preserve the live file separately before reconciling to the verified before-state in journal %q. Recreating the after-state bytes will not unblock recovery", filename, reason, journalDir)
}

func inspectGitExclusionReceiptRecovery(root *os.Root, receipt *gitExclusionReceiptEdit) (fileSnapshot, error) {
	if receipt == nil {
		return fileSnapshot{}, nil
	}
	current, err := snapshotFile(root, gitExcludeReceipt)
	if err != nil {
		return fileSnapshot{}, err
	}
	if matchesReceiptBefore(receipt, current) ||
		(receipt.AfterReady && matchesReceiptHash(current, receipt.AfterHash)) ||
		(receipt.RollbackHash != "" && matchesReceiptHash(current, receipt.RollbackHash)) {
		return current, nil
	}
	return fileSnapshot{}, fmt.Errorf("%s matches neither journal before-state nor an installed receipt", gitExcludeReceiptPath)
}

func matchesReceiptBefore(receipt *gitExclusionReceiptEdit, current fileSnapshot) bool {
	if len(receipt.Before) == 0 {
		return !current.exists
	}
	return current.exists && current.hash == contentHash(receipt.Before) && uint32(current.mode) == receipt.BeforeMode
}

func matchesReceiptHash(current fileSnapshot, hash string) bool {
	if hash == "" {
		return !current.exists
	}
	return current.exists && current.hash == hash && current.mode == gitExcludeReceiptMode
}

func rollbackGitExclusion(journalDir string, root *os.Root, entry journalEntry, before []byte, current, currentReceipt fileSnapshot, restore bool) error {
	edit := entry.ExclusionEdit
	check := func() error {
		live, err := snapshotFile(root, entry.PhysicalPath)
		if err != nil {
			return err
		}
		if !matchesGitSnapshot(current, live) {
			return errors.New(gitExclusionRecoveryDetail(filepath.Join(root.Name(), entry.PhysicalPath), journalDir, "changed during recovery"))
		}
		if edit.Receipt != nil {
			liveReceipt, err := snapshotFile(root, gitExcludeReceipt)
			if err != nil {
				return err
			}
			if !matchesGitSnapshot(currentReceipt, liveReceipt) {
				return fmt.Errorf("%s changed during recovery; keep it stable and retry", gitExcludeReceiptPath)
			}
		}
		return nil
	}
	var restored fileGeneration
	if restore {
		if entry.BeforeExists {
			if err := replaceFileAtomic(root, entry.PhysicalPath, before, os.FileMode(entry.BeforeMode), check, &restored); err != nil {
				return err
			}
		} else {
			if err := check(); err != nil {
				return err
			}
			if err := root.Remove(entry.PhysicalPath); err != nil {
				return err
			}
		}
		current = fileSnapshot{exists: entry.BeforeExists, hash: entry.BeforeHash, mode: os.FileMode(entry.BeforeMode), generation: restored}
	}
	receipt := edit.Receipt
	if receipt == nil {
		return nil
	}
	data, mode := receipt.Before, os.FileMode(receipt.BeforeMode)
	// A matching before hash on retry is not proof that this invocation
	// restored the file. Rebind only using the replacement we actually made.
	if restored != (fileGeneration{}) {
		separator := provenGitSeparator(receipt.Before, before, entry.BeforeHash, edit.BeforeGeneration)
		if separator != 0 {
			var err error
			data, err = json.Marshal(gitSeparatorReceipt{gitExcludeReceiptVersion, separator, entry.BeforeHash, restored})
			if err != nil {
				return err
			}
			mode = gitExcludeReceiptMode
			receipt.RollbackHash = contentHash(data)
			if err := updateGitExclusionJournal(journalDir, edit); err != nil {
				return err
			}
		}
	} else if receipt.RollbackHash != "" && matchesReceiptHash(currentReceipt, receipt.RollbackHash) {
		return nil // A previous recovery already installed the rebound receipt.
	}
	if len(data) == 0 {
		if !currentReceipt.exists {
			return nil
		}
		if err := check(); err != nil {
			return err
		}
		return root.Remove(gitExcludeReceipt)
	}
	if currentReceipt.exists && currentReceipt.hash == contentHash(data) && currentReceipt.mode == mode {
		return nil
	}
	return replaceFileAtomic(root, gitExcludeReceipt, data, mode, check, nil)
}
