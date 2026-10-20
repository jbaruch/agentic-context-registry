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
