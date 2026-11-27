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
// authorize checks local recovery consent under both claims before rollback.
func PrepareGitExclusion(ctx context.Context, projectDirectory string, authorize func(string) error) (err error) {
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
	parent := filepath.Join(projectDirectory, transactionDirectory)
	canonicalDir := filepath.Join(parent, fence.Journal)
	stagedName := exclusionRetirementPrefix + fence.Journal
	retiredDir := filepath.Join(parent, stagedName)
	cleanupName := ".staging-complete-" + fence.Journal
	cleanupDir := filepath.Join(parent, cleanupName)

	canonical, err := fenceJournalExists(projectDirectory, fence.Journal)
	if err != nil {
		return claim.fenceBusy(original, err.Error())
	}
	staged, err := fenceJournalExists(projectDirectory, stagedName)
	if err != nil {
		return claim.fenceBusy(original, err.Error())
	}
	cleanup, err := fenceJournalExists(projectDirectory, cleanupName)
	if err != nil {
		return claim.fenceBusy(original, err.Error())
	}
	if cleanup && (canonical || staged) {
		evidenceDir := retiredDir
		if canonical {
			evidenceDir = canonicalDir
		}
		return claim.fenceBusy(original, fmt.Sprintf("conflicting journal directories %q and %q; preserve both and restore the original records before retrying", evidenceDir, cleanupDir))
	}
	if canonical && (fence.Retirement != "" || staged) {
		return claim.fenceBusy(original, fmt.Sprintf("live journal %q contradicts retirement evidence at %q or in the fence; preserve the original records before retrying", canonicalDir, retiredDir))
	}
	_, err = inspectTransactions(projectDirectory)
	var pending *PendingTransactionError
	if err != nil && !(canonical && errors.As(err, &pending) && pending.ID == fence.Journal) {
		return err
	}
	if !canonical && !staged && fence.Retirement == "" {
		if cleanup {
			return claim.fenceBusy(original, fmt.Sprintf("cleanup directory %q lacks terminal retirement proof; preserve it and restore the original records before retrying", cleanupDir))
		}
		return claim.fenceBusy(original, fmt.Sprintf("the matching journal is missing at %q and %q; restore it before preparing this worktree for removal", canonicalDir, retiredDir))
	}
	if !canonical && !staged && fence.draftRetirement {
		return claim.fenceBusy(original, fmt.Sprintf("%s (matching journal absent at %q and %q)", draftRetirementGuidance, canonicalDir, retiredDir))
	}
	if canonical || staged {
		journalDir := retiredDir
		if canonical {
			journalDir = canonicalDir
		}
		manifestPath := filepath.Join(journalDir, journalManifestFilename)
		manifest, err := loadJournalAt(manifestPath, fence.Journal)
		if err != nil {
			return fmt.Errorf("read journal evidence at %q; preserve the owner, its Git registration, journal and fence and refer to the Factory judge if original journal evidence is unavailable: %w", manifestPath, err)
		}
		if !fence.matchesJournal(manifest) {
			return claim.fenceBusy(original, fmt.Sprintf("journal %q does not agree with the fence before-images; restore the original records before retrying", manifestPath))
		}
		if canonical {
			if err := authorize(projectDirectory); err != nil {
				return err
			}
			claim.hasJournal, claim.preparing = true, &fence
			if err := recoverJournal(ctx, projectDirectory, claim, fence.Journal, manifest); err != nil {
				return err
			}
			staged = true
		} else {
			// Both an unsealed stage and a visible proof may be left by
			// an interrupted attempt. Neither permits replaying writes.
			projectRoot, err := os.OpenRoot(projectDirectory)
			if err != nil {
				return WrapProjectAccessError(projectDirectory, "open project directory to verify retired rollback", err)
			}
			verifyErr := verifyAndSyncOwnerRollback(projectRoot, stagedName, manifest)
			if err := errors.Join(verifyErr, projectRoot.Close()); err != nil {
				return fmt.Errorf("verify and sync staged rollback in %q: %w", retiredDir, err)
			}
			if _, err := claim.verifyFenceBefore(fence, true); err != nil {
				return claim.fenceBusy(original, err.Error())
			}
			if err := claim.sync(); err != nil {
				return err
			}
			if err := journalDirectorySync(parent); err != nil {
				return fmt.Errorf("sync retired exclusion journal in %q before publishing proof: %w", parent, err)
			}
		}
	} else if _, err := claim.verifyFenceBefore(fence, true); err != nil {
		return claim.fenceBusy(original, err.Error())
	}
	sealing := fence.Retirement == "" || fence.draftRetirement
	revalidate := func() error {
		if err := verifyOwner(); err != nil {
			return err
		}
		if _, err := inspectTransactions(projectDirectory); err != nil {
			return err
		}
		if staged {
			exists, err := fenceJournalExists(projectDirectory, stagedName)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("the retired journal at %q disappeared before publication; restore it before retrying", retiredDir)
			}
		}
		if _, err := claim.verifyFenceBefore(fence, false); err != nil {
			return err
		}
		return claim.checkExactFence(original)
	}
	if sealing {
		fence.SchemaVersion = gitExclusionFenceVersion
		fence.Retirement, err = fence.retirementDigest(false)
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
		// Proof is durable. Keep the evidence intact until the rename to
		// disposable residue is durable too, so retries never need a
		// manifest that recursive deletion may already have removed.
		exists, err := fenceJournalExists(projectDirectory, cleanupName)
		if err != nil {
			return claim.fenceBusy(original, err.Error())
		}
		if exists {
			return claim.fenceBusy(original, fmt.Sprintf("conflicting journal directories %q and %q; preserve both and restore the original records before retrying", retiredDir, cleanupDir))
		}
		if err := journalRename(retiredDir, cleanupDir); err != nil {
			return fmt.Errorf("rename retired exclusion journal %q for cleanup at %q: %w", retiredDir, cleanupDir, err)
		}
		cleanup = true
	}
	if cleanup {
		if err := journalDirectorySync(parent); err != nil {
			return fmt.Errorf("sync transaction parent %q before removing cleanup directory %q: %w", parent, cleanupDir, err)
		}
		if err := os.RemoveAll(cleanupDir); err != nil {
			return fmt.Errorf("remove retired exclusion journal cleanup directory %q: %w", cleanupDir, err)
		}
	}
	if err := journalDirectorySync(parent); err != nil {
		return fmt.Errorf("sync transaction parent %q to confirm exclusion journal cleanup: %w", parent, err)
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
