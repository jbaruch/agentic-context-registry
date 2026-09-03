package migrate

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

const (
	DiffMissingInACR   = "missing-in-acr"
	DiffMissingInTessl = "missing-in-tessl"
	DiffBody           = "body"
	DiffActivation     = "activation"
	DiffEvent          = "event"
	DiffLossy          = "lossy"
)

// EffectiveKey is the native-filename-independent identity of an artifact.
type EffectiveKey struct {
	Package string `json:"package"`
	Kind    string `json:"kind"`
	ID      string `json:"id"`
}

// EffectiveItem is the behaviorally relevant form of one package artifact.
type EffectiveItem struct {
	EffectiveKey
	Digest     string                  `json:"digest,omitempty"`
	Activation manifest.RuleActivation `json:"activation,omitempty"`
	Event      manifest.HookEvent      `json:"event,omitempty"`
	Lossy      []string                `json:"lossy,omitempty"`
	Natives    []string                `json:"natives,omitempty"`
}

// EffectiveSet is sorted by package, kind, and artifact ID.
type EffectiveSet []EffectiveItem

// EffectiveDiff names one exhaustive semantic difference between two sets.
type EffectiveDiff struct {
	EffectiveKey
	Reason       string   `json:"reason"`
	TesslDigest  string   `json:"tesslDigest,omitempty"`
	ACRDigest    string   `json:"acrDigest,omitempty"`
	TesslNatives []string `json:"tesslNatives,omitempty"`
	ACRNatives   []string `json:"acrNatives,omitempty"`
}

// FromInventory converts Tessl inventory through the common effective model.
func FromInventory(report Report) EffectiveSet {
	var result EffectiveSet
	for _, pkg := range report.Packages {
		for _, artifact := range pkg.Artifacts {
			item := EffectiveItem{
				EffectiveKey: EffectiveKey{Package: pkg.TesslIdentity, Kind: artifact.Kind, ID: artifact.ID},
				Digest:       artifact.Digest,
				Event:        manifest.HookEvent(artifact.Event),
				Lossy:        append([]string(nil), artifact.Lossy...),
				Natives:      append([]string(nil), artifact.Natives...),
			}
			if artifact.Activation != nil {
				item.Activation = manifest.RuleActivation{Mode: manifest.ActivationMode(artifact.Activation.Mode), Paths: append([]string(nil), artifact.Activation.Paths...)}
			}
			result = append(result, item)
		}
	}
	return sortEffective(result)
}

// FromPackage converts a resolved ACR package through the same effective
// model. Rule frontmatter is presentation metadata and is stripped before
// hashing; hook launch wrappers are deliberately absent from the digest.
func FromPackage(packageName string, pkg adapter.Package) (EffectiveSet, error) {
	var result EffectiveSet
	for _, rule := range pkg.Manifest.Artifacts.Rules {
		content, err := fs.ReadFile(pkg.Root, rule.Path)
		if err != nil {
			return nil, fmt.Errorf("read rule %q: %w", rule.Path, err)
		}
		body, err := adapter.StripLeadingFrontmatter(content)
		if err != nil {
			return nil, fmt.Errorf("normalize rule %q: %w", rule.Path, err)
		}
		result = append(result, EffectiveItem{
			EffectiveKey: EffectiveKey{Package: packageName, Kind: kindRule, ID: rule.ID},
			Digest:       contentDigest(body),
			Activation:   normalizedActivation(rule.Activation),
		})
	}
	for _, skill := range pkg.Manifest.Artifacts.Skills {
		digest, err := effectiveSkillDigest(pkg.Root, skill.Path)
		if err != nil {
			return nil, fmt.Errorf("normalize skill %q: %w", skill.Path, err)
		}
		result = append(result, EffectiveItem{
			EffectiveKey: EffectiveKey{Package: packageName, Kind: kindSkill, ID: skill.ID},
			Digest:       digest,
		})
	}
	for _, hook := range pkg.Manifest.Artifacts.Hooks {
		content, err := fs.ReadFile(pkg.Root, hook.Path)
		if err != nil {
			return nil, fmt.Errorf("read hook %q: %w", hook.Path, err)
		}
		result = append(result, EffectiveItem{
			EffectiveKey: EffectiveKey{Package: packageName, Kind: kindHook, ID: hook.ID},
			Digest:       hookDigest(content, hook.Args),
			Event:        hook.Event,
		})
	}
	return sortEffective(result), nil
}

