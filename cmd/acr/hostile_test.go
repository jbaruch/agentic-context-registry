package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"github.com/jbaruch/agentic-context-registry/internal/realizeapp"
)

const (
	hostileBinaryHeldSource    = "github:binary/held"
	hostileBinarySiblingSource = "github:binary/sibling"
)

type hostileBinaryPackage struct {
	root     string
	manifest manifest.Manifest
}

type hostileBinaryLoader struct {
	packages map[string]hostileBinaryPackage
	calls    int
}

func (loader *hostileBinaryLoader) MaterializeLocked(_ context.Context, locked dependency.LockedDependency) (dependency.MaterializedPackage, func() error, error) {
	loader.calls++
	pkg, ok := loader.packages[locked.Source]
	if !ok {
		return dependency.MaterializedPackage{}, nil, errors.New("hostile fixture has no package")
	}
	return dependency.MaterializedPackage{Root: pkg.root, Manifest: pkg.manifest}, func() error { return nil }, nil
}

type hostileBinaryFixture struct {
	root      string
	stateHome string
	loader    *hostileBinaryLoader
	service   *realizeapp.Service
}

func newHostileBinaryFixture(t *testing.T) hostileBinaryFixture {
	t.Helper()
	root := t.TempDir()
	loader := &hostileBinaryLoader{packages: map[string]hostileBinaryPackage{}}
	for _, source := range []string{hostileBinaryHeldSource, hostileBinarySiblingSource} {
		packageRoot := t.TempDir()
		reverify2Put(t, packageRoot, "rules/policy.md", "# "+source+"\n", 0o644)
		reverify2Put(t, packageRoot, "hooks/boot.sh", "#!/bin/sh\nexit 0\n", 0o755)
		loader.packages[source] = hostileBinaryPackage{
			root: packageRoot,
			manifest: manifest.Manifest{Artifacts: manifest.Artifacts{
				Rules: []manifest.RuleArtifact{{
					ID: "policy", Path: "rules/policy.md",
					Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways},
				}},
				Hooks: []manifest.HookArtifact{{ID: "boot", Event: manifest.HookSessionStart, Path: "hooks/boot.sh"}},
			}},
		}
	}
	state := dependency.State{
		Project: dependency.Project{
			SchemaVersion: dependency.CurrentSchemaVersion,
			Agents:        []string{"claude-code", "codex"},
			Freshness:     "outdated",
			Dependencies: []dependency.Declaration{
				{
					Source: hostileBinaryHeldSource, Requested: "latest",
					Hold: &dependency.Hold{Pin: "v1.0.0", Rejected: "v2.0.0"},
				},
				{Source: hostileBinarySiblingSource, Requested: "latest"},
			},
		},
		Lock: dependency.Lockfile{
			SchemaVersion: dependency.CurrentSchemaVersion,
			Dependencies: []dependency.LockedDependency{
				{
					Source: hostileBinaryHeldSource, Requested: "latest", Kind: dependency.ResolutionRelease,
					ReleaseID: 1, Tag: "v1.0.0", Commit: strings.Repeat("a", 40), PackageVersion: "1.0.0",
					ContentHash: "sha256:" + strings.Repeat("a", 64),
					Hold: &dependency.LockHold{
						RejectedTag: "v2.0.0", RejectedReleaseID: 2, RejectedCommit: strings.Repeat("b", 40),
					},
				},
				{
					Source: hostileBinarySiblingSource, Requested: "latest", Kind: dependency.ResolutionRelease,
					ReleaseID: 3, Tag: "v3.0.0", Commit: strings.Repeat("c", 40), PackageVersion: "3.0.0",
					ContentHash: "sha256:" + strings.Repeat("c", 64),
				},
			},
		},
	}
	if err := dependency.WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	service := realizeapp.NewService(loader)
	if _, err := service.Run(context.Background(), root, nil, realize.ModeApply); err != nil {
		t.Fatalf("realize hostile binary fixture: %v", err)
	}
	loaded, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := realize.DecodeLedger(loaded.Lock.Realization)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, target := range ledger.Targets {
		for _, entry := range target.Entries {
			seen[entry.Adapter] = true
		}
	}
	if !seen["claude-code"] || !seen["codex"] {
		t.Fatalf("fixture was not realized for both agents: %#v", seen)
	}
	return hostileBinaryFixture{root: root, stateHome: t.TempDir(), loader: loader, service: service}
}

