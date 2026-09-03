package realize

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// VendorTreeRemovalPlan binds removal of one ACR-owned vendored package to
// the exact files observed before uninstall starts.
type VendorTreeRemovalPlan struct {
	Path        string `json:"path"`
	Files       int    `json:"files"`
	edits       []FileTransactionEdit
	directories []string
}

// PlanVendorTreeRemoval snapshots one .agents/vendor/<workspace>/<package>
// tree. Existing contents are accepted without comparing them to the vendor
// lock because the whole tree is ACR-owned.
func PlanVendorTreeRemoval(projectDirectory, relativeRoot string) (VendorTreeRemovalPlan, error) {
	if err := validateVendorTreeRoot(relativeRoot); err != nil {
		return VendorTreeRemovalPlan{}, err
	}
	plan := VendorTreeRemovalPlan{Path: relativeRoot}
	projectRoot, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return VendorTreeRemovalPlan{}, fmt.Errorf("open project directory %q: %w", projectDirectory, err)
	}
	defer projectRoot.Close()
	if err := ValidateParentDirectories(projectRoot, relativeRoot); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return plan, nil
		}
		return VendorTreeRemovalPlan{}, fmt.Errorf("inspect vendor tree %q: %w", relativeRoot, err)
	}
	info, err := projectRoot.Lstat(relativeRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return plan, nil
		}
		return VendorTreeRemovalPlan{}, fmt.Errorf("inspect vendor tree %q: %w", relativeRoot, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return VendorTreeRemovalPlan{}, fmt.Errorf("vendor tree %q must be a regular directory", relativeRoot)
	}

	absoluteRoot := filepath.Join(projectDirectory, filepath.FromSlash(relativeRoot))
	err = filepath.WalkDir(absoluteRoot, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(projectDirectory, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			plan.directories = append(plan.directories, relative)
			return nil
		}
		edit := FileTransactionEdit{Path: relative, Operation: "vendor-remove", BeforeMode: uint32(info.Mode().Perm())}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(filename)
			if err != nil {
				return err
			}
			edit.LinkTarget = target
		case info.Mode().IsRegular():
			content, err := os.ReadFile(filename)
			if err != nil {
				return err
			}
			edit.Before = content
		default:
			return fmt.Errorf("vendor tree entry %q must be a regular file or symbolic link", relative)
		}
		plan.edits = append(plan.edits, edit)
		return nil
	})
	if err != nil {
		return VendorTreeRemovalPlan{}, fmt.Errorf("snapshot vendor tree %q: %w", relativeRoot, err)
	}
	sort.Slice(plan.edits, func(i, j int) bool { return plan.edits[i].Path < plan.edits[j].Path })
	sort.Slice(plan.directories, func(i, j int) bool {
		leftDepth := strings.Count(plan.directories[i], "/")
		rightDepth := strings.Count(plan.directories[j], "/")
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return plan.directories[i] > plan.directories[j]
	})
	plan.Files = len(plan.edits)
	return plan, nil
}

// ApplyVendorTreeRemoval removes a previously snapshotted vendor tree through
// the durable file journal.
func ApplyVendorTreeRemoval(projectDirectory string, plan VendorTreeRemovalPlan) error {
	return applyVendorTreeRemovalWithHooks(projectDirectory, plan, FileTransactionHooks{})
}

func applyVendorTreeRemovalWithHooks(projectDirectory string, plan VendorTreeRemovalPlan, hooks FileTransactionHooks) error {
	if err := validateVendorTreeRoot(plan.Path); err != nil {
		return err
	}
	if plan.Files != len(plan.edits) {
		return fmt.Errorf("vendor tree removal plan for %q is incomplete", plan.Path)
	}
	finalize := func() error {
		root, err := os.OpenRoot(projectDirectory)
		if err != nil {
			return err
		}
		defer root.Close()
		for _, directory := range plan.directories {
			if err := root.Remove(directory); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove emptied vendor directory %q: %w", directory, err)
			}
		}
		workspace := path.Dir(plan.Path)
		for _, directory := range []string{workspace, ".agents/vendor"} {
			if err := root.Remove(directory); err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
				return fmt.Errorf("remove empty vendor parent %q: %w", directory, err)
			}
		}
		return nil
	}
	if len(plan.edits) == 0 {
		return finalize()
	}
	return ApplyFileTransactionWithHooks(projectDirectory, plan.edits, finalize, hooks)
}

func validateVendorTreeRoot(relativeRoot string) error {
	if err := validateRelativePath(relativeRoot); err != nil {
		return fmt.Errorf("invalid vendor tree path %q: %w", relativeRoot, err)
	}
	parts := strings.Split(relativeRoot, "/")
	if len(parts) != 4 || parts[0] != ".agents" || parts[1] != "vendor" || parts[2] == "" || parts[3] == "" {
		return fmt.Errorf("invalid vendor tree path %q; expected .agents/vendor/<workspace>/<package>", relativeRoot)
	}
	return nil
}

func validateVendorRemovalPath(filename string) error {
	if err := validateRelativePath(filename); err != nil {
		return err
	}
	parts := strings.Split(filename, "/")
	if len(parts) < 5 || parts[0] != ".agents" || parts[1] != "vendor" || parts[2] == "" || parts[3] == "" {
		return fmt.Errorf("vendor removal path %q is outside a package tree", filename)
	}
	return nil
}
