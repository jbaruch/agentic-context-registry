package cli

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestLeavesEnumeratesTheExecutableSurface pins the inventory an acceptance
// suite has to cover. A new command or subcommand changes this count, and the
// journey enforcement in cmd/acr fails until it also has a journey.
func TestLeavesEnumeratesTheExecutableSurface(t *testing.T) {
	t.Parallel()

	want := []string{
		"init", "install", "realize", "list", "outdated", "freshness run", "update",
		"resume", "uninstall", "check", "publish", "migrate tessl", "migrate tessl-plugin",
		"version", "help",
	}
	leaves := Leaves()
	if len(leaves) != len(want) {
		t.Fatalf("Leaves() = %v, want %d leaves", leaves, len(want))
	}
	seen := map[string]bool{}
	for index, leaf := range leaves {
		if leaf.String() != want[index] {
			t.Errorf("Leaves()[%d] = %q, want %q", index, leaf.String(), want[index])
		}
		if seen[leaf.String()] {
			t.Errorf("Leaves() repeats %q", leaf.String())
		}
		seen[leaf.String()] = true
	}
}

// TestSurfaceIsTheDispatchRegistry holds the inventory's input against the map
// commandFor actually accepts. Registering a command was once enough to make
// it dispatchable while the inventory, derived from the display order alone,
// never saw it. Deriving the surface from the registry closes that gap, and
// this is the test that fails if the derivation drifts back.
func TestSurfaceIsTheDispatchRegistry(t *testing.T) {
	t.Parallel()

	surface := Surface()
	if len(surface.Subcommands) != len(commandSpecs) {
		t.Fatalf("Surface() dispatches %d commands, the registry accepts %d", len(surface.Subcommands), len(commandSpecs))
	}
	for command, spec := range commandSpecs {
		subcommands, dispatchable := surface.Subcommands[string(command)]
		if !dispatchable {
			t.Errorf("Surface() omits registered command %q", command)
			continue
		}
		if !reflect.DeepEqual(subcommands, spec.subcommands) && !(len(subcommands) == 0 && len(spec.subcommands) == 0) {
			t.Errorf("Surface()[%q] subcommands = %v, want %v", command, subcommands, spec.subcommands)
		}
	}
	for name := range surface.Subcommands {
		if _, accepted := commandFor(name); !accepted {
			t.Errorf("Surface() names %q, which the parser does not accept", name)
		}
	}
	for _, command := range surface.Order {
		if _, dispatchable := surface.Subcommands[command]; !dispatchable {
			t.Errorf("Surface() orders %q, which the parser does not dispatch", command)
		}
	}
}

// TestLeavesOfNamesACommandRegisteredWithoutADisplayOrder is the counterexample
// the review demonstrated, run against the production surface: a command added
// to the dispatch registry alone reaches the application, so the inventory has
// to name it whether or not anyone remembered the display order.
func TestLeavesOfNamesACommandRegisteredWithoutADisplayOrder(t *testing.T) {
	t.Parallel()

	surface := Surface()
	before := len(LeavesOf(surface))
	surface.Subcommands["journey-probe"] = nil

	leaves := LeavesOf(surface)
	if len(leaves) != before+1 {
		t.Fatalf("LeavesOf() returned %d leaves, want %d", len(leaves), before+1)
	}
	named := false
	for _, leaf := range leaves {
		named = named || leaf.String() == "journey-probe"
	}
	if !named {
		t.Fatalf("LeavesOf() = %v, want it to name the registered command", leaves)
	}

	// The curated order still decides how the shipped leaves are presented.
	shipped := LeavesOf(Surface())
	if shipped[0].String() != "init" || shipped[len(shipped)-1].String() != "help" {
		t.Fatalf("Leaves() lost its display order: %v", shipped)
	}
}

// TestRootHelpNamesEveryDispatchableCommand keeps help and dispatch on the same
// registry: a command a user can run is a command root help documents.
func TestRootHelpNamesEveryDispatchableCommand(t *testing.T) {
	t.Parallel()

	help := rootHelp()
	for command := range commandSpecs {
		if !strings.Contains(help, string(command)) {
			t.Errorf("root help does not name dispatchable command %q", command)
		}
	}
}

// TestEveryLeafIsReachableAndDocumented drives each leaf through the actual
// runner. A leaf the parser rejects, or one root help never names, is not a
// command a user can run however carefully it is registered.
func TestEveryLeafIsReachableAndDocumented(t *testing.T) {
	t.Parallel()

	var rootStdout bytes.Buffer
	if exit := New(&rootStdout, &bytes.Buffer{}, rejectingApplication(t), Build{Version: "test"}).Run(context.Background(), nil); exit != ExitSuccess {
		t.Fatalf("root help exit = %d", exit)
	}

	for _, leaf := range Leaves() {
		leaf := leaf
		t.Run(leaf.String(), func(t *testing.T) {
			t.Parallel()

			if !strings.Contains(rootStdout.String(), leaf.Command) {
				t.Errorf("root help does not name %q", leaf.Command)
			}
			var stdout, stderr bytes.Buffer
			exit := New(&stdout, &stderr, rejectingApplication(t), Build{Version: "test"}).Run(context.Background(), append(leaf.Args(), "--help"))
			if exit != ExitSuccess {
				t.Fatalf("%v --help exit = %d, stderr = %q", leaf.Args(), exit, stderr.String())
			}
			if !strings.Contains(stdout.String(), "Usage:") || stderr.Len() != 0 {
				t.Fatalf("%v --help stdout = %q, stderr = %q", leaf.Args(), stdout.String(), stderr.String())
			}
			if leaf.Subcommand != "" && !strings.Contains(stdout.String(), leaf.Subcommand) {
				t.Fatalf("%v --help does not document the %q subcommand: %q", leaf.Args(), leaf.Subcommand, stdout.String())
			}
		})
	}
}

// TestSubcommandDescriptorGatesTheParser proves the accepted-subcommand list
// is the parser's own predicate rather than documentation beside it.
func TestSubcommandDescriptorGatesTheParser(t *testing.T) {
	t.Parallel()

	for _, command := range commandOrder {
		spec := commandSpecs[command]
		if len(spec.subcommands) == 0 {
			continue
		}
		command := command
		t.Run(string(command), func(t *testing.T) {
			t.Parallel()

			for _, subcommand := range spec.subcommands {
				if _, help, err := parseInvocation(command, []string{subcommand}); err != nil || help {
					t.Errorf("parseInvocation(%s %s) = help %t, err %v", command, subcommand, help, err)
				}
			}
			var stdout, stderr bytes.Buffer
			exit := New(&stdout, &stderr, rejectingApplication(t), Build{Version: "test"}).Run(context.Background(), []string{string(command), "not-a-subcommand"})
			if exit != ExitUsage {
				t.Fatalf("%s not-a-subcommand exit = %d, want %d", command, exit, ExitUsage)
			}
			if !strings.Contains(stderr.String(), spec.subcommandNoun) || !strings.Contains(stderr.String(), "not-a-subcommand") {
				t.Fatalf("%s refusal = %q, want it to name the %s", command, stderr.String(), spec.subcommandNoun)
			}
			if stdout.Len() != 0 {
				t.Fatalf("%s refusal wrote %q to stdout", command, stdout.String())
			}
			if _, _, err := parseInvocation(command, nil); err == nil {
				t.Fatalf("parseInvocation(%s) accepted a missing subcommand", command)
			}
		})
	}
}
