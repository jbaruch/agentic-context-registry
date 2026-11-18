package cli

import "strconv"

// DiagnosticToken renders untrusted data as a bounded, quoted display token.
// Its output must never be advertised as an argument or replacement value to copy.
func DiagnosticToken(value string) string {
	const maxBytes = 128
	if len(value) > maxBytes {
		value = value[:maxBytes] + "...[truncated]"
	}
	return strconv.Quote(value)
}
