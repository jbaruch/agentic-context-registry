package release

import (
	"fmt"
	"strings"
	"testing"
)

func TestSBOMRequiredFields(t *testing.T) {
	t.Parallel()

	target := Target{GOOS: "linux", GOARCH: "amd64"}
	if err := ValidateSBOM([]byte(sbomFixture("CycloneDX", "1.6", "acr", "1.2.3", target.GOOS, target.GOARCH)), "1.2.3", target); err != nil {
		t.Fatal(err)
	}
}

func TestSBOMRejectsMissingOrMismatchedIdentity(t *testing.T) {
	t.Parallel()

	target := Target{GOOS: "linux", GOARCH: "amd64"}
	tests := []struct {
		name     string
		document string
		want     string
	}{
		{name: "malformed", document: `{`, want: "decode CycloneDX JSON"},
		{name: "format", document: sbomFixture("Other", "1.6", "acr", "1.2.3", "linux", "amd64"), want: "bomFormat"},
		{name: "spec", document: sbomFixture("CycloneDX", "", "acr", "1.2.3", "linux", "amd64"), want: "specVersion"},
		{name: "name", document: sbomFixture("CycloneDX", "1.6", "module", "1.2.3", "linux", "amd64"), want: "component name"},
		{name: "version", document: sbomFixture("CycloneDX", "1.6", "acr", "1.2.4", "linux", "amd64"), want: "component version"},
		{name: "missing GOOS", document: sbomFixtureWithoutProperty("linux", "amd64", cyclonedxPropertyGOOS), want: cyclonedxPropertyGOOS},
		{name: "missing GOARCH", document: sbomFixtureWithoutProperty("linux", "amd64", cyclonedxPropertyGOARCH), want: cyclonedxPropertyGOARCH},
		{name: "missing CGO", document: sbomFixtureWithoutProperty("linux", "amd64", cyclonedxPropertyCGO), want: cyclonedxPropertyCGO},
		{name: "duplicate GOOS", document: sbomFixtureDuplicateGOOS(), want: "appears more than once"},
		{name: "cgo enabled", document: sbomFixtureWithCGO("linux", "amd64", "1"), want: "CGO_ENABLED"},
		{name: "missing purl", document: sbomFixtureWithoutPURL("linux", "amd64"), want: "purl"},
		{name: "purl without goos", document: sbomFixtureWithPURL("linux", "amd64", "pkg:golang/github.com/jbaruch/agentic-context-registry@v0.1.3?goarch=amd64&type=module#cmd/acr"), want: "goos"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateSBOM([]byte(test.document), "1.2.3", target); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateSBOM() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSBOMRejectsMismatchedAndSwappedTargets(t *testing.T) {
	t.Parallel()

	documents := map[Target][]byte{}
	for _, target := range Targets() {
		documents[target] = []byte(sbomFixture("CycloneDX", "1.6", "acr", "1.2.3", target.GOOS, target.GOARCH))
		if err := ValidateSBOM(documents[target], "1.2.3", target); err != nil {
			t.Fatalf("ValidateSBOM(%s) = %v", target.SBOMName(), err)
		}
	}
	for _, target := range Targets() {
		for _, other := range Targets() {
			if other == target {
				continue
			}
			err := ValidateSBOM(documents[other], "1.2.3", target)
			if err == nil {
				t.Fatalf("ValidateSBOM accepted %s document as %s", other.SBOMName(), target.SBOMName())
			}
			if !strings.Contains(err.Error(), target.SBOMName()) {
				t.Fatalf("swapped SBOM error %q does not name %s", err, target.SBOMName())
			}
		}
	}
}

func TestSBOMFilenameIsNotPlatformProof(t *testing.T) {
	t.Parallel()

	linux := Target{GOOS: "linux", GOARCH: "amd64"}
	darwin := Target{GOOS: "darwin", GOARCH: "arm64"}
	renamedLinux := []byte(sbomFixture("CycloneDX", "1.6", "acr", "1.2.3", linux.GOOS, linux.GOARCH))
	if err := ValidateSBOM(renamedLinux, "1.2.3", darwin); err == nil || !strings.Contains(err.Error(), darwin.SBOMName()) {
		t.Fatalf("renamed linux SBOM error = %v, want %s mismatch", err, darwin.SBOMName())
	}
}

func TestExpectedAssetNamesAreTenSortedCLIAssets(t *testing.T) {
	t.Parallel()

	got := ExpectedAssetNames()
	want := []string{
		"acr-darwin-amd64.cdx.json",
		"acr-darwin-amd64.tar.gz",
		"acr-darwin-arm64.cdx.json",
		"acr-darwin-arm64.tar.gz",
		"acr-linux-amd64.cdx.json",
		"acr-linux-amd64.tar.gz",
		"acr-linux-arm64.cdx.json",
		"acr-linux-arm64.tar.gz",
		ChecksumsAssetName,
		SignatureAssetName,
	}
	if len(got) != 10 {
		t.Fatalf("ExpectedAssetNames() = %d, want 10: %v", len(got), got)
	}
	for index, name := range want {
		if got[index] != name {
			t.Fatalf("ExpectedAssetNames()[%d] = %q, want %q", index, got[index], name)
		}
	}
}

func sbomFixture(format, specification, name, version, goos, goarch string) string {
	return fmt.Sprintf(
		`{"bomFormat":%q,"specVersion":%q,"metadata":{"component":{"name":%q,"version":%q,"purl":%q,"properties":[{"name":%q,"value":"0"},{"name":%q,"value":%q},{"name":%q,"value":%q}]}}}`,
		format, specification, name, version,
		fmt.Sprintf("pkg:golang/github.com/jbaruch/agentic-context-registry@v0.1.3?goarch=%s&goos=%s&type=module#cmd/acr", goarch, goos),
		cyclonedxPropertyCGO, cyclonedxPropertyGOARCH, goarch, cyclonedxPropertyGOOS, goos,
	)
}

func sbomFixtureWithoutProperty(goos, goarch, omit string) string {
	properties := []string{
		fmt.Sprintf(`{"name":%q,"value":"0"}`, cyclonedxPropertyCGO),
		fmt.Sprintf(`{"name":%q,"value":%q}`, cyclonedxPropertyGOARCH, goarch),
		fmt.Sprintf(`{"name":%q,"value":%q}`, cyclonedxPropertyGOOS, goos),
	}
	kept := make([]string, 0, 2)
	for _, property := range properties {
		if strings.Contains(property, omit) {
			continue
		}
		kept = append(kept, property)
	}
	return fmt.Sprintf(
		`{"bomFormat":"CycloneDX","specVersion":"1.6","metadata":{"component":{"name":"acr","version":"1.2.3","purl":%q,"properties":[%s]}}}`,
		fmt.Sprintf("pkg:golang/github.com/jbaruch/agentic-context-registry@v0.1.3?goarch=%s&goos=%s&type=module#cmd/acr", goarch, goos),
		strings.Join(kept, ","),
	)
}

func sbomFixtureDuplicateGOOS() string {
	return `{"bomFormat":"CycloneDX","specVersion":"1.6","metadata":{"component":{"name":"acr","version":"1.2.3","purl":"pkg:golang/github.com/jbaruch/agentic-context-registry@v0.1.3?goarch=amd64&goos=linux&type=module#cmd/acr","properties":[{"name":"cdx:gomod:build:env:CGO_ENABLED","value":"0"},{"name":"cdx:gomod:build:env:GOARCH","value":"amd64"},{"name":"cdx:gomod:build:env:GOOS","value":"linux"},{"name":"cdx:gomod:build:env:GOOS","value":"darwin"}]}}}`
}

func sbomFixtureWithCGO(goos, goarch, cgo string) string {
	return fmt.Sprintf(
		`{"bomFormat":"CycloneDX","specVersion":"1.6","metadata":{"component":{"name":"acr","version":"1.2.3","purl":%q,"properties":[{"name":%q,"value":%q},{"name":%q,"value":%q},{"name":%q,"value":%q}]}}}`,
		fmt.Sprintf("pkg:golang/github.com/jbaruch/agentic-context-registry@v0.1.3?goarch=%s&goos=%s&type=module#cmd/acr", goarch, goos),
		cyclonedxPropertyCGO, cgo, cyclonedxPropertyGOARCH, goarch, cyclonedxPropertyGOOS, goos,
	)
}

func sbomFixtureWithoutPURL(goos, goarch string) string {
	return fmt.Sprintf(
		`{"bomFormat":"CycloneDX","specVersion":"1.6","metadata":{"component":{"name":"acr","version":"1.2.3","properties":[{"name":%q,"value":"0"},{"name":%q,"value":%q},{"name":%q,"value":%q}]}}}`,
		cyclonedxPropertyCGO, cyclonedxPropertyGOARCH, goarch, cyclonedxPropertyGOOS, goos,
	)
}

func sbomFixtureWithPURL(goos, goarch, purl string) string {
	return fmt.Sprintf(
		`{"bomFormat":"CycloneDX","specVersion":"1.6","metadata":{"component":{"name":"acr","version":"1.2.3","purl":%q,"properties":[{"name":%q,"value":"0"},{"name":%q,"value":%q},{"name":%q,"value":%q}]}}}`,
		purl, cyclonedxPropertyCGO, cyclonedxPropertyGOARCH, goarch, cyclonedxPropertyGOOS, goos,
	)
}
