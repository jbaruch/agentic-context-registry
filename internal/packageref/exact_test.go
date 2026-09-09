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
