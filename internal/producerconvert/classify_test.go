package producerconvert

import (
	"fmt"
	"testing"
)

func TestManualMigrationReasons(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{"tessl custom", "unknown or custom Tessl command requires manual migration"},
		{"tessl.json", "Tessl configuration/manifest reference requires manual migration; state, pins and rollback cannot be inferred"},
		{"TESSL_PLUGIN_DIR", "custom Tessl environment or dynamic path operation requires manual migration"},
		{".tessl/cache", "custom Tessl state or dynamic installed path requires manual migration"},
	} {
		if got := semanticOperation([]byte(tc.source)); got != tc.want {
			t.Fatalf("%q: reason=%q; want %q", tc.source, got, tc.want)
		}
	}
	const runtime = "explicit retired .tessl-plugin directory reference requires manual migration; preserve metadata-dependent behavior before removing its manifest"
	if got := runtimeSemanticOperation([]byte("Path('.tessl-plugin') / 'plugin.json'")); got != runtime {
		t.Fatalf("runtime reason=%q; want %q", got, runtime)
	}
	const instruction = "executable Markdown example references the retired .tessl-plugin directory and requires manual migration; preserve metadata-dependent behavior before removing its manifest"
	if got := instructionSemanticOperation([]byte("Run `cat '.tessl-plugin'/'plugin.json'`.")); got != instruction {
		t.Fatalf("instruction reason=%q; want %q", got, instruction)
	}
}

func TestCorrectionHistoricalDetection(t *testing.T) {
	const history = "https://github.com/tessl-labs/original/blob/main/.tessl-plugin/plugin.json"
	for _, executable := range []bool{false, true} {
		t.Run(fmt.Sprintf("operation=%t", executable), func(t *testing.T) {
			root, opts := fixture(t)
			files := map[string]string{
				".github/ISSUE_TEMPLATE/history.md":     "# Research\nSee " + history + "\n",
				".github/workflows/history.yml":         "name: Tessl history\non: push\njobs:\n  check:\n    runs-on: ubuntu-latest\n    steps:\n      - run: curl " + history + "\n",
				"plugins/orbit/skills/check/history.sh": "#!/bin/sh\n# Historical source " + history + "\ncurl " + history + "\n",
			}
			if executable {
				files[".github/ISSUE_TEMPLATE/history.md"] += "Run `tessl install owner/policy`.\n"
			}
			for name, body := range files {
				put(t, root, name, body, 0o640)
			}
			original := treeAt(t, root)
			plan, err := Prepare(opts)
			if !matches(original, treeAt(t, root)) {
				t.Fatal("planning mutated history")
			}
			absent(t, root, ReceiptPath)
			absent(t, root, transactionPath)
			if executable {
				if err == nil {
					t.Fatal("actual operation escaped refusal")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := plan.Apply(); err != nil {
				t.Fatal(err)
			}
			for name, body := range files {
				if read(t, root, name) != body {
					t.Fatalf("history changed: %s", name)
				}
			}
		})
	}
}