func hostileRunBinary(t *testing.T, binary, stateHome string, stdin io.Reader, args ...string) (string, string, int) {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Stdin = stdin
	command.Env = environmentWith("ACR_STATE_HOME", stateHome)
	return hostileRunCommand(t, command)
}

func hostileRunClosedStdin(t *testing.T, binary, stateHome string, args ...string) (string, string, int) {
	t.Helper()
	commandArgs := append([]string{"-test.run=^TestHostileBuiltBinarySuppressesDowngradePromptsForEveryNonTTYMode$", "--", binary}, args...)
	command := exec.Command(os.Args[0], commandArgs...)
	command.Env = append(environmentWith("ACR_STATE_HOME", stateHome), "ACR_HOSTILE_EXEC_CLOSED_STDIN=1")
	return hostileRunCommand(t, command)
}

func hostileRunCommand(t *testing.T, command *exec.Cmd) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("run %v: %v", command.Args, err)
	}
	return stdout.String(), stderr.String(), exitError.ExitCode()
}

func TestHostileBuiltBinarySuppressesDowngradePromptsForEveryNonTTYMode(t *testing.T) {
	if os.Getenv("ACR_HOSTILE_EXEC_CLOSED_STDIN") == "1" {
		separator := -1
		for index, argument := range os.Args {
			if argument == "--" {
				separator = index
				break
			}
		}
		if separator < 0 || separator+1 >= len(os.Args) {
			t.Fatal("closed-stdin helper has no target command")
		}
		// Exercise the process boundary: Go reopens a closed standard descriptor
		// to /dev/null before main, which differs from merely closing an *os.File.
		if err := syscall.Close(0); err != nil {
			t.Fatalf("close stdin: %v", err)
		}
		if err := syscall.Exec(os.Args[separator+1], os.Args[separator+1:], os.Environ()); err != nil {
			t.Fatalf("exec target with closed stdin: %v", err)
		}
	}
	binary := reverifyBuildACR(t)
	fixture := newHostileBinaryFixture(t)
	fixture.loader.calls = 0
	stable := reverify2HashTree(t, fixture.root)

	tests := []struct {
		name string
		args []string
		run  func(*testing.T, []string) (string, string, int)
	}{
		{
			name: "json",
			args: []string{"--json"},
			run: func(t *testing.T, args []string) (string, string, int) {
				return hostileRunBinary(t, binary, fixture.stateHome, strings.NewReader("hold\n"), args...)
			},
		},
		{
			name: "non-interactive",
			args: []string{"--non-interactive"},
			run: func(t *testing.T, args []string) (string, string, int) {
				return hostileRunBinary(t, binary, fixture.stateHome, strings.NewReader("hold\n"), args...)
			},
		},
		{
			name: "pipe",
			run: func(t *testing.T, args []string) (string, string, int) {
				reader, writer, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := writer.WriteString("hold\n"); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				return hostileRunBinary(t, binary, fixture.stateHome, reader, args...)
			},
		},
		{
			name: "closed stdin",
			run: func(t *testing.T, args []string) (string, string, int) {
				return hostileRunClosedStdin(t, binary, fixture.stateHome, args...)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"install", hostileBinaryHeldSource + "@v0.5.0", "--project", fixture.root}, test.args...)
			stdout, stderr, exitCode := test.run(t, args)
			if exitCode != 2 || stdout != "" {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
			}
			if !strings.Contains(stderr, "--hold") || !strings.Contains(stderr, "--pin") ||
				strings.Contains(stderr, "Record it as:") || strings.Contains(stdout, "Record it as:") {
				t.Fatalf("non-TTY output contains a prompt or lacks recovery flags: stdout = %q stderr = %q", stdout, stderr)
			}
			if test.name == "json" {
				if strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, `"code":"downgrade_choice_required"`) {
					t.Fatalf("JSON refusal is not one envelope: %q", stderr)
				}
			}
			if after := reverify2HashTree(t, fixture.root); !reflect.DeepEqual(after, stable) {
				t.Fatalf("%s changed the realized project:\n before %#v\n after  %#v", test.name, stable, after)
			}
		})
	}
	if fixture.loader.calls != 0 {
		t.Fatalf("built-binary refusal unexpectedly used the fixture loader %d time(s)", fixture.loader.calls)
	}
}

