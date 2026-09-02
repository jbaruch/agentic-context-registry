package release

import (
	"encoding/json"
	"fmt"
	"regexp"
)

var cyclonedxVersionPattern = regexp.MustCompile(`^1\.[0-9]+$`)

type cyclonedxDocument struct {
	BOMFormat   string `json:"bomFormat"`
	SpecVersion string `json:"specVersion"`
	Metadata    struct {
		Component struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"component"`
	} `json:"metadata"`
}

// ValidateSBOM verifies the release identity fields in a generated CycloneDX
// document. The upstream generator remains responsible for the full schema.
func ValidateSBOM(contents []byte, version string) error {
	var document cyclonedxDocument
	if err := json.Unmarshal(contents, &document); err != nil {
		return fmt.Errorf("validate release SBOM: decode CycloneDX JSON: %w; regenerate acr.cdx.json and retry", err)
	}
	if document.BOMFormat != "CycloneDX" {
		return fmt.Errorf("validate release SBOM: bomFormat is %q, expected CycloneDX; regenerate acr.cdx.json and retry", document.BOMFormat)
	}
	if !cyclonedxVersionPattern.MatchString(document.SpecVersion) {
		return fmt.Errorf("validate release SBOM: specVersion is %q, expected a CycloneDX 1.x version; regenerate acr.cdx.json and retry", document.SpecVersion)
	}
	if document.Metadata.Component.Name != "acr" {
		return fmt.Errorf("validate release SBOM: metadata component name is %q, expected acr; set the generated application identity and retry", document.Metadata.Component.Name)
	}
	if document.Metadata.Component.Version != version {
		return fmt.Errorf("validate release SBOM: metadata component version is %q, expected %q; regenerate the SBOM for this release", document.Metadata.Component.Version, version)
	}
	return nil
}
