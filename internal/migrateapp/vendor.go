package migrateapp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/diagnostic"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

var hashVendorTree = dependency.HashVendorTree

type vendorCollisionError struct {
	Destination string
}

func (err *vendorCollisionError) Error() string {
	return fmt.Sprintf("vendor destination %q already exists with different content", err.Destination)
}

func planVendorInstalls(projectDirectory string, plans []migrate.VendorPlan) ([]realize.VendorTreeInstall, error) {
	if len(plans) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return nil, fmt.Errorf("open project root: %w", diagnostic.OpaqueError(err))
	}
	defer root.Close()
	var installs []realize.VendorTreeInstall
	for _, plan := range plans {
		if err := realize.ValidateParentDirectories(root, plan.Destination); err != nil {
			return nil, err
		}
		if _, err := root.Lstat(plan.Destination); err == nil {
			if err := verifyVendorDestination(projectDirectory, plan); err != nil {
				return nil, err
			}
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("inspect vendor destination %q: %w", plan.Destination, diagnostic.OpaqueError(err))
		}
		installs = append(installs, vendorTreeCopy(plan))
	}
	return installs, nil
}

func vendorTreeCopy(plan migrate.VendorPlan) realize.VendorTreeInstall {
	files := make([]realize.VendorTreeFile, 0, len(plan.Files))
	for _, file := range plan.Files {
		files = append(files, realize.VendorTreeFile{Path: file.Path, Content: file.Content, Mode: file.Mode})
	}
	return realize.VendorTreeInstall{Path: plan.Destination, Files: files}
}

func verifyVendorDestination(projectDirectory string, plan migrate.VendorPlan) error {
	hash, err := hashVendorTree(filepath.Join(projectDirectory, filepath.FromSlash(plan.Destination)))
	if err != nil {
		return fmt.Errorf("verify existing vendor destination %q: %w", plan.Destination, err)
	}
	if hash != plan.ContentHash {
		return &vendorCollisionError{Destination: plan.Destination}
	}
	return nil
}

// A failed supersede can require compensation after realization has committed.
// Limit removal to newly installed trees whose content still matches the plan.
func removeNewVendorTrees(projectDirectory string, installs []realize.VendorTreeInstall, plans []migrate.VendorPlan) error {
	destinations := make(map[string]bool, len(installs))
	for _, install := range installs {
		destinations[install.Path] = true
	}
	var removals []realize.VendorTreeRemovalPlan
	for _, plan := range plans {
		if !destinations[plan.Destination] {
			continue
		}
		if err := verifyVendorDestination(projectDirectory, plan); err != nil {
			return err
		}
		removal, err := realize.PlanVendorTreeRemoval(projectDirectory, plan.Destination)
		if err != nil {
			return err
		}
		removals = append(removals, removal)
	}
	return realize.ApplyVendorTreeRemovals(projectDirectory, removals)
}
