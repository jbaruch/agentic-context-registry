package diagnostic

import (
	"errors"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
	"golang.org/x/text/encoding/unicode"
)

func TestAnchoredYAMLMappingEntryMark(t *testing.T) {
	const source = "mapping: &row # ? is in a comment\n  !!map\n  ? # explicit entry\n    # key follows\n\n    name\n  : value\n  : ignored\n"
	for _, fixture := range []struct {
		name, lineBreak string
		utf16           bool
	}{
		{"LF", "\n", false},
		{"CRLF UTF-16", "\r\n", true},
		{"CR", "\r", false},
		{"NEL", "\u0085", false},
		{"LS", "\u2028", false},
		{"PS", "\u2029", false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			input := []byte(strings.ReplaceAll(source, "\n", fixture.lineBreak))
			if fixture.utf16 {
				var err error
				input, err = unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder().Bytes(input)
				if err != nil {
					t.Fatal(err)
				}
			}
			var tree yaml.Node
			if err := yaml.Unmarshal(input, &tree); err != nil {
				t.Fatal(err)
			}
			err := CheckYAMLMappingKeys(input, &tree)
			var syntax *yaml.LoadError
			if !errors.As(err, &syntax) || syntax.ContextMark.Line != 3 || syntax.ContextMark.Column != 3 {
				t.Fatalf("mapping context = %+v, want line 3 column 3 in %v", syntax, err)
			}
		})
	}
}
