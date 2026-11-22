package cli

import "github.com/jbaruch/agentic-context-registry/internal/diagnostic"

// DiagnosticToken renders untrusted data as a bounded, quoted display token.
// Its output must never be advertised as an argument or replacement value to copy.
func DiagnosticToken(value string) string {
	return diagnostic.Token(value)
}
