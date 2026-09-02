package migrate

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

const (
	reasonMissingSkill     = "missing-skill"
	reasonSkillEscape      = "skill-tree-escape"
	reasonNativeDivergence = "native-skill-divergence"
	reasonDuplicateSkill   = "duplicate-native-skill"
)

var skillNativeDirs = []struct {
	dir    string
	prefix string
}{
	{dir: ".claude/skills", prefix: "tessl__"},
	{dir: ".codex/skills", prefix: "tessl__"},
	{dir: ".cursor/skills", prefix: "tessl__"},
	{dir: ".github/skills", prefix: "tessl__"},
	{dir: ".vscode/skills", prefix: "tessl__"},
	{dir: ".agents/skills", prefix: "tessl__"},
	{dir: ".openhands/skills", prefix: "tessl-"},
}

// NormalizedSkill is one Tessl skill directory on the #4 artifact model.
type NormalizedSkill struct {
	ID          string
	Path        string
	Digest      string
	Natives     []string
	ExtraFiles  []string
	Ambiguous   bool
	Unsupported bool
	Reason      string
}

// NormalizeSkills digests the plugin-tree skill directory and records native
// tessl__* paths as evidence, never as a second artifact.
func NormalizeSkills(snapshot adapter.Snapshot, install PackageInstall) ([]NormalizedSkill, error) {
	directories, err := directorySnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	skills := make([]NormalizedSkill, 0, len(install.Skills))
	for _, declared := range install.Skills {
		skill, err := normalizeDeclaredSkill(directories, install, declared)
		if err != nil {
			return nil, err
		}
		skills = append(skills, skill)
	}
	sort.Slice(skills, func(left, right int) bool { return skills[left].ID < skills[right].ID })
	return skills, nil
}

func normalizeDeclaredSkill(snapshot adapter.DirectorySnapshot, install PackageInstall, declared DeclaredPath) (NormalizedSkill, error) {
	skill := NormalizedSkill{ID: declared.ID, Path: declared.Path, Ambiguous: declared.Ambiguous}
	if declared.Ambiguous && skill.Reason == "" {
		skill.Reason = "manifest-disagreement"
	}
	root := posixJoin(install.Root, declared.Path)
	_, present, err := readOptional(snapshot, posixJoin(root, "SKILL.md"))
	if err != nil {
		// Unreadable SKILL.md is the same shape as a missing one: the declared
		// skill is reported as missing-skill, never dropped from the inventory.
		present = false
	}
	if !present {
		skill.Ambiguous = true
		skill.Reason = reasonMissingSkill
		natives, extra, diverged, unsupported, nativeErr := skillNatives(snapshot, declared.ID, root, nil)
		if nativeErr != nil {
			return NormalizedSkill{}, nativeErr
		}
		skill.Natives = natives
		skill.ExtraFiles = extra
		if diverged {
			skill.Ambiguous = true
		}
		if unsupported {
			skill.Unsupported = true
			skill.Reason = reasonSkillEscape
		}
		return skill, nil
	}
	files, escaped, err := readSkillTree(snapshot, root)
	if err != nil {
		return NormalizedSkill{}, err
	}
	if escaped {
		skill.Unsupported = true
		skill.Reason = reasonSkillEscape
		natives, _, _, _, nativeErr := skillNatives(snapshot, declared.ID, root, nil)
		if nativeErr != nil {
			return NormalizedSkill{}, nativeErr
		}
		skill.Natives = natives
		return skill, nil
	}
	skill.Digest = skillDigest(root, files)
	natives, extra, diverged, unsupported, err := skillNatives(snapshot, declared.ID, root, files)
	if err != nil {
		return NormalizedSkill{}, err
	}
	skill.Natives = natives
	skill.ExtraFiles = extra
	if diverged {
		skill.Ambiguous = true
		if skill.Reason == "" {
			skill.Reason = reasonNativeDivergence
		}
	}
	if unsupported {
		skill.Unsupported = true
		skill.Reason = reasonSkillEscape
	}
	return skill, nil
}

func readSkillTree(snapshot adapter.DirectorySnapshot, root string) ([]adapter.ObservedFile, bool, error) {
	entries, err := adapter.WalkSnapshot(snapshot, root)
	if err != nil {
		return nil, false, fmt.Errorf("walk skill tree %q: %w", root, err)
	}
	var files []adapter.ObservedFile
	for _, entry := range entries {
		if entry.Mode&fs.ModeSymlink != 0 {
			return nil, true, nil
		}
		if entry.Mode.IsDir() {
			continue
		}
		if !entry.Mode.IsRegular() {
			return nil, true, nil
		}
		observed, err := snapshot.ReadFile(entry.Path)
		if err != nil {
			return nil, false, fmt.Errorf("read skill file %q: %w", entry.Path, err)
		}
		files = append(files, observed)
	}
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	return files, false, nil
}

func skillDigest(root string, files []adapter.ObservedFile) string {
	var buffer strings.Builder
	prefix := strings.TrimSuffix(root, "/") + "/"
	for _, file := range files {
		relative := strings.TrimPrefix(file.Path, prefix)
		execBit := 0
		if file.Mode.Perm()&0o111 != 0 {
			execBit = 1
		}
		fmt.Fprintf(&buffer, "%s\x00%d\x00%s\n", relative, execBit, file.Hash)
	}
	return contentDigest([]byte(buffer.String()))
}

func skillNatives(snapshot adapter.DirectorySnapshot, id, skillRoot string, pluginFiles []adapter.ObservedFile) (natives, extra []string, diverged, unsupported bool, err error) {
	pluginByRel := make(map[string]adapter.ObservedFile, len(pluginFiles))
	prefix := strings.TrimSuffix(skillRoot, "/") + "/"
	for _, file := range pluginFiles {
		pluginByRel[strings.TrimPrefix(file.Path, prefix)] = file
	}
	for _, native := range skillNativeDirs {
		entries, readErr := readDir(snapshot, native.dir)
		if readErr != nil {
			return nil, nil, false, false, readErr
		}
		want := native.prefix + id
		for _, entry := range entries {
			if path.Base(entry.Path) != want {
				continue
			}
			natives = append(natives, entry.Path)
			if entry.Mode&fs.ModeSymlink != 0 {
				continue
			}
			if !entry.Mode.IsDir() {
				continue
			}
			copyFiles, escaped, walkErr := readSkillTree(snapshot, entry.Path)
			if walkErr != nil {
				return nil, nil, false, false, walkErr
			}
			if escaped {
				unsupported = true
				continue
			}
			seen := make(map[string]struct{})
			copyRoot := entry.Path
			for _, file := range copyFiles {
				rel := strings.TrimPrefix(file.Path, copyRoot+"/")
				seen[rel] = struct{}{}
				pluginFile, ok := pluginByRel[rel]
				if !ok {
					extra = append(extra, file.Path)
					continue
				}
				if pluginFile.Hash != file.Hash {
					diverged = true
				}
			}
			for rel := range pluginByRel {
				if _, ok := seen[rel]; !ok {
					diverged = true
				}
			}
		}
	}
	sort.Strings(natives)
	sort.Strings(extra)
	return natives, extra, diverged, unsupported, nil
}
