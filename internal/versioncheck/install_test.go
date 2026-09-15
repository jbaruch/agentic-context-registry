package versioncheck

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestClassifyDecidesFromInjectedPathsAlone is the detection table: every row
// of the owner's design, plus the boundaries a prefix comparison would get
// wrong, all decided from injected values and never from this machine.
func TestClassifyDecidesFromInjectedPathsAlone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  Environment
		want Installation
	}{
		{name: "homebrew apple silicon", env: Environment{Executable: "/opt/homebrew/Cellar/acr/0.2.1/bin/acr"}, want: InstallationHomebrew},
		{name: "homebrew intel", env: Environment{Executable: "/usr/local/Cellar/acr/0.2.1/bin/acr"}, want: InstallationHomebrew},
		{name: "linuxbrew", env: Environment{Executable: "/home/linuxbrew/.linuxbrew/Cellar/acr/0.2.1/bin/acr"}, want: InstallationHomebrew},
		{name: "cellar beats go bin", env: Environment{Executable: "/srv/go/bin/Cellar/acr", GOBIN: "/srv/go/bin/Cellar"}, want: InstallationHomebrew},
		{name: "binary named Cellar is not a cellar", env: Environment{Executable: "/opt/tools/Cellar", Home: "/home/u"}, want: InstallationDirect},
		{name: "cellar as a prefix is not a component", env: Environment{Executable: "/opt/Cellars/acr", Home: "/home/u"}, want: InstallationDirect},
		{name: "GOBIN", env: Environment{Executable: "/srv/gobin/acr", GOBIN: "/srv/gobin", GOPATH: "/home/u/go", Home: "/home/u"}, want: InstallationGo},
		{name: "GOBIN with trailing separator", env: Environment{Executable: "/srv/gobin/acr", GOBIN: "/srv/gobin/"}, want: InstallationGo},
		{name: "GOBIN set excludes GOPATH bin", env: Environment{Executable: "/home/u/go/bin/acr", GOBIN: "/srv/gobin", GOPATH: "/home/u/go", Home: "/home/u"}, want: InstallationDirect},
		{name: "GOPATH bin", env: Environment{Executable: "/home/u/go/bin/acr", GOPATH: "/home/u/go", Home: "/home/u"}, want: InstallationGo},
		{name: "GOPATH list uses its first entry", env: Environment{Executable: "/first/bin/acr", GOPATH: "/first" + string(filepath.ListSeparator) + "/second"}, want: InstallationGo},
		{name: "GOPATH list ignores later entries", env: Environment{Executable: "/second/bin/acr", GOPATH: "/first" + string(filepath.ListSeparator) + "/second"}, want: InstallationDirect},
		{name: "GOPATH set excludes HOME go bin", env: Environment{Executable: "/home/u/go/bin/acr", GOPATH: "/elsewhere", Home: "/home/u"}, want: InstallationDirect},
		{name: "HOME go bin", env: Environment{Executable: "/home/u/go/bin/acr", Home: "/home/u"}, want: InstallationGo},
		{name: "sibling directory sharing the prefix", env: Environment{Executable: "/home/u/go/binx/acr", Home: "/home/u"}, want: InstallationDirect},
		{name: "hyphenated sibling sharing the prefix", env: Environment{Executable: "/home/u/go/bin-acr/acr", Home: "/home/u"}, want: InstallationDirect},
		{name: "nested beneath the bin directory", env: Environment{Executable: "/home/u/go/bin/nested/acr", Home: "/home/u"}, want: InstallationDirect},
		{name: "bin directory itself is not a binary", env: Environment{Executable: "/home/u/go/bin", Home: "/home/u"}, want: InstallationDirect},
		{name: "direct download in usr local bin", env: Environment{Executable: "/usr/local/bin/acr", Home: "/home/u"}, want: InstallationDirect},
		{name: "direct download with no home", env: Environment{Executable: "/tmp/acr"}, want: InstallationDirect},
		{name: "unresolvable", env: Environment{Home: "/home/u"}, want: InstallationUnknown},
		{name: "relative path is unresolvable", env: Environment{Executable: "acr", Home: "/home/u"}, want: InstallationUnknown},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(test.env); got != test.want {
				t.Fatalf("Classify(%+v) = %q, want %q", test.env, got, test.want)
			}
		})
	}
}

// TestRenderNamesBothVersionsAndTheMatchingCommand pins the exact line for
// every installation, byte for byte, including the trailing newline and the
// absence of a dangling space when there is no command to name.
func TestRenderNamesBothVersionsAndTheMatchingCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		installation Installation
		want         string
	}{
		{name: "homebrew", installation: InstallationHomebrew, want: "acr 0.2.0 is available (you have 0.1.0). Upgrade: brew upgrade jbaruch/agentic-context-registry/acr\n"},
		{name: "go", installation: InstallationGo, want: "acr 0.2.0 is available (you have 0.1.0). Upgrade: go install github.com/jbaruch/agentic-context-registry/cmd/acr@latest\n"},
		{name: "direct", installation: InstallationDirect, want: "acr 0.2.0 is available (you have 0.1.0). Upgrade: download https://github.com/jbaruch/agentic-context-registry/releases/latest and verify it as described in https://github.com/jbaruch/agentic-context-registry/blob/main/docs/install.md\n"},
		{name: "unknown", installation: InstallationUnknown, want: "acr 0.2.0 is available (you have 0.1.0).\n"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := Render("0.1.0", "v0.2.0", test.installation)
			if got != test.want {
				t.Fatalf("Render() = %q, want %q", got, test.want)
			}
			if strings.Count(got, "\n") != 1 {
				t.Fatalf("Render() = %q, want exactly one line", got)
			}
		})
	}
}

// TestProcessEnvironmentReadsTheVariablesGoInstallReads proves the shipped
// detection reads GOBIN, GOPATH and HOME from the process, never from a go
// env subprocess, and resolves the test binary's own path.
func TestProcessEnvironmentReadsTheVariablesGoInstallReads(t *testing.T) {
	t.Setenv("GOBIN", "/injected/gobin")
	t.Setenv("GOPATH", "/injected/gopath")
	t.Setenv("HOME", "/injected/home")

	env := ProcessEnvironment()
	if env.GOBIN != "/injected/gobin" || env.GOPATH != "/injected/gopath" || env.Home != "/injected/home" {
		t.Fatalf("ProcessEnvironment() = %+v, want the injected variables", env)
	}
	if env.Executable == "" || !filepath.IsAbs(env.Executable) {
		t.Fatalf("ProcessEnvironment().Executable = %q, want the resolved test binary", env.Executable)
	}
}
