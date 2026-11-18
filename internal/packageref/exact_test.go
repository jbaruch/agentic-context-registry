package packageref

import (
	"strings"
	"testing"
)

func TestExactOwnedPathsAndForeignBoundaries(t *testing.T) {
	files := map[string]string{"skills/check/run.sh": "plugins/demo/skills/check/run.sh", ".tessl/plugins/old/demo/skills/check/run.sh": "plugins/demo/skills/check/run.sh"}
	roots := []string{"skills/check/", ".tessl/plugins/old/demo/"}
	source := "Run `skills/check/run.sh`.\n[helper](.tessl/plugins/old/demo/skills/check/run.sh)\nFILE=\"skills/check/run.sh\"\nhttps://example.test/skills/check/run.sh\n.tessl/plugins/foreign/demo/skills/check/run.sh\n"
	want := "Run `plugins/demo/skills/check/run.sh`.\n[helper](plugins/demo/skills/check/run.sh)\nFILE=\"plugins/demo/skills/check/run.sh\"\nhttps://example.test/skills/check/run.sh\n.tessl/plugins/foreign/demo/skills/check/run.sh\n"
	got, err := RewriteFiles([]byte(source), files, roots)
	if err != nil || string(got) != want {
		t.Fatalf("rewrite=%s %v", got, err)
	}
	for _, invalid := range []string{`{"path":"skills/check/run.sh"}`, `open("skills/check/run.sh")`, "skills/check/run.sh$SUFFIX", "skills/check/$HELPER", "skills/check/missing.sh", "skills/check/../run.sh", ".tessl/plugins/old/demo/skills/check/"} {
		if _, err := RewriteFiles([]byte(invalid), files, roots); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}
func TestNativePrefixAPIStillRebasesCrossSkillReferences(t *testing.T) {
	source := []byte("Run **`skills/b/run.sh`** and `.tessl/plugins/old/demo/skills/a/read.md`. Keep https://host/skills/a/read.md\n")
	got := RebasePackageReferences(source, SkillReferences{Identities: []string{"old/demo"}, Rebases: []SkillRebase{{SourceRoot: "skills/a", NativeRoot: ".agents/skills/a"}, {SourceRoot: "skills/b", NativeRoot: ".agents/skills/b"}}})
	if !strings.Contains(string(got), "**`.agents/skills/b/run.sh`**") || !strings.Contains(string(got), "`.agents/skills/a/read.md`") || !strings.Contains(string(got), "https://host/skills/a/read.md") {
		t.Fatalf("%s", got)
	}
}

func TestOwnedReferenceManualMigrationReasons(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{"**skills/check/run.sh**", `owned reference "skills/check/run.sh**" is in an unsupported context; the file requires manual migration`},
		{"skills/check/missing.sh", `owned reference "skills/check/missing.sh" is not a complete declared file path; missing destinations, dynamic suffixes or unsupported punctuation require manual migration`},
	} {
		_, err := RewriteFiles([]byte(tc.source), map[string]string{"skills/check/run.sh": "plugins/demo/skills/check/run.sh"}, []string{"skills/check/"})
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%q: error=%v; want %q", tc.source, err, tc.want)
		}
	}
}

func TestFirstOwnedReferenceFailureIsReported(t *testing.T) {
	source := "**skills/check/one.sh**\nskills/check/two.sh\nopen(\"skills/check/three.sh\")\n"
	want := `owned reference "skills/check/one.sh**" is in an unsupported context; the file requires manual migration`
	got, err := RewriteFiles([]byte(source), nil, []string{"skills/check/"})
	if err == nil || err.Error() != want || got != nil {
		t.Fatalf("rewrite=%q err=%v; want %s", got, err, want)
	}
}

func TestNonASCIIReferenceRootsRefuse(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{"Read **é/check/missing.sh**.", `owned reference "é/check/missing.sh**." is in an unsupported context; the file requires manual migration`},
		{"Read `é/check/missing.sh`.", "owned reference \"é/check/missing.sh`.\" is not a complete declared file path; missing destinations, dynamic suffixes or unsupported punctuation require manual migration"},
	} {
		got, err := RewriteFiles([]byte(tc.source), nil, []string{"é/check/"})
		if got != nil || err == nil || err.Error() != tc.want {
			t.Fatalf("%q: rewrite=%q err=%v; want %s", tc.source, got, err, tc.want)
		}
	}
}

func TestUnsupportedOwnedPositionsAndForeignURLs(t *testing.T) {
	files := map[string]string{"skills/check/run.sh": "plugins/demo/skills/check/run.sh"}
	roots := []string{"skills/check/", ".tessl/plugins/old/demo/", "rules/context.md", "hooks/start.sh"}
	for _, source := range []string{"**skills/check/run.sh**", "*skills/check/run.sh*", "path:skills/check/run.sh", "→skills/check/run.sh", `"see skills/check/run.sh"`, "echo x >skills/check/run.sh", "`rules/context.md`", "`hooks/start.sh`", "ROOT=.tessl/plugins/old/demo", "**.tessl/plugins/old/demo/skills/check/run.sh**"} {
		if _, err := RewriteFiles([]byte(source), files, roots); err == nil {
			t.Fatalf("accepted unsupported %s", source)
		}
	}
	for _, source := range []string{"https://host/skills/check/run.sh", "https://host/?file=skills/check/run.sh", "https://host/a'skills/check/run.sh", ".tessl/plugins/foreign/demo/skills/check/run.sh", "other/rules/context.md", "skills/checkmate/run.sh"} {
		got, err := RewriteFiles([]byte(source), files, roots)
		if err != nil || string(got) != source {
			t.Fatalf("foreign reference changed: %q %v", got, err)
		}
	}
}
