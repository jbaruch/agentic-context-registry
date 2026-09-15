// Package versioncheck tells a terminal operator that a newer acr release
// exists and names the upgrade command for the way this copy was installed.
//
// The notice is served from a machine-local cache so the network is never in
// front of the operator's command: Notice reads one small file before the
// command runs, and Refresh rewrites that file after the command's output, at
// most once a day per machine, under a hard timeout. Every failure along the
// way is classified and returned to the caller, never printed: the command's
// own output and exit code are the contract, and this package adds one line
// to stderr or nothing at all.
package versioncheck

import (
	"strings"

	"golang.org/x/mod/semver"
)

// canonical puts a version in the form the semver package compares. Release
// tags carry the v prefix and linker-supplied versions do not; both name the
// same release.
func canonical(version string) string {
	return "v" + strings.TrimPrefix(version, "v")
}

// comparable reports whether a version orders at all. A development build
// reports "dev", which is not a version and never produces a notice.
func comparable(version string) bool {
	return semver.IsValid(canonical(version))
}

// stable reports whether a version is a release, not a prerelease. The check
// watches stable releases only, so a prerelease tag never counts as latest.
func stable(version string) bool {
	return comparable(version) && semver.Prerelease(canonical(version)) == ""
}

// Newer reports whether latest is a stable release strictly after running.
// It is false whenever either side cannot be compared, so an unknown running
// version and a malformed cached version are both silent.
func Newer(running, latest string) bool {
	return comparable(running) && stable(latest) && semver.Compare(canonical(latest), canonical(running)) > 0
}

// Display renders a version the way the notice names it: without the v
// prefix, matching what acr version prints for a release build.
func Display(version string) string {
	return strings.TrimPrefix(version, "v")
}
