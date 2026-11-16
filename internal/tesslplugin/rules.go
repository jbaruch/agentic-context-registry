package tesslplugin

import (
	"bytes"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"go.yaml.in/yaml/v3"
)

const emDash = "\u2014"

type ruleFrontmatter struct {
	AlwaysApply *bool  `yaml:"alwaysApply"`
	ApplyTo     string `yaml:"applyTo"`
	Globs       string `yaml:"globs"`
	Paths       string `yaml:"paths"`
	Description string `yaml:"description"`
}

// LossyItem is a Tessl concept copied into the report and omitted from YAML.
type LossyItem struct {
	ID     string `json:"id,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Field  string `json:"field"`
	Value  string `json:"value,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type ruleActivation struct {
	Activation manifest.RuleActivation
	Lossy      []LossyItem
}

func activationFromRuleFile(relative string, content []byte) (ruleActivation, error) {
	metadata, _, hasFrontmatter := splitFirstFrontmatter(content)
	if !hasFrontmatter {
		return ruleActivation{}, conversionError(string(manifest.CodeInvalidRuleActivation), relative,
			"rule %s has no YAML frontmatter; set alwaysApply and convert again", relative)
	}
	var frontmatter ruleFrontmatter
	if err := yaml.Unmarshal(metadata, &frontmatter); err != nil {
		return ruleActivation{}, conversionError(string(manifest.CodeInvalidRuleActivation), relative,
			"decode frontmatter in %s: %v; repair the YAML and convert again", relative, err)
	}
	return activationFromFrontmatter(relative, frontmatter)
}

func activationFromFrontmatter(relative string, frontmatter ruleFrontmatter) (ruleActivation, error) {
	var lossy []LossyItem
	if frontmatter.Description != "" {
		lossy = append(lossy, LossyItem{ID: "", Kind: "rule", Field: relative + "#description", Value: frontmatter.Description, Reason: "description"})
	}
	scoped := firstNonEmpty(frontmatter.ApplyTo, frontmatter.Globs, frontmatter.Paths)
	if frontmatter.AlwaysApply != nil && *frontmatter.AlwaysApply {
		if scoped != "" {
			if strings.Contains(scoped, emDash) {
				paths, prose, _ := parseApplyTo(scoped)
				if len(paths) != 0 {
					lossy = append(lossy, LossyItem{Kind: "rule", Field: relative + "#applyTo", Value: strings.Join(paths, ", "), Reason: "applyTo-globs"})
				}
				if prose != "" {
					lossy = append(lossy, LossyItem{Kind: "rule", Field: relative + "#applyTo", Value: prose, Reason: "applyTo-prose"})
				}
			} else {
				lossy = append(lossy, LossyItem{Kind: "rule", Field: relative + "#applyTo", Value: strings.TrimSpace(scoped), Reason: "applyTo-prose"})
			}
		}
		return ruleActivation{Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}, Lossy: lossy}, nil
	}
	if frontmatter.AlwaysApply == nil {
		return ruleActivation{}, conversionError(string(manifest.CodeInvalidRuleActivation), relative,
			"rule %s must set alwaysApply; activation is not a Tessl manifest field", relative)
	}
	if scoped == "" {
		return ruleActivation{}, conversionError(string(manifest.CodeInvalidRuleActivation), relative,
			"rule %s sets alwaysApply false with no applyTo/globs/paths; add an em-dash-separated glob list", relative)
	}
	if !strings.Contains(scoped, emDash) {
		return ruleActivation{}, conversionError(string(manifest.CodeInvalidRuleActivation), relative,
			"rule %s applyTo/globs/paths must split glob and prose on an em dash; activation cannot be guessed", relative)
	}
	paths, prose, ok := parseApplyTo(scoped)
	if !ok {
		return ruleActivation{}, conversionError(string(manifest.CodeInvalidRuleActivation), relative,
			"rule %s applyTo glob half is empty; declare at least one package-relative glob", relative)
	}
	if prose != "" {
		lossy = append(lossy, LossyItem{Kind: "rule", Field: relative + "#applyTo", Value: prose, Reason: "applyTo-prose"})
	}
	return ruleActivation{
		Activation: manifest.RuleActivation{Mode: manifest.ActivationPaths, Paths: uniquePreserve(paths)},
		Lossy:      lossy,
	}, nil
}

