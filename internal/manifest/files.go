package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/tarball"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// ListFiles returns the provider's leaf paths for one validated declaration.
// Directory requests select descendants. An absent required leaf returns no
// paths, so selection retains the authored declaration for local diagnostics.
// The (".", true) query returns package-root leaves for implicit license selection;
// providers may return descendants, which selection ignores for this query.
type ListFiles func(relative string, wantDirectory bool) ([]string, error)

// ContentFiles returns the historical v1 content-hash closure: the manifest and
// declared artifacts. Implicit root licenses belong only to the distribution.
func ContentFiles(root string, value Manifest) ([]string, error) {
	if err := Validate(root, value); err != nil {
		return nil, err
	}
	return collectContentFilesFS(os.DirFS(root), value)
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
		files, err := collectPackageFilesFS(os.DirFS(root), value)
		return value, files, err
	}
	files, err := SelectListed(value, list, func(name string) (fs.FileMode, error) {
		info, err := fs.Lstat(os.DirFS(root), name)
		if err != nil {
			return 0, err
		}
		return info.Mode(), nil
	})
	return value, files, err
}

// SelectListed applies declarations and implicit root-license inclusion, exact
// deduplication and ordering. Callers validate the manifest before selection.
func SelectListed(value Manifest, list ListFiles, modeOf func(string) (fs.FileMode, error)) ([]string, error) {
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
	licenses, err := list(".", true)
	if err != nil {
		return nil, err
	}
	for _, name := range licenses {
		license, err := CheckRootLicense(name, licenses, modeOf)
		if err != nil {
			return nil, err
		}
		if license {
			files[name] = struct{}{}
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

func collectPackageFilesFS(packageFS fs.FS, value Manifest) ([]string, error) {
	files, err := collectContentFilesFS(packageFS, value)
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(packageFS, ".")
	if err != nil {
		return nil, fmt.Errorf("list package root for licenses: %w", err)
	}
	selected := make(map[string]struct{}, len(files))
	for _, name := range files {
		selected[name] = struct{}{}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	for _, name := range names {
		license, err := CheckRootLicense(name, names, func(name string) (fs.FileMode, error) {
			info, err := fs.Lstat(packageFS, name)
			if err != nil {
				return 0, err
			}
			return info.Mode(), nil
		})
		if err != nil {
			return nil, err
		}
		if license {
			if _, exists := selected[name]; !exists {
				files = append(files, name)
			}
		}
	}

	sort.Strings(files)
	return files, nil
}

// CheckRootLicense decides whether a root entry is a shippable implicit license.
// Nested or unrelated names return false. Matching names must satisfy the same
// lexical contract as archive entries, be regular files, and have no distinct
// caseless, canonically equivalent sibling. Exact provider repeats are allowed.
func CheckRootLicense(name string, rootNames []string, modeOf func(string) (fs.FileMode, error)) (bool, error) {
	if !rootLicenseName(name) {
		return false, nil
	}
	invalidPath := func(reason string) (bool, error) {
		return false, &ValidationErrors{Issues: []ValidationError{{Code: CodeInvalidPath, Field: name,
			Message: fmt.Sprintf("root license %q %s; rename it to a unique normalized root name such as LICENSE-MIT, then commit and retag before publishing", name, reason)}}}
	}
	if err := tarball.ValidateName(name); err != nil {
		return invalidPath("is not normalized relative POSIX syntax (no backslashes, NUL, absolute paths or dot segments)")
	}
	key := norm.NFD.String(cases.Fold().String(norm.NFD.String(name)))
	for _, other := range rootNames {
		if other != name && rootLicenseName(other) && norm.NFD.String(cases.Fold().String(norm.NFD.String(other))) == key {
			return invalidPath(fmt.Sprintf("collides with %q under Unicode normalization and case folding", other))
		}
	}
	mode, err := modeOf(name)
	code := CodeInvalidArtifactType
	var message string
	switch {
	case errors.Is(err, fs.ErrNotExist):
		code = CodePathNotFound
		message = fmt.Sprintf("root license %q is missing; ACR automatically includes root license files; restore the file or commit its deletion before retrying", name)
	case err != nil:
		message = fmt.Sprintf("cannot inspect root license %q: %v; ACR automatically includes root license files; make the file accessible before retrying", name, err)
	case mode&fs.ModeSymlink != 0:
		message = fmt.Sprintf("root license %q is a symbolic link; ACR automatically includes root license files; replace it with a regular-file copy or rename it so it no longer matches LICENSE*, LICENCE*, COPYING*, or UNLICENSE*", name)
	case !mode.IsRegular():
		message = fmt.Sprintf("root license %q is not a regular file; ACR automatically includes root license files; replace it with a regular file or rename it so it no longer matches LICENSE*, LICENCE*, COPYING*, or UNLICENSE*", name)
	default:
		return true, nil
	}
	return false, &ValidationErrors{Issues: []ValidationError{{Code: code, Field: name, Message: message}}}
}

func rootLicenseName(name string) bool {
	if strings.Contains(name, "/") {
		return false
	}
	upper := []byte(name)
	for i, ch := range upper {
		if ch >= 'a' && ch <= 'z' {
			upper[i] = ch - ('a' - 'A')
		}
	}
	for _, prefix := range []string{"LICENSE", "LICENCE", "COPYING", "UNLICENSE"} {
		if strings.HasPrefix(string(upper), prefix) {
			return true
		}
	}
	return false
}

func collectContentFilesFS(packageFS fs.FS, value Manifest) ([]string, error) {
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
