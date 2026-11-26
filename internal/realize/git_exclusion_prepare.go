package realize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// PrepareGitExclusion rolls back the linked owner's exact pending transaction
// and records durable retirement before an operator removes its worktree.
func PrepareGitExclusion(ctx context.Context, projectDirectory string) (err error) {
	projectClaim, err := claimTransactions(projectDirectory)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, projectClaim.Close()) }()
	claim := &projectClaim.exclusion
	root, _, err := resolveGitExclude(ctx, projectDirectory)
	if err != nil {
		return err
	}
	if err := claim.lock(root); err != nil {
		return err
	}
	fence, original, err := readGitExclusionFence(claim.root)
	if err != nil {
		return claim.fenceBusy(original, err.Error())
	}
	if !original.exists {
		return errors.New("no Git exclusion fence exists to prepare")
	}
	if fence.Registration == (gitFenceRegistration{}) {
		return claim.fenceBusy(original, "preparation requires the linked worktree that owns the fence")
	}
	verifyOwner := func() error {
		repo, err := claim.repository(ctx, projectDirectory)
		if err != nil {
			return err
		}
		if err := fence.checkRepository(repo); err != nil {
			return err
		}
		info, err := os.Stat(projectDirectory)
		if err != nil {
			return err
		}
		if !fence.Project.matches(info) || repo.registration != fence.Registration {
			return fmt.Errorf("preparation requires the matching owner recorded at %q and its original Git registration; locate or restore them before retrying", fence.Project.Path)
		}
		return nil
	}
	if err := verifyOwner(); err != nil {
		return claim.fenceBusy(original, err.Error())
	}
	canonical, err := fenceJournalExists(projectDirectory, fence.Journal)
	if err != nil {
		return claim.fenceBusy(original, err.Error())
	}
	stagedName := exclusionRetirementPrefix + fence.Journal
	staged, err := fenceJournalExists(projectDirectory, stagedName)
	if err != nil {
		return claim.fenceBusy(original, err.Error())
	}
	_, err = inspectTransactions(projectDirectory)
	var pending *PendingTransactionError
	if err != nil && !(canonical && errors.As(err, &pending) && pending.ID == fence.Journal) {
		return err
	}
	if canonical && (fence.Retirement != "" || staged) {
		return claim.fenceBusy(original, "live journal contradicts retirement evidence; preserve the journal and fence and restore the original records before retrying")
	}
	if !canonical && !staged && fence.Retirement == "" {
		return claim.fenceBusy(original, "the matching journal is missing; restore it before preparing this worktree for removal")
	}
	parent := filepath.Join(projectDirectory, transactionDirectory)
	retiredDir := filepath.Join(parent, stagedName)
	if fence.Retirement == "" {
		journalDir := retiredDir
		if canonical {
			journalDir = filepath.Join(parent, fence.Journal)
		}
		manifest, err := loadJournalAt(filepath.Join(journalDir, journalManifestFilename), fence.Journal)
		if err != nil {
			return err
		}
		if !fence.matchesJournal(manifest) {
			return claim.fenceBusy(original, "the matching journal does not agree with the fence before-images; restore the original records before retrying")
		}
		if canonical {
			claim.hasJournal, claim.preparing = true, &fence
			if err := recoverJournal(ctx, projectDirectory, claim, fence.Journal, manifest); err != nil {
				return err
			}
			staged = true
		}
	}
	if staged && !canonical {
		// A previous invocation may have stopped immediately after renaming
		// the canonical journal. Do not publish proof until that is durable.
		if err := journalDirectorySync(parent); err != nil {
			return fmt.Errorf("sync retired exclusion journal in %q before publishing proof: %w", parent, err)
		}
	}
	sealing := fence.Retirement == ""
	if _, err := claim.verifyFenceBefore(fence, true); err != nil {
		return claim.fenceBusy(original, err.Error())
	}
	revalidate := func() error {
		if err := verifyOwner(); err != nil {
			return err
		}
		if _, err := inspectTransactions(projectDirectory); err != nil {
			return err
		}
		if sealing {
			exists, err := fenceJournalExists(projectDirectory, stagedName)
			if err != nil {
				return err
			}
			if !exists {
				return errors.New("the retired journal disappeared before publication; restore it before retrying")
			}
		}
		if _, err := claim.verifyFenceBefore(fence, false); err != nil {
			return err
		}
		return claim.checkExactFence(original)
	}
	if sealing {
		fence.SchemaVersion = gitExclusionFenceVersion
		fence.Retirement, err = fence.retirementDigest()
		if err != nil {
			return fmt.Errorf("bind Git exclusion retirement proof: %w", err)
		}
		encoded, err := json.Marshal(fence)
		if err != nil {
			return fmt.Errorf("encode Git exclusion retirement proof: %w", err)
		}
		if len(encoded) > maxGitExclusionFenceBytes {
			return errors.New("Git exclusion retirement proof exceeds the fence size limit; preserve the original fence")
		}
		if err := replaceFileAtomic(claim.root, gitExclusionJournal, encoded, 0o600, revalidate, nil); err != nil {
			return claim.fenceBusy(original, fmt.Sprintf("publish Git exclusion retirement proof: %v", err))
		}
		transactionRenameHook("exclusion-retirement-proof")
	} else if err := revalidate(); err != nil {
		return claim.fenceBusy(original, err.Error())
	}
	if err := claim.sync(); err != nil {
		return err
	}
	if staged {
		if err := os.RemoveAll(retiredDir); err != nil {
			return fmt.Errorf("clean retired exclusion journal at %q: %w", retiredDir, err)
		}
		if err := journalDirectorySync(parent); err != nil {
			return fmt.Errorf("sync retired exclusion journal cleanup in %q: %w", parent, err)
		}
	}
	return nil
}

func (fence gitExclusionFence) matchesJournal(manifest journalManifest) bool {
	if manifest.SchemaVersion != exclusionJournalSchemaVersion || manifest.ID != fence.Journal {
		return false
	}
	for _, entry := range manifest.Entries {
		if !entry.GitExclusion {
			continue
		}
		if entry.PhysicalRoot != fence.Info.Path || entry.BeforeExists != fence.Exclude.Exists ||
			entry.BeforeSize != int64(fence.Exclude.Size) || entry.BeforeHash != fence.Exclude.Hash || entry.BeforeMode != fence.Exclude.Mode {
			return false
		}
		receipt := entry.ExclusionEdit.Receipt
		if fence.Receipt == nil || receipt == nil {
			return fence.Receipt == nil && receipt == nil
		}
		return fence.Receipt.Exists == (len(receipt.Before) != 0) && uint32(receipt.BeforeMode) == fence.Receipt.Mode &&
			bytes.Equal(receipt.Before, fence.Receipt.Bytes)
	}
	return false
}
