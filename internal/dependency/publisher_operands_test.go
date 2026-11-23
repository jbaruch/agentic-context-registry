package dependency

import (
	"bytes"
	"strings"
	"testing"
)

func TestReleaseOperandsInWarningsAndHoldGuidance(t *testing.T) {
	t.Parallel()
	const hostile = "bad\nFAKE-GUIDANCE\x1b[2J\u202e"
	tag := hostile + strings.Repeat("z", 1<<20)
	token := `"bad\nFAKE-GUIDANCE\x1b[2J\u202e` + strings.Repeat("z", 128-len(hostile)) + `...[truncated]"`

	var warning bytes.Buffer
	if err := newResolver(&fakeGitHub{}, &warning).warnIgnoredReleaseMetadata(tag, "download failed: "+hostile); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, got, want string }{
		{"warning", warning.String(), "warning: ignored release metadata for tag " + token + `: download failed: bad\u000aFAKE-GUIDANCE\u001b[2J\u202e` + "\n"},
		{"newer candidate", resumeSuggestion("github:owner/plugin", &Hold{Pin: "v1.0.0", Rejected: tag}, tag),
			"github:owner/plugin is held at v1.0.0 after " + token + " was rejected; " + token + " is newer than the barrier. Review it, then run 'acr resume github:owner/plugin' to resume latest."},
		{"rollback", (&DowngradeRequiredError{Source: "github:owner/plugin", CurrentTag: tag, RequestedRef: "v1.0.0"}).Error(),
			"github:owner/plugin@v1.0.0 is a rollback from the locked " + token + "; pass --hold to roll back temporarily until a newer stable release exists, or --pin to replace latest with a permanent pin"},
	} {
		if test.got != test.want {
			t.Fatalf("%s = %.500q, want %q", test.name, test.got, test.want)
		}
	}
}