func TestHostileBuiltBinaryRefusalsDoNotTouchARealizedTwoPackageProject(t *testing.T) {
	binary := reverifyBuildACR(t)
	fixture := newHostileBinaryFixture(t)
	stable := reverify2HashTree(t, fixture.root)

	tests := [][]string{
		{"uninstall", hostileBinaryHeldSource, "--project", fixture.root, "--agent", "codex", "--json"},
		{"uninstall", "vendor:ws/pkg", "--project", fixture.root, "--json"},
		{"uninstall", "github:Owner/x", "--project", fixture.root, "--json"},
	}
	for _, args := range tests {
		stdout, stderr, exitCode := hostileRunBinary(t, binary, fixture.stateHome, strings.NewReader(""), args...)
		if exitCode != 2 || stdout != "" || strings.Count(stderr, "\n") != 1 {
			t.Fatalf("run %v exit = %d, stdout = %q, stderr = %q", args, exitCode, stdout, stderr)
		}
		if after := reverify2HashTree(t, fixture.root); !reflect.DeepEqual(after, stable) {
			t.Fatalf("refusal %v changed the realized project", args)
		}
	}

	if _, err := fixture.service.Uninstall(context.Background(), fixture.root, hostileBinaryHeldSource, false); err != nil {
		t.Fatalf("fixture uninstall: %v", err)
	}
	stable = reverify2HashTree(t, fixture.root)
	stdout, stderr, exitCode := hostileRunBinary(t, binary, fixture.stateHome, strings.NewReader(""),
		"uninstall", hostileBinaryHeldSource, "--project", fixture.root, "--json")
	if exitCode != 2 || stdout != "" || !strings.Contains(stderr, `"code":"dependency_not_declared"`) {
		t.Fatalf("second uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if after := reverify2HashTree(t, fixture.root); !reflect.DeepEqual(after, stable) {
		t.Fatalf("second built-binary uninstall changed the tree")
	}
}

func TestHostileBuiltBinaryInitHonoursDetectionStoredStateAndAgentOverride(t *testing.T) {
	binary := reverifyBuildACR(t)
	root := t.TempDir()
	reverify2Put(t, root, "CLAUDE.md", "# Claude\n", 0o644)
	reverify2Put(t, root, "AGENTS.md", "# Agents\n", 0o644)

	stdout, stderr, exitCode := hostileRunBinary(t, binary, t.TempDir(), strings.NewReader(""),
		"init", "--project", root, "--non-interactive", "--json")
	if exitCode != 0 || stderr != "" || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("fresh init exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	state, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Project.Agents, []string{"claude-code", "codex"}) || state.Project.Freshness != "outdated" {
		t.Fatalf("detected selection = %#v", state.Project)
	}

	stdout, stderr, exitCode = hostileRunBinary(t, binary, t.TempDir(), strings.NewReader(""),
		"init", "--project", root, "--agent", "cursor", "--agent", "cursor", "--json")
	if exitCode != 0 || stderr != "" || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("override init exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	state, err = dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Project.Agents, []string{"cursor"}) {
		t.Fatalf("override selection = %#v", state.Project.Agents)
	}
}
