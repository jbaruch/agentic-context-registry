package tesslplugin

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"go.yaml.in/yaml/v3"
)

func sortManifest(value *manifest.Manifest) {
	sort.Slice(value.Artifacts.Rules, func(left, right int) bool {
		return value.Artifacts.Rules[left].ID < value.Artifacts.Rules[right].ID
	})
	sort.Slice(value.Artifacts.Skills, func(left, right int) bool {
		return value.Artifacts.Skills[left].ID < value.Artifacts.Skills[right].ID
	})
	sort.Slice(value.Artifacts.Scripts, func(left, right int) bool {
		return value.Artifacts.Scripts[left].ID < value.Artifacts.Scripts[right].ID
	})
	sort.Slice(value.Artifacts.Hooks, func(left, right int) bool {
		return value.Artifacts.Hooks[left].ID < value.Artifacts.Hooks[right].ID
	})
}

func renderManifest(value manifest.Manifest) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("encode %s: %w", manifest.Filename, err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close YAML encoder: %w", err)
	}
	return bytes.TrimPrefix(buffer.Bytes(), []byte("---\n")), nil
}

func writeManifest(root *os.Root, rendered []byte, dryRun bool) (wrote bool, err error) {
	existing, present, err := readExistingManifest(root)
	if err != nil {
		return false, err
	}
	if present {
		if bytes.Equal(existing, rendered) {
			return false, nil
		}
		fields := conflictingFields(existing, rendered)
		detail := "delete agent-plugin.yaml and re-run"
		if len(fields) != 0 {
			detail = "fields that moved: " + strings.Join(fields, ", ") + "; delete agent-plugin.yaml and re-run"
		}
		return false, conversionError(CodeManifestConflict, manifest.Filename,
			"existing %s differs from conversion output (%s)", manifest.Filename, detail)
	}
	if dryRun {
		return false, nil
	}
	file, err := root.OpenFile(manifest.Filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false, fmt.Errorf("create %s: %w", manifest.Filename, err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", manifest.Filename, closeErr))
		}
	}()
	if _, err := io.Copy(file, bytes.NewReader(rendered)); err != nil {
		return false, fmt.Errorf("write %s: %w", manifest.Filename, err)
	}
	return true, nil
}

func readExistingManifest(root *os.Root) ([]byte, bool, error) {
	data, err := root.ReadFile(manifest.Filename)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read existing %s: %w", manifest.Filename, err)
	}
	return data, true, nil
}

func conflictingFields(existingBytes, rendered []byte) []string {
	var existing, next manifest.Manifest
	if err := yaml.Unmarshal(existingBytes, &existing); err != nil {
		return nil
	}
	if err := yaml.Unmarshal(rendered, &next); err != nil {
		return nil
	}
	var fields []string
	if existing.SchemaVersion != next.SchemaVersion {
		fields = append(fields, "schemaVersion")
	}
	if existing.Name != next.Name {
		fields = append(fields, "name")
	}
	if existing.Version != next.Version {
		fields = append(fields, "version")
	}
	if existing.Description != next.Description {
		fields = append(fields, "description")
	}
	if existing.Source.Repository != next.Source.Repository {
		fields = append(fields, "source.repository")
	}
	fields = append(fields, artifactFieldDiffs("rules", idsOfRules(existing.Artifacts.Rules), idsOfRules(next.Artifacts.Rules))...)
	fields = append(fields, artifactFieldDiffs("skills", idsOfSkills(existing.Artifacts.Skills), idsOfSkills(next.Artifacts.Skills))...)
	fields = append(fields, artifactFieldDiffs("hooks", idsOfHooks(existing.Artifacts.Hooks), idsOfHooks(next.Artifacts.Hooks))...)
	return fields
}

func idsOfRules(values []manifest.RuleArtifact) []string {
	ids := make([]string, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.ID)
	}
	return ids
}

func idsOfSkills(values []manifest.SkillArtifact) []string {
	ids := make([]string, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.ID)
	}
	return ids
}

func idsOfHooks(values []manifest.HookArtifact) []string {
	ids := make([]string, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.ID)
	}
	return ids
}

func artifactFieldDiffs(kind string, existing, next []string) []string {
	seen := make(map[string]struct{}, len(existing))
	for _, id := range existing {
		seen[id] = struct{}{}
	}
	nextSeen := make(map[string]struct{}, len(next))
	for _, id := range next {
		nextSeen[id] = struct{}{}
	}
	var fields []string
	for _, id := range existing {
		if _, ok := nextSeen[id]; !ok {
			fields = append(fields, "artifacts."+kind+"."+id)
		}
	}
	for _, id := range next {
		if _, ok := seen[id]; !ok {
			fields = append(fields, "artifacts."+kind+"."+id)
		}
	}
	sort.Strings(fields)
	return fields
}
