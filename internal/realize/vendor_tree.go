package realize

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
)

var vendorTreeSnapshotAfterOpen = func(string) {}

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

	if err := walkVendorRemovalTree(projectRoot, relativeRoot, &plan); err != nil {
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

func walkVendorRemovalTree(root *os.Root, directory string, plan *VendorTreeRemovalPlan) error {
	entries, err := readVendorRemovalDirectory(root, directory)
	if err != nil {
		return err
	}
	plan.directories = append(plan.directories, directory)
	for _, entry := range entries {
		relative := path.Join(directory, entry.Name())
		info, err := root.Lstat(relative)
		if err != nil {
			return fmt.Errorf("inspect vendor tree entry %q: %w", relative, err)
		}
		switch {
		case info.IsDir() && info.Mode()&fs.ModeSymlink == 0:
			if err := walkVendorRemovalTree(root, relative, plan); err != nil {
				return err
			}
		case info.Mode()&fs.ModeSymlink != 0:
			target, mode, err := snapshotVendorSymlink(root, relative)
			if err != nil {
				return err
			}
			plan.edits = append(plan.edits, FileTransactionEdit{Path: relative, Operation: "vendor-remove", BeforeMode: uint32(mode.Perm()), LinkTarget: target})
		case info.Mode().IsRegular():
			content, mode, err := snapshotVendorRemovalFile(root, relative)
			if err != nil {
				return err
			}
			plan.edits = append(plan.edits, FileTransactionEdit{Path: relative, Operation: "vendor-remove", BeforeMode: uint32(mode.Perm()), Before: content})
		default:
			return fmt.Errorf("vendor tree entry %q must be a regular file or symbolic link", relative)
		}
	}
	return nil
}

func readVendorRemovalDirectory(root *os.Root, directory string) (entries []fs.DirEntry, err error) {
	if err := ValidateParentDirectories(root, directory); err != nil {
		return nil, err
	}
	info, err := root.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect vendor directory %q: %w", directory, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("vendor directory %q must be a regular directory", directory)
	}
	dir, err := root.Open(directory)
	if err != nil {
		return nil, fmt.Errorf("open vendor directory %q: %w", directory, err)
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close vendor directory %q: %w", directory, closeErr))
		}
	}()
	opened, err := dir.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened vendor directory %q: %w", directory, err)
	}
	vendorTreeSnapshotAfterOpen(directory)
	current, err := root.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect vendor directory %q after opening: %w", directory, err)
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(opened, current) {
		return nil, fmt.Errorf("vendor directory %q changed while being opened; keep it stable and retry", directory)
	}
	entries, err = dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("read vendor directory %q: %w", directory, err)
	}
	openedAfter, err := dir.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened vendor directory %q after reading: %w", directory, err)
	}
	current, err = root.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect vendor directory %q after reading: %w", directory, err)
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(openedAfter, current) || vendorTreeMetadataChanged(opened, openedAfter) || vendorTreeMetadataChanged(openedAfter, current) {
		return nil, fmt.Errorf("vendor directory %q changed while being read; keep it stable and retry", directory)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func snapshotVendorRemovalFile(root *os.Root, relative string) (content []byte, mode fs.FileMode, err error) {
	if err := ValidateParentDirectories(root, relative); err != nil {
		return nil, 0, err
	}
	info, err := root.Lstat(relative)
	if err != nil {
		return nil, 0, fmt.Errorf("inspect vendor file %q: %w", relative, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("vendor file %q must be a regular file", relative)
	}
	file, err := root.Open(relative)
	if err != nil {
		return nil, 0, fmt.Errorf("open vendor file %q: %w", relative, err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close vendor file %q: %w", relative, closeErr))
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("inspect opened vendor file %q: %w", relative, err)
	}
	vendorTreeSnapshotAfterOpen(relative)
	current, err := root.Lstat(relative)
	if err != nil {
		return nil, 0, fmt.Errorf("inspect vendor file %q after opening: %w", relative, err)
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return nil, 0, fmt.Errorf("vendor file %q changed while being opened; keep it stable and retry", relative)
	}
	content, err = io.ReadAll(file)
	if err != nil {
		return nil, 0, fmt.Errorf("read vendor file %q: %w", relative, err)
	}
	openedAfter, err := file.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("inspect opened vendor file %q after reading: %w", relative, err)
	}
	current, err = root.Lstat(relative)
	if err != nil {
		return nil, 0, fmt.Errorf("inspect vendor file %q after reading: %w", relative, err)
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(openedAfter, current) || vendorTreeMetadataChanged(opened, openedAfter) || vendorTreeMetadataChanged(openedAfter, current) {
		return nil, 0, fmt.Errorf("vendor file %q changed while being read; keep it stable and retry", relative)
	}
	return content, current.Mode(), nil
}

func snapshotVendorSymlink(root *os.Root, relative string) (string, fs.FileMode, error) {
	if err := ValidateParentDirectories(root, relative); err != nil {
		return "", 0, err
	}
	info, err := root.Lstat(relative)
	if err != nil {
		return "", 0, fmt.Errorf("inspect vendor symlink %q: %w", relative, err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return "", 0, fmt.Errorf("vendor entry %q changed before reading; keep it stable and retry", relative)
	}
	target, err := root.Readlink(relative)
	if err != nil {
		return "", 0, fmt.Errorf("read vendor symlink %q: %w", relative, err)
	}
	current, err := root.Lstat(relative)
	if err != nil {
		return "", 0, fmt.Errorf("inspect vendor symlink %q after reading: %w", relative, err)
	}
	if current.Mode()&fs.ModeSymlink == 0 || !os.SameFile(info, current) || vendorTreeMetadataChanged(info, current) {
		return "", 0, fmt.Errorf("vendor symlink %q changed while being read; keep it stable and retry", relative)
	}
	return target, current.Mode(), nil
}

func vendorTreeMetadataChanged(before, after fs.FileInfo) bool {
	return before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode()
}

// ApplyVendorTreeRemoval removes a previously snapshotted vendor tree through
// the durable file journal.
func ApplyVendorTreeRemoval(projectDirectory string, plan VendorTreeRemovalPlan) error {
	return ApplyVendorTreeRemovals(projectDirectory, []VendorTreeRemovalPlan{plan})
}

// ApplyVendorTreeRemovals removes every snapshotted vendor tree through one
// durable transaction so a failure restores all of them together.
func ApplyVendorTreeRemovals(projectDirectory string, plans []VendorTreeRemovalPlan) error {
	return applyVendorTreeRemovalsWithHooks(projectDirectory, plans, FileTransactionHooks{})
}

func applyVendorTreeRemovalWithHooks(projectDirectory string, plan VendorTreeRemovalPlan, hooks FileTransactionHooks) error {
	return applyVendorTreeRemovalsWithHooks(projectDirectory, []VendorTreeRemovalPlan{plan}, hooks)
}

func applyVendorTreeRemovalsWithHooks(projectDirectory string, plans []VendorTreeRemovalPlan, hooks FileTransactionHooks) error {
	if len(plans) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(plans))
	var edits []FileTransactionEdit
	for _, plan := range plans {
		if err := validateVendorTreeRoot(plan.Path); err != nil {
			return err
		}
		if _, exists := seen[plan.Path]; exists {
			return fmt.Errorf("vendor tree removal plan for %q is repeated", plan.Path)
		}
		seen[plan.Path] = struct{}{}
		if plan.Files != len(plan.edits) {
			return fmt.Errorf("vendor tree removal plan for %q is incomplete", plan.Path)
		}
		edits = append(edits, plan.edits...)
	}
	finalize := func() error {
		root, err := os.OpenRoot(projectDirectory)
		if err != nil {
			return err
		}
		defer root.Close()
		for _, plan := range plans {
			for _, directory := range plan.directories {
				if err := root.Remove(directory); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("remove emptied vendor directory %q: %w", directory, err)
				}
			}
		}
		workspaces := make([]string, 0, len(plans))
		for _, plan := range plans {
			workspaces = append(workspaces, path.Dir(plan.Path))
		}
		sort.Strings(workspaces)
		workspaces = append(workspaces, ".agents/vendor")
		for index, directory := range workspaces {
			if index != 0 && directory == workspaces[index-1] {
				continue
			}
			if err := root.Remove(directory); err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
				return fmt.Errorf("remove empty vendor parent %q: %w", directory, err)
			}
		}
		return nil
	}
	if len(edits) == 0 {
		return finalize()
	}
	return ApplyFileTransactionWithHooks(projectDirectory, edits, finalize, hooks)
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
