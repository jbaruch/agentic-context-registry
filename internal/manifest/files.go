package manifest

import (
	"fmt"
	"io/fs"
	"os"
	"sort"
)

// PackageFiles returns the sorted, package-relative files included in a release.
func PackageFiles(root string, value Manifest) ([]string, error) {
	if err := Validate(root, value); err != nil {
		return nil, err
	}

	return collectPackageFiles(root, value)
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
		skillFiles, err := collectSkillFilesFS(packageFS, skill.Path)
		if err != nil {
			return nil, err
		}
		for _, skillFile := range skillFiles {
			files[skillFile] = struct{}{}
		}
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
	var files []string
	err := fs.WalkDir(packageFS, relative, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk skill directory %q: %w", relative, walkErr)
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
