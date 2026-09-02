package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestBuildInfoResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		linkedVersion string
		linkedCommit  string
		info          *debug.BuildInfo
		want          Build
	}{
		{name: "ldflags", linkedVersion: "1.2.3", linkedCommit: "linked", info: info("v9.9.9", "embedded"), want: Build{Version: "1.2.3", Commit: "linked"}},
		{name: "module version", linkedVersion: "dev", info: info("v1.2.3", ""), want: Build{Version: "v1.2.3"}},
		{name: "vcs revision", linkedVersion: "dev", info: info("(devel)", "embedded"), want: Build{Version: "dev", Commit: "embedded"}},
		{name: "devel", linkedVersion: "dev", info: info("(devel)", ""), want: Build{Version: "dev"}},
		{name: "proxy install", linkedVersion: "dev", info: info("v1.2.3", ""), want: Build{Version: "v1.2.3"}},
		{name: "missing build info", linkedVersion: "", info: nil, want: Build{Version: "dev"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := Resolve(test.linkedVersion, test.linkedCommit, test.info); got != test.want {
				t.Fatalf("Resolve() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestBuildString(t *testing.T) {
	t.Parallel()

	if got := (Build{Version: "1.2.3"}).String(); got != "1.2.3" {
		t.Fatalf("Build.String() = %q", got)
	}
	if got := (Build{Version: "1.2.3", Commit: "abc123"}).String(); got != "1.2.3 (abc123)" {
		t.Fatalf("Build.String() = %q", got)
	}
}

func info(version, revision string) *debug.BuildInfo {
	result := &debug.BuildInfo{Main: debug.Module{Version: version}}
	if revision != "" {
		result.Settings = []debug.BuildSetting{{Key: "vcs.revision", Value: revision}}
	}
	return result
}
