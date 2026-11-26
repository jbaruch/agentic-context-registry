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
func (fence gitExclusionFence) ownerRemoved(repo gitFenceRepository) (bool, error) {
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
	project, err := os.Stat(fence.Project.Path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect the recorded project at %q: %w", fence.Project.Path, err)
	}
	if fence.Project.matches(project) {
		return false, errors.New("the recorded project still exists but its Git registration is missing; restore the registration rather than reconciling as an orphan")
	}
	return true, nil
}

// ReconcileGitExclusion verifies manually restored shared before-state for a
// removed linked worktree. It retires only the exact fence the operator names.
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
		return err
	}
	if !original.exists || original.hash != fenceHash {
		return errors.New("no Git exclusion fence matches the supplied digest; review the current fence before retrying")
	}
	verifyOwner := func() error {
		repo, err := claim.repository(ctx, projectDirectory)
		if err != nil {
			return err
		}
		if err := fence.checkRepository(repo); err != nil {
			return err
		}
		removed, err := fence.ownerRemoved(repo)
		if err != nil {
			return fmt.Errorf("cannot confirm that the owner was removed: %w", err)
		}
		if !removed {
			return errors.New("the owner is not a confirmed removed linked worktree; recover its journal in the owning project, using the new path if it moved")
		}
		return nil
	}
	if err := verifyOwner(); err != nil {
		return err
	}
	targets := [...]struct {
		name   string
		image  *gitFenceImage
		synced fileSnapshot
	}{{name: "exclude", image: fence.Exclude}, {name: gitExcludeReceipt, image: fence.Receipt}}
	verifyBefore := func(sync bool) error {
		for i := range targets {
			target := &targets[i]
			if target.image == nil {
				continue
			}
			current, err := snapshotFile(claim.root, target.name)
			if err != nil {
				return err
			}
			if current.exists != target.image.Exists || uint32(current.mode) != target.image.Mode ||
				!bytes.Equal(current.content, target.image.Bytes) {
				return fmt.Errorf("%q does not match the fence before-state; preserve the live shared files separately and restore the exact bytes and mode or original absence using docs/realization.md", filepath.Join(root, target.name))
			}
			if !sync && !matchesGitSnapshot(target.synced, current) {
				return fmt.Errorf("%q changed during reconciliation; keep the shared files stable and retry", filepath.Join(root, target.name))
			}
			if sync && current.exists {
				file, err := claim.root.Open(target.name)
				if err != nil {
					return fmt.Errorf("open reconciled Git exclusion file %q: %w", target.name, err)
				}
				info, statErr := file.Stat()
				if statErr != nil {
					return errors.Join(fmt.Errorf("inspect reconciled Git exclusion file %q: %w", target.name, statErr), file.Close())
				}
				if fileGenerationOf(info) != current.generation {
					return errors.Join(fmt.Errorf("%q changed before syncing; keep the shared files stable and retry", filepath.Join(root, target.name)), file.Close())
				}
				if err := errors.Join(file.Sync(), file.Close()); err != nil {
					return fmt.Errorf("sync reconciled Git exclusion file %q: %w", target.name, err)
				}
			}
			if sync {
				current.content = nil
				target.synced = current
			}
		}
		return nil
	}
	if err := verifyBefore(true); err != nil {
		return err
	}
	if err := claim.sync(); err != nil {
		return err
	}
	if err := verifyOwner(); err != nil {
		return err
	}
	if err := verifyBefore(false); err != nil {
		return err
	}
	current, err := snapshotFileLimit(claim.root, gitExclusionJournal, maxGitExclusionFenceBytes)
	if err != nil {
		return err
	}
	if !matchesGitSnapshot(original, current) {
		return errors.New("Git exclusion fence changed during reconciliation; review the current fence before retrying")
	}
	return claim.clearJournal()
}
