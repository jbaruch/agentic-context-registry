package manifest

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
)

// PackageFiles returns the sorted, package-relative files included in a release.
func PackageFiles(root string, value Manifest) ([]string, error) {
	if err := Validate(root, value); err != nil {
		return nil, err
	}

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
		skillFiles, err := collectSkillFiles(root, skill.Path)
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
	skillRoot := filepath.Join(root, filepath.FromSlash(relative))
	var files []string
	err := filepath.WalkDir(skillRoot, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk skill directory %q: %w", relative, walkErr)
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			entryRelative, err := filepath.Rel(root, current)
			if err != nil {
				return fmt.Errorf("resolve skill entry %q: %w", current, err)
			}
			return fmt.Errorf("skill %q contains symbolic link %q; replace it with a regular file or directory", relative, filepath.ToSlash(entryRelative))
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect skill entry %q: %w", current, err)
		}
		if !info.Mode().IsRegular() {
			entryRelative, err := filepath.Rel(root, current)
			if err != nil {
				return fmt.Errorf("resolve skill entry %q: %w", current, err)
			}
			return fmt.Errorf("skill %q contains non-regular file %q; keep only regular files and directories", relative, filepath.ToSlash(entryRelative))
		}
		entryRelative, err := filepath.Rel(root, current)
		if err != nil {
			return fmt.Errorf("resolve skill entry %q: %w", current, err)
		}
		files = append(files, filepath.ToSlash(entryRelative))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}
