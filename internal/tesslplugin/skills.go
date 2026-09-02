package tesslplugin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

func expandRules(root *os.Root, spec PathSpec) ([]NamedPath, error) {
	switch spec.Kind {
	case PathSpecEmpty:
		return nil, nil
	case PathSpecList:
		result := make([]NamedPath, 0, len(spec.List))
		for _, relative := range spec.List {
			if err := validateEmittedPath(relative); err != nil {
				return nil, err
			}
			result = append(result, NamedPath{Path: relative})
		}
		return result, nil
	case PathSpecNamed:
		result := make([]NamedPath, 0, len(spec.Named))
		for _, named := range spec.Named {
			if err := validateEmittedPath(named.Path); err != nil {
				return nil, err
			}
			result = append(result, named)
		}
		return result, nil
	case PathSpecDirectory:
		return expandRuleDirectory(root, spec.Directory)
	default:
		return nil, fmt.Errorf("internal error: unknown rules path spec")
	}
}

func expandSkills(root *os.Root, spec PathSpec) ([]NamedPath, error) {
	switch spec.Kind {
	case PathSpecEmpty:
		return nil, nil
	case PathSpecList:
		result := make([]NamedPath, 0, len(spec.List))
		for _, relative := range spec.List {
			if err := validateEmittedPath(relative); err != nil {
				return nil, err
			}
			result = append(result, NamedPath{Path: relative})
		}
		return result, nil
	case PathSpecNamed:
		result := make([]NamedPath, 0, len(spec.Named))
		for _, named := range spec.Named {
			if err := validateEmittedPath(named.Path); err != nil {
				return nil, err
			}
			result = append(result, NamedPath{ID: named.ID, Path: skillDirectory(named.Path)})
		}
		return result, nil
	case PathSpecDirectory:
		return expandSkillDirectory(root, spec.Directory)
	default:
		return nil, fmt.Errorf("internal error: unknown skills path spec")
	}
}

func expandRuleDirectory(root *os.Root, directory string) ([]NamedPath, error) {
	dir := strings.TrimSuffix(directory, "/")
	entries, err := fs.ReadDir(root.FS(), dir)
	if err != nil {
		return nil, fmt.Errorf("read rule directory %s: %w", dir, err)
	}
	var names []string
	for _, entry := range entries {
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil, conversionError("invalid_artifact_type", path.Join(dir, entry.Name()),
				"rule directory %s contains a symbolic link; replace it with a regular Markdown file", dir)
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		names = append(names, path.Join(dir, entry.Name()))
	}
	sort.Strings(names)
	result := make([]NamedPath, 0, len(names))
	for _, relative := range names {
		result = append(result, NamedPath{Path: relative})
	}
	return result, nil
}

func expandSkillDirectory(root *os.Root, directory string) ([]NamedPath, error) {
	dir := strings.TrimSuffix(directory, "/")
	present, err := hasSkillMarkdown(root, dir)
	if err != nil {
		return nil, err
	}
	if present {
		return []NamedPath{{Path: dir}}, nil
	}
	entries, err := fs.ReadDir(root.FS(), dir)
	if err != nil {
		return nil, fmt.Errorf("read skill directory %s: %w", dir, err)
	}
	var names []string
	for _, entry := range entries {
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil, conversionError("invalid_artifact_type", path.Join(dir, entry.Name()),
				"skill directory %s contains a symbolic link; replace it with a regular directory", dir)
		}
		if !entry.IsDir() {
			continue
		}
		relative := path.Join(dir, entry.Name())
		present, err := hasSkillMarkdown(root, relative)
		if err != nil {
			return nil, err
		}
		if present {
			names = append(names, relative)
		}
	}
	sort.Strings(names)
	result := make([]NamedPath, 0, len(names))
	for _, relative := range names {
		result = append(result, NamedPath{Path: relative})
	}
	return result, nil
}

func hasSkillMarkdown(root *os.Root, directory string) (bool, error) {
	_, err := root.ReadFile(path.Join(directory, "SKILL.md"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read %s/SKILL.md: %w", directory, err)
	}
	return true, nil
}

func skillDirectory(declared string) string {
	cleaned := path.Clean(declared)
	if path.Base(cleaned) == "SKILL.md" {
		return path.Dir(cleaned)
	}
	return cleaned
}

func basenameID(relative string) string {
	base := path.Base(relative)
	return strings.TrimSuffix(base, path.Ext(base))
}
