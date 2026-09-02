package release

import (
	"fmt"
	"strings"
	"testing"
)

func TestSBOMRequiredFields(t *testing.T) {
	t.Parallel()

	document := []byte(`{
  "bomFormat": "CycloneDX",
  "specVersion": "1.6",
  "metadata": {
    "component": {
      "type": "application",
      "name": "acr",
      "version": "1.2.3"
    }
  },
  "components": []
}`)
	if err := ValidateSBOM(document, "1.2.3"); err != nil {
		t.Fatal(err)
	}
}

func TestSBOMRejectsMissingOrMismatchedIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		document string
		want     string
	}{
		{name: "malformed", document: `{`, want: "decode CycloneDX JSON"},
		{name: "format", document: sbomFixture("Other", "1.6", "acr", "1.2.3"), want: "bomFormat"},
		{name: "spec", document: sbomFixture("CycloneDX", "", "acr", "1.2.3"), want: "specVersion"},
		{name: "name", document: sbomFixture("CycloneDX", "1.6", "module", "1.2.3"), want: "component name"},
		{name: "version", document: sbomFixture("CycloneDX", "1.6", "acr", "1.2.4"), want: "component version"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateSBOM([]byte(test.document), "1.2.3"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateSBOM() error = %v, want %q", err, test.want)
			}
		})
	}
}

func sbomFixture(format, specification, name, version string) string {
	return fmt.Sprintf(`{"bomFormat":%q,"specVersion":%q,"metadata":{"component":{"name":%q,"version":%q}}}`, format, specification, name, version)
}
