package producerconvert

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCorrectionOwnedReferencePositionsRefuseBeforeWrites(t *testing.T) {
	for _, owned := range []string{"skills/check/check.sh", ".tessl/plugins/upstream/orbit/skills/check/check.sh", "rules/context.md", "hooks/start.sh", ".tessl/plugins/upstream/orbit/rules/context.md", ".tessl/plugins/upstream/orbit/hooks/start.sh"} {
		for _, shape := range []string{"**%s**", "*%s*", "key:%s", "→%s", "\"see %s\"", "echo x >%s", "{\"path\":\"%s\"}"} {
			t.Run(owned+shape, func(t *testing.T) {
				root, opts := fixture(t)
				plugin := read(t, root, "plugins/orbit/.tessl-plugin/plugin.json")
				plugin = strings.Replace(plugin, `"skills":`, `"rules":["rules/context.md"],"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/start.sh"]}]}]},"skills":`, 1)
				put(t, root, "plugins/orbit/.tessl-plugin/plugin.json", plugin, 0o644)
				put(t, root, "plugins/orbit/rules/context.md", "---\nalwaysApply: true\n---\n# Rule\n", 0o644)
				put(t, root, "plugins/orbit/hooks/start.sh", "#!/bin/sh\nexit 0\n", 0o755)
				filename := "plugins/orbit/skills/check/paths.txt"
				put(t, root, filename, strings.ReplaceAll(shape, "%s", owned)+"\n", 0o644)
				before := treeAt(t, root)
				for _, dry := range []bool{true, false} {
					opts.DryRun = dry
					report, err := Convert(opts)
					if err == nil || report.Wrote {
						t.Fatalf("accepted %q: %+v", shape, report)
					}
					found := false
					for _, blocker := range report.Blockers {
						if blocker.Path == filename {
							found = true
						}
					}
					if !found {
						t.Fatalf("missing path: %+v %v", report.Blockers, err)
					}
					if !matches(before, treeAt(t, root)) {
						t.Fatal("refusal changed files")
					}
					absent(t, root, ReceiptPath)
					absent(t, root, transactionPath)
				}
			})
		}
	}
}

func TestCorrectionReceiptIgnoresUnrelatedInputs(t *testing.T) {
	root, opts := fixture(t)
	put(t, root, "ancestor-secret.txt", "UNRELATED PRIVATE SENTINEL\n", 0o600)
	put(t, root, "other-project/notes.md", "unrelated\n", 0o644)
	plan, err := Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := json.Marshal(plan.Report)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"ancestor-secret.txt", "UNRELATED PRIVATE SENTINEL", "other-project/notes.md", "AGENTS.md", "tessl.json"} {
		if strings.Contains(string(serialized), unwanted) {
			t.Fatalf("preview leaked unrelated input %s", unwanted)
		}
	}
	put(t, root, "ancestor-secret.txt", "independent edit\n", 0o644)
	put(t, root, "docs/new.md", "independent addition\n", 0o644)
	if report, err := plan.Apply(); err != nil || !report.Wrote {
		t.Fatalf("unrelated race blocked: %+v %v", report, err)
	}
	if err := os.Remove(filepath.Join(root, "other-project/notes.md")); err != nil {
		t.Fatal(err)
	}
	if report, err := Convert(opts); err != nil || !report.Current || report.Wrote {
		t.Fatalf("unrelated rerun blocked: %+v %v", report, err)
	}
}

func TestCorrectionDoesNotReadUnrelatedFiles(t *testing.T) {
	root, opts := fixture(t)
	for _, name := range []string{"unreadable.txt", "other-project/private.txt"} {
		put(t, root, name, "PRIVATE SENTINEL\n", 0)
		t.Cleanup(func() {
			if err := os.Chmod(filepath.Join(root, name), 0o600); err != nil {
				t.Error(err)
			}
		})
	}
	// An unrelated unreadable directory cannot make a selected package unreadable.
	blocked := filepath.Join(root, "unreadable-directory")
	if err := os.Mkdir(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(blocked, 0o700); err != nil {
			t.Error(err)
		}
	})
	opts.DryRun = true
	report, err := Convert(opts)
	if err != nil || report.Wrote {
		t.Fatalf("unrelated access blocked: %+v %v", report, err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "unreadable") || strings.Contains(string(encoded), "PRIVATE SENTINEL") {
		t.Fatal("unneeded path/content leaked")
	}
	opts.DryRun = false
	if applied, err := Convert(opts); err != nil || !applied.Wrote {
		t.Fatalf("unrelated access blocked apply: %+v %v", applied, err)
	}
	if current, err := Convert(opts); err != nil || !current.Current || current.Wrote {
		t.Fatalf("unrelated access blocked rerun: %+v %v", current, err)
	}
}

