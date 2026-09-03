package realize

import (
	"errors"
	"testing"
)

func TestValidateTesslOwnedTargetsRejectsHandBuiltPlan(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		path    string
		blocked bool
	}{
		"plugin tree":   {path: ".tessl/plugins/example/alpha/rules/always.md", blocked: true},
		"plugin prefix": {path: ".tessl", blocked: true},
		"native prefix": {path: ".cursor/rules/tessl__rule__example.mdc", blocked: true},
		"shared host":   {path: "AGENTS.md"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan := Plan{Operations: []Operation{{Kind: OperationUpdate, Path: test.path}}}
			err := ValidateTesslOwnedTargets(plan)
			var targetErr *TesslOwnedTargetError
			if test.blocked {
				if !errors.As(err, &targetErr) || targetErr.Path != test.path {
					t.Fatalf("ValidateTesslOwnedTargets() error = %#v, want TesslOwnedTargetError for %q", err, test.path)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateTesslOwnedTargets() error = %v, want nil", err)
			}
		})
	}
}
