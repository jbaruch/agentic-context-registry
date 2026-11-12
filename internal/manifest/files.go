package manifest

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
)

// PathFilter decides whether an entry found beneath a declared skill directory
// belongs to the package. name is the package-relative slash path and isDir
// reports whether the entry is a directory. A nil PathFilter keeps every entry.
// Declared rule, script and hook paths are explicit, so they never pass through
// the filter.
type PathFilter func(name string, isDir bool) bool

// PackageFiles returns the sorted, package-relative files included in a release.
func PackageFiles(root string, value Manifest) ([]string, error) {
	return PackageFilesFiltered(root, value, nil)
}

// PackageFilesFiltered is PackageFiles with entries under skill directories
// limited to those the filter keeps. Validation walks the same selection, so an
// excluded entry can neither appear in the result nor fail the skill tree check.
func PackageFilesFiltered(root string, value Manifest, filter PathFilter) ([]string, error) {
	packageFS := os.DirFS(root)
	if err := validateFS(packageFS, value, true, filter); err != nil {
		return nil, err
	}

	return collectPackageFilesFiltered(packageFS, value, filter)
}

// PlannedPackageFiles selects the same distribution files before manifest creation.
func PlannedPackageFiles(root string, value Manifest) ([]string, error) {
	return PlannedPackageFilesFS(os.DirFS(root), value)
}

// PlannedPackageFilesFS validates and walks the caller's opened package image.
func PlannedPackageFilesFS(packageFS fs.FS, value Manifest) ([]string, error) {
	if err := ValidatePlannedFS(packageFS, value); err != nil {
		return nil, err
	}
	return collectPackageFilesFS(packageFS, value)
}

func collectPackageFiles(root string, value Manifest) ([]string, error) {
	return collectPackageFilesFS(os.DirFS(root), value)
}

func collectPackageFilesFS(packageFS fs.FS, value Manifest) ([]string, error) {
	return collectPackageFilesFiltered(packageFS, value, nil)
}

func collectPackageFilesFiltered(packageFS fs.FS, value Manifest, filter PathFilter) ([]string, error) {
	files := map[string]struct{}{Filename: {}}
	for _, rule := range value.Artifacts.Rules {
		files[rule.Path] = struct{}{}
	}
	for _, script := range value.Artifacts.Scripts {
		files[script.Path] = struct{}{}
	}
	for _, hook := range value.Artifacts.Hooks {
		files[hook.Path] = struct{}{}
	}
	for _, skill := range value.Artifacts.Skills {
		skillFiles, err := collectSkillFilesFiltered(packageFS, skill.Path, filter)
		if err != nil {
			return nil, err
		}
		for _, skillFile := range skillFiles {
			files[skillFile] = struct{}{}
		}
		// SKILL.md is required and validated on disk; like the other declared
		// files it stays in the selection even when the filter would hide it.
		files[path.Join(skill.Path, "SKILL.md")] = struct{}{}
	}

	result := make([]string, 0, len(files))
	for file := range files {
		result = append(result, file)
	}
	sort.Strings(result)
	return result, nil
}

func collectSkillFiles(root, relative string) ([]string, error) {
	return collectSkillFilesFS(os.DirFS(root), relative)
}

func collectSkillFilesFS(packageFS fs.FS, relative string) ([]string, error) {
	return collectSkillFilesFiltered(packageFS, relative, nil)
}

func collectSkillFilesFiltered(packageFS fs.FS, relative string, filter PathFilter) ([]string, error) {
	var files []string
	err := fs.WalkDir(packageFS, relative, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk skill directory %q: %w", relative, walkErr)
		}
		if current != relative && filter != nil && !filter(current, entry.IsDir()) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("skill %q contains symbolic link %q; replace it with a regular file or directory", relative, current)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect skill entry %q: %w", current, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("skill %q contains non-regular file %q; keep only regular files and directories", relative, current)
		}
		files = append(files, current)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}
