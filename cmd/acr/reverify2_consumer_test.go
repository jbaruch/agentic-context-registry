package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reverify2Put(t *testing.T, root, relative, content string, mode os.FileMode) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// reverify2ConsumerProject is a Tessl consumer installation: a manifest, a
// vendored package with a rule, a skill and a hook, one native tree Tessl owns,
// and one prose file it must leave alone. #1's inventory is what reads it.
func reverify2ConsumerProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	reverify2Put(t, root, "tessl.json",
		`{"name":"consumer","mode":"vendored","dependencies":{"example/alpha":{"version":"1.0.0"}}}`+"\n", 0o644)
	reverify2Put(t, root, ".tessl/plugins/example/alpha/.tessl-plugin/plugin.json",
		`{"name":"example/alpha","version":"1.0.0","rules":["rules/always-rule.md"],`+
			`"skills":["skills/review-change"],"hooks":{"SessionStart":[{"hooks":[{"type":"command",`+
			`"command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/session-start.sh"]}]}]}}`+"\n", 0o644)
	reverify2Put(t, root, ".tessl/plugins/example/alpha/rules/always-rule.md",
		"---\nalwaysApply: true\n---\n# Always\n", 0o644)
	reverify2Put(t, root, ".tessl/plugins/example/alpha/skills/review-change/SKILL.md", "# Review\n", 0o644)
	reverify2Put(t, root, ".tessl/plugins/example/alpha/hooks/session-start.sh", "#!/bin/sh\necho start\n", 0o755)
	reverify2Put(t, root, "AGENTS.md", "# User\n\n## Agent Rules <!-- tessl-managed -->\n\n@.tessl/RULES.md\n", 0o644)

	nativeDir := filepath.Join(root, ".claude", "skills")
	if err := os.MkdirAll(nativeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, ".tessl", "plugins", "example", "alpha", "skills", "review-change")
	relative, err := filepath.Rel(nativeDir, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relative, filepath.Join(nativeDir, "tessl__review-change")); err != nil {
		t.Fatal(err)
	}
	return root
}

func reverify2HashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(filename)
			if err != nil {
				return err
			}
			result[relative] = "link:" + filepath.ToSlash(target)
			return nil
		}
		if entry.IsDir() {
			result[relative] = "dir"
			return nil
		}
		content, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		result[relative] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

type reverify2Inventory struct {
	SchemaVersion int  `json:"schemaVersion"`
	DryRun        bool `json:"dryRun"`
	Wrote         bool `json:"wrote"`
	Agents        []struct {
		ID       string   `json:"id"`
		Covered  bool     `json:"covered"`
		Evidence []string `json:"evidence"`
	} `json:"agents"`
	Packages []struct {
		Name      string `json:"name"`
		Version   string `json:"version"`
		Manifest  string `json:"manifest"`
		Artifacts []struct {
			ID             string `json:"id"`
			Kind           string `json:"kind"`
			Classification string `json:"classification"`
		} `json:"artifacts"`
	} `json:"packages"`
	Preserved []struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	} `json:"preserved"`
}

// #1 landed consumer inventory in the same internal/migrateapp package as #11's
// producer conversion. Through the shipped binary, an unmapped consumer must
// reach the coexistence migrator and fail closed, not the producer decorator.
func TestReverify2ConsumerMigrationSurvivesTheProducerMerge(t *testing.T) {
	t.Parallel()

	binary := reverifyBuildACR(t)
	project := reverify2ConsumerProject(t)
	before := reverify2HashTree(t, project)

	stdout, stderr, exitCode := reverifyRunACR(t, binary, t.TempDir(),
		"migrate", "tessl", "--dry-run", "--project", project, "--json")
	if exitCode != 1 {
		t.Fatalf("exit = %d, want 1; stdout = %q stderr = %q", exitCode, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") {
		t.Fatalf("stderr must be one envelope line, got %q", stderr)
	}

	var envelope struct {
		OK      bool   `json:"ok"`
		Command string `json:"command"`
		Error   struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatalf("stderr is not JSON: %v (%q)", err, stderr)
	}
	if envelope.OK || envelope.Command != "migrate" || envelope.Error.Code != "unmapped_package" {
		t.Fatalf("envelope = %+v", envelope)
	}

	if after := reverify2HashTree(t, project); !reverify2TreesEqual(before, after) {
		t.Fatalf("failed dry-run migration mutated the project\nbefore=%v\nafter=%v", before, after)
	}
}

// The same binary still converts a producer package. Consumer inventory and
// producer conversion share one application boundary after the merge; neither
// may shadow the other.
func TestReverify2ProducerConversionSurvivesTheConsumerMerge(t *testing.T) {
	t.Parallel()

	binary := reverifyBuildACR(t)
	pkg := t.TempDir()
	reverify2Put(t, pkg, ".tessl-plugin/plugin.json",
		`{"name":"example/beta","version":"2.0.0","description":"beta plugin",`+
			`"repository":"https://github.com/example/beta","rules":["rules/always.md"],`+
			`"skills":["skills/review-change"]}`+"\n", 0o644)
	reverify2Put(t, pkg, "rules/always.md", "---\nalwaysApply: true\n---\n# Always\n", 0o644)
	reverify2Put(t, pkg, "skills/review-change/SKILL.md", "# Review\n", 0o644)
	before := reverify2HashTree(t, pkg)

	stdout, stderr, exitCode := reverifyRunACR(t, binary, t.TempDir(),
		"migrate", "tessl-plugin", pkg, "--dry-run", "--json")
	if exitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}

	var envelope struct {
		OK      bool   `json:"ok"`
		Command string `json:"command"`
		Result  struct {
			ReportVersion  int             `json:"reportVersion"`
			DryRun         bool            `json:"dryRun"`
			Wrote          bool            `json:"wrote"`
			SourceManifest string          `json:"sourceManifest"`
			Package        string          `json:"package"`
			Version        string          `json:"version"`
			Ignored        json.RawMessage `json:"ignored"`
			Artifacts      []struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"artifacts"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, stdout)
	}
	if !envelope.OK || envelope.Command != "migrate" {
		t.Fatalf("envelope = %+v", envelope)
	}
	result := envelope.Result
	if result.ReportVersion != 1 || !result.DryRun || result.Wrote {
		t.Fatalf("result = reportVersion %d dryRun %t wrote %t, want a version-1 dry-run report",
			result.ReportVersion, result.DryRun, result.Wrote)
	}
	if result.SourceManifest != "plugin.json" || result.Package != "example/beta" || result.Version != "2.0.0" {
		t.Fatalf("result identity = %+v", result)
	}
	if string(result.Ignored) != "[]" {
		t.Fatalf("result.ignored = %s, want []", result.Ignored)
	}
	if len(result.Artifacts) != 2 {
		t.Fatalf("artifacts = %+v, want the rule and the skill", result.Artifacts)
	}

	if after := reverify2HashTree(t, pkg); !reverify2TreesEqual(before, after) {
		t.Fatalf("dry-run conversion mutated the package\nbefore=%v\nafter=%v", before, after)
	}
}

func reverify2TreesEqual(before, after map[string]string) bool {
	if len(before) != len(after) {
		return false
	}
	for path, digest := range before {
		if after[path] != digest {
			return false
		}
	}
	return true
}
