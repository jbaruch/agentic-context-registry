package migrateapp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/migrate"
)

// applyVendorPlan stages an entire package and promotes it with one rename.
// The returned rollback is intentionally separate from realization so callers
// can unwind the two transactions in reverse order.
func applyVendorPlan(projectDirectory string, plan migrate.VendorPlan) (rollback func() error, err error) {
	agentsDirectory := filepath.Join(projectDirectory, ".agents")
	if err := os.MkdirAll(agentsDirectory, 0o755); err != nil {
		return nil, fmt.Errorf("create vendor parent: %w", err)
	}
	staging, err := os.MkdirTemp(agentsDirectory, ".acr-vendor-")
	if err != nil {
		return nil, fmt.Errorf("create vendor staging directory: %w", err)
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
		return nil, fmt.Errorf("open vendor staging root: %w", err)
	}
	for _, file := range plan.Files {
		if err := stagingRoot.MkdirAll(path.Dir(file.Path), 0o755); err != nil {
			stagingRoot.Close()
			return nil, fmt.Errorf("create vendor directory for %q: %w", file.Path, err)
		}
		opened, openErr := stagingRoot.OpenFile(file.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, file.Mode)
		if openErr != nil {
			stagingRoot.Close()
			return nil, fmt.Errorf("create vendor file %q: %w", file.Path, openErr)
		}
		_, writeErr := opened.Write(file.Content)
		chmodErr := opened.Chmod(file.Mode)
		closeErr := opened.Close()
		if writeErr != nil || chmodErr != nil || closeErr != nil {
			stagingRoot.Close()
			return nil, fmt.Errorf("write vendor file %q: %w", file.Path, errors.Join(writeErr, chmodErr, closeErr))
		}
	}
	if err := stagingRoot.Close(); err != nil {
		return nil, fmt.Errorf("close vendor staging root: %w", err)
	}
	projectRoot, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return nil, fmt.Errorf("open project root: %w", err)
	}
	defer projectRoot.Close()
	if _, err := projectRoot.Lstat(plan.Destination); err == nil {
		return nil, fmt.Errorf("vendor destination %q already exists", plan.Destination)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("inspect vendor destination %q: %w", plan.Destination, err)
	}
	if err := projectRoot.MkdirAll(path.Dir(plan.Destination), 0o755); err != nil {
		return nil, fmt.Errorf("create vendor destination parent: %w", err)
	}
	if err := projectRoot.Rename(stagingName, plan.Destination); err != nil {
		return nil, fmt.Errorf("promote vendor package: %w", err)
	}
	return func() error {
		target := filepath.Join(projectDirectory, filepath.FromSlash(plan.Destination))
		return os.RemoveAll(target)
	}, nil
}