func parseApplyTo(value string) (paths []string, prose string, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, "", false
	}
	globHalf, rest, found := strings.Cut(value, emDash)
	if found {
		prose = strings.TrimSpace(rest)
	} else {
		globHalf = value
	}
	globHalf = strings.TrimSpace(globHalf)
	if globHalf == "" {
		return nil, prose, false
	}
	for _, part := range strings.Split(globHalf, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			paths = append(paths, part)
		}
	}
	return paths, prose, len(paths) != 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func uniquePreserve(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

type physicalLine struct {
	start      int
	contentEnd int
	end        int
}

func splitFirstFrontmatter(content []byte) (metadata, body []byte, ok bool) {
	lines := physicalLines(content)
	if len(lines) == 0 || !bytes.Equal(content[lines[0].start:lines[0].contentEnd], []byte("---")) {
		return nil, content, false
	}
	closing := -1
	for index := 1; index < len(lines); index++ {
		if bytes.Equal(content[lines[index].start:lines[index].contentEnd], []byte("---")) {
			closing = index
			break
		}
	}
	if closing < 0 {
		return nil, content, false
	}
	return content[lines[0].end:lines[closing].start], content[lines[closing].end:], true
}

func physicalLines(content []byte) []physicalLine {
	if len(content) == 0 {
		return nil
	}
	var lines []physicalLine
	for start := 0; start < len(content); {
		newline := bytes.IndexByte(content[start:], '\n')
		if newline < 0 {
			lines = append(lines, physicalLine{start: start, contentEnd: len(content), end: len(content)})
			break
		}
		end := start + newline + 1
		contentEnd := end - 1
		if contentEnd > start && content[contentEnd-1] == '\r' {
			contentEnd--
		}
		lines = append(lines, physicalLine{start: start, contentEnd: contentEnd, end: end})
		start = end
	}
	return lines
}

// RewriteRuleReferences admits flat, single-line metadata before rewriting any
// references. Only the selected activation scalar is preserved byte for byte.
func RewriteRuleReferences(relative string, content []byte, rewrite func([]byte) ([]byte, error)) ([]byte, manifest.RuleActivation, error) {
	metadata, _, _ := splitFirstFrontmatter(content)
	// Decode selection exactly as activationFromRuleFile does, including null and
	// empty fallback. Admission below never relies on YAML node positions.
	var frontmatter ruleFrontmatter
	decodeErr := yaml.Unmarshal(metadata, &frontmatter)
	selected := ""
	if frontmatter.AlwaysApply != nil && !*frontmatter.AlwaysApply {
		for _, field := range []struct{ key, value string }{
			{"applyTo", frontmatter.ApplyTo}, {"globs", frontmatter.Globs}, {"paths", frontmatter.Paths},
		} {
			if strings.TrimSpace(field.value) != "" {
				selected = field.key
				break
			}
		}
	}
	start, end, err := admitFlatRuleMetadata(relative, metadata, selected)
	if err != nil {
		return nil, manifest.RuleActivation{}, err
	}
	if decodeErr != nil {
		return nil, manifest.RuleActivation{}, conversionError(string(manifest.CodeInvalidRuleActivation), relative,
			"decode frontmatter in %s: %v; repair the YAML and convert again", relative, decodeErr)
	}
	parsed, err := activationFromRuleFile(relative, content)
	if err != nil {
		return nil, manifest.RuleActivation{}, err
	}
	// The parser retains this prose as lossy metadata. Check it separately rather
	// than hiding an unsupported destination beside a valid activation glob.
	for _, loss := range parsed.Lossy {
		if loss.Reason == "applyTo-prose" {
			changed, e := rewrite([]byte(loss.Value))
			if e != nil {
				return nil, parsed.Activation, e
			}
			if !bytes.Equal(changed, []byte(loss.Value)) {
				return nil, parsed.Activation, conversionError(string(manifest.CodeInvalidRuleActivation), relative, "activation prose requires explicit content adaptation")
			}
		}
	}
	if selected == "" {
		changed, e := rewrite(content)
		return changed, parsed.Activation, e
	}
	offset := physicalLines(content)[0].end
	start, end = start+offset, end+offset
	before, err := rewrite(content[:start])
	if err != nil {
		return nil, parsed.Activation, err
	}
	after, err := rewrite(content[end:])
	if err != nil {
		return nil, parsed.Activation, err
	}
	result := append([]byte{}, before...)
	result = append(result, content[start:end]...)
	return append(result, after...), parsed.Activation, nil
}

// admitFlatRuleMetadata bounds the exemption to one same-line scalar. Every
// metadata entry must fit this grammar, including entries we do not decode.
func admitFlatRuleMetadata(relative string, metadata []byte, selected string) (start, end int, err error) {
	refuse := func(construct, repair string) (int, int, error) {
		return 0, 0, conversionError(string(manifest.CodeInvalidRuleActivation), relative,
			"rule %s uses unsupported %s; %s and convert again", relative, construct, repair)
	}
	for i, b := range metadata {
		if b == '\r' && (i+1 == len(metadata) || metadata[i+1] != '\n') {
			return refuse("lone CR line break", "use LF or CRLF line endings in frontmatter")
		}
	}
	if bytes.ContainsAny(metadata, "\u0085\u2028\u2029") {
		return refuse("NEL, LS or PS line break", "use LF or CRLF line endings in frontmatter")
	}
	indent := -1
	seen := map[string]bool{}
	for _, line := range physicalLines(metadata) {
		text := metadata[line.start:line.contentEnd]
		spaces := len(text) - len(bytes.TrimLeft(text, " "))
		text = text[spaces:]
		if trimmed := bytes.TrimLeft(text, " \t"); len(trimmed) == 0 || trimmed[0] == '#' {
			continue
		}
		if indent != -1 && spaces != indent {
			return refuse("inconsistent indentation, nesting or continuation", "flatten metadata with consistent space indentation and put every scalar on its key's line")
		}
		indent = spaces
		keyEnd := 0
		for keyEnd < len(text) {
			b := text[keyEnd]
			if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '_' || keyEnd > 0 && b >= '0' && b <= '9') {
				break
			}
			keyEnd++
		}
		colon := keyEnd
		for colon < len(text) && (text[colon] == ' ' || text[colon] == '\t') {
			colon++
		}
		if keyEnd == 0 || colon == len(text) || text[colon] != ':' || colon+1 < len(text) && text[colon+1] != ' ' && text[colon+1] != '\t' {
			construct := "non-identifier key or next-line value"
			switch text[0] {
			case '\'', '"':
				construct = "quoted or escaped key"
			case '?':
				construct = "explicit key"
			case '&':
				construct = "root anchor"
			case '!':
				construct = "root explicit tag"
			case '*':
				construct = "root alias"
			case '<':
				construct = "merge key"
			case '{', '[':
				construct = "root flow collection"
			case '-':
				construct = "block sequence"
			}
			return refuse(construct, "use a flat block mapping with plain ASCII identifier keys and same-line scalar values")
		}
		key := string(text[:keyEnd])
		if seen[key] {
			return refuse("duplicate key "+key, "keep only one entry for each metadata key")
		}
		seen[key] = true
		valueStart := colon + 1
		for valueStart < len(text) && (text[valueStart] == ' ' || text[valueStart] == '\t') {
			valueStart++
		}
		if valueStart == len(text) || text[valueStart] == '#' {
			continue // Empty/null entry; any continuation must be refused separately.
		}
		valueEnd := valueStart
		if quote := text[valueStart]; quote == '\'' || quote == '"' {
			valueEnd++
			closed := false
			for valueEnd < len(text) {
				if quote == '"' && text[valueEnd] == '\\' {
					valueEnd += 2
					continue
				}
				if text[valueEnd] == quote {
					valueEnd++
					if quote == '\'' && valueEnd < len(text) && text[valueEnd] == '\'' {
						valueEnd++
						continue
					}
					closed = true
					break
				}
				valueEnd++
			}
			if !closed {
				return refuse("multiline or unclosed quoted scalar", "close each quoted scalar on its key's line")
			}
			tail := bytes.TrimLeft(text[valueEnd:], " \t")
			if len(tail) > 0 && (len(tail) == len(text[valueEnd:]) || tail[0] != '#') {
				return refuse("text after quoted scalar", "leave only whitespace or a separated comment after the closing quote")
			}
		} else {
			if strings.ContainsRune("!&*{[|>%@`,]}\"'", rune(text[valueStart])) || ((text[valueStart] == '-' || text[valueStart] == '?') && (valueStart+1 == len(text) || text[valueStart+1] == ' ' || text[valueStart+1] == '\t')) {
				return refuse("tag, anchor, alias, collection or block scalar", "flatten metadata and use an empty/null or one-line plain or quoted scalar on its key's line")
			}
			valueEnd = len(text)
			for pos := valueStart; pos < len(text); pos++ {
				if text[pos] == '#' && pos > valueStart && (text[pos-1] == ' ' || text[pos-1] == '\t') {
					valueEnd = pos
					break
				}
				if text[pos] == ':' && (pos+1 == len(text) || text[pos+1] == ' ' || text[pos+1] == '\t') {
					return refuse("nested mapping", "flatten metadata and quote scalar text containing a colon followed by whitespace")
				}
			}
			valueEnd = valueStart + len(bytes.TrimRight(text[valueStart:valueEnd], " \t"))
		}
		if key == selected {
			start, end = line.start+spaces+valueStart, line.start+spaces+valueEnd
		}
	}
	return start, end, nil
}
