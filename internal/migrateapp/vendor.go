package migrateapp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
)

var hashVendorTree = dependency.HashVendorTree

type vendorCollisionError struct {
	Destination string
}

func (err *vendorCollisionError) Error() string {
	return fmt.Sprintf("vendor destination %q already exists with different content", err.Destination)
}

func (service *Service) applyVendorPlans(projectDirectory string, plans []migrate.VendorPlan) (bool, []func() error, error) {
	var rollbacks []func() error
	changed := false
	for _, plan := range plans {
		planChanged, rollback, err := service.applyVendor(projectDirectory, plan)
		if err != nil {
			rollbackErr := rollbackVendors(rollbacks)
			if rollbackErr != nil {
				rollbackErr = fmt.Errorf("roll back previously staged vendor trees: %w", rollbackErr)
			}
			return false, nil, errors.Join(classifyVendorError(err), rollbackErr)
		}
		changed = changed || planChanged
		rollbacks = append(rollbacks, rollback)
	}
	return changed, rollbacks, nil
}

// applyVendorPlan stages an entire package and promotes it with one rename.
// The returned rollback is intentionally separate from realization so callers
// can unwind the two transactions in reverse order.
func applyVendorPlan(projectDirectory string, plan migrate.VendorPlan) (changed bool, rollback func() error, err error) {
	agentsDirectory := filepath.Join(projectDirectory, ".agents")
	if err := os.MkdirAll(agentsDirectory, 0o755); err != nil {
		return false, nil, fmt.Errorf("create vendor parent: %w", err)
	}
	staging, err := os.MkdirTemp(agentsDirectory, ".acr-vendor-")
	if err != nil {
		return false, nil, fmt.Errorf("create vendor staging directory: %w", err)
	}
	stagingName := filepath.ToSlash(strings.TrimPrefix(staging, projectDirectory+string(filepath.Separator)))
	removeStaging := func() error { return os.RemoveAll(staging) }
	defer func() {
		if err != nil {
			err = errors.Join(err, removeStaging())
		}
	}()
	stagingRoot, err := os.OpenRoot(staging)
	if err != nil {
		return false, nil, fmt.Errorf("open vendor staging root: %w", err)
	}
	for _, file := range plan.Files {
		if err := stagingRoot.MkdirAll(path.Dir(file.Path), 0o755); err != nil {
			stagingRoot.Close()
			return false, nil, fmt.Errorf("create vendor directory for %q: %w", file.Path, err)
		}
		opened, openErr := stagingRoot.OpenFile(file.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, file.Mode)
		if openErr != nil {
			stagingRoot.Close()
			return false, nil, fmt.Errorf("create vendor file %q: %w", file.Path, openErr)
		}
		_, writeErr := opened.Write(file.Content)
		chmodErr := opened.Chmod(file.Mode)
		closeErr := opened.Close()
		if writeErr != nil || chmodErr != nil || closeErr != nil {
			stagingRoot.Close()
			return false, nil, fmt.Errorf("write vendor file %q: %w", file.Path, errors.Join(writeErr, chmodErr, closeErr))
		}
	}
	if err := stagingRoot.Close(); err != nil {
		return false, nil, fmt.Errorf("close vendor staging root: %w", err)
	}
	projectRoot, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return false, nil, fmt.Errorf("open project root: %w", err)
	}
	defer projectRoot.Close()
	if _, err := projectRoot.Lstat(plan.Destination); err == nil {
		existingHash, hashErr := hashVendorTree(filepath.Join(projectDirectory, filepath.FromSlash(plan.Destination)))
		if hashErr != nil {
			return false, nil, fmt.Errorf("verify existing vendor destination %q: %w", plan.Destination, hashErr)
		}
		if existingHash != plan.ContentHash {
			return false, nil, &vendorCollisionError{Destination: plan.Destination}
		}
		if removeErr := removeStaging(); removeErr != nil {
			return false, nil, fmt.Errorf("remove redundant vendor staging directory: %w", removeErr)
		}
		return false, func() error { return nil }, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, nil, fmt.Errorf("inspect vendor destination %q: %w", plan.Destination, err)
	}
	if err := projectRoot.MkdirAll(path.Dir(plan.Destination), 0o755); err != nil {
		return false, nil, fmt.Errorf("create vendor destination parent: %w", err)
	}
	if err := projectRoot.Rename(stagingName, plan.Destination); err != nil {
		return false, nil, fmt.Errorf("promote vendor package: %w", err)
	}
	return true, func() error {
		target := filepath.Join(projectDirectory, filepath.FromSlash(plan.Destination))
		return os.RemoveAll(target)
	}, nil
}
