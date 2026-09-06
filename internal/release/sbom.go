package release

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const (
	cyclonedxBuildEnvPrefix = "cdx:gomod:build:env:"
	cyclonedxPropertyGOOS   = cyclonedxBuildEnvPrefix + "GOOS"
	cyclonedxPropertyGOARCH = cyclonedxBuildEnvPrefix + "GOARCH"
	cyclonedxPropertyCGO    = cyclonedxBuildEnvPrefix + "CGO_ENABLED"
)

var cyclonedxVersionPattern = regexp.MustCompile(`^1\.[0-9]+$`)

type cyclonedxDocument struct {
	BOMFormat   string `json:"bomFormat"`
	SpecVersion string `json:"specVersion"`
	Metadata    struct {
		Component struct {
			Name       string `json:"name"`
			Version    string `json:"version"`
			PackageURL string `json:"purl"`
			Properties []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"properties"`
		} `json:"component"`
	} `json:"metadata"`
}

// ValidateSBOM verifies the release identity and cyclonedx-gomod build
// constraints for one target document. The upstream generator remains
// responsible for the full schema.
func ValidateSBOM(contents []byte, version string, target Target) error {
	name := target.SBOMName()
	var document cyclonedxDocument
	if err := json.Unmarshal(contents, &document); err != nil {
		return fmt.Errorf("validate release SBOM %s: decode CycloneDX JSON: %w; regenerate %s and retry", name, err, name)
	}
	if document.BOMFormat != "CycloneDX" {
		return fmt.Errorf("validate release SBOM %s: bomFormat is %q, expected CycloneDX; regenerate %s and retry", name, document.BOMFormat, name)
	}
	if !cyclonedxVersionPattern.MatchString(document.SpecVersion) {
		return fmt.Errorf("validate release SBOM %s: specVersion is %q, expected a CycloneDX 1.x version; regenerate %s and retry", name, document.SpecVersion, name)
	}
	if document.Metadata.Component.Name != "acr" {
		return fmt.Errorf("validate release SBOM %s: metadata component name is %q, expected acr; set the generated application identity and retry", name, document.Metadata.Component.Name)
	}
	if document.Metadata.Component.Version != version {
		return fmt.Errorf("validate release SBOM %s: metadata component version is %q, expected %q; regenerate %s for this release", name, document.Metadata.Component.Version, version, name)
	}
	goos, err := cyclonedxProperty(document, cyclonedxPropertyGOOS)
	if err != nil {
		return fmt.Errorf("validate release SBOM %s: %w; regenerate %s with GOOS=%s and retry", name, err, name, target.GOOS)
	}
	if goos != target.GOOS {
		return fmt.Errorf("validate release SBOM %s: %s is %q, expected %q; regenerate %s for %s/%s and retry", name, cyclonedxPropertyGOOS, goos, target.GOOS, name, target.GOOS, target.GOARCH)
	}
	goarch, err := cyclonedxProperty(document, cyclonedxPropertyGOARCH)
	if err != nil {
		return fmt.Errorf("validate release SBOM %s: %w; regenerate %s with GOARCH=%s and retry", name, err, name, target.GOARCH)
	}
	if goarch != target.GOARCH {
		return fmt.Errorf("validate release SBOM %s: %s is %q, expected %q; regenerate %s for %s/%s and retry", name, cyclonedxPropertyGOARCH, goarch, target.GOARCH, name, target.GOOS, target.GOARCH)
	}
	cgo, err := cyclonedxProperty(document, cyclonedxPropertyCGO)
	if err != nil {
		return fmt.Errorf("validate release SBOM %s: %w; regenerate %s with CGO_ENABLED=0 and retry", name, err, name)
	}
	if cgo != "0" {
		return fmt.Errorf("validate release SBOM %s: %s is %q, expected 0; regenerate %s with CGO_ENABLED=0 and retry", name, cyclonedxPropertyCGO, cgo, name)
	}
	goos, goarch, err = cyclonedxPURLTarget(document.Metadata.Component.PackageURL)
	if err != nil {
		return fmt.Errorf("validate release SBOM %s: %w; regenerate %s for %s/%s and retry", name, err, name, target.GOOS, target.GOARCH)
	}
	if goos != target.GOOS || goarch != target.GOARCH {
		return fmt.Errorf("validate release SBOM %s: purl describes %s/%s, expected %s/%s; regenerate %s for %s/%s and retry", name, goos, goarch, target.GOOS, target.GOARCH, name, target.GOOS, target.GOARCH)
	}
	return nil
}

func cyclonedxProperty(document cyclonedxDocument, name string) (string, error) {
	seen := false
	value := ""
	for _, property := range document.Metadata.Component.Properties {
		if property.Name != name {
			continue
		}
		if seen {
			return "", fmt.Errorf("metadata component property %s appears more than once", name)
		}
		seen = true
		value = property.Value
	}
	if !seen || value == "" {
		return "", fmt.Errorf("metadata component property %s is absent", name)
	}
	return value, nil
}

func cyclonedxPURLTarget(purl string) (string, string, error) {
	_, query, found := strings.Cut(purl, "?")
	if !found {
		return "", "", fmt.Errorf("purl %q has no query string", purl)
	}
	query, _, _ = strings.Cut(query, "#")
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", "", fmt.Errorf("parse purl query: %w", err)
	}
	goos := values["goos"]
	goarch := values["goarch"]
	if len(goos) != 1 || goos[0] == "" {
		return "", "", fmt.Errorf("purl %q does not have exactly one goos qualifier", purl)
	}
	if len(goarch) != 1 || goarch[0] == "" {
		return "", "", fmt.Errorf("purl %q does not have exactly one goarch qualifier", purl)
	}
	return goos[0], goarch[0], nil
}
