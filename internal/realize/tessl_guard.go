package realize

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// TesslOwnedTargetError reports a realization operation that would mutate a
// path whose ownership is established by a live Tessl manifest.
type TesslOwnedTargetError struct {
	Path string
}

func (err *TesslOwnedTargetError) Error() string {
	return fmt.Sprintf("realization plan targets Tessl-owned path %q; leave that path unchanged and move ACR-managed content to a non-Tessl host before retrying", err.Path)
}

// ValidateTesslOwnedTargets rejects operations that target Tessl-owned path
// prefixes. Callers use it only after a regular tessl.json establishes live
// Tessl ownership.
func ValidateTesslOwnedTargets(plan Plan) error {
	for _, operation := range plan.Operations {
		filename := path.Clean(filepath.ToSlash(operation.Path))
		if tesslOwnedTarget(filename) {
			return &TesslOwnedTargetError{Path: filename}
		}
	}
	return nil
}

func liveTesslManifest(projectDirectory string) (bool, error) {
	info, err := os.Lstat(filepath.Join(projectDirectory, "tessl.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect Tessl ownership manifest: %w", err)
	}
	return info.Mode().IsRegular(), nil
}

func tesslOwnedTarget(filename string) bool {
	if filename == ".tessl" || strings.HasPrefix(filename, ".tessl/") {
		return true
	}
	for _, component := range strings.Split(filename, "/") {
		if strings.HasPrefix(component, "tessl__") {
			return true
		}
	}
	return false
}