// WithNatives returns a copy of a set with realized native paths attached by
// semantic key. Native paths are evidence only and never affect equality.
func (set EffectiveSet) WithNatives(natives map[EffectiveKey][]string) EffectiveSet {
	result := append(EffectiveSet(nil), set...)
	for index := range result {
		result[index].Natives = append([]string(nil), natives[result[index].EffectiveKey]...)
		sort.Strings(result[index].Natives)
	}
	return result
}

// CompareEffective returns the complete, deterministic semantic delta.
func CompareEffective(tessl, acr EffectiveSet) []EffectiveDiff {
	tesslByKey := effectiveByKey(tessl)
	acrByKey := effectiveByKey(acr)
	keys := make(map[EffectiveKey]struct{}, len(tesslByKey)+len(acrByKey))
	for key := range tesslByKey {
		keys[key] = struct{}{}
	}
	for key := range acrByKey {
		keys[key] = struct{}{}
	}
	ordered := make([]EffectiveKey, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(i, j int) bool { return effectiveKeyLess(ordered[i], ordered[j]) })

	var diffs []EffectiveDiff
	for _, key := range ordered {
		left, hasLeft := tesslByKey[key]
		right, hasRight := acrByKey[key]
		reason := ""
		switch {
		case !hasRight:
			reason = DiffMissingInACR
		case !hasLeft:
			reason = DiffMissingInTessl
		case len(left.Lossy) != 0 || len(right.Lossy) != 0:
			reason = DiffLossy
		case left.Digest != right.Digest:
			reason = DiffBody
		case !activationEqual(left.Activation, right.Activation):
			reason = DiffActivation
		case left.Event != right.Event:
			reason = DiffEvent
		}
		if reason != "" {
			diffs = append(diffs, EffectiveDiff{
				EffectiveKey: key,
				Reason:       reason,
				TesslDigest:  left.Digest,
				ACRDigest:    right.Digest,
				TesslNatives: append([]string(nil), left.Natives...),
				ACRNatives:   append([]string(nil), right.Natives...),
			})
		}
	}
	return diffs
}

func effectiveSkillDigest(root fs.FS, skillRoot string) (string, error) {
	var records []string
	err := fs.WalkDir(root, skillRoot, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("skill contains non-regular path %q", name)
		}
		content, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		executable := 0
		if info.Mode().Perm()&0o111 != 0 {
			executable = 1
		}
		relative := strings.TrimPrefix(name, strings.TrimSuffix(skillRoot, "/")+"/")
		records = append(records, fmt.Sprintf("%s\x00%d\x00%s\n", relative, executable, contentDigest(content)))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(records)
	return contentDigest([]byte(strings.Join(records, ""))), nil
}

func normalizedActivation(value manifest.RuleActivation) manifest.RuleActivation {
	result := manifest.RuleActivation{Mode: value.Mode, Paths: append([]string(nil), value.Paths...)}
	if result.Mode == "" {
		result.Mode = manifest.ActivationAlways
	}
	sort.Strings(result.Paths)
	return result
}

func activationEqual(left, right manifest.RuleActivation) bool {
	left = normalizedActivation(left)
	right = normalizedActivation(right)
	if left.Mode != right.Mode || len(left.Paths) != len(right.Paths) {
		return false
	}
	for index := range left.Paths {
		if left.Paths[index] != right.Paths[index] {
			return false
		}
	}
	return true
}

func effectiveByKey(set EffectiveSet) map[EffectiveKey]EffectiveItem {
	result := make(map[EffectiveKey]EffectiveItem, len(set))
	for _, item := range set {
		result[item.EffectiveKey] = item
	}
	return result
}

func sortEffective(set EffectiveSet) EffectiveSet {
	result := append(EffectiveSet(nil), set...)
	for index := range result {
		result[index].Activation = normalizedActivation(result[index].Activation)
		sort.Strings(result[index].Lossy)
		sort.Strings(result[index].Natives)
	}
	sort.Slice(result, func(i, j int) bool { return effectiveKeyLess(result[i].EffectiveKey, result[j].EffectiveKey) })
	return result
}

func effectiveKeyLess(left, right EffectiveKey) bool {
	if left.Package != right.Package {
		return left.Package < right.Package
	}
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	return left.ID < right.ID
}
