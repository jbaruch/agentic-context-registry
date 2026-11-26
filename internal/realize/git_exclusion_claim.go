package realize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	gitExclusionJournal      = "acr-exclusion-journal"
	gitExclusionFenceVersion = 2
	// Base64 expansion for the existing target and receipt limits, plus
	// directory paths and record fields.
	maxGitExclusionFenceBytes = (maxTargetBytes+maxGitExcludeReceiptBytes+2)*4/3 + (64 << 10)
)

// The info directory is stable across atomic replacements of exclude. Hold
// its flock from the exclusion snapshot through commit or rollback. The
// journal fence keeps siblings out after a crash releases the flock, until the
// owning project recovers or an operator reconciles a removed worktree.
type gitExclusionClaim struct {
	root       *os.Root
	directory  *os.File
	hasJournal bool
	preparing  *gitExclusionFence
}

type gitExclusionFence struct {
	SchemaVersion int                  `json:"schema_version"`
	Project       gitFenceDirectory    `json:"project"`
	Common        gitFenceDirectory    `json:"common"`
	Info          gitFenceDirectory    `json:"info"`
	Journal       string               `json:"journal"`
	Registration  gitFenceRegistration `json:"registration,omitzero"`
	Exclude       *gitFenceImage       `json:"exclude"`
	Receipt       *gitFenceImage       `json:"receipt"`
	Retirement    string               `json:"retirement,omitempty"`

	draftRetirement bool // Derived on read, never persisted.
}

type gitFenceDirectory struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type gitFenceRegistration struct {
	gitFenceDirectory
	WorktreeID string `json:"worktree_id"`
}

func (directory gitFenceDirectory) matches(info os.FileInfo) bool {
	device, inode, ok := filesystemIdentity(info)
	return ok && info.IsDir() && device == directory.Device && inode == directory.Inode
}

