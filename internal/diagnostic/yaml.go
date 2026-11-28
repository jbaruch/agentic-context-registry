package diagnostic

import (
	"errors"
	"strings"

	"go.yaml.in/yaml/v4"
)

// OpaqueYAMLError retains the decoder cause and the legacy multiline display
// for YAML field errors. YAML v4 exposes the legacy entries through TypeError
// but no longer renders them on separate lines.
func OpaqueYAMLError(err error) error {
	var fields *yaml.TypeError
	if errors.As(err, &fields) {
		return boundOpaqueError(err, "yaml: unmarshal errors:\n  "+strings.Join(fields.Errors, "\n  "))
	}
	return OpaqueError(err)
}
