package tesslplugin

import (
	"errors"
	"fmt"
)

// Error is a conversion failure with a stable machine-readable code.
type Error struct {
	Code    string
	Field   string
	Message string
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

func recordUnmapped(report *Report, err error) {
	var conv *Error
	if !errors.As(err, &conv) {
		return
	}
	switch conv.Code {
	case CodeUnmappedField, CodeUnknownField, CodeAgentWidening:
		report.Unmapped = append(report.Unmapped, UnmappedItem{Field: conv.Field, Reason: conv.Message})
	}
}