type gitFenceImage struct {
	Exists bool   `json:"exists"`
	Bytes  []byte `json:"bytes,omitempty"`
	Size   int    `json:"size,omitempty"`
	Hash   string `json:"hash,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
}

func fenceImage(snapshot fileSnapshot) *gitFenceImage {
	return &gitFenceImage{snapshot.exists, snapshot.content, len(snapshot.content), snapshot.hash, uint32(snapshot.mode)}
}

func (image *gitFenceImage) valid(limit int) bool {
	if !image.Exists {
		return len(image.Bytes) == 0 && image.Size == 0 && image.Hash == "" && image.Mode == 0
	}
	return image.Size == len(image.Bytes) && image.Size <= limit && image.Mode <= 0o777 && image.Hash == contentHash(image.Bytes)
}

func (claim *gitExclusionClaim) lock(rootPath string) error {
	if claim.root != nil {
		if claim.root.Name() != rootPath {
			return fmt.Errorf("Git exclusion directory changed from %q to %q; retry", claim.root.Name(), rootPath)
		}
		return nil
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("open Git exclusion directory %q: %w", rootPath, err)
	}
	dir, err := root.Open(".")
	if err != nil {
		return errors.Join(fmt.Errorf("open Git exclusion claim at %q: %w", rootPath, err), root.Close())
	}
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		var lockErr error = &TransactionLockUnavailableError{Path: rootPath, Err: err}
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			lockErr = &TransactionBusyError{Path: rootPath}
		}
		return errors.Join(lockErr, dir.Close(), root.Close())
	}
	claim.root, claim.directory = root, dir
	return nil
}

func readGitExclusionFence(root *os.Root) (gitExclusionFence, fileSnapshot, error) {
	snapshot, err := snapshotFileLimit(root, gitExclusionJournal, maxGitExclusionFenceBytes)
	path := filepath.Join(root.Name(), gitExclusionJournal)
	if err != nil {
		return gitExclusionFence{}, fileSnapshot{}, fmt.Errorf("cannot read Git exclusion fence %q; restore access to the original record before retrying: %w", path, err)
	}
	if !snapshot.exists {
		return gitExclusionFence{}, snapshot, nil
	}
	malformed := fmt.Errorf("invalid Git exclusion fence %q; preserve it and restore the original valid private record before retrying", path)
	var header *struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(snapshot.content, &header); err != nil || header == nil {
		return gitExclusionFence{}, snapshot, malformed
	}
	if header.SchemaVersion != 1 && header.SchemaVersion != gitExclusionFenceVersion {
		return gitExclusionFence{}, snapshot, fmt.Errorf("unsupported Git exclusion fence schema_version %d at %q; preserve it and use an ACR reader that supports the recorded version (for an unversioned draft, follow docs/realization.md)", header.SchemaVersion, path)
	}
	var fence gitExclusionFence
	if snapshot.mode != 0o600 || json.Unmarshal(snapshot.content, &fence) != nil ||
		fence.Retirement != "" && (fence.SchemaVersion != gitExclusionFenceVersion || fence.Registration == (gitFenceRegistration{})) ||
		!validFenceDirectory(fence.Project) || !validFenceDirectory(fence.Common) || !validFenceDirectory(fence.Info) ||
		!fenceBasename(fence.Journal) || strings.HasPrefix(fence.Journal, ".") ||
		!fence.Registration.valid(fence.Common.Path) || fence.Exclude == nil || !fence.Exclude.valid(maxTargetBytes) ||
		fence.Receipt != nil && (!fence.Receipt.valid(maxGitExcludeReceiptBytes) ||
			fence.Receipt.Exists && decodeGitSeparatorReceipt(fence.Receipt.Bytes) == (gitSeparatorReceipt{})) {
		return gitExclusionFence{}, snapshot, malformed
	}
	if fence.Retirement != "" {
		proof, err := fence.retirementDigest(false)
		if err != nil {
			return gitExclusionFence{}, snapshot, malformed
		}
		if proof != fence.Retirement {
			draft, err := fence.retirementDigest(true)
			if err != nil || draft != fence.Retirement {
				return gitExclusionFence{}, snapshot, malformed
			}
			fence.draftRetirement = true
		}
	}
	canonical, err := json.Marshal(fence)
	if err != nil || !bytes.Equal(canonical, snapshot.content) {
		return gitExclusionFence{}, snapshot, malformed
	}
	return fence, snapshot, nil
}

// Domain-separate proof from the draft that did not sync owner rollback.
// The draft digest is recognized only to keep its evidence fenced.
func (fence gitExclusionFence) retirementDigest(draft bool) (string, error) {
	fence.Retirement = ""
	record, err := json.Marshal(fence)
	if err != nil {
		return "", err
	}
	if !draft {
		record = append(record, "\nacr:durable-owner-rollback"...)
	}
	return contentHash(record), nil
}

func validFenceDirectory(directory gitFenceDirectory) bool {
	return filepath.IsAbs(directory.Path) && filepath.Clean(directory.Path) == directory.Path && directory.Inode != 0 &&
		!strings.ContainsAny(directory.Path, "\x00\r\n")
}

func fenceBasename(name string) bool {
	return ValidateTargetPath(name) == nil && filepath.Base(name) == name
}

func (claim *gitExclusionClaim) acquire(ctx context.Context, rootPath, projectDirectory, recoveryID string) error {
	if claim.root != nil {
		return claim.lock(rootPath)
	}
	if err := claim.lock(rootPath); err != nil {
		return err
	}
	fence, snapshot, err := readGitExclusionFence(claim.root)
	if err != nil {
		return claim.fenceBusy(snapshot, err.Error())
	}
	if !snapshot.exists {
		return nil
	}
	return claim.decideFence(ctx, projectDirectory, recoveryID, fence, snapshot, false)
}

func (claim *gitExclusionClaim) fenceBusy(snapshot fileSnapshot, detail string) error {
	busy := &TransactionBusyError{Path: claim.root.Name()}
	if snapshot.hash != "" {
		return fmt.Errorf("%w; Git exclusion fence %s: %s", busy, snapshot.hash, detail)
	}
	return fmt.Errorf("%w; %s", busy, detail)
}

// ownerPath accepts the original owner or a caller in the same project after a
// Git worktree move. A reused path is not an owner; in particular a regular
// replacement must not be opened as a directory or changed.
func (claim *gitExclusionClaim) ownerPath(ctx context.Context, projectDirectory string, fence gitExclusionFence, repo gitFenceRepository) (string, error) {
	for i, candidate := range []string{projectDirectory, fence.Project.Path} {
		if i == 1 {
			info, err := os.Lstat(candidate)
			if errors.Is(err, os.ErrNotExist) || err == nil && info.Mode().IsRegular() {
				continue
			}
			if err != nil {
				return "", fmt.Errorf("inspect recorded Git exclusion owner %q: %w", candidate, err)
			}
			if !info.IsDir() {
				return "", fmt.Errorf("recorded Git exclusion owner path %q contains a symlink or special file; preserve it and locate the matching owner", candidate)
			}
		}
		identity, err := os.Stat(candidate)
		if err != nil {
			return "", fmt.Errorf("inspect Git exclusion owner %q: %w", candidate, err)
		}
		if !fence.Project.matches(identity) {
			continue
		}
		ownerRepo := repo
		if i != 0 {
			ownerRepo, err = claim.repository(ctx, candidate)
			if err != nil {
				return "", err
			}
		}
		if err := fence.checkRepository(ownerRepo); err != nil {
			return "", err
		}
		if ownerRepo.registration != fence.Registration {
			return "", fmt.Errorf("cannot verify the Git registration of the owning project at %q; restore its Git metadata before retrying", candidate)
		}
		return candidate, nil
	}
	return "", nil
}

func (claim *gitExclusionClaim) checkExactFence(original fileSnapshot) error {
	current, err := snapshotFileLimit(claim.root, gitExclusionJournal, maxGitExclusionFenceBytes)
	if err != nil {
		return err
	}
	if !matchesGitSnapshot(original, current) {
		return errors.New("Git exclusion fence changed; review the current fence before retrying")
	}
	return nil
}

func (claim *gitExclusionClaim) publishJournal(ctx context.Context, entries []journalEntry, projectDirectory, journalDir string) error {
	for _, entry := range entries {
		if !entry.GitExclusion {
			continue
		}
		if claim.root == nil || claim.root.Name() != entry.PhysicalRoot || entry.PhysicalPath != "exclude" || entry.ExclusionEdit == nil {
			return fmt.Errorf("Git exclusion at %q has no matching transaction claim", entry.PhysicalRoot)
		}
		repo, err := claim.repository(ctx, projectDirectory)
		if err != nil {
			return err
		}
		project, err := filepath.Abs(projectDirectory)
		if err == nil {
			project, err = filepath.EvalSymlinks(project)
		}
		if err != nil {
			return fmt.Errorf("resolve Git exclusion journal project: %w", err)
		}
		owner, err := identifyFenceDirectory(project)
		if err != nil {
			return err
		}
		fence := gitExclusionFence{
			SchemaVersion: gitExclusionFenceVersion, Project: owner, Journal: filepath.Base(journalDir),
			Common: repo.common, Info: repo.info, Registration: repo.registration,
		}
		before, err := snapshotFile(claim.root, "exclude")
		if err != nil {
			return err
		}
		if before.exists != entry.BeforeExists || before.hash != entry.BeforeHash || int64(len(before.content)) != entry.BeforeSize ||
			uint32(before.mode) != entry.BeforeMode || before.generation != entry.ExclusionEdit.BeforeGeneration {
			return fmt.Errorf("%s changed before publishing the Git exclusion fence; keep it stable and retry", gitExcludePath)
		}
		fence.Exclude = fenceImage(before)
		if receipt := entry.ExclusionEdit.Receipt; receipt != nil {
			before, err := snapshotFile(claim.root, gitExcludeReceipt)
			if err != nil {
				return err
			}
			if !matchesReceiptBefore(receipt, before) {
				return fmt.Errorf("%s changed before publishing the Git exclusion fence; keep it stable and retry", gitExcludeReceiptPath)
			}
			fence.Receipt = fenceImage(before)
		}
		encoded, err := json.Marshal(fence)
		if err != nil {
			return fmt.Errorf("encode Git exclusion journal fence: %w", err)
		}
		if len(encoded) > maxGitExclusionFenceBytes {
			return fmt.Errorf("Git exclusion fence is too large; reduce the size of %s and retry", gitExcludePath)
		}
		if err := replaceFileAtomic(claim.root, gitExclusionJournal, encoded, 0o600, func() error {
			if _, err := claim.root.Lstat(gitExclusionJournal); !errors.Is(err, os.ErrNotExist) {
				if err != nil {
					return err
				}
				return errors.New("an exclusion fence appeared while preparing the transaction; preserve it and retry")
			}
			return nil
		}, nil); err != nil {
			return fmt.Errorf("publish Git exclusion journal fence: %w", err)
		}
		claim.hasJournal = true
		return claim.sync()
	}
	return nil
}

func (claim *gitExclusionClaim) clearJournal() error {
	if err := claim.root.Remove(gitExclusionJournal); err != nil {
		return fmt.Errorf("remove Git exclusion journal fence: %w", err)
	}
	claim.hasJournal = false
	transactionRenameHook(gitExclusionJournal)
	return claim.sync()
}

var gitExclusionDirectorySync = (*os.File).Sync

func (claim *gitExclusionClaim) sync() error {
	if err := gitExclusionDirectorySync(claim.directory); err != nil {
		return fmt.Errorf("sync Git exclusion directory %q: %w", claim.root.Name(), err)
	}
	return nil
}

func (claim *gitExclusionClaim) close() error {
	if claim.root == nil {
		return nil
	}
	// Closing the locked directory descriptor releases the advisory lock.
	return errors.Join(claim.directory.Close(), claim.root.Close())
}
