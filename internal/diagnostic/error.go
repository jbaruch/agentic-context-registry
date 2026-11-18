// Package diagnostic bounds opaque external error details for display.
package diagnostic

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

type opaqueError struct {
	display  string
	original error
}

func (err *opaqueError) Error() string { return err.display }
func (err *opaqueError) Unwrap() error { return err.original }

// OpaqueError bounds a native error's display while preserving its identity.
// A directly received URL error displays its cause, without the request URL.
// ACR context and recovery instructions belong outside this wrapper.
func OpaqueError(err error) error {
	if err == nil {
		return nil
	}
	source := err
	if urlErr, ok := err.(*url.Error); ok && urlErr.Err != nil {
		source = urlErr.Err
	}
	display := strings.ToValidUTF8(source.Error(), "\ufffd")
	const limit = 16384
	if len(display) > limit {
		end := limit
		for !utf8.RuneStart(display[end]) {
			end--
		}
		display = display[:end] + "...[truncated]"
	}
	return &opaqueError{display: display, original: err}
}