func TestCorrectionRelevantInventoryStillBinds(t *testing.T) {
	for _, change := range []string{"addition", "removal", "workflow", "second-producer", "root-producer", "mode"} {
		t.Run(change, func(t *testing.T) {
			root, opts := fixture(t)
			plan, err := Prepare(opts)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "addition":
				put(t, root, "plugins/orbit/skills/check/new.txt", "new", 0o644)
			case "removal":
				if err := os.Remove(filepath.Join(root, "plugins/orbit/skills/check/data.txt")); err != nil {
					t.Fatal(err)
				}
			case "workflow":
				put(t, root, ".github/workflows/new.yml", "on: push\n", 0o644)
			case "second-producer":
				put(t, root, "other/.tessl-plugin/plugin.json", "{}", 0o644)
			case "root-producer":
				put(t, root, ".tessl-plugin/plugin.json", "{}", 0o644)
			case "mode":
				if err := os.Chmod(filepath.Join(root, "plugins/orbit/skills/check/check.sh"), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			_, err = plan.Apply()
			var refusal *Error
			if !errors.As(err, &refusal) || refusal.Code != "source_changed" {
				t.Fatalf("relevant race accepted: %v", err)
			}
			absent(t, root, ReceiptPath)
			absent(t, root, transactionPath)
		})
	}
}

func TestCorrectionReferenceRefusalsCoverPublishedRulesAndHooks(t *testing.T) {
	for _, name := range []string{"rules/context.md", "hooks/start.sh"} {
		t.Run(name, func(t *testing.T) {
			root, opts := fixture(t)
			plugin := read(t, root, "plugins/orbit/.tessl-plugin/plugin.json")
			plugin = strings.Replace(plugin, `"skills":`, `"rules":["rules/context.md"],"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/start.sh"]}]}]},"skills":`, 1)
			put(t, root, "plugins/orbit/.tessl-plugin/plugin.json", plugin, 0o644)
			put(t, root, "plugins/orbit/rules/context.md", "---\nalwaysApply: true\n---\n# Rule\n", 0o644)
			put(t, root, "plugins/orbit/hooks/start.sh", "#!/bin/sh\nexit 0\n", 0o755)
			full := "plugins/orbit/" + name
			put(t, root, full, read(t, root, full)+"# Run **skills/check/check.sh**\n", 0o755)
			opts.DryRun = true
			report, err := Convert(opts)
			if err == nil || len(report.Blockers) != 1 || report.Blockers[0].Path != full {
				t.Fatalf("published reference accepted: %+v %v", report.Blockers, err)
			}
			absent(t, root, ReceiptPath)
			absent(t, root, transactionPath)
		})
	}
}

func TestCorrectionIgnoreInventoryAndProvenanceAreDisclosed(t *testing.T) {
	root, opts := fixture(t)
	plugin := read(t, root, "plugins/orbit/.tessl-plugin/plugin.json")
	put(t, root, "plugins/orbit/.tessl-plugin/plugin.json", strings.Replace(plugin, `"version":`, `"repository":"https://github.com/upstream/orbit","version":`, 1), 0o644)
	put(t, root, "plugins/orbit/.tesslignore", "skills/check/data.txt\nskills/check/*.sh\n", 0o644)
	opts.DryRun = true
	report, err := Convert(opts)
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(report.Notes, "\n")
	if !strings.Contains(notes, "matches in publishedFiles: [skills/check/data.txt]") || !strings.Contains(notes, "matches in publishedFiles: [skills/check/check.sh]") {
		t.Fatalf("inventory widening undisclosed: %s", notes)
	}
	for _, change := range report.Changes {
		if change.Path == "agent-plugin.yaml" {
			if !strings.Contains(change.After, `# Original name: "upstream/orbit"`) || !strings.Contains(change.After, `# Original repository: "https://github.com/upstream/orbit"`) {
				t.Fatalf("lost provenance: %s", change.After)
			}
			return
		}
	}
	t.Fatal("missing manifest")
}
