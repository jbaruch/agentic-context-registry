package realize

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type gitFenceRepository struct {
	common, info gitFenceDirectory
	registration gitFenceRegistration
}

func identifyFenceDirectory(path string) (gitFenceDirectory, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return gitFenceDirectory{}, fmt.Errorf("inspect Git exclusion directory identity at %q: %w", path, err)
	}
	device, inode, ok := filesystemIdentity(info)
	identity := gitFenceDirectory{path, device, inode}
	if !ok || !info.IsDir() || !validFenceDirectory(identity) {
		return gitFenceDirectory{}, fmt.Errorf("cannot identify Git exclusion directory at %q; use a real directory on a filesystem supporting directory identity", path)
	}
	return identity, nil
}

func (claim *gitExclusionClaim) repository(ctx context.Context, project string) (gitFenceRepository, error) {
	var repo gitFenceRepository
	output, err := runInspectionGit(ctx, project, "identify the Git repository and worktree registration",
		"rev-parse", "--git-common-dir", "--absolute-git-dir", "--show-toplevel")
	if err != nil {
		return repo, err
	}
	paths := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(paths) != 3 || paths[0] == "" || !filepath.IsAbs(paths[1]) || !filepath.IsAbs(paths[2]) ||
		strings.ContainsAny(strings.Join(paths, ""), "\x00\r") {
		return repo, errors.New("Git returned invalid registration paths; check 'git worktree list' and repair the repository metadata before retrying")
	}
	common := paths[0]
	if !filepath.IsAbs(common) {
		physical, err := filepath.EvalSymlinks(project)
		if err != nil {
			return repo, fmt.Errorf("resolve the project directory for Git registration: %w", err)
		}
		common, err = filepath.Abs(filepath.Join(physical, common))
		if err != nil {
			return repo, fmt.Errorf("resolve the absolute Git common directory: %w", err)
		}
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return repo, fmt.Errorf("resolve the Git common directory: %w", err)
	}
	private, err := filepath.EvalSymlinks(paths[1])
	if err != nil {
		return repo, fmt.Errorf("resolve the Git worktree directory: %w", err)
	}
	repo.common, err = identifyFenceDirectory(common)
	if err != nil {
		return repo, err
	}
	infoPath, err := filepath.EvalSymlinks(filepath.Join(common, "info"))
	if err != nil {
		return repo, fmt.Errorf("resolve the Git common exclusion directory: %w", err)
	}
	repo.info, err = identifyFenceDirectory(infoPath)
	if err != nil {
		return repo, err
	}
	locked, err := claim.directory.Stat()
	if err != nil {
		return repo, fmt.Errorf("inspect the locked Git exclusion directory: %w", err)
	}
	if infoPath != claim.root.Name() || !repo.info.matches(locked) {
		return repo, errors.New("Git exclusion directory no longer matches the repository; keep Git metadata stable and retry")
	}
	if common != private {
		if !fenceBasename(filepath.Base(private)) || filepath.Dir(private) != filepath.Join(common, "worktrees") {
			return repo, errors.New("Git returned an unrecognized linked-worktree registration; check 'git worktree list' and repair the repository metadata before retrying")
		}
		repo.registration.gitFenceDirectory, err = identifyFenceDirectory(private)
		if err != nil {
			return repo, err
		}
		// Git rewrites gitdir on move, but the commondir-based identity stays
		// stable and distinguishes a re-registered slot if an inode is reused.
		repo.registration.WorktreeID, err = gitWorktreeID(private)
		if err != nil {
			return repo, err
		}
		if !repo.registration.valid(common) {
			return repo, errors.New("Git registration is missing its linked identity; repair the Git metadata before retrying")
		}
		admin, err := os.OpenRoot(private)
		if err != nil {
			return repo, fmt.Errorf("open the linked-worktree registration: %w", err)
		}
		defer admin.Close()
		backlink, err := snapshotFileLimit(admin, "gitdir", 64<<10)
		if err != nil {
			return repo, fmt.Errorf("read the linked-worktree registration: %w", err)
		}
		name := strings.TrimSuffix(string(backlink.content), "\n")
		if name == "" || strings.ContainsAny(name, "\x00\r\n") {
			return repo, errors.New("Git registration is missing its worktree location; repair the Git metadata before retrying")
		}
		if !filepath.IsAbs(name) {
			name = filepath.Join(private, name)
		}
		gitfile := filepath.Join(paths[2], ".git")
		registered, err := os.Lstat(name)
		if err != nil {
			return repo, fmt.Errorf("inspect the registered worktree gitfile at %q: %w", name, err)
		}
		live, err := os.Lstat(gitfile)
		if err != nil {
			return repo, fmt.Errorf("inspect the current worktree gitfile at %q: %w", gitfile, err)
		}
		if !registered.Mode().IsRegular() || !os.SameFile(registered, live) {
			return repo, errors.New("Git registration does not point to the current worktree gitfile; repair the Git metadata before retrying")
		}
	}
	return repo, nil
}

