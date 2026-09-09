package producerconvert

import (
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
