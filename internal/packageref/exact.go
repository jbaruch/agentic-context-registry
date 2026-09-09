package packageref

import (
	"bytes"
	"fmt"
	"strings"
)

// RewriteFiles rewrites only complete, declared file paths at the same token
// boundaries used by native realization. Roots identify owned but unsupported
// paths: missing files, directory references and dynamic suffixes must refuse.
// Foreign paths and URLs retain every byte; unsupported owned positions refuse.
func RewriteFiles(content []byte, files map[string]string, roots []string) ([]byte, error) {
	result := make([]byte, 0, len(content))
	scanner := newReferenceScanner(content)
	for scanner.index < len(content) {
		// Inspect every unsupported position, including emphasis, redirections
		// and quoted interiors. A longer foreign path or URL is not owned.
		if !scanner.atReferenceStart() && scanner.index > 0 && !pathByte(content[scanner.index-1]) && ownedPrefix(content[scanner.index:], roots) && !inURL(content, scanner.index) {
			return nil, fmt.Errorf("owned reference %q is in an unsupported context; a semantic conversion is required", boundedToken(content[scanner.index:]))
		}
		if scanner.atReferenceStart() {
			rest := content[scanner.index:]
			carried := 0
			if rest[0] == '\\' {
				carried++
			}
			carried += assignmentWidth(rest[carried:])
			token := rest[carried:]
			if ownedPrefix(token, roots) && !inURL(content, scanner.index) {
				end := 0
				for end < len(token) && pathByte(token[end]) {
					end++
				}
				old := string(token[:end])
				next, exists := files[old]
				if !exists || end < len(token) && !pathTerminator(token[end]) {
					return nil, fmt.Errorf("owned reference %q is not a complete declared file path; missing destinations, dynamic suffixes or unsupported punctuation require a semantic conversion", boundedToken(token))
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

// Roots may name directories or individual artifacts. Match a whole path
// component so similarly named foreign artifacts are not mistaken for ours.
func ownedPrefix(token []byte, roots []string) bool {
	for _, root := range roots {
		root = strings.TrimSuffix(root, "/")
		if bytes.HasPrefix(token, []byte(root)) && (len(token) == len(root) || token[len(root)] == '/' || !pathByte(token[len(root)])) {
			return true
		}
	}
	return false
}

func inURL(content []byte, index int) bool {
	start := index
	for start > 0 && !isReferenceSpace(content[start-1]) {
		start--
	}
	return bytes.Contains(content[start:index], []byte("://"))
}
