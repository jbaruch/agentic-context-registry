package preserve

import (
	"bytes"
	"fmt"
	"regexp"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

const CodeMarkerConflict = "marker_conflict"

var (
	beginMarkerPattern = regexp.MustCompile(`^<!-- acr:begin id=([0-9a-f]{64}) source=(github:[a-z0-9][a-z0-9-]*/[a-z0-9][a-z0-9._-]*) artifact=([a-z][a-z0-9]*(?:-[a-z0-9]+)*) adapter=([a-z][a-z0-9]*(?:-[a-z0-9]+)*) prefix=(none|lf|crlf) -->$`)
	endMarkerPattern   = regexp.MustCompile(`^<!-- acr:end id=([0-9a-f]{64}) -->$`)
)

type markerLine struct {
	start      int
	contentEnd int
	end        int
	eol        []byte
}

type markdownBlock struct {
	id         string
	source     string
	artifactID string
	adapterID  string
	prefix     string
	start      int
	openStart  int
	bodyStart  int
	bodyEnd    int
	end        int
	eol        []byte
	raw        []byte
}

func parseMarkdownBlocks(path string, content []byte) ([]markdownBlock, error) {
	lines := markerLineSpans(content)
	var blocks []markdownBlock
	var open *markdownBlock
	seen := make(map[string]bool)
	for _, line := range lines {
		physical := content[line.start:line.contentEnd]
		begin := beginMarkerPattern.FindSubmatch(physical)
		end := endMarkerPattern.FindSubmatch(physical)
		switch {
		case begin != nil:
			if open != nil {
				return nil, conflict(CodeMarkerConflict, path, "managed Markdown blocks may not nest")
			}
			id := string(begin[1])
			source := string(begin[2])
			artifactID := string(begin[3])
			adapterID := string(begin[4])
			prefix := string(begin[5])
			owner := adapter.OwnerRef{Source: source, ArtifactID: artifactID}
			if id != adapter.CanonicalMarkdownBlockID(owner, adapterID) {
				return nil, conflict(CodeMarkerConflict, path, fmt.Sprintf("marker %s does not match its source, artifact, and adapter attribution", id))
			}
			if seen[id] {
				return nil, conflict(CodeMarkerConflict, path, fmt.Sprintf("managed marker %s occurs more than once", id))
			}
			if len(line.eol) == 0 {
				return nil, conflict(CodeMarkerConflict, path, fmt.Sprintf("opening marker %s must be a complete physical line", id))
			}
			start, err := markerOwnedStart(content, line.start, prefix)
			if err != nil {
				return nil, conflict(CodeMarkerConflict, path, fmt.Sprintf("marker %s: %v", id, err))
			}
			open = &markdownBlock{
				id: id, source: source, artifactID: artifactID, adapterID: adapterID,
				prefix: prefix, start: start, openStart: line.start, bodyStart: line.end,
				eol: append([]byte(nil), line.eol...),
			}
		case end != nil:
			if open == nil {
				return nil, conflict(CodeMarkerConflict, path, "managed Markdown end marker has no matching begin marker")
			}
			if string(end[1]) != open.id {
				return nil, conflict(CodeMarkerConflict, path, fmt.Sprintf("managed Markdown end marker %s does not match begin marker %s", end[1], open.id))
			}
			open.bodyEnd = line.start
			open.end = line.end
			open.raw = content[open.start:open.end]
			blocks = append(blocks, *open)
			seen[open.id] = true
			open = nil
		case bytes.HasPrefix(physical, []byte("<!-- acr:")):
			return nil, conflict(CodeMarkerConflict, path, fmt.Sprintf("ambiguous ACR marker-looking line %q", physical))
		}
	}
	if open != nil {
		return nil, conflict(CodeMarkerConflict, path, fmt.Sprintf("managed marker %s has no matching end marker", open.id))
	}
	return blocks, nil
}

func markerLineSpans(content []byte) []markerLine {
	if len(content) == 0 {
		return nil
	}
	var lines []markerLine
	for start := 0; start < len(content); {
		index := bytes.IndexByte(content[start:], '\n')
		if index < 0 {
			lines = append(lines, markerLine{start: start, contentEnd: len(content), end: len(content)})
			break
		}
		end := start + index + 1
		contentEnd := end - 1
		eol := []byte{'\n'}
		if contentEnd > start && content[contentEnd-1] == '\r' {
			contentEnd--
			eol = []byte{'\r', '\n'}
		}
		lines = append(lines, markerLine{start: start, contentEnd: contentEnd, end: end, eol: eol})
		start = end
	}
	return lines
}

func markerOwnedStart(content []byte, openingStart int, prefix string) (int, error) {
	switch prefix {
	case "none":
		return openingStart, nil
	case "lf":
		if openingStart < 1 || content[openingStart-1] != '\n' || (openingStart >= 2 && content[openingStart-2] == '\r') {
			return 0, fmt.Errorf("prefix=lf is not backed by an owned LF separator")
		}
		return openingStart - 1, nil
	case "crlf":
		if openingStart < 2 || !bytes.Equal(content[openingStart-2:openingStart], []byte("\r\n")) {
			return 0, fmt.Errorf("prefix=crlf is not backed by an owned CRLF separator")
		}
		return openingStart - 2, nil
	default:
		return 0, fmt.Errorf("unsupported prefix %q", prefix)
	}
}

func firstEOL(content []byte) []byte {
	index := bytes.IndexByte(content, '\n')
	if index > 0 && content[index-1] == '\r' {
		return []byte("\r\n")
	}
	if index >= 0 {
		return []byte("\n")
	}
	return []byte("\n")
}
