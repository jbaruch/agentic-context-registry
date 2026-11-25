package realize

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
)

const legacyVendorPrefix = ".acr-vendor-"

var errLegacyVendorMismatch = errors.New("tree does not match a legacy vendor package")

type legacyVendorShape struct {
	files    map[string]VendorTreeFile
	children map[string]int
}

func isLegacyVendorPath(filename string) bool {
	if filename != path.Join(".agents", path.Base(filename)) {
		return false
	}
	suffix, ok := strings.CutPrefix(path.Base(filename), legacyVendorPrefix)
	value, err := strconv.ParseUint(suffix, 10, 32)
	return ok && err == nil && suffix == strconv.FormatUint(value, 10)
}

// PlanLegacyVendorRetirements recognizes complete private stages made by the
// old per-package writer. An exact match is bounded by a current copy plan.
// Ambiguous names, shapes and contents are left for the operator to inspect.
func PlanLegacyVendorRetirements(projectDirectory string, packages []VendorTreeInstall) (retirements []VendorTreeRetirement, ambiguous []string, err error) {
	if len(packages) == 0 {
		return nil, nil, nil
	}
	root, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return nil, nil, WrapProjectAccessError(projectDirectory, "open project directory for legacy vendor staging", err)
	}
	defer root.Close()
	entries, err := readVendorRemovalDirectory(root, ".agents", -1)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("inspect legacy vendor staging: %w", err)
	}
stages:
	for _, entry := range entries {
		filename := path.Join(".agents", entry.Name())
		if !isLegacyVendorPath(filename) {
			continue
		}
		if !entry.IsDir() {
			ambiguous = append(ambiguous, filename)
			continue
		}
		for _, pkg := range packages {
			if len(pkg.Files) == 0 {
				continue
			}
			expected := legacyVendorShape{files: make(map[string]VendorTreeFile, len(pkg.Files)), children: map[string]int{filename: 0}}
			for _, file := range pkg.Files {
				target := path.Join(filename, file.Path)
				expected.files[target] = file
				for directory := path.Dir(target); ; directory = path.Dir(directory) {
					count, known := expected.children[directory]
					expected.children[directory] = count + 1
					if known {
						break
					}
				}
			}
			hash, err := fingerprintVendorTree(root, filename, false, &expected)
			if errors.Is(err, errLegacyVendorMismatch) {
				continue
			}
			if err != nil {
				return nil, nil, fmt.Errorf("verify legacy vendor stage %q: %w", filename, err)
			}
			retirements = append(retirements, VendorTreeRetirement{path: filename, hash: hash})
			continue stages
		}
		ambiguous = append(ambiguous, filename)
	}
	return retirements, ambiguous, nil
}
