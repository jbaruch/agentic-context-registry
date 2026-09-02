// Package buildinfo resolves the version and source revision embedded in acr.
package buildinfo

import "runtime/debug"

// Build identifies the source used to produce an acr executable.
type Build struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
}

// String returns the human-readable build identifier.
func (build Build) String() string {
	if build.Commit == "" {
		return build.Version
	}
	return build.Version + " (" + build.Commit + ")"
}

// Resolve applies release linker values before Go's embedded module and VCS
// information. A local development build remains identifiable as dev.
func Resolve(linkedVersion, linkedCommit string, info *debug.BuildInfo) Build {
	build := Build{Version: "dev"}
	if linkedVersion != "" && linkedVersion != "dev" {
		build.Version = linkedVersion
	} else if info != nil && info.Main.Version != "" && info.Main.Version != "(devel)" {
		build.Version = info.Main.Version
	}

	if linkedCommit != "" {
		build.Commit = linkedCommit
		return build
	}
	if info == nil {
		return build
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			build.Commit = setting.Value
			break
		}
	}
	return build
}
