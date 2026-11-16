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
				header := strings.ReplaceAll("---\nalwaysApply: false\n"+key+": >-\n  skills/example/** — when checking\ndescription: Keep independent checks\n---\n", "\n", newline)
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

func TestRewriteRuleReferencesRetainsUnsupportedMetadataRefusals(t *testing.T) {
	for _, body := range []string{
		"---\nalwaysApply: false\napplyTo: skills/example/**\n---\nBody",
		"---\nalwaysApply: false\napplyTo: 'skills/example/** — read skills/example/missing.md'\n---\nBody",
		"---\nalwaysApply: false\napplyTo: 'skills/example/** — when checking'\nexample: skills/example/missing.md\n---\nBody",
		"---\nalwaysApply: false\napplyTo: 'skills/example/** — when checking'\n---\nRead skills/example/missing.md",
	} {
		_, _, err := RewriteRuleReferences("rules/policy.md", []byte(body), func(data []byte) ([]byte, error) {
			return packageref.RewriteFiles(data, nil, []string{"skills/example/"})
		})
		if err == nil {
			t.Fatalf("unsupported reference hidden: %s", body)
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
		{"plain-inline", "---\nalwaysApply: false\napplyTo: skills/example/** — checking # Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\napplyTo: skills/example/** — checking # Read native/helper.sh\n---\n", []string{"skills/example/**"}},
		{"single-inline", "---\nalwaysApply: false\napplyTo: 'skills/example/** — checking' # Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\napplyTo: 'skills/example/** — checking' # Read native/helper.sh\n---\n", []string{"skills/example/**"}},
		{"double-inline", "---\nalwaysApply: false\napplyTo: \"skills/example/** — checking\" # Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\napplyTo: \"skills/example/** — checking\" # Read native/helper.sh\n---\n", []string{"skills/example/**"}},
		{"before-plain", "---\nalwaysApply: false\napplyTo: # Read skills/example/helper.sh\n  # Read skills/example/helper.sh\n  skills/example/** — checking\n---\n", "---\nalwaysApply: false\napplyTo: # Read native/helper.sh\n  # Read native/helper.sh\n  skills/example/** — checking\n---\n", []string{"skills/example/**"}},
		{"before-quoted", "---\nalwaysApply: false\napplyTo:\n  # Read skills/example/helper.sh\n  'skills/example/** — checking'\n---\n", "---\nalwaysApply: false\napplyTo:\n  # Read native/helper.sh\n  'skills/example/** — checking'\n---\n", []string{"skills/example/**"}},
		{"literal-header", "---\nalwaysApply: false\napplyTo: | # Read skills/example/helper.sh\n  skills/example/** — checking\n---\n", "---\nalwaysApply: false\napplyTo: | # Read native/helper.sh\n  skills/example/** — checking\n---\n", []string{"skills/example/**"}},
		{"folded-header", "---\nalwaysApply: false\napplyTo: >- # Read skills/example/helper.sh\n  skills/example/** — checking\n---\n", "---\nalwaysApply: false\napplyTo: >- # Read native/helper.sh\n  skills/example/** — checking\n---\n", []string{"skills/example/**"}},
		{"block-trailing", "---\nalwaysApply: false\napplyTo: |+\n\n  skills/example/** — checking\n\n# Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\napplyTo: |+\n\n  skills/example/** — checking\n\n# Read native/helper.sh\n---\n", []string{"skills/example/**"}},
		{"block-indent-strip", "---\nalwaysApply: false\napplyTo: |2- # Read skills/example/helper.sh\n    skills/example/** — checking\n  continued\n # Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\napplyTo: |2- # Read native/helper.sh\n    skills/example/** — checking\n  continued\n # Read native/helper.sh\n---\n", []string{"skills/example/**"}},
		{"block-keep-indent", "---\nalwaysApply: false\napplyTo: >+2 # Read skills/example/helper.sh\n  skills/example/** — checking\n\n# Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\napplyTo: >+2 # Read native/helper.sh\n  skills/example/** — checking\n\n# Read native/helper.sh\n---\n", []string{"skills/example/**"}},
		{"single-escape-hash", "---\nalwaysApply: false\napplyTo: 'skills/example/it''s#hash/** — checking' # Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\napplyTo: 'skills/example/it''s#hash/** — checking' # Read native/helper.sh\n---\n", []string{"skills/example/it's#hash/**"}},
		{"double-escape-hash", "---\nalwaysApply: false\napplyTo: \"skills/example/a\\\"#hash/** — checking\" # Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\napplyTo: \"skills/example/a\\\"#hash/** — checking\" # Read native/helper.sh\n---\n", []string{"skills/example/a\"#hash/**"}},
		{"plain-hash-continuation", "---\nalwaysApply: false\napplyTo: skills/example/a#hash/**,\n\n  skills/example/b/** — checking\n# Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\napplyTo: skills/example/a#hash/**,\n\n  skills/example/b/** — checking\n# Read native/helper.sh\n---\n", []string{"skills/example/a#hash/**", "skills/example/b/**"}},
		{"quoted-continuation", "---\nalwaysApply: false\napplyTo: \"skills/example/a/**,\n\n  skills/example/b/** — checking\" # Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\napplyTo: \"skills/example/a/**,\n\n  skills/example/b/** — checking\" # Read native/helper.sh\n---\n", []string{"skills/example/a/**", "skills/example/b/**"}},
		{"block-hash-value", "---\nalwaysApply: false\napplyTo: |- # Read skills/example/helper.sh\n  skills/example/a#hash/**,\n  #literal/** — checking\n---\n", "---\nalwaysApply: false\napplyTo: |- # Read native/helper.sh\n  skills/example/a#hash/**,\n  #literal/** — checking\n---\n", []string{"skills/example/a#hash/**", "#literal/**"}},
		{"null-fallback", "---\nalwaysApply: false\napplyTo: null # Read skills/example/helper.sh\nglobs: skills/example/** — checking\n---\n", "---\nalwaysApply: false\napplyTo: null # Read native/helper.sh\nglobs: skills/example/** — checking\n---\n", []string{"skills/example/**"}},
		{"empty-fallback", "---\nalwaysApply: false\napplyTo: '' # Read skills/example/helper.sh\nglobs: # Read skills/example/helper.sh\npaths: skills/example/** — checking\n---\n", "---\nalwaysApply: false\napplyTo: '' # Read native/helper.sh\nglobs: # Read native/helper.sh\npaths: skills/example/** — checking\n---\n", []string{"skills/example/**"}},
		{"empty-alias-fallback", "---\nalwaysApply: false\nempty: &empty ''\napplyTo: *empty # Read skills/example/helper.sh\nglobs: skills/example/** — checking\n---\n", "---\nalwaysApply: false\nempty: &empty ''\napplyTo: *empty # Read native/helper.sh\nglobs: skills/example/** — checking\n---\n", []string{"skills/example/**"}},
		{"precedence-applyTo", "---\nalwaysApply: false\npaths: skills/example/helper.sh\nglobs: skills/example/helper.sh\napplyTo: skills/example/** — checking\n# Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\npaths: native/helper.sh\nglobs: native/helper.sh\napplyTo: skills/example/** — checking\n# Read native/helper.sh\n---\n", []string{"skills/example/**"}},
		{"precedence-globs", "---\nalwaysApply: false\npaths: skills/example/helper.sh\nglobs: skills/example/** — checking\n# Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\npaths: native/helper.sh\nglobs: skills/example/** — checking\n# Read native/helper.sh\n---\n", []string{"skills/example/**"}},
		{"properties", "---\nalwaysApply: false\napplyTo: !!str &scope # Read skills/example/helper.sh\n  # Read skills/example/helper.sh\n  'skills/example/** — checking'\n---\n", "---\nalwaysApply: false\napplyTo: !!str &scope # Read native/helper.sh\n  # Read native/helper.sh\n  'skills/example/** — checking'\n---\n", []string{"skills/example/**"}},
		{"unicode-key", "---\nalwaysApply: false\n? \"apply\\u0054o\"\n: 'skills/example/** — checking' # Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\n? \"apply\\u0054o\"\n: 'skills/example/** — checking' # Read native/helper.sh\n---\n", []string{"skills/example/**"}},
		{"flow-mapping", "---\n{alwaysApply: false, applyTo: 'src/** — checking'} # Read skills/example/helper.sh\n---\nBody", "---\n{alwaysApply: false, applyTo: 'src/** — checking'} # Read native/helper.sh\n---\nBody", []string{"src/**"}},
		{"quoted-hash-reference", "---\nalwaysApply: false\napplyTo: 'skills/example/**, # skills/example/keep.sh — checking' # Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\napplyTo: 'skills/example/**, # skills/example/keep.sh — checking' # Read native/helper.sh\n---\n", []string{"skills/example/**", "# skills/example/keep.sh"}},
		{"block-hash-reference", "---\nalwaysApply: false\napplyTo: |- # Read skills/example/helper.sh\n  skills/example/**,\n  # skills/example/keep.sh — checking\n---\n", "---\nalwaysApply: false\napplyTo: |- # Read native/helper.sh\n  skills/example/**,\n  # skills/example/keep.sh — checking\n---\n", []string{"skills/example/**", "# skills/example/keep.sh"}},
		{"explicit-block-key", "---\nalwaysApply: false\n? applyTo\n: |2- # Read skills/example/helper.sh\n  skills/example/** — checking\n# Read skills/example/helper.sh\n---\n", "---\nalwaysApply: false\n? applyTo\n: |2- # Read native/helper.sh\n  skills/example/** — checking\n# Read native/helper.sh\n---\n", []string{"skills/example/**"}},
		{"no-final-newline", "---\nalwaysApply: false\npaths: 'skills/example/** — checking' # Read skills/example/helper.sh\n---", "---\nalwaysApply: false\npaths: 'skills/example/** — checking' # Read native/helper.sh\n---", []string{"skills/example/**"}},
		{"crlf", "---\r\nalwaysApply: false\r\napplyTo: >- # Read skills/example/helper.sh\r\n\r\n  skills/example/** — checking\r\n# Read skills/example/helper.sh\r\n---\r\nBody", "---\r\nalwaysApply: false\r\napplyTo: >- # Read native/helper.sh\r\n\r\n  skills/example/** — checking\r\n# Read native/helper.sh\r\n---\r\nBody", []string{"skills/example/**"}},
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
					if err == nil || got != nil || string(input) != original {
						t.Fatalf("location=%d ref=%q output=%q err=%v mutated=%v", location, ref, got, err, string(input) != original)
					}
				}
			}
		})
	}
}

