// Package diagnostic bounds and escapes untrusted error details for display.
package diagnostic

import (
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"
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
	return boundOpaqueError(err, source.Error())
}

func boundOpaqueError(err error, detail string) error {
	display := strings.ToValidUTF8(detail, "\ufffd")
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

// Token bounds and quotes one untrusted operand. It lives here for the
// packages cli imports, which cannot call cli.DiagnosticToken.
func Token(value string) string {
	const limit = 128
	if len(value) > limit {
		value = value[:limit] + "...[truncated]"
	}
	return strconv.Quote(value)
}

// PathCause is OpaqueError for a caller that already displays the failed path
// as a bounded operand, so the cause does not repeat that untrusted path.
// Composed operation context remains intact.
func PathCause(err error) error {
	source := err
	if opaque, ok := source.(*opaqueError); ok {
		source = opaque.original
	}
	switch pathErr := source.(type) {
	case *fs.PathError:
		source = pathErr.Err
	case *os.LinkError:
		source = pathErr.Err
	default:
		return OpaqueError(err)
	}
	return &opaqueError{display: OpaqueError(source).Error(), original: err}
}

// Escape makes a composed explanation safe to print without truncating it.
// Untrusted operands inside the explanation are bounded by their producers.
func Escape(value string) string {
	var builder strings.Builder
	consumed := 0
	for consumed < len(value) {
		r, size := utf8.DecodeRuneInString(value[consumed:])
		consumed += size
		if unicode.IsPrint(r) {
			builder.WriteRune(r)
		} else if r <= 0xffff {
			fmt.Fprintf(&builder, `\u%04x`, r)
		} else {
			fmt.Fprintf(&builder, `\U%08x`, r)
		}
	}
	return builder.String()
}
