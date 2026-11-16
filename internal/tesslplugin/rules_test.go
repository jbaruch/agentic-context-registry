package tesslplugin

import (
	"errors"
	"fmt"
	"github.com/jbaruch/agentic-context-registry/internal/packageref"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestActivationFromFrontmatterNotManifest(t *testing.T) {
	t.Parallel()

	always, err := activationFromRuleFile("rules/always.md", []byte("---\nalwaysApply: true\n---\n# Always\n"))
	if err != nil {
		t.Fatal(err)
	}
	if always.Activation.Mode != manifest.ActivationAlways || len(always.Activation.Paths) != 0 {
		t.Fatalf("always = %#v", always.Activation)
	}

	paths, err := activationFromRuleFile("rules/paths.md", []byte("---\nalwaysApply: false\napplyTo: \"skills/**/*.md — when authoring skills\"\n---\n# Paths\n"))
	if err != nil {
		t.Fatal(err)
	}
	if paths.Activation.Mode != manifest.ActivationPaths || len(paths.Activation.Paths) != 1 || paths.Activation.Paths[0] != "skills/**/*.md" {
		t.Fatalf("paths = %#v", paths.Activation)
	}
	if len(paths.Lossy) != 1 || paths.Lossy[0].Reason != "applyTo-prose" || paths.Lossy[0].Value != "when authoring skills" {
		t.Fatalf("lossy = %#v", paths.Lossy)
	}
}

func TestApplyToWithoutEmDashBlocks(t *testing.T) {
	t.Parallel()

	_, err := activationFromRuleFile("rules/paths.md", []byte("---\nalwaysApply: false\napplyTo: \"**/*.go\"\n---\n# Paths\n"))
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != string(manifest.CodeInvalidRuleActivation) {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingFrontmatterBlocks(t *testing.T) {
	t.Parallel()

	_, err := activationFromRuleFile("rules/always.md", []byte("# Always\n"))
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != string(manifest.CodeInvalidRuleActivation) {
		t.Fatalf("err = %v", err)
	}
}

func TestFalseWithoutGlobsBlocks(t *testing.T) {
	t.Parallel()

	_, err := activationFromRuleFile("rules/paths.md", []byte("---\nalwaysApply: false\n---\n# Paths\n"))
	var conv *Error
	if !errors.As(err, &conv) || conv.Code != string(manifest.CodeInvalidRuleActivation) {
		t.Fatalf("err = %v", err)
	}
}

func TestAlwaysApplyWithApplyToReportsBothHalves(t *testing.T) {
	t.Parallel()

	result, err := activationFromRuleFile("rules/always.md", []byte("---\nalwaysApply: true\napplyTo: \"skills/**/*.md — when authoring skills\"\n---\n# Always\n"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Activation.Mode != manifest.ActivationAlways || len(result.Activation.Paths) != 0 {
		t.Fatalf("always = %#v", result.Activation)
	}
	reasons := map[string]string{}
	for _, item := range result.Lossy {
		reasons[item.Reason] = item.Value
	}
	if reasons["applyTo-globs"] != "skills/**/*.md" || reasons["applyTo-prose"] != "when authoring skills" {
		t.Fatalf("lossy = %#v", result.Lossy)
	}
}

func TestAlwaysApplyPureProseApplyToReportsProse(t *testing.T) {
	t.Parallel()

	result, err := activationFromRuleFile("rules/always.md", []byte("---\nalwaysApply: true\napplyTo: \"when authoring skills\"\n---\n# Always\n"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Activation.Mode != manifest.ActivationAlways || len(result.Activation.Paths) != 0 {
		t.Fatalf("always = %#v", result.Activation)
	}
	if len(result.Lossy) != 1 || result.Lossy[0].Reason != "applyTo-prose" || result.Lossy[0].Value != "when authoring skills" {
		t.Fatalf("lossy = %#v", result.Lossy)
	}
}

func TestDescriptionIsLossy(t *testing.T) {
	t.Parallel()

	result, err := activationFromRuleFile("rules/always.md", []byte("---\nalwaysApply: true\ndescription: always on\n---\n# Always\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Lossy) != 1 || result.Lossy[0].Reason != "description" || result.Lossy[0].Value != "always on" {
		t.Fatalf("lossy = %#v", result.Lossy)
	}
}

func TestDuplicateGlobsDroppedInFrontmatterOrder(t *testing.T) {
	t.Parallel()

	result, err := activationFromRuleFile("rules/paths.md", []byte("---\nalwaysApply: false\napplyTo: \"a.md, b.md, a.md — twice\"\n---\n# Paths\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Activation.Paths) != 2 || result.Activation.Paths[0] != "a.md" || result.Activation.Paths[1] != "b.md" {
		t.Fatalf("paths = %#v", result.Activation.Paths)
	}
}

func TestRewriteRuleReferencesSeparatesActivationFromContent(t *testing.T) {
	for _, key := range []string{"applyTo", "globs", "paths"} {
		for _, newline := range []string{"\n", "\r\n"} {
			t.Run(key+fmt.Sprintf("/newline%d", len(newline)), func(t *testing.T) {
				header := strings.ReplaceAll("---\nalwaysApply: false\n"+key+": 'skills/example/** — when checking'\ndescription: Keep independent checks\n---\n", "\n", newline)
				body := "Read [schema](skills/example/state-schema.md)."
				rewrite := func(data []byte) ([]byte, error) {
					return packageref.RewriteFiles(data, map[string]string{"skills/example/state-schema.md": "native/state-schema.md"}, []string{"skills/example/"})
				}
				got, activation, err := RewriteRuleReferences("rules/policy.md", []byte(header+body), rewrite)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != header+"Read [schema](native/state-schema.md)." {
					t.Fatalf("changed activation or lost body: %q", got)
				}
				if !reflect.DeepEqual(activation.Paths, []string{"skills/example/**"}) {
					t.Fatalf("activation=%+v", activation)
				}
			})
		}
	}
}

func TestRewriteRuleReferencesRefusesInvalidActivationAndLossyReferences(t *testing.T) {
	cases := []struct{ input, diagnostic string }{
		{"---\nalwaysApply: false\napplyTo: skills/example/**\n---\nBody", "rule rules/policy.md applyTo/globs/paths must split glob and prose on an em dash; activation cannot be guessed"},
		{"---\nalwaysApply: false\napplyTo: 'skills/example/** — read skills/example/missing.md'\n---\nBody", "owned reference \"skills/example/missing.md\" is not a complete declared file path; missing destinations, dynamic suffixes or unsupported punctuation require a semantic conversion"},
	}
	for _, tc := range cases {
		input := []byte(tc.input)
		got, _, err := RewriteRuleReferences("rules/policy.md", input, func(data []byte) ([]byte, error) {
			return packageref.RewriteFiles(data, nil, []string{"skills/example/"})
		})
		if got != nil || err == nil || err.Error() != tc.diagnostic || string(input) != tc.input {
			t.Fatalf("output=%q err=%v mutated=%v", got, err, string(input) != tc.input)
		}
	}
}

func TestRewriteRuleReferencesScalarCommentBoundaries(t *testing.T) {
	cases := []struct {
		name, input, want string
		paths             []string
	}{
		{"adjacent-key", "---\nalwaysApply: false\napplyTo: skills/example/** — checking\n# Read skills/example/helper.sh\ndescription: Check\n---\nBody\n", "---\nalwaysApply: false\napplyTo: skills/example/** — checking\n# Read native/helper.sh\ndescription: Check\n---\nBody\n", []string{"skills/example/**"}},
		{"adjacent-delimiter", "---\nalwaysApply: false\napplyTo: skills/example/** — checking\n\n# Read skills/example/helper.sh\n---\nBody", "---\nalwaysApply: false\napplyTo: skills/example/** — checking\n\n# Read native/helper.sh\n---\nBody", []string{"skills/example/**"}},
		{"plain-inline", "---\nalwaysApply: false\napplyTo: skills/example/** — checking # Read skills/example/helper.sh\n---\nRead skills/example/helper.sh", "---\nalwaysApply: false\napplyTo: skills/example/** — checking # Read native/helper.sh\n---\nRead native/helper.sh", []string{"skills/example/**"}},
		{"single-inline", "---\nalwaysApply: false\napplyTo: 'skills/example/** — checking' # Read skills/example/helper.sh\n---\nRead skills/example/helper.sh", "---\nalwaysApply: false\napplyTo: 'skills/example/** — checking' # Read native/helper.sh\n---\nRead native/helper.sh", []string{"skills/example/**"}},
		{"double-inline", "---\nalwaysApply: false\napplyTo: \"skills/example/** — checking\" # Read skills/example/helper.sh\n---\nRead skills/example/helper.sh", "---\nalwaysApply: false\napplyTo: \"skills/example/** — checking\" # Read native/helper.sh\n---\nRead native/helper.sh", []string{"skills/example/**"}},
		{"single-escape-hash", "---\nalwaysApply: false\napplyTo: 'skills/example/it''s#hash/** — checking' # Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\napplyTo: 'skills/example/it''s#hash/** — checking' # Read native/helper.sh\n---\n", []string{"skills/example/it's#hash/**"}},
		{"double-escape-hash", "---\nalwaysApply: false\napplyTo: \"skills/example/a\\\"#hash/** — checking\" # Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\napplyTo: \"skills/example/a\\\"#hash/** — checking\" # Read native/helper.sh\n---\n", []string{"skills/example/a\"#hash/**"}},
		{"null-fallback", "---\nalwaysApply: false\napplyTo: null # Read skills/example/helper.sh\nglobs: skills/example/** — checking\n---\n", "---\nalwaysApply: false\napplyTo: null # Read native/helper.sh\nglobs: skills/example/** — checking\n---\n", []string{"skills/example/**"}},
		{"empty-fallback", "---\nalwaysApply: false\napplyTo: '' # Read skills/example/helper.sh\nglobs: # Read skills/example/helper.sh\npaths: skills/example/** — checking\n---\n", "---\nalwaysApply: false\napplyTo: '' # Read native/helper.sh\nglobs: # Read native/helper.sh\npaths: skills/example/** — checking\n---\n", []string{"skills/example/**"}},
		{"precedence-applyTo", "---\nalwaysApply: false\npaths: skills/example/helper.sh\nglobs: skills/example/helper.sh\napplyTo: skills/example/** — checking\n# Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\npaths: native/helper.sh\nglobs: native/helper.sh\napplyTo: skills/example/** — checking\n# Read native/helper.sh\n---\n", []string{"skills/example/**"}},
		{"precedence-globs", "---\nalwaysApply: false\npaths: skills/example/helper.sh\nglobs: skills/example/** — checking\n# Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\npaths: native/helper.sh\nglobs: skills/example/** — checking\n# Read native/helper.sh\n---\n", []string{"skills/example/**"}},
		{"quoted-hash-reference", "---\nalwaysApply: false\napplyTo: 'skills/example/**, # skills/example/keep.sh — checking' # Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\napplyTo: 'skills/example/**, # skills/example/keep.sh — checking' # Read native/helper.sh\n---\n", []string{"skills/example/**", "# skills/example/keep.sh"}},
		{"no-final-newline", "---\nalwaysApply: false\npaths: 'skills/example/** — checking' # Read skills/example/helper.sh\n---", "---\nalwaysApply: false\npaths: 'skills/example/** — checking' # Read native/helper.sh\n---", []string{"skills/example/**"}},
		{"crlf", "---\r\nalwaysApply: false\r\napplyTo: 'skills/example/** — checking' # Read skills/example/helper.sh\r\n---\r\nBody", "---\r\nalwaysApply: false\r\napplyTo: 'skills/example/** — checking' # Read native/helper.sh\r\n---\r\nBody", []string{"skills/example/**"}},
		{"indented", "---\n  alwaysApply: false\n  applyTo: 'skills/example/** — checking' # ! & * Read skills/example/helper.sh\n  note: '! & * literal'\n---\nRead skills/example/helper.sh", "---\n  alwaysApply: false\n  applyTo: 'skills/example/** — checking' # ! & * Read native/helper.sh\n  note: '! & * literal'\n---\nRead native/helper.sh", []string{"skills/example/**"}},
		{"body-unicode", "---\nalwaysApply: false\napplyTo: skills/example/** — checking\n---\n\u0085\u2028\u2029 Read skills/example/helper.sh", "---\nalwaysApply: false\napplyTo: skills/example/** — checking\n---\n\u0085\u2028\u2029 Read native/helper.sh", []string{"skills/example/**"}},
		{"plain-indicators", "---\nalwaysApply: false\napplyTo: skills/example/** — checking\nquestion: ?literal\nhyphen: -literal\ncolon: :literal\n---\nRead skills/example/helper.sh", "---\nalwaysApply: false\napplyTo: skills/example/** — checking\nquestion: ?literal\nhyphen: -literal\ncolon: :literal\n---\nRead native/helper.sh", []string{"skills/example/**"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(tc.input)
			rewrite := func(data []byte) ([]byte, error) {
				return packageref.RewriteFiles(data, map[string]string{"skills/example/helper.sh": "native/helper.sh", "skills/example/keep.sh": "native/keep.sh"}, []string{"skills/example/"})
			}
			got, activation, err := RewriteRuleReferences("rules/policy.md", input, rewrite)
			if err != nil || string(got) != tc.want {
				t.Fatalf("output=%q, want=%q, err=%v", got, tc.want, err)
			}
			if activation.Mode != manifest.ActivationPaths || !reflect.DeepEqual(activation.Paths, tc.paths) {
				t.Fatalf("activation=%+v, want paths=%q", activation, tc.paths)
			}
			if string(input) != tc.input {
				t.Fatal("success mutated input")
			}
			// Every exposed location must also reject missing and dynamic paths.
			for _, ref := range []string{"skills/example/missing.sh", "skills/example/${HELPER}"} {
				for location := 0; location < strings.Count(tc.input, "skills/example/helper.sh"); location++ {
					pos := 0
					for n := 0; n <= location; n++ {
						pos += strings.Index(tc.input[pos:], "skills/example/helper.sh")
						if n < location {
							pos += len("skills/example/helper.sh")
						}
					}
					original := tc.input[:pos] + ref + tc.input[pos+len("skills/example/helper.sh"):]
					input := []byte(original)
					got, _, err := RewriteRuleReferences("rules/policy.md", input, rewrite)
					wantError := fmt.Sprintf("owned reference %q is not a complete declared file path; missing destinations, dynamic suffixes or unsupported punctuation require a semantic conversion", ref)
					if err == nil || err.Error() != wantError || got != nil || string(input) != original {
						t.Fatalf("location=%d ref=%q output=%q err=%v mutated=%v", location, ref, got, err, string(input) != original)
					}
				}
			}
		})
	}
}

func TestRewriteRuleReferencesRefusesUnsupportedSyntaxBeforeCallbacks(t *testing.T) {
	cases := []struct{ name, input, code, path string }{
		{"before-plain", "---\nalwaysApply: false\napplyTo: # Read skills/example/helper.sh\n  # Read skills/example/helper.sh\n  skills/example/** — checking\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"before-quoted", "---\nalwaysApply: false\napplyTo:\n  # Read skills/example/helper.sh\n  'skills/example/** — checking'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"literal-header", "---\nalwaysApply: false\napplyTo: | # Read skills/example/helper.sh\n  skills/example/** — checking\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"folded-header", "---\nalwaysApply: false\napplyTo: >- # Read skills/example/helper.sh\n  skills/example/** — checking\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"block-trailing", "---\nalwaysApply: false\napplyTo: |+\n\n  skills/example/** — checking\n\n# Read skills/example/helper.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"block-indent-strip", "---\nalwaysApply: false\napplyTo: |2- # Read skills/example/helper.sh\n    skills/example/** — checking\n  continued\n # Read skills/example/helper.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"block-keep-indent", "---\nalwaysApply: false\napplyTo: >+2 # Read skills/example/helper.sh\n  skills/example/** — checking\n\n# Read skills/example/helper.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"plain-hash-continuation", "---\nalwaysApply: false\napplyTo: skills/example/a#hash/**,\n\n  skills/example/b/** — checking\n# Read skills/example/helper.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"quoted-continuation", "---\nalwaysApply: false\napplyTo: \"skills/example/a/**,\n\n  skills/example/b/** — checking\" # Read skills/example/helper.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"block-hash-value", "---\nalwaysApply: false\napplyTo: |- # Read skills/example/helper.sh\n  skills/example/a#hash/**,\n  #literal/** — checking\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"empty-alias-fallback", "---\nalwaysApply: false\nempty: &empty ''\napplyTo: *empty # Read skills/example/helper.sh\nglobs: skills/example/** — checking\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"properties", "---\nalwaysApply: false\napplyTo: !!str &scope # Read skills/example/helper.sh\n  # Read skills/example/helper.sh\n  'skills/example/** — checking'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"unicode-key", "---\nalwaysApply: false\n? \"apply\\u0054o\"\n: 'skills/example/** — checking' # Read skills/example/helper.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"flow-mapping", "---\n{alwaysApply: false, applyTo: 'src/** — checking'} # Read skills/example/helper.sh\n---\nBody", "invalid_rule_activation", "rules/policy.md"},
		{"block-hash-reference", "---\nalwaysApply: false\napplyTo: |- # Read skills/example/helper.sh\n  skills/example/**,\n  # skills/example/keep.sh — checking\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"explicit-block-key", "---\nalwaysApply: false\n? applyTo\n: |2- # Read skills/example/helper.sh\n  skills/example/** — checking\n# Read skills/example/helper.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"root-anchor", "---\n&root\n  alwaysApply: false\n  applyTo: 'src/** — checking'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"root-nonspecific-tag", "---\n!\nalwaysApply: false\napplyTo: 'src/** — checking'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"root-map-tag", "---\n!!map\nalwaysApply: false\napplyTo: 'src/** — checking'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"root-verbatim-tag", "---\n!<tag:yaml.org,2002:map>\nalwaysApply: false\napplyTo: 'src/** — checking'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"scalar-anchor", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: &note hello\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"nonspecific-tag", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: ! hello\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"str-tag", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: !!str hello\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"verbatim-tag", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: !<tag:yaml.org,2002:str> hello\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"alias", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: *scope\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"merge", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\n<<: *base\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"metadata-flow-map", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: {a: b}\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"metadata-flow-sequence", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: [a, b]\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"root-sequence", "---\n- alwaysApply: true\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"nested-map", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote:\n  a: b\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"nested-sequence", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote:\n  - a\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"selected-map", "---\nalwaysApply: false\napplyTo: {a: b}\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"selected-sequence", "---\nalwaysApply: false\napplyTo: [a, b]\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"multiline-plain", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: first\n  second\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"multiline-single", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: 'first\n  second'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"multiline-double", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: \"first\n  second\"\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"quoted-key", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\n\"note\": hello\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"single-quoted-key", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\n'note': hello\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"escaped-key", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\n\"no\\u0074e\": hello\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"explicit-key", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\n? note\n: hello\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"inconsistent-indent", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\n  note: hello\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"duplicate", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\napplyTo: 'other/** — checking'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"quoted-tail", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: 'hello' extra\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"nested-inline", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: a: b\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"tab-indent", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\n\tnote: hello\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"metadata-block-|", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: |\n  value\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"metadata-block->", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: >\n  value\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"metadata-block-|2-", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: |2-\n  value\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"metadata-block->+2", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: >+2\n  value\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"qa-root-&root-plain", "---\n&root\n  alwaysApply: false\n  applyTo: skills/check/** — checking\n  # Read skills/check/check.sh\n  note: skills/check/missing.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"qa-root-&root-block", "---\n&root\n  alwaysApply: false\n  applyTo: |2-\n    skills/check/** — checking\n  # Read skills/check/check.sh\n  note: skills/check/missing.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"qa-root-!!map-plain", "---\n!!map\n  alwaysApply: false\n  applyTo: skills/check/** — checking\n  # Read skills/check/check.sh\n  note: skills/check/missing.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"qa-root-!!map-block", "---\n!!map\n  alwaysApply: false\n  applyTo: |2-\n    skills/check/** — checking\n  # Read skills/check/check.sh\n  note: skills/check/missing.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"qa-root-!-plain", "---\n!\n  alwaysApply: false\n  applyTo: skills/check/** — checking\n  # Read skills/check/check.sh\n  note: skills/check/missing.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"qa-root-!-block", "---\n!\n  alwaysApply: false\n  applyTo: |2-\n    skills/check/** — checking\n  # Read skills/check/check.sh\n  note: skills/check/missing.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"cr-before", "---\nalwaysApply: false\rapplyTo: 'src/** — checking'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"cr-after", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: 'a\rb'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"nel-before", "---\nalwaysApply: false\u0085applyTo: 'src/** — checking'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"nel-after", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: 'a\u0085b'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"ls-before", "---\nalwaysApply: false\u2028applyTo: 'src/** — checking'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"ls-after", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: 'a\u2028b'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"ps-before", "---\nalwaysApply: false\u2029applyTo: 'src/** — checking'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"ps-after", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: 'a\u2029b'\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"code-review-escape", "---\nalwaysApply: false\rapplyTo: 'src/** — checking'\n# Read skills/example/missing.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"code-review-panic", "---\nalwaysApply: false\ndescription: 'a\u0085b'\napplyTo: 'src/** — checking' # Read skills/example/helper.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"qa-exact-&root", "---\n&root\n  alwaysApply: false\n  applyTo: |2-\n    skills/check/** — checking\n  # Read skills/check/check.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"qa-exact-!!map", "---\n!!map\n  alwaysApply: false\n  applyTo: |2-\n    skills/check/** — checking\n  # Read skills/check/check.sh\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"scalar-map-tag", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nnote: !!map {a: b}\n---\n", "invalid_rule_activation", "rules/policy.md"},
		{"root-flow-sequence", "---\n[alwaysApply, true]\n---\n", "invalid_rule_activation", "rules/policy.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(tc.input)
			called := false
			got, _, err := RewriteRuleReferences(tc.path, input, func(data []byte) ([]byte, error) {
				called = true
				return data, nil
			})
			var conv *Error
			if !errors.As(err, &conv) || conv.Code != tc.code || conv.Field != tc.path || !strings.Contains(conv.Message, tc.path) || !strings.Contains(conv.Message, "convert again") || got != nil || string(input) != tc.input || called {
				t.Fatalf("output=%q err=%v mutated=%v callback=%v", got, err, string(input) != tc.input, called)
			}
		})
	}
}

func TestRewriteRuleReferencesRefusesLossyProseChanges(t *testing.T) {
	input := []byte("---\nalwaysApply: false\napplyTo: 'src/** — read skills/example/helper.sh'\n---\n")
	got, _, err := RewriteRuleReferences("rules/policy.md", input, func(data []byte) ([]byte, error) {
		return packageref.RewriteFiles(data, map[string]string{"skills/example/helper.sh": "native/helper.sh"}, []string{"skills/example/"})
	})
	var conv *Error
	if got != nil || !errors.As(err, &conv) || conv.Code != "invalid_rule_activation" || conv.Field != "rules/policy.md" || conv.Message != "activation prose requires explicit content adaptation" {
		t.Fatalf("output=%q err=%v", got, err)
	}
}

func TestRewriteRuleReferencesAlwaysRewritesWholeContentAfterAdmission(t *testing.T) {
	original := "---\nalwaysApply: true # Read skills/example/helper.sh\nnote: '! & * literal'\nexample: skills/example/helper.sh\n---\nRead skills/example/helper.sh"
	input := []byte(original)
	got, activation, err := RewriteRuleReferences("rules/policy.md", input, func(data []byte) ([]byte, error) {
		return packageref.RewriteFiles(data, map[string]string{"skills/example/helper.sh": "native/helper.sh"}, []string{"skills/example/"})
	})
	want := "---\nalwaysApply: true # Read native/helper.sh\nnote: '! & * literal'\nexample: native/helper.sh\n---\nRead native/helper.sh"
	if err != nil || string(got) != want || activation.Mode != manifest.ActivationAlways || string(input) != original {
		t.Fatalf("output=%q activation=%+v err=%v mutated=%v", got, activation, err, string(input) != original)
	}
}
