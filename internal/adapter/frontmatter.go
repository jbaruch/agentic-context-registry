package adapter

import (
	"bytes"

	"go.yaml.in/yaml/v3"
)

// StripLeadingFrontmatter removes exactly one leading YAML document. Source
// metadata never overrides activation declared by the package manifest.
func StripLeadingFrontmatter(content []byte) ([]byte, error) {
	lines := markerLineSpansForFrontmatter(content)
	if len(lines) == 0 || !bytes.Equal(content[lines[0].start:lines[0].contentEnd], []byte("---")) {
		return append([]byte(nil), content...), nil
	}
	closing := -1
	for index := 1; index < len(lines); index++ {
		if bytes.Equal(content[lines[index].start:lines[index].contentEnd], []byte("---")) {
			closing = index
			break
		}
	}
	if closing < 0 {
		return nil, NativeError(CodeMalformedFrontmatter, "leading YAML document has no closing marker")
	}
	metadata := content[lines[0].end:lines[closing].start]
	var decoded any
	if err := yaml.Unmarshal(metadata, &decoded); err != nil {
		return nil, NativeError(CodeMalformedFrontmatter, "decode leading YAML document: %v", err)
	}
	body := append([]byte(nil), content[lines[closing].end:]...)
	adjacent := bytes.TrimLeft(body, "\r\n")
	if bytes.HasPrefix(adjacent, []byte("---\n")) || bytes.HasPrefix(adjacent, []byte("---\r\n")) || bytes.Equal(adjacent, []byte("---")) {
		return nil, NativeError(CodeMalformedFrontmatter, "a second adjacent YAML frontmatter document is not allowed")
	}
	return body, nil
}

type frontmatterLine struct {
	start      int
	contentEnd int
	end        int
}

func markerLineSpansForFrontmatter(content []byte) []frontmatterLine {
	if len(content) == 0 {
		return nil
	}
	var lines []frontmatterLine
	for start := 0; start < len(content); {
		newline := bytes.IndexByte(content[start:], '\n')
		if newline < 0 {
			lines = append(lines, frontmatterLine{start: start, contentEnd: len(content), end: len(content)})
			break
		}
		end := start + newline + 1
		contentEnd := end - 1
		if contentEnd > start && content[contentEnd-1] == '\r' {
			contentEnd--
		}
		lines = append(lines, frontmatterLine{start: start, contentEnd: contentEnd, end: end})
		start = end
	}
	return lines
}

// ValidateSingleCursorFrontmatter checks the exact typed activation metadata
// Cursor accepts and returns the remaining Markdown body.
func ValidateSingleCursorFrontmatter(content []byte) (map[string]any, []byte, error) {
	lines := markerLineSpansForFrontmatter(content)
	if len(lines) < 2 || !bytes.Equal(content[lines[0].start:lines[0].contentEnd], []byte("---")) {
		return nil, nil, NativeError(CodeMalformedFrontmatter, "Cursor rule must start with YAML frontmatter at byte zero")
	}
	closing := -1
	for index := 1; index < len(lines); index++ {
		if bytes.Equal(content[lines[index].start:lines[index].contentEnd], []byte("---")) {
			closing = index
			break
		}
	}
	if closing < 0 {
		return nil, nil, NativeError(CodeMalformedFrontmatter, "Cursor rule frontmatter has no closing marker")
	}
	metadata := content[lines[0].end:lines[closing].start]
	var values map[string]any
	if err := yaml.Unmarshal(metadata, &values); err != nil {
		return nil, nil, NativeError(CodeMalformedFrontmatter, "decode Cursor rule frontmatter: %v", err)
	}
	body := append([]byte(nil), content[lines[closing].end:]...)
	adjacent := bytes.TrimLeft(body, "\r\n")
	if bytes.HasPrefix(adjacent, []byte("---\n")) || bytes.HasPrefix(adjacent, []byte("---\r\n")) || bytes.Equal(adjacent, []byte("---")) {
		return nil, nil, NativeError(CodeMalformedFrontmatter, "Cursor rule contains a second adjacent frontmatter document")
	}
	if values == nil {
		return nil, nil, NativeError(CodeMalformedFrontmatter, "Cursor rule frontmatter must be a mapping")
	}
	return values, body, nil
}
