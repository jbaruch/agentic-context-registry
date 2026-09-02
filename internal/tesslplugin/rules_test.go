package tesslplugin

import (
	"errors"
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
