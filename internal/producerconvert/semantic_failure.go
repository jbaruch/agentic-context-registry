package producerconvert

import "fmt"

// Classify at the trusted validation origin, never from diagnostic wording.
// Attribution may locate a path later, but cannot turn an operational failure
// (including a cleanup error joined to validation feedback) into repair evidence.
type semanticValidationFailure struct{ cause error }

func (e *semanticValidationFailure) Error() string { return e.cause.Error() }
func (e *semanticValidationFailure) Unwrap() error { return e.cause }
func semanticValidation(err error) error {
	if err == nil {
		return nil
	}
	return &semanticValidationFailure{err}
}
func semanticErrorf(format string, args ...any) error {
	return semanticValidation(fmt.Errorf(format, args...))
}
func onlySemanticValidation(err error) bool {
	if _, ok := err.(*semanticValidationFailure); ok {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlySemanticValidation(child) {
				return false
			}
		}
		return true
	}
	return false
}
func onlySemanticRefusal(err error) bool {
	if refusal, ok := err.(*Error); ok {
		return refusal.Code == "unsupported_semantic_conversion"
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlySemanticRefusal(child) {
				return false
			}
		}
		return true
	}
	return false
}
