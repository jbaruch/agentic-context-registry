package tesslplugin

import (
	"errors"
	"fmt"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// Error is a conversion failure with a stable machine-readable code.
type Error struct {
	Code    string
	Field   string
	Message string
	// Activation mapping and manifest validation share a public error code.
	// Only mapping failures require a manual policy decision.
	manualMigration bool
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

const (
	CodeUnknownField         = "unknown_field"
	CodeUnmappedField        = "unmapped_field"
	CodeAgentWidening        = "agent_widening"
	CodeAmbiguousManifest    = "ambiguous_manifest"
	CodeManifestConflict     = "manifest_conflict"
	CodeUnpublishableContent = "unpublishable_content"
)

func conversionError(code, field, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

// ManualMigrationErrors returns all semantic mapping failures, or nil if any
// failure needs an ordinary error response instead of semantic conversion.
func ManualMigrationErrors(err error) []*Error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var result []*Error
		for _, child := range joined.Unwrap() {
			findings := ManualMigrationErrors(child)
			if len(findings) == 0 {
				return nil
			}
			result = append(result, findings...)
		}
		return result
	}
	var conv *Error
	if errors.As(err, &conv) {
		switch conv.Code {
		case CodeUnmappedField, CodeAgentWidening:
			return []*Error{conv}
		case string(manifest.CodeInvalidRuleActivation):
			if conv.manualMigration {
				return []*Error{conv}
			}
		}
	}
	return nil
}

func recordUnmapped(report *Report, err error) {
	if findings := ManualMigrationErrors(err); len(findings) != 0 {
		for _, conv := range findings {
			report.Unmapped = append(report.Unmapped, UnmappedItem{Field: conv.Field, Reason: conv.Message})
		}
		return
	}
	var conv *Error
	if !errors.As(err, &conv) {
		return
	}
	switch conv.Code {
	case CodeUnknownField:
		report.Unmapped = append(report.Unmapped, UnmappedItem{Field: conv.Field, Reason: conv.Message})
	}
}
