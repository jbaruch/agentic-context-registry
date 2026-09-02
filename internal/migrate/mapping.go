package migrate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	MappingSchemaVersion  = 1
	MappingOriginManifest = "manifest"
	MappingOriginFile     = "mapping-file"
	MappingOriginCLI      = "cli"
)

// Mapping declares how one Tessl package maps to an ACR package source.
type Mapping struct {
	From         string   `json:"from" yaml:"from"`
	Source       string   `json:"source" yaml:"source"`
	Requested    string   `json:"requested,omitempty" yaml:"requested,omitempty"`
	TesslVersion string   `json:"tesslVersion,omitempty" yaml:"-"`
	Origin       string   `json:"origin,omitempty" yaml:"-"`
	Overrides    []string `json:"overrides,omitempty" yaml:"-"`
}

type mappingDocument struct {
	SchemaVersion int       `yaml:"schemaVersion"`
	Packages      []Mapping `yaml:"packages"`
}

// MappingConflictError reports contradictory declarations within one tier.
type MappingConflictError struct {
	Origin string
	From   string
}

func (err *MappingConflictError) Error() string {
	return fmt.Sprintf("mapping %q is declared more than once with different values in %s; keep one mapping for that package", err.From, err.Origin)
}

// UnmappedPackageError reports a Tessl package with no explicit repository evidence.
type UnmappedPackageError struct {
	Package   string
	Candidate string
}

func (err *UnmappedPackageError) Error() string {
	return fmt.Sprintf("Tessl package %q has no repository mapping; add --map %s=%s or declare it in --mapping-file", err.Package, err.Package, err.Candidate)
}

// DecodeMappingFile parses the versioned, strict migration mapping document.
func DecodeMappingFile(content []byte) ([]Mapping, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	var document mappingDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode migration mapping file: %w; use schemaVersion: %d with a packages list", err, MappingSchemaVersion)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("decode trailing migration mapping content: %w; keep one YAML document", err)
		}
		return nil, errors.New("migration mapping file contains multiple YAML documents; keep one document")
	}
	if document.SchemaVersion != MappingSchemaVersion {
		return nil, fmt.Errorf("unsupported migration mapping schemaVersion %d; use schemaVersion %d", document.SchemaVersion, MappingSchemaVersion)
	}
	return canonicalTier(document.Packages, MappingOriginFile)
}

// ParseInlineMapping parses FROM=github:owner/repository[@REQUESTED].
func ParseInlineMapping(value string) (Mapping, error) {
	from, target, found := strings.Cut(value, "=")
	if !found || from == "" || target == "" {
		return Mapping{}, fmt.Errorf("invalid --map %q; use FROM=github:owner/repository[@REQUESTED]", value)
	}
	if !validTesslIdentity(from) {
		return Mapping{}, fmt.Errorf("invalid --map package %q; use a workspace/package identity", from)
	}
	source, requested, err := parseMappingTarget(target)
	if err != nil {
		return Mapping{}, err
	}
	return Mapping{From: from, Source: source, Requested: requested}, nil
}

// ParseInlineMappings parses and canonicalizes one CLI mapping tier.
func ParseInlineMappings(values []string) ([]Mapping, error) {
	mappings := make([]Mapping, 0, len(values))
	for _, value := range values {
		mapping, err := ParseInlineMapping(value)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, mapping)
	}
	return canonicalTier(mappings, MappingOriginCLI)
}

func parseMappingTarget(value string) (string, string, error) {
	source := value
	requested := ""
	if separator := strings.LastIndexByte(value, '@'); separator >= 0 {
		if separator == 0 || separator == len(value)-1 || strings.Contains(value[separator+1:], "@") {
			return "", "", fmt.Errorf("invalid mapping target %q; use github:owner/repository[@REQUESTED]", value)
		}
		source, requested = value[:separator], value[separator+1:]
	}
	identity, found := strings.CutPrefix(source, "github:")
	if !found || !validTesslIdentity(identity) {
		return "", "", fmt.Errorf("invalid mapping source %q; use github:owner/repository with lowercase canonical names", source)
	}
	return source, requested, nil
}

// ResolveMappings applies CLI, mapping-file, then manifest precedence.
func ResolveMappings(packages []PackageReport, fileMappings, cliMappings []Mapping) ([]Mapping, error) {
	fileTier, err := canonicalTier(fileMappings, MappingOriginFile)
	if err != nil {
		return nil, err
	}
	cliTier, err := canonicalTier(cliMappings, MappingOriginCLI)
	if err != nil {
		return nil, err
	}
	fileByPackage := indexMappings(fileTier)
	cliByPackage := indexMappings(cliTier)

	ordered := append([]PackageReport(nil), packages...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].TesslIdentity < ordered[right].TesslIdentity })
	result := make([]Mapping, 0, len(ordered))
	for _, pkg := range ordered {
		var tiers []Mapping
		if pkg.PackageMapping == mappingGitHub {
			tiers = append(tiers, Mapping{From: pkg.TesslIdentity, Source: pkg.MappingCandidate, Origin: MappingOriginManifest})
		}
		if mapping, ok := fileByPackage[pkg.TesslIdentity]; ok {
			tiers = append(tiers, mapping)
		}
		if mapping, ok := cliByPackage[pkg.TesslIdentity]; ok {
			tiers = append(tiers, mapping)
		}
		if len(tiers) == 0 {
			return nil, &UnmappedPackageError{Package: pkg.TesslIdentity, Candidate: pkg.MappingCandidate}
		}
		selected := tiers[len(tiers)-1]
		selected.TesslVersion = pkg.Version
		if selected.Requested == "" {
			selected.Requested = pkg.Version
		}
		for index := len(tiers) - 2; index >= 0; index-- {
			if tiers[index].Source != selected.Source || effectiveRequested(tiers[index], pkg.Version) != selected.Requested {
				selected.Overrides = append(selected.Overrides, tiers[index].Origin)
			}
		}
		sort.Strings(selected.Overrides)
		result = append(result, selected)
	}
	return result, nil
}

func effectiveRequested(mapping Mapping, tesslVersion string) string {
	if mapping.Requested != "" {
		return mapping.Requested
	}
	return tesslVersion
}

func canonicalTier(mappings []Mapping, origin string) ([]Mapping, error) {
	byPackage := make(map[string]Mapping, len(mappings))
	for _, mapping := range mappings {
		if !validTesslIdentity(mapping.From) {
			return nil, fmt.Errorf("invalid %s mapping package %q; use a workspace/package identity", origin, mapping.From)
		}
		if _, _, err := parseMappingTarget(mapping.Source + requestedSuffix(mapping.Requested)); err != nil {
			return nil, err
		}
		mapping.Origin = origin
		mapping.TesslVersion = ""
		mapping.Overrides = nil
		if previous, exists := byPackage[mapping.From]; exists {
			if previous.Source != mapping.Source || previous.Requested != mapping.Requested {
				return nil, &MappingConflictError{Origin: origin, From: mapping.From}
			}
			continue
		}
		byPackage[mapping.From] = mapping
	}
	result := make([]Mapping, 0, len(byPackage))
	for _, mapping := range byPackage {
		result = append(result, mapping)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].From < result[right].From })
	return result, nil
}

func requestedSuffix(requested string) string {
	if requested == "" {
		return ""
	}
	return "@" + requested
}

func indexMappings(mappings []Mapping) map[string]Mapping {
	result := make(map[string]Mapping, len(mappings))
	for _, mapping := range mappings {
		result[mapping.From] = mapping
	}
	return result
}
