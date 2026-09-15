package versioncheck

import "testing"

// TestNewerOrdersStableReleasesOnly is the comparison table: a notice fires
// only for a stable release strictly after a comparable running version.
func TestNewerOrdersStableReleasesOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		running string
		latest  string
		want    bool
	}{
		{name: "older linker version", running: "0.1.0", latest: "v0.2.0", want: true},
		{name: "older module version", running: "v0.1.0", latest: "v0.2.0", want: true},
		{name: "patch behind", running: "0.2.0", latest: "v0.2.1", want: true},
		{name: "pseudo-version behind", running: "v0.0.0-20260901120000-abcdefabcdef", latest: "v0.2.0", want: true},
		{name: "running prerelease of the latest", running: "v0.2.0-rc1", latest: "v0.2.0", want: true},
		{name: "short form latest orders as semver does", running: "0.1.0", latest: "v0.2", want: true},
		{name: "current", running: "0.2.0", latest: "v0.2.0", want: false},
		{name: "current without prefix on either side", running: "0.2.0", latest: "0.2.0", want: false},
		{name: "newer", running: "0.3.0", latest: "v0.2.0", want: false},
		{name: "running prerelease of a later version", running: "v0.3.0-rc1", latest: "v0.2.0", want: false},
		{name: "development build", running: "dev", latest: "v0.2.0", want: false},
		{name: "empty running", running: "", latest: "v0.2.0", want: false},
		{name: "latest prerelease is not stable", running: "0.1.0", latest: "v0.2.0-rc1", want: false},
		{name: "latest garbage", running: "0.1.0", latest: "latest", want: false},
		{name: "latest empty", running: "0.1.0", latest: "", want: false},
		{name: "latest with too many parts", running: "0.1.0", latest: "v1.2.3.4", want: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := Newer(test.running, test.latest); got != test.want {
				t.Fatalf("Newer(%q, %q) = %t, want %t", test.running, test.latest, got, test.want)
			}
		})
	}
}

func TestDisplayDropsThePrefixOnly(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{"v0.2.0": "0.2.0", "0.2.0": "0.2.0", "v0.2.0-rc1": "0.2.0-rc1"} {
		if got := Display(input); got != want {
			t.Errorf("Display(%q) = %q, want %q", input, got, want)
		}
	}
}
