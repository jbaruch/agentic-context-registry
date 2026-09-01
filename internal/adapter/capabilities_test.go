package adapter

import (
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestUnsupportedCombinationsReportsEveryMiss(t *testing.T) {
	t.Parallel()

	full := stubAdapter{
		descriptor: testDescriptor("full", "1.0.0"),
		artifacts:  []ArtifactKind{ArtifactHook, ArtifactRule, ArtifactScript, ArtifactSkill},
		events:     []manifest.HookEvent{manifest.HookPostToolUse, manifest.HookPreToolUse, manifest.HookSessionEnd, manifest.HookSessionStart, manifest.HookStop, manifest.HookUserPromptSubmit},
	}
	rulesOnly := stubAdapter{
		descriptor: testDescriptor("rules-only", "1.0.0"),
		artifacts:  []ArtifactKind{ArtifactRule},
	}
	pkg := Package{Source: "github:owner/pkg", Manifest: manifest.Manifest{
		Artifacts: manifest.Artifacts{
			Rules: []manifest.RuleArtifact{{ID: "rule-a"}},
			Hooks: []manifest.HookArtifact{{ID: "hook-a", Event: manifest.HookSessionEnd}},
		},
	}}

	combinations := unsupportedCombinations([]Adapter{full, rulesOnly}, []Package{pkg})
	if len(combinations) != 1 {
		t.Fatalf("unsupportedCombinations() = %#v, want exactly one miss", combinations)
	}
	got := combinations[0]
	want := UnsupportedCombination{AdapterID: "rules-only", Source: "github:owner/pkg", ArtifactID: "hook-a", Kind: ArtifactHook, Event: manifest.HookSessionEnd}
	if got != want {
		t.Fatalf("unsupportedCombinations()[0] = %#v, want %#v", got, want)
	}
}

func TestUnsupportedCombinationsChecksUnsupportedEventSeparatelyFromKind(t *testing.T) {
	t.Parallel()

	noStop := stubAdapter{
		descriptor: testDescriptor("no-stop", "1.0.0"),
		artifacts:  []ArtifactKind{ArtifactHook},
		events:     []manifest.HookEvent{manifest.HookSessionStart},
	}
	pkg := Package{Source: "github:owner/pkg", Manifest: manifest.Manifest{
		Artifacts: manifest.Artifacts{Hooks: []manifest.HookArtifact{{ID: "hook-a", Event: manifest.HookStop}}},
	}}

	combinations := unsupportedCombinations([]Adapter{noStop}, []Package{pkg})
	if len(combinations) != 1 || combinations[0].Kind != ArtifactHook || combinations[0].Event != manifest.HookStop {
		t.Fatalf("unsupportedCombinations() = %#v", combinations)
	}
}

func TestUnsupportedErrorMessageNamesEveryCombination(t *testing.T) {
	t.Parallel()

	err := &UnsupportedError{Combinations: []UnsupportedCombination{
		{AdapterID: "fixture", Source: "github:owner/pkg", ArtifactID: "hook-a", Kind: ArtifactHook, Event: manifest.HookStop},
		{AdapterID: "fixture", Source: "github:owner/pkg", ArtifactID: "rule-a", Kind: ArtifactRule},
	}}
	message := err.Error()
	if !strings.Contains(message, CodeUnsupportedAdapterCapability) || !strings.Contains(message, "hook-a") || !strings.Contains(message, "rule-a") {
		t.Fatalf("UnsupportedError.Error() = %q", message)
	}
}

func TestUnsupportedCombinationsSortedDeterministically(t *testing.T) {
	t.Parallel()

	adapterA := stubAdapter{descriptor: testDescriptor("adapter-a", "1.0.0")}
	adapterB := stubAdapter{descriptor: testDescriptor("adapter-b", "1.0.0")}
	pkg := Package{Source: "github:owner/pkg", Manifest: manifest.Manifest{
		Artifacts: manifest.Artifacts{Rules: []manifest.RuleArtifact{{ID: "rule-b"}, {ID: "rule-a"}}},
	}}

	first := unsupportedCombinations([]Adapter{adapterB, adapterA}, []Package{pkg})
	second := unsupportedCombinations([]Adapter{adapterA, adapterB}, []Package{pkg})
	if len(first) != 4 {
		t.Fatalf("unsupportedCombinations() len = %d, want 4", len(first))
	}
	for index := range first {
		if first[index] != second[index] {
			t.Fatalf("unsupportedCombinations() order depends on adapter input order: %#v vs %#v", first, second)
		}
	}
	if first[0].AdapterID != "adapter-a" || first[1].AdapterID != "adapter-a" || first[2].AdapterID != "adapter-b" {
		t.Fatalf("unsupportedCombinations() not sorted by adapter ID: %#v", first)
	}
}
