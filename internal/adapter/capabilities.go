package adapter

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// CodeUnsupportedAdapterCapability is UnsupportedError's stable,
// machine-readable code.
const CodeUnsupportedAdapterCapability = "unsupported_adapter_capability"

// UnsupportedCombination names one (adapter, source, artifact, event)
// pairing a selected adapter cannot realize. Event is empty for non-hook
// artifact classes.
type UnsupportedCombination struct {
	AdapterID  string
	Source     string
	ArtifactID string
	Kind       ArtifactKind
	Event      manifest.HookEvent
}

// UnsupportedError reports every unsupported combination found during the
// coordinator's capability preflight, before any adapter's Plan runs.
type UnsupportedError struct {
	Combinations []UnsupportedCombination
}

func (err *UnsupportedError) Error() string {
	parts := make([]string, 0, len(err.Combinations))
	for _, combination := range err.Combinations {
		if combination.Event != "" {
			parts = append(parts, fmt.Sprintf("%s: adapter %q cannot realize %s %q event %q from %s",
				CodeUnsupportedAdapterCapability, combination.AdapterID, combination.Kind, combination.ArtifactID, combination.Event, combination.Source))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: adapter %q cannot realize %s %q from %s",
			CodeUnsupportedAdapterCapability, combination.AdapterID, combination.Kind, combination.ArtifactID, combination.Source))
	}
	return "unsupported adapter capabilities: " + strings.Join(parts, "; ")
}

// unsupportedCombinations checks every (adapter, source, artifact ID,
// artifact kind, hook event) implied by packages against every adapter's
// SupportedArtifacts and SupportedEvents, returning every miss in
// deterministic order. It performs no I/O and calls no adapter method.
func unsupportedCombinations(adapters []Adapter, packages []Package) []UnsupportedCombination {
	var found []UnsupportedCombination
	for _, candidate := range adapters {
		descriptor := candidate.Descriptor()
		artifacts := artifactSet(candidate.SupportedArtifacts())
		events := eventSet(candidate.SupportedEvents())
		for _, pkg := range packages {
			for _, rule := range pkg.Manifest.Artifacts.Rules {
				if !artifacts[ArtifactRule] {
					found = append(found, UnsupportedCombination{descriptor.ID, pkg.Source, rule.ID, ArtifactRule, ""})
				}
			}
			for _, skill := range pkg.Manifest.Artifacts.Skills {
				if !artifacts[ArtifactSkill] {
					found = append(found, UnsupportedCombination{descriptor.ID, pkg.Source, skill.ID, ArtifactSkill, ""})
				}
			}
			for _, script := range pkg.Manifest.Artifacts.Scripts {
				if !artifacts[ArtifactScript] {
					found = append(found, UnsupportedCombination{descriptor.ID, pkg.Source, script.ID, ArtifactScript, ""})
				}
			}
			for _, hook := range pkg.Manifest.Artifacts.Hooks {
				if !artifacts[ArtifactHook] || !events[hook.Event] {
					found = append(found, UnsupportedCombination{descriptor.ID, pkg.Source, hook.ID, ArtifactHook, hook.Event})
				}
			}
		}
	}
	sort.Slice(found, func(left, right int) bool {
		return combinationKey(found[left]) < combinationKey(found[right])
	})
	return found
}

func combinationKey(combination UnsupportedCombination) string {
	return strings.Join([]string{
		combination.AdapterID, combination.Source, combination.ArtifactID,
		string(combination.Kind), string(combination.Event),
	}, "\x00")
}

func artifactSet(kinds []ArtifactKind) map[ArtifactKind]bool {
	set := make(map[ArtifactKind]bool, len(kinds))
	for _, kind := range kinds {
		set[kind] = true
	}
	return set
}

func eventSet(events []manifest.HookEvent) map[manifest.HookEvent]bool {
	set := make(map[manifest.HookEvent]bool, len(events))
	for _, event := range events {
		set[event] = true
	}
	return set
}

func sortedAndUnique[T ~string](values []T) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] >= values[index] {
			return false
		}
	}
	return true
}
