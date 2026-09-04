package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

// journeyKind separates the two obligations every executable leaf carries: it
// has to do something useful, and it has to refuse something it should not do.
// A refusal cannot stand in for a success, which is what makes a suite of
// empty-project smoke tests fail this gate instead of satisfying it.
type journeyKind string

const (
	journeySuccess journeyKind = "success"
	journeyRefusal journeyKind = "refusal"
)

// journeyCase is one runnable journey. run executes real argv against a real
// fixture and returns how many positive outcomes it asserted: dependencies
// locked, artifacts written, updates applied, targets removed, packages
// converted, refusals proven. Returning zero fails the case, so a journey
// cannot register coverage by naming a command and asserting nothing.
type journeyCase struct {
	leaf string
	name string
	kind journeyKind
	run  func(*testing.T) int
}

// cliJourneys is the coverage table. Adding a public command or subcommand
// without adding an entry here fails TestCLIJourneyInventory.
func cliJourneys() []journeyCase {
	var cases []journeyCase
	cases = append(cases, metaJourneys()...)
	cases = append(cases, consumerJourneys()...)
	cases = append(cases, stateJourneys()...)
	cases = append(cases, producerJourneys()...)
	cases = append(cases, streamJourneys()...)
	cases = append(cases, terminalJourneys()...)
	return cases
}

// TestCLIJourneyInventory runs every journey and then holds the results
// against the production command inventory. It is the whole acceptance suite's
// entry point: the journeys are the evidence and this is the gate.
func TestCLIJourneyInventory(t *testing.T) {
	cases := cliJourneys()
	evidence := map[string]map[journeyKind]int{}
	for _, journey := range cases {
		journey := journey
		t.Run(journey.leaf+"/"+journey.name, func(t *testing.T) {
			outcomes := journey.run(t)
			if outcomes <= 0 {
				t.Fatalf("journey %s/%s asserted no positive outcome", journey.leaf, journey.name)
			}
			if evidence[journey.leaf] == nil {
				evidence[journey.leaf] = map[journeyKind]int{}
			}
			evidence[journey.leaf][journey.kind] += outcomes
		})
	}

	if duplicates := duplicateJourneyNames(cases); len(duplicates) != 0 {
		t.Errorf("duplicate journey names: %v", duplicates)
	}
	leaves := make([]string, 0, len(cli.Leaves()))
	for _, leaf := range cli.Leaves() {
		leaves = append(leaves, leaf.String())
	}
	for _, complaint := range journeyCoverageComplaints(leaves, cases, evidence) {
		t.Error(complaint)
	}
}

// journeyCoverageComplaints reports every way the coverage table and the
// command inventory disagree. It is a pure function so the enforcement itself
// can be proven against inventories this build does not have.
func journeyCoverageComplaints(leaves []string, cases []journeyCase, evidence map[string]map[journeyKind]int) []string {
	registered := map[string]bool{}
	for _, leaf := range leaves {
		registered[leaf] = true
	}
	var complaints []string
	for _, journey := range cases {
		if journey.run == nil {
			complaints = append(complaints, fmt.Sprintf("journey %s/%s has no runnable function", journey.leaf, journey.name))
		}
		if !registered[journey.leaf] {
			complaints = append(complaints, fmt.Sprintf("journey %s/%s covers %q, which is not a command any user can run", journey.leaf, journey.name, journey.leaf))
		}
	}
	for _, leaf := range leaves {
		proven := evidence[leaf]
		if proven[journeySuccess] <= 0 {
			complaints = append(complaints, fmt.Sprintf("no journey proved a successful outcome for %q", leaf))
		}
		if proven[journeyRefusal] <= 0 {
			complaints = append(complaints, fmt.Sprintf("no journey proved a refusal for %q", leaf))
		}
	}
	sort.Strings(complaints)
	return complaints
}

func duplicateJourneyNames(cases []journeyCase) []string {
	seen := map[string]bool{}
	var duplicates []string
	for _, journey := range cases {
		name := journey.leaf + "/" + journey.name
		if seen[name] {
			duplicates = append(duplicates, name)
		}
		seen[name] = true
	}
	return duplicates
}