func TestRewriteRuleReferencesRetainsYAMLRefusals(t *testing.T) {
	cases := []struct{ name, input, code string }{
		{"duplicate", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\napplyTo: 'other/** — checking' # Read skills/example/helper.sh\n---\n", string(manifest.CodeInvalidRuleActivation)},
		{"flow-sequence", "---\nalwaysApply: false\napplyTo: [\n  'src/** — checking',\n  # Read skills/example/helper.sh\n  'other/** — checking'\n]\n---\n", string(manifest.CodeInvalidRuleActivation)},
		{"block-sequence", "---\nalwaysApply: false\napplyTo:\n  - 'src/** — checking'\n  # Read skills/example/helper.sh\n  - 'other/** — checking'\n---\n", string(manifest.CodeInvalidRuleActivation)},
		{"alias", "---\nalwaysApply: false\nvalue: &scope 'skills/example/** — checking'\napplyTo: *scope # Read skills/example/helper.sh\n---\n", ""},
		{"merge", "---\nalwaysApply: false\nbase: &base\n  applyTo: 'skills/example/** — checking'\n<<: *base # Read skills/example/helper.sh\n---\n", ""},
		{"flow-owned-glob", "---\n{alwaysApply: false, applyTo: 'skills/example/** — checking'} # Read skills/example/helper.sh\n---\n", ""},
		{"unselected-glob", "---\nalwaysApply: false\napplyTo: 'src/** — checking'\nglobs: 'skills/example/** — checking' # Read skills/example/helper.sh\n---\n", ""},
		{"lossy-prose", "---\nalwaysApply: false\napplyTo: 'src/** — read skills/example/helper.sh'\n---\n", string(manifest.CodeInvalidRuleActivation)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(tc.input)
			got, _, err := RewriteRuleReferences("rules/policy.md", input, func(data []byte) ([]byte, error) {
				return packageref.RewriteFiles(data, map[string]string{"skills/example/helper.sh": "native/helper.sh", "skills/example/keep.sh": "native/keep.sh"}, []string{"skills/example/"})
			})
			if err == nil || got != nil || string(input) != tc.input {
				t.Fatalf("output=%q err=%v mutated=%v", got, err, string(input) != tc.input)
			}
			if tc.code != "" {
				var conv *Error
				if !errors.As(err, &conv) || conv.Code != tc.code {
					t.Fatalf("err=%v, want code=%s", err, tc.code)
				}
			}
		})
	}
}
