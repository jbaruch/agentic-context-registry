package versioncheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Installation classifies where the running executable came from, which
// decides the upgrade command the notice names.
type Installation string

const (
	// InstallationHomebrew is a binary Homebrew linked out of its cellar.
	InstallationHomebrew Installation = "homebrew"
	// InstallationGo is a binary go install wrote into the Go bin directory.
	InstallationGo Installation = "go"
	// InstallationDirect is a binary at any other resolved path, such as a
	// verified direct download.
	InstallationDirect Installation = "direct"
	// InstallationUnknown is a binary whose path could not be resolved.
	InstallationUnknown Installation = "unknown"
)

const (
	// HomebrewUpgrade names the formula in the tap the release workflow
	// publishes to, jbaruch/homebrew-agentic-context-registry.
	HomebrewUpgrade = "brew upgrade jbaruch/agentic-context-registry/acr"
	// GoUpgrade installs the latest stable module version of the command.
	GoUpgrade = "go install github.com/jbaruch/agentic-context-registry/cmd/acr@latest"
	// ReleasesURL is where a direct download starts.
	ReleasesURL = "https://github.com/jbaruch/agentic-context-registry/releases/latest"
	// InstallDocURL describes how to verify a downloaded archive.
	InstallDocURL = "https://github.com/jbaruch/agentic-context-registry/blob/main/docs/install.md"
)

// Environment is everything install detection reads. Detection is pure path
// inspection over these values, so it runs offline, spawns no brew or go
// process, and is tested with injected paths rather than the developer's own
// machine layout.
type Environment struct {
	// Executable is the running binary's path with symlinks resolved, or ""
	// when it could not be resolved.
	Executable string
	GOBIN      string
	GOPATH     string
	Home       string
}

// ProcessEnvironment reads the running process: its executable, resolved
// through every symlink so a Homebrew link lands in the cellar, and the three
// variables that decide where go install writes.
func ProcessEnvironment() Environment {
	env := Environment{GOBIN: os.Getenv("GOBIN"), GOPATH: os.Getenv("GOPATH"), Home: os.Getenv("HOME")}
	executable, err := os.Executable()
	if err != nil {
		return env
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return env
	}
	env.Executable = resolved
	return env
}

// Classify decides the installation from the resolved executable path.
//
// A path with a Cellar directory component is Homebrew's. A binary sitting
// directly in the one directory go install writes to — GOBIN, else the first
// GOPATH entry's bin, else $HOME/go/bin — is a go install. The comparison is
// on the whole parent directory, so a sibling directory that merely shares
// the prefix, or a nested directory beneath the bin directory, is not one.
// Anything else resolvable is a direct install, and an unresolvable path is
// unknown, which the notice answers with no command rather than a guess.
func Classify(env Environment) Installation {
	if env.Executable == "" {
		return InstallationUnknown
	}
	executable := filepath.Clean(env.Executable)
	if !filepath.IsAbs(executable) {
		return InstallationUnknown
	}
	if hasDirectoryComponent(executable, "Cellar") {
		return InstallationHomebrew
	}
	if bin, known := goBinDirectory(env); known && filepath.Dir(executable) == bin {
		return InstallationGo
	}
	return InstallationDirect
}

// hasDirectoryComponent reports whether any directory on the path is named
// component. The basename is excluded: a binary called Cellar is not a cellar.
func hasDirectoryComponent(path, component string) bool {
	for _, element := range strings.Split(filepath.ToSlash(filepath.Dir(path)), "/") {
		if element == component {
			return true
		}
	}
	return false
}

// goBinDirectory returns the directory go install writes executables to, in
// the precedence go itself applies, without running go env.
func goBinDirectory(env Environment) (string, bool) {
	if env.GOBIN != "" {
		return filepath.Clean(env.GOBIN), true
	}
	if entries := filepath.SplitList(env.GOPATH); len(entries) != 0 && entries[0] != "" {
		return filepath.Join(entries[0], "bin"), true
	}
	if env.Home != "" {
		return filepath.Join(env.Home, "go", "bin"), true
	}
	return "", false
}

// Guidance returns the upgrade half of the notice for one installation, and
// "" for an unknown one: the notice still names the versions, and stops
// there rather than printing a command that will fail.
func Guidance(installation Installation) string {
	switch installation {
	case InstallationHomebrew:
		return "Upgrade: " + HomebrewUpgrade
	case InstallationGo:
		return "Upgrade: " + GoUpgrade
	case InstallationDirect:
		return "Upgrade: download " + ReleasesURL + " and verify it as described in " + InstallDocURL
	default:
		return ""
	}
}

// Render is the whole notice: one line, newline-terminated, naming the latest
// version, the running version, and the guidance for the installation.
func Render(running, latest string, installation Installation) string {
	line := fmt.Sprintf("acr %s is available (you have %s).", Display(latest), Display(running))
	if guidance := Guidance(installation); guidance != "" {
		line += " " + guidance
	}
	return line + "\n"
}