// TestCLIJourneyInventoryFailsForTheIntendedCause proves the gate itself. Each
// mutation changes the inventory or the evidence the way a real regression
// would, and the complaint has to name what went missing.
func TestCLIJourneyInventoryFailsForTheIntendedCause(t *testing.T) {
	t.Parallel()

	shipped := []string{"install", "migrate tessl"}
	covered := []journeyCase{
		{leaf: "install", name: "success", kind: journeySuccess, run: func(*testing.T) int { return 1 }},
		{leaf: "install", name: "refusal", kind: journeyRefusal, run: func(*testing.T) int { return 1 }},
		{leaf: "migrate tessl", name: "success", kind: journeySuccess, run: func(*testing.T) int { return 1 }},
		{leaf: "migrate tessl", name: "refusal", kind: journeyRefusal, run: func(*testing.T) int { return 1 }},
	}
	full := map[string]map[journeyKind]int{
		"install":       {journeySuccess: 2, journeyRefusal: 1},
		"migrate tessl": {journeySuccess: 3, journeyRefusal: 1},
	}
	if complaints := journeyCoverageComplaints(shipped, covered, full); len(complaints) != 0 {
		t.Fatalf("a fully covered inventory complained: %v", complaints)
	}

	for _, mutation := range []struct {
		name      string
		leaves    []string
		cases     []journeyCase
		evidence  map[string]map[journeyKind]int
		wantNamed string
	}{
		{
			name:      "a new top-level command has no journey",
			leaves:    append(append([]string(nil), shipped...), "doctor"),
			cases:     covered,
			evidence:  full,
			wantNamed: `no journey proved a successful outcome for "doctor"`,
		},
		{
			name:      "a new subcommand has no journey",
			leaves:    append(append([]string(nil), shipped...), "migrate tessl-plugin"),
			cases:     covered,
			evidence:  full,
			wantNamed: `no journey proved a successful outcome for "migrate tessl-plugin"`,
		},
		{
			name:      "a journey was removed",
			leaves:    shipped,
			cases:     covered[:2],
			evidence:  map[string]map[journeyKind]int{"install": full["install"]},
			wantNamed: `no journey proved a successful outcome for "migrate tessl"`,
		},
		{
			name:      "a command only refuses",
			leaves:    shipped,
			cases:     covered,
			evidence:  map[string]map[journeyKind]int{"install": full["install"], "migrate tessl": {journeyRefusal: 4}},
			wantNamed: `no journey proved a successful outcome for "migrate tessl"`,
		},
		{
			name:      "a command only succeeds",
			leaves:    shipped,
			cases:     covered,
			evidence:  map[string]map[journeyKind]int{"install": full["install"], "migrate tessl": {journeySuccess: 4}},
			wantNamed: `no journey proved a refusal for "migrate tessl"`,
		},
		{
			name:      "a journey covers a command nobody can run",
			leaves:    shipped,
			cases:     append(append([]journeyCase(nil), covered...), journeyCase{leaf: "retired", name: "stale", kind: journeySuccess, run: func(*testing.T) int { return 1 }}),
			evidence:  full,
			wantNamed: `which is not a command any user can run`,
		},
		{
			name:      "a journey has no runnable function",
			leaves:    shipped,
			cases:     append(append([]journeyCase(nil), covered...), journeyCase{leaf: "install", name: "empty", kind: journeySuccess}),
			evidence:  full,
			wantNamed: "has no runnable function",
		},
	} {
		mutation := mutation
		t.Run(mutation.name, func(t *testing.T) {
			t.Parallel()

			complaints := journeyCoverageComplaints(mutation.leaves, mutation.cases, mutation.evidence)
			if len(complaints) == 0 {
				t.Fatalf("the gate accepted %s", mutation.name)
			}
			if !strings.Contains(strings.Join(complaints, "\n"), mutation.wantNamed) {
				t.Fatalf("complaints = %v, want one naming %q", complaints, mutation.wantNamed)
			}
		})
	}
}

// TestCLIJourneyInventoryRejectsAnEmptyOutcome proves the other half of the
// gate: a journey that runs a command and asserts nothing positive about the
// result cannot register coverage for it.
func TestCLIJourneyInventoryRejectsAnEmptyOutcome(t *testing.T) {
	t.Parallel()

	silent := journeyCase{leaf: "install", name: "empty-result", kind: journeySuccess, run: func(*testing.T) int { return 0 }}
	if outcomes := silent.run(t); outcomes > 0 {
		t.Fatal("fixture case is not empty")
	}
	evidence := map[string]map[journeyKind]int{"install": {journeySuccess: 0, journeyRefusal: 1}}
	complaints := journeyCoverageComplaints([]string{"install"}, []journeyCase{silent}, evidence)
	if len(complaints) == 0 || !strings.Contains(strings.Join(complaints, "\n"), `no journey proved a successful outcome for "install"`) {
		t.Fatalf("complaints = %v, want the empty outcome to fail the gate", complaints)
	}
}
