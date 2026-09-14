package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequireChangelogVersionAcceptsADocumentedVersion(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name      string
		changelog string
	}{
		{name: "heading with a date", changelog: "# Changelog\n\n## 0.2.0 — 2026-09-14\n\n### Fixed\n\n- Something.\n"},
		{name: "bare heading", changelog: "# Changelog\n\n## 0.2.0\n\n### Fixed\n\n- Something.\n"},
		{name: "trailing whitespace", changelog: "# Changelog\n\n## 0.2.0 — 2026-09-14   \n"},
		{name: "not the newest heading", changelog: "# Changelog\n\n## 0.3.0 — 2026-09-20\n\n## 0.2.0 — 2026-09-14\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if err := RequireChangelogVersion([]byte(testCase.changelog), "v0.2.0"); err != nil {
				t.Fatalf("RequireChangelogVersion() = %v, want nil", err)
			}
		})
	}
}

func TestRequireChangelogVersionRefusesAnUndocumentedVersion(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name      string
		changelog string
	}{
		{name: "only a placeholder heading", changelog: "# Changelog\n\n## Next\n\n### Fixed\n\n- Something.\n"},
		{name: "only older versions", changelog: "# Changelog\n\n## 0.1.1 — 2026-09-04\n\n## 0.1.0 — 2026-09-04\n"},
		{name: "empty changelog", changelog: ""},
		{name: "a longer version sharing the prefix", changelog: "# Changelog\n\n## 0.2.01 — 2026-09-14\n"},
		{name: "the heading only appears inside a fence", changelog: "# Changelog\n\n## 0.1.0 — 2026-09-04\n\n- We wrote:\n\n```\n## 0.2.0 — 2026-09-14\n```\n"},
		{name: "a deeper heading level", changelog: "# Changelog\n\n### 0.2.0 — 2026-09-14\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := RequireChangelogVersion([]byte(testCase.changelog), "v0.2.0")
			if err == nil {
				t.Fatal("RequireChangelogVersion() = nil, want a refusal")
			}
			if !IsCode(err, CodeChangelogHeading) {
				t.Fatalf("refusal %v does not carry %q", err, CodeChangelogHeading)
			}
			for _, want := range []string{"## 0.2.0", "v0.2.0", "push the tag again"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not name %q", err.Error(), want)
				}
			}
		})
	}
}

func TestRequireChangelogVersionRefusesANonCanonicalTag(t *testing.T) {
	t.Parallel()

	changelog := []byte("# Changelog\n\n## 0.2.0 — 2026-09-14\n")
	for _, tag := range []string{"0.2.0", "v0.2", "v0.2.0-rc.1", "release-0.2.0"} {
		t.Run(tag, func(t *testing.T) {
			t.Parallel()
			if err := RequireChangelogVersion(changelog, tag); err == nil {
				t.Fatalf("RequireChangelogVersion(%q) = nil, want a refusal", tag)
			}
		})
	}
}

func TestShippedChangelogDocumentsEveryReleasedVersion(t *testing.T) {
	t.Parallel()

	path, err := filepath.Abs("../../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	changelog, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"v0.1.0", "v0.1.1", "v0.1.2", "v0.1.3", "v0.1.4", "v0.1.5", "v0.1.6", "v0.2.0"} {
		if err := RequireChangelogVersion(changelog, tag); err != nil {
			t.Errorf("shipped CHANGELOG.md does not document %s: %v", tag, err)
		}
	}
}
