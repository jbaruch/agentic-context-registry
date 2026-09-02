package migrate

import (
	"reflect"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestRuleActivationFromSourceFrontmatter(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeCursorMDC(t, root, "example/alpha", "always-rule", ruleSource(t, root, "example/alpha", "always-rule"))
	writeCursorMDC(t, root, "example/alpha", "paths-rule", ruleSource(t, root, "example/alpha", "paths-rule"))

	rules := normalizeTestRules(t, root, "example/alpha")
	always := ruleByID(t, rules, "always-rule")
	if always.Activation.Mode != manifest.ActivationAlways || len(always.Activation.Paths) != 0 {
		t.Fatalf("always-rule activation = %+v", always.Activation)
	}
	paths := ruleByID(t, rules, "paths-rule")
	if paths.Activation.Mode != manifest.ActivationPaths || !reflect.DeepEqual(paths.Activation.Paths, []string{"*.go"}) {
		t.Fatalf("paths-rule activation = %+v", paths.Activation)
	}
	if !reflect.DeepEqual(paths.Lossy, []string{lossyDescription, lossyApplyToProse}) && !reflect.DeepEqual(paths.Lossy, []string{lossyApplyToProse, lossyDescription}) {
		t.Fatalf("paths-rule lossy = %v", paths.Lossy)
	}
}

func TestCursorWrapperContradictsSourceActivation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	source := ruleSource(t, root, "example/alpha", "paths-rule")
	writeCursorMDC(t, root, "example/alpha", "paths-rule", source)

	rule := ruleByID(t, normalizeTestRules(t, root, "example/alpha"), "paths-rule")
	if rule.Activation.Mode != manifest.ActivationPaths {
		t.Fatalf("wrapper alwaysApply:true must not override source activation, got %+v", rule.Activation)
	}
	if rule.Ambiguous {
		t.Fatalf("byte-identical wrapper remainder is not drift: %+v", rule)
	}
}

func TestNativeMdcDriftIsAmbiguous(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	source := ruleSource(t, root, "example/alpha", "always-rule")
	writeCursorMDCMutated(t, root, "example/alpha", "always-rule", source, func(content []byte) []byte {
		return append(content, []byte("hand-edited\n")...)
	})

	rule := ruleByID(t, normalizeTestRules(t, root, "example/alpha"), "always-rule")
	if !rule.Ambiguous || rule.Reason != reasonMdcDrift {
		t.Fatalf("drifted .mdc = %+v, want ambiguous %s", rule, reasonMdcDrift)
	}
}

func TestMissingRuleReferencedFromRulesMd(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
	seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
	writeRulesMD(t, root, []string{
		"example/alpha/rules/always-rule.md",
		"example/alpha/rules/paths-rule.md",
		"example/alpha/rules/gone.md",
	})

	rules := normalizeTestRules(t, root, "example/alpha")
	gone := ruleByID(t, rules, "gone")
	if !gone.Ambiguous || gone.Reason != reasonMissingRule {
		t.Fatalf("missing RULES.md target = %+v", gone)
	}
	if ruleByID(t, rules, "always-rule").Ambiguous {
		t.Fatalf("readable declared rule must stay migratable")
	}
}

func normalizeTestRules(t *testing.T, root, identity string) []NormalizedRule {
	t.Helper()
	install := installByIdentity(t, loadTestInstalls(t, root), identity)
	rules, err := NormalizeRules(openSnapshot(t, root), install)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func ruleByID(t *testing.T, rules []NormalizedRule, id string) NormalizedRule {
	t.Helper()
	for _, rule := range rules {
		if rule.ID == id {
			return rule
		}
	}
	t.Fatalf("missing rule %s in %#v", id, rules)
	return NormalizedRule{}
}