func (registration gitFenceRegistration) valid(common string) bool {
	if registration == (gitFenceRegistration{}) {
		return true // Primary worktree, with no linked registration to reconcile.
	}
	name := filepath.Base(registration.Path)
	if !validFenceDirectory(registration.gitFenceDirectory) || !fenceBasename(name) ||
		filepath.Dir(registration.Path) != filepath.Join(common, "worktrees") {
		return false
	}
	identity, ok := strings.CutPrefix(registration.WorktreeID, "linked:"+name+":")
	parts := strings.Split(identity, ":")
	if !ok || len(parts) != 3 {
		return false
	}
	_, deviceErr := strconv.ParseInt(parts[0], 10, 64)
	inode, inodeErr := strconv.ParseUint(parts[1], 10, 64)
	_, stampErr := strconv.ParseInt(parts[2], 10, 64)
	return deviceErr == nil && inodeErr == nil && inode != 0 && stampErr == nil
}

func (fence gitExclusionFence) checkRepository(repo gitFenceRepository) error {
	if repo.common != fence.Common || repo.info != fence.Info {
		return errors.New("Git exclusion fence belongs to a different repository or exclusion directory; preserve it and restore the original Git metadata before retrying")
	}
	return nil
}

// Git preserves the administrative slot on worktree move. Its absence from
// the same common directory, where we observed it at publication, is needed
// in addition to the original project no longer identifying the owner.
// These observations do not prove physical removal of the worktree.
func (fence gitExclusionFence) registrationAbsent(repo gitFenceRepository) (bool, error) {
	if fence.Registration == (gitFenceRegistration{}) {
		return false, nil
	}
	root, err := os.OpenRoot(repo.common.Path)
	if err != nil {
		return false, fmt.Errorf("reopen the Git worktree registry: %w", err)
	}
	defer root.Close()
	common, err := root.Stat(".")
	if err != nil {
		return false, fmt.Errorf("verify the Git worktree registry: %w", err)
	}
	if !repo.common.matches(common) {
		return false, errors.New("Git worktree registry changed; keep Git metadata stable and retry")
	}
	registry, err := root.Lstat("worktrees")
	if err == nil && !registry.IsDir() {
		return false, errors.New("Git worktree registry is not a real directory; repair the Git metadata before retrying")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect the Git worktree registry: %w", err)
	}
	if err == nil {
		slot := filepath.Join("worktrees", filepath.Base(fence.Registration.Path))
		if registration, err := root.Lstat(slot); err == nil {
			if !registration.IsDir() {
				return false, fmt.Errorf("Git registration %q is not a real directory; repair the Git metadata before retrying", slot)
			}
			// Even a reused slot cannot prove absence. Never mistake an edited
			// or corrupt registration for Git having removed the owner.
			if !fence.Registration.matches(registration) {
				return false, fmt.Errorf("Git registration %q no longer matches the recorded owner; resolve the registration discrepancy before retrying", slot)
			}
			id, err := gitWorktreeID(fence.Registration.Path)
			if err != nil || id != fence.Registration.WorktreeID {
				return false, fmt.Errorf("Git registration %q no longer has the recorded worktree identity; repair the registration before retrying", slot)
			}
			return false, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("inspect Git worktree registration %q: %w", slot, err)
		}
		// Git retains the slot name on move. A directory with the same
		// identity under another name contradicts absence rather than proving it.
		slots, err := root.Open("worktrees")
		if err != nil {
			return false, fmt.Errorf("open the Git worktree registry: %w", err)
		}
		defer slots.Close()
		current, err := slots.Stat()
		if err != nil || !os.SameFile(registry, current) {
			return false, errors.New("Git worktree registry changed; keep Git metadata stable and retry")
		}
		for {
			entries, err := slots.ReadDir(64)
			if err != nil && !errors.Is(err, io.EOF) {
				return false, fmt.Errorf("read the Git worktree registry: %w", err)
			}
			for _, entry := range entries {
				const discrepancy = "the recorded Git registration slot or directory identity still exists; resolve the registration discrepancy before retrying"
				if filepath.Base(slot) == entry.Name() {
					return false, errors.New(discrepancy)
				}
				if !entry.IsDir() {
					continue
				}
				info, statErr := entry.Info()
				if statErr != nil {
					return false, fmt.Errorf("inspect Git worktree registration %q: %w", entry.Name(), statErr)
				}
				if fence.Registration.matches(info) {
					return false, errors.New(discrepancy)
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
		}
	}
	project, err := os.Lstat(fence.Project.Path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect the recorded project at %q: %w", fence.Project.Path, err)
	}
	if fence.Project.matches(project) || !project.IsDir() && !project.Mode().IsRegular() {
		return false, errors.New("the recorded project may still exist but its Git registration is missing; locate the owner and restore its registration")
	}
	return true, nil
}

const exclusionRetirementPrefix = ".retiring-exclusion-"

func fenceJournalExists(project, name string) (bool, error) {
	root, err := os.OpenRoot(project)
	if err != nil {
		return false, err
	}
	defer root.Close()
	journal := filepath.Join(transactionDirectory, name)
	if err := ValidateParentDirectories(root, journal); err != nil {
		return false, err
	}
	info, err := root.Lstat(journal)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("expected a real journal directory at %q", filepath.Join(project, journal))
	}
	return true, nil
}

// decideFence is the shared ownership decision. Ordinary acquisition may
// recover a verified live owner, but only explicit reconciliation can discard
// the terminal proof after that linked registration is gone.
func (claim *gitExclusionClaim) decideFence(ctx context.Context, project, recoveryID string, fence gitExclusionFence, original fileSnapshot, reconcile bool) error {
	busy := func(err error) error {
		return fmt.Errorf("%w: %w", claim.fenceBusy(original, "cannot release the claim"), err)
	}
	repo, err := claim.repository(ctx, project)
	if err != nil {
		return busy(err)
	}
	if err := fence.checkRepository(repo); err != nil {
		return busy(err)
	}
	owner, err := claim.ownerPath(ctx, project, fence, repo)
	if err != nil {
		return busy(err)
	}
	if owner != "" {
		canonical, err := fenceJournalExists(owner, fence.Journal)
		if err != nil {
			return busy(err)
		}
		staged, err := fenceJournalExists(owner, exclusionRetirementPrefix+fence.Journal)
		if err != nil {
			return busy(err)
		}
		switch {
		case canonical && (fence.Retirement != "" || staged):
			return claim.fenceBusy(original, "live journal contradicts retirement evidence; preserve the journal and fence and restore the original records before retrying")
		case canonical:
			if !reconcile && owner == project && recoveryID == fence.Journal {
				claim.hasJournal = true
				return nil
			}
			return claim.fenceBusy(original, fmt.Sprintf("recover the pending journal in its owning project %q by retrying the mutating command there", owner))
		case fence.Retirement != "" || staged:
			return claim.fenceBusy(original, fmt.Sprintf("run 'acr prepare-exclusion' in the owning project %q before removing its worktree with Git", owner))
		case reconcile:
			return claim.fenceBusy(original, fmt.Sprintf("the owning project %q is still live; retry the original mutating command there", owner))
		default:
			// Normal commit/rollback can have retired its canonical journal
			// before retiring its fence. Confirm and sync in the live owner.
			parent := filepath.Join(owner, transactionDirectory)
			if err := journalDirectorySync(parent); err != nil && !errors.Is(err, os.ErrNotExist) {
				return busy(fmt.Errorf("sync Git exclusion journal parent %q before releasing the fence; retry: %w", parent, err))
			}
			if info, err := os.Stat(owner); err != nil || !fence.Project.matches(info) {
				return claim.fenceBusy(original, "the owner changed during acquisition; locate the matching project and retry")
			}
			if err := claim.checkExactFence(original); err != nil {
				return busy(err)
			}
			return claim.clearJournal()
		}
	}
	unresolved := fmt.Sprintf("locate or restore the matching owner recorded at %q and its Git metadata before retrying", fence.Project.Path)
	absent, err := fence.registrationAbsent(repo)
	if err != nil {
		return claim.fenceBusy(original, fmt.Sprintf("%v; %s", err, unresolved))
	}
	if !absent {
		if fence.Retirement == "" {
			return claim.fenceBusy(original, fmt.Sprintf("recover the pending journal in its owning project (recorded at %q); use 'git worktree list' and retry in its new location if it moved", fence.Project.Path))
		}
		return claim.fenceBusy(original, "the recorded Git registration still exists; "+unresolved)
	}
	if fence.Retirement == "" {
		return claim.fenceBusy(original, "ACR has no terminal retirement proof; "+unresolved)
	}
	if !reconcile {
		if _, err := claim.verifyFenceBefore(fence, false); err != nil {
			return busy(err)
		}
		return claim.fenceBusy(original, fmt.Sprintf("the owner was prepared and its Git registration is absent; run 'acr reconcile-exclusion %s' from this surviving worktree", original.hash))
	}
	before, err := claim.verifyFenceBefore(fence, true)
	if err != nil {
		return busy(err)
	}
	if err := claim.sync(); err != nil {
		return busy(err)
	}
	repo, err = claim.repository(ctx, project)
	if err != nil {
		return busy(err)
	}
	if err := fence.checkRepository(repo); err != nil {
		return busy(err)
	}
	absent, err = fence.registrationAbsent(repo)
	if err != nil {
		return busy(err)
	}
	if !absent {
		return claim.fenceBusy(original, "the recorded Git registration reappeared; "+unresolved)
	}
	after, err := claim.verifyFenceBefore(fence, false)
	if err != nil {
		return busy(err)
	}
	for i := range before {
		if !matchesGitSnapshot(before[i], after[i]) {
			return claim.fenceBusy(original, "the shared files changed during reconciliation; keep them stable and retry")
		}
	}
	if err := claim.checkExactFence(original); err != nil {
		return busy(err)
	}
	return claim.clearJournal()
}

func (claim *gitExclusionClaim) verifyFenceBefore(fence gitExclusionFence, sync bool) ([2]fileSnapshot, error) {
	var snapshots [2]fileSnapshot
	targets := [...]struct {
		name  string
		image *gitFenceImage
	}{{"exclude", fence.Exclude}, {gitExcludeReceipt, fence.Receipt}}
	for i, target := range targets {
		if target.image == nil {
			continue
		}
		current, err := snapshotFile(claim.root, target.name)
		if err != nil {
			return snapshots, err
		}
		filename := filepath.Join(claim.root.Name(), target.name)
		if current.exists != target.image.Exists || uint32(current.mode) != target.image.Mode ||
			!bytes.Equal(current.content, target.image.Bytes) {
			return snapshots, fmt.Errorf("%q does not match the fence before-state; preserve the live files and resolve the exact bytes and mode or original absence as described in docs/realization.md", filename)
		}
		if sync && current.exists {
			file, err := claim.root.Open(target.name)
			if err != nil {
				return snapshots, fmt.Errorf("open Git exclusion file %q before syncing: %w", filename, err)
			}
			info, err := file.Stat()
			if err != nil {
				return snapshots, errors.Join(fmt.Errorf("inspect Git exclusion file %q: %w", filename, err), file.Close())
			}
			if fileGenerationOf(info) != current.generation {
				return snapshots, errors.Join(fmt.Errorf("%q changed before syncing; keep it stable and retry", filename), file.Close())
			}
			if err := errors.Join(file.Sync(), file.Close()); err != nil {
				return snapshots, fmt.Errorf("sync Git exclusion file %q: %w", filename, err)
			}
		}
		current.content = nil
		snapshots[i] = current
	}
	return snapshots, nil
}

// ReconcileGitExclusion retires only the named terminal fence after verifying
// that its linked registration is absent and the shared before-state is intact.
func ReconcileGitExclusion(ctx context.Context, projectDirectory, fenceHash string) (err error) {
	if !validHash(fenceHash) {
		return errors.New("supply the sha256: digest of the reviewed Git exclusion fence")
	}
	root, _, err := resolveGitExclude(ctx, projectDirectory)
	if err != nil {
		return err
	}
	var claim gitExclusionClaim
	defer func() { err = errors.Join(err, claim.close()) }()
	if err := claim.lock(root); err != nil {
		return err
	}
	fence, original, err := readGitExclusionFence(claim.root)
	if err != nil {
		return claim.fenceBusy(original, err.Error())
	}
	if !original.exists {
		return errors.New("no Git exclusion fence matches the supplied digest; review the current fence before retrying")
	}
	if original.hash != fenceHash {
		return claim.fenceBusy(original, "no Git exclusion fence matches the supplied digest; review the current fence before retrying")
	}
	return claim.decideFence(ctx, projectDirectory, "", fence, original, true)
}
