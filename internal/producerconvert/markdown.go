package producerconvert

import (
	"bytes"
	"regexp"
)

// markdownCode returns the literal code a Markdown instruction asks a reader to
// run: fenced code blocks, indented code blocks and inline code spans holding a
// command with arguments. Prose, links and single-name spans such as
// `.tessl-plugin` stay ordinary content. List items are followed far enough to
// keep their continuation paragraphs out of the indented form; block quotes and
// raw HTML are not interpreted.
func markdownCode(data []byte) []byte {
	var code bytes.Buffer
	var fence []byte
	var htmlEnd []byte
	quoted := false
	var items []int // content columns of the open list items
	indented, blockStart := false, true
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := bytes.TrimRight(raw, "\r")
		content := bytes.TrimLeft(line, " \t")
		indent := columns(line[:len(line)-len(content)])
		if htmlEnd != nil {
			if len(content) == 0 || bytes.Contains(bytes.ToLower(content), htmlEnd) {
				htmlEnd, blockStart = nil, true
			}
			continue
		}
		if fence != nil {
			if indent-listColumn(items) <= 3 && closesFence(content, fence) {
				fence, blockStart = nil, true
				continue
			}
			code.Write(line)
			code.WriteByte('\n')
			continue
		}
		if indented {
			if len(content) == 0 || indent >= listColumn(items)+4 {
				code.Write(line)
				code.WriteByte('\n')
				continue
			}
			indented = false
		}
		if len(content) == 0 {
			blockStart, quoted = true, false
			continue
		}
		for len(items) > 0 && indent < items[len(items)-1] {
			items = items[:len(items)-1]
		}
		base := listColumn(items)
		if indent-base <= 3 {
			if opened := openFence(content); opened != nil {
				fence = opened
				continue
			}
		}
		if blockStart && indent >= base+4 {
			indented, blockStart = true, false
			code.Write(line)
			code.WriteByte('\n')
			continue
		}
		if thematicBreak(content) {
			blockStart = true
			continue
		}
		if width := listMarker(content); width > 0 {
			items = append(items, indent+width)
			content = content[min(width, len(content)):]
		}
		if bytes.HasPrefix(content, []byte(">")) {
			quoted, blockStart = true, true
			continue
		}
		if quoted && !heading(content) {
			// A paragraph may lazily continue a quote without another marker.
			continue
		}
		quoted = false
		if end := rawHTMLEnd(content); end != nil {
			if !bytes.Contains(bytes.ToLower(content), end) {
				htmlEnd = end
			}
			blockStart = true
			continue
		}
		blockStart = heading(content)
		code.Write(inlineCommands(content))
	}
	return code.Bytes()
}

var rawHTMLTag = regexp.MustCompile(`^</?([A-Za-z][A-Za-z0-9-]*)(?:[\t />]|$)`)

func rawHTMLEnd(content []byte) []byte {
	for _, delimiters := range [][2]string{{"<!--", "-->"}, {"<?", "?>"}, {"<![CDATA[", "]]>"}, {"<!", ">"}} {
		if bytes.HasPrefix(content, []byte(delimiters[0])) {
			return []byte(delimiters[1])
		}
	}
	if match := rawHTMLTag.FindSubmatch(content); match != nil {
		if bytes.HasPrefix(content, []byte("</")) || bytes.HasSuffix(content, []byte("/>")) {
			return []byte(">")
		}
		return append(append([]byte("</"), bytes.ToLower(match[1])...), '>')
	}
	return nil
}

func columns(prefix []byte) int {
	width := 0
	for _, b := range prefix {
		if b == '\t' {
			width += 4 - width%4
		} else {
			width++
		}
	}
	return width
}

func listColumn(items []int) int {
	if len(items) == 0 {
		return 0
	}
	return items[len(items)-1]
}

func openFence(content []byte) []byte {
	if len(content) < 3 || content[0] != '`' && content[0] != '~' {
		return nil
	}
	n := 0
	for n < len(content) && content[n] == content[0] {
		n++
	}
	if n < 3 || content[0] == '`' && bytes.IndexByte(content[n:], '`') >= 0 {
		return nil
	}
	return content[:n]
}

func closesFence(content, fence []byte) bool {
	n := 0
	for n < len(content) && content[n] == fence[0] {
		n++
	}
	return n >= len(fence) && len(bytes.TrimSpace(content[n:])) == 0
}

func thematicBreak(content []byte) bool {
	marker := content[0]
	if marker != '-' && marker != '*' && marker != '_' {
		return false
	}
	count := 0
	for _, b := range content {
		switch b {
		case marker:
			count++
		case ' ', '\t':
		default:
			return false
		}
	}
	return count >= 3
}

func heading(content []byte) bool {
	n := 0
	for n < len(content) && content[n] == '#' {
		n++
	}
	return n >= 1 && n <= 6 && (n == len(content) || content[n] == ' ' || content[n] == '\t')
}

// listMarker returns the width of a bullet or ordered marker with its
// following spaces, or zero when the line does not open a list item.
func listMarker(content []byte) int {
	n := 0
	switch content[0] {
	case '-', '+', '*':
		n = 1
	default:
		for n < len(content) && n < 9 && content[n] >= '0' && content[n] <= '9' {
			n++
		}
		if n == 0 || n == len(content) || content[n] != '.' && content[n] != ')' {
			return 0
		}
		n++
	}
	if n == len(content) {
		return n + 1
	}
	if content[n] != ' ' && content[n] != '\t' {
		return 0
	}
	spaces := 0
	for n+spaces < len(content) && spaces < 4 && content[n+spaces] == ' ' {
		spaces++
	}
	return n + max(spaces, 1)
}

// A code span is an operation when it holds more than one word. A lone path or
// name is a mention that stays with the prose around it.
func inlineCommands(line []byte) []byte {
	var out []byte
	for i := 0; i < len(line); {
		if line[i] != '`' {
			i++
			continue
		}
		n := 0
		for i+n < len(line) && line[i+n] == '`' {
			n++
		}
		start, end := i+n, -1
		for j := start; j < len(line) && end < 0; {
			if line[j] != '`' {
				j++
				continue
			}
			m := 0
			for j+m < len(line) && line[j+m] == '`' {
				m++
			}
			if m == n {
				end = j
			}
			j += m
		}
		if end < 0 {
			i = start
			continue
		}
		if span := bytes.TrimSpace(line[start:end]); bytes.ContainsAny(span, " \t") {
			out = append(append(out, span...), '\n')
		}
		i = end + n
	}
	return out
}
