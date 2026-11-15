package manifest

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
)

// ListFiles returns the provider's leaf paths for one validated declaration.
// Directory requests select descendants. An absent required leaf returns no
// paths, so selection retains the authored declaration for local diagnostics.
type ListFiles func(relative string, wantDirectory bool) ([]string, error)

// PackageFiles returns the sorted, package-relative files included in a release.
func PackageFiles(root string, value Manifest) ([]string, error) {
	if err := Validate(root, value); err != nil {
		return nil, err
	}
	return collectPackageFiles(root, value)
}

// LoadListed validates only provider-selected local files and returns their
// distribution inventory. A nil provider preserves the full filesystem walk.
func LoadListed(root string, list ListFiles) (Manifest, []string, error) {
	contents, err := readManifest(root)
	if err != nil {
		return Manifest{}, nil, err
	}
	value, err := decodeManifest(contents, root)
	if err != nil {
		return Manifest{}, nil, err
	}
	if list != nil {
		// Use one inventory per declaration for both local checks and the
		// returned distribution, even if the worktree changes during loading.
		provider := list
		type selection struct {
			path      string
			directory bool
		}
		inventory := map[selection][]string{}
		list = func(relative string, directory bool) ([]string, error) {
			key := selection{relative, directory}
			if paths, found := inventory[key]; found {
				return paths, nil
			}
			paths, err := provider(relative, directory)
			if err != nil {
				return nil, err
			}
			inventory[key] = paths
			return paths, nil
		}
	}
	if err := validateFS(os.DirFS(root), value, true, list); err != nil {
		return Manifest{}, nil, err
	}
	if list == nil {
		files, err := collectPackageFiles(root, value)
		return value, files, err
	}
	files, err := SelectListed(value, list)
	return value, files, err
}

// SelectListed applies declaration semantics, exact deduplication and ordering
// to a provider's inventory. Callers validate the manifest before selection.
func SelectListed(value Manifest, list ListFiles) ([]string, error) {
	files := map[string]struct{}{}
	selectPath := func(relative string, directory bool) error {
		paths, err := list(relative, directory)
		if err != nil {
			return err
		}
		if !directory && len(paths) == 0 {
			paths = []string{relative}
		}
		for _, name := range paths {
			files[name] = struct{}{}
		}
		return nil
	}
	if err := selectPath(Filename, false); err != nil {
		return nil, err
	}
	for _, rule := range value.Artifacts.Rules {
		if err := selectPath(rule.Path, false); err != nil {
			return nil, err
		}
	}
	for _, script := range value.Artifacts.Scripts {
		if err := selectPath(script.Path, false); err != nil {
			return nil, err
		}
	}
	for _, hook := range value.Artifacts.Hooks {
		if err := selectPath(hook.Path, false); err != nil {
			return nil, err
		}
	}
	for _, skill := range value.Artifacts.Skills {
		if err := selectPath(skill.Path, true); err != nil {
			return nil, err
		}
		if err := selectPath(path.Join(skill.Path, "SKILL.md"), false); err != nil {
			return nil, err
		}
	}
	result := make([]string, 0, len(files))
	for file := range files {
		result = append(result, file)
	}
	sort.Strings(result)
	return result, nil
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
