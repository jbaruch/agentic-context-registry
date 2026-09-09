package packageref

import (
	"bytes"
	"fmt"
	"strings"
)

// RewriteFiles rewrites only complete, declared file paths at the same token
// boundaries used by native realization. Roots identify owned but unsupported
// paths: missing files, directory references and dynamic suffixes must refuse.
// Foreign paths and text outside reference positions retain every byte.
func RewriteFiles(content []byte, files map[string]string, roots []string) ([]byte, error) {
	result := make([]byte, 0, len(content))
	scanner := newReferenceScanner(content)
	for scanner.index < len(content) {
		if !scanner.atReferenceStart() && scanner.index > 0 && (content[scanner.index-1] == '\'' || content[scanner.index-1] == '"') {
			// A quoted string in an opaque context (compact JSON, a function
			// call, or an argument interior) is outside native rebasing's
			// grammar. Do not silently leave an owned runtime path behind.
			for _, root := range roots {
				if bytes.HasPrefix(content[scanner.index:], []byte(root)) {
					return nil, fmt.Errorf("owned reference %q is in an unsupported quoted/structured context; a semantic conversion is required", boundedToken(content[scanner.index:]))
				}
			}
		}
		if scanner.atReferenceStart() {
			rest := content[scanner.index:]
			carried := 0
			if rest[0] == '\\' {
				carried++
			}
			carried += assignmentWidth(rest[carried:])
			token := rest[carried:]
			owned := false
			for _, root := range roots {
				if bytes.HasPrefix(token, []byte(root)) {
					owned = true
					break
				}
			}
			if owned {
				end := 0
				for end < len(token) && pathByte(token[end]) {
					end++
				}
				old := string(token[:end])
				next, exists := files[old]
				if !exists || end < len(token) && !pathTerminator(token[end]) {
					return nil, fmt.Errorf("owned reference %q is not a complete declared file path; dynamic paths and missing destinations require a semantic conversion", boundedToken(token))
				}
				result = append(result, rest[:carried]...)
				result = append(result, next...)
				scanner.consume(carried + end)
				continue
			}
		}
		result = append(result, content[scanner.index])
		scanner.consume(1)
	}
	return result, nil
}

func pathByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("/._-", rune(b))
}
func pathTerminator(b byte) bool {
	return isReferenceSpace(b) || strings.ContainsRune("\"'`)]>,;:#", rune(b))
}
func boundedToken(token []byte) string {
	end := 0
	for end < len(token) && end < 160 && !isReferenceSpace(token[end]) {
		end++
	}
	return string(token[:end])
}
