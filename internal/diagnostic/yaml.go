package diagnostic

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v4"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// OpaqueYAMLError retains the decoder cause and the legacy multiline display
// for YAML field errors. YAML v4 exposes the legacy entries through TypeError
// but no longer renders them on separate lines.
func OpaqueYAMLError(err error) error {
	var fields *yaml.TypeError
	if errors.As(err, &fields) {
		return boundOpaqueError(err, "yaml: unmarshal errors:\n  "+strings.Join(fields.Errors, "\n  "))
	}
	return OpaqueError(err)
}

// CheckYAMLMappingKeys restores the strict readers' refusal of omitted keys.
// YAML v4 creates an empty null scalar for them, which struct decoding silently
// skips even with KnownFields. Written keys and empty values are unaffected.
func CheckYAMLMappingKeys(source []byte, node *yaml.Node) error {
	for i, child := range node.Content {
		if node.Kind == yaml.MappingNode && i%2 == 0 &&
			child.Kind == yaml.ScalarNode && child.Tag == "!!null" && child.Value == "" &&
			child.Style == 0 && child.Anchor == "" {
			err := &yaml.LoadError{
				Stage:   yaml.ParserStage,
				Message: "did not find expected node content",
				Mark:    yaml.Mark{Line: child.Line, Column: child.Column},
			}
			if node.Style&yaml.FlowStyle == 0 {
				err.Message = "did not find expected key"
				err.ContextMark = yaml.Mark{Line: node.Line, Column: node.Column}
				if node.Anchor != "" || node.Style&yaml.TaggedStyle != 0 {
					err.ContextMark = firstYAMLMappingEntry(source, node)
				}
			}
			return err
		}
		// Only inspect written nodes. Following aliases would repeat the walk
		// and could traverse a cycle before the strict decoder rejects it.
		if err := CheckYAMLMappingKeys(source, child); err != nil {
			return err
		}
	}
	return nil
}

// YAML v4 marks a mapping with properties at its first property, whereas v3
// reported the first entry. Only an explicit '?' can start that entry before
// the first key node. Look for it between the two marks in the parsed source.
func firstYAMLMappingEntry(source []byte, mapping *yaml.Node) yaml.Mark {
	key := mapping.Content[0]
	mark := yaml.Mark{Line: key.Line, Column: key.Column}
	// Match the YAML reader's BOM-based decoding and line breaks.
	text, _, err := transform.String(unicode.BOMOverride(transform.Nop), string(source))
	if err != nil {
		return mark
	}
	lines := strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n")
	number := 0
	for line := range strings.SplitSeq(lines.Replace(text), "\n") {
		number++
		if number <= mapping.Line {
			continue
		}
		if number > key.Line {
			break
		}
		entry := strings.TrimLeft(strings.TrimPrefix(line, "\uFEFF"), " \t")
		rest, found := strings.CutPrefix(entry, "?")
		if found && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
			return yaml.Mark{Line: number, Column: utf8.RuneCountInString(line[:len(line)-len(entry)]) + 1}
		}
	}
	return mark
}

// LegacyYAMLSyntaxError displays the v3 parser and scanner line convention
// while retaining the v4 error and its original marks.
func LegacyYAMLSyntaxError(err error) error {
	syntax, ok := err.(*yaml.LoadError)
	if !ok || (syntax.Stage != yaml.ParserStage && syntax.Stage != yaml.ScannerStage) {
		return OpaqueYAMLError(err)
	}

	// v3 preferred a context mark beyond the first line and omitted first-line
	// marks. Parser lines were zero-based, but scanner lines were incremented.
	line := syntax.ContextMark.Line
	if line <= 1 {
		line = syntax.Mark.Line
	}
	detail := "yaml: "
	if line > 1 {
		if syntax.Stage == yaml.ParserStage {
			line--
		}
		detail += fmt.Sprintf("line %d: ", line)
	}
	return boundOpaqueError(err, detail+syntax.Message)
}
