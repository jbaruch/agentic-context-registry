package realizeapp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// Digests from 292057b8 include the selection, lock, and every native file's
// path, mode and content. The checkout-local receipt contains inode metadata.
func TestLegacyAgentOutputs(t *testing.T) {
	expected := map[string]string{
		"claude-code/shared=false":              "f5635d6694593665906c7071ce88ba1337dbee90bab96cacf3859ed3adeb25fe",
		"claude-code/shared=true":               "17b021b5e5a4b50648c230f955592d22e7c737a93d7bc6fb503d026ee892ea17",
		"codex/shared=false":                    "f5748b5f0d247e55d56e395ebce3ec0f4dc09e88cdbe5988ac5e158cb461e7dd",
		"codex/shared=true":                     "95e437a4163c1037ad58e3cc54452ba4c5be2abf3938fa51a1161cc1781d9124",
		"claude-code+codex/shared=false":        "c05243e23b02032b6cae00ac74ff90903836c0dc921221c19ac336d97dc1a69c",
		"claude-code+codex/shared=true":         "2e0dce8f51a53c591ce6419c0f03492d0437537b8ef69501c6fc4ad0f4455c0b",
		"cursor/shared=false":                   "47d364d87823e32c2b524b6ac41ffbfb14fa779ca84f3a38c3fb0fdb0e1885e6",
		"cursor/shared=true":                    "42df31d5eeaecd7fff82085710c388c715b0f9585f3ded936daf76360100247e",
		"claude-code+cursor/shared=false":       "e5da13be4a4c3cbee15151c76dd72d3663f5782e3a0eba8ce1a9aecb2b41145d",
		"claude-code+cursor/shared=true":        "97c7daa5b35fa9b76c5d66ce587e927967ffe99b8bcb9aa75ef629bbdebe680a",
		"codex+cursor/shared=false":             "a46e3e86637bae6136f901088e9a76ea9bc07906a5105247d2b1b104098851df",
		"codex+cursor/shared=true":              "f2e387957d90003700bd79ae081a00850e36bcd58cba22f5759b8f78389cb62d",
		"claude-code+codex+cursor/shared=false": "1707588a7815c4e15bd14acd67840b6a83da8942e37980b9a6a91715aea85e93",
		"claude-code+codex+cursor/shared=true":  "3cf80ba68e4a697aee13f672c1f696c4313a7730915590e0322197294dba9866",
	}
	for mask := 1; mask < 8; mask++ {
		var agents []string
		for i, id := range []string{"claude-code", "codex", "cursor"} {
			if mask&(1<<i) != 0 {
				agents = append(agents, id)
			}
		}
		for _, shared := range []bool{false, true} {
			key := fmt.Sprintf("%s/shared=%t", strings.Join(agents, "+"), shared)
			t.Run(key, func(t *testing.T) {
				root, loader, service := sharedSkillFixture(t, agents, shared, firstSource)
				p := loader.packages[firstSource]
				p.manifest.Artifacts.Rules = []manifest.RuleArtifact{{ID: "guide", Path: "guide.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}}
				p.manifest.Artifacts.Scripts = []manifest.ScriptArtifact{{ID: "script", Path: "sample.sh"}}
				writeFixture(t, filepath.Join(p.root, "guide.md"), []byte("sample guidance\n"), 0644)
				writeFixture(t, filepath.Join(p.root, "sample.sh"), []byte("#!/bin/sh\nexit 0\n"), 0755)
				loader.packages[firstSource] = p
				initRepository(t, root)
				if _, err := service.Run(context.Background(), root, nil, realize.ModeApply); err != nil {
					t.Fatal(err)
				}
				tree := hashProjectTree(t, root)
				delete(tree, appliedPath)
				content, err := json.Marshal(tree)
				if err != nil {
					t.Fatal(err)
				}
				got := fmt.Sprintf("%x", sha256.Sum256(content))
				if got != expected[key] {
					t.Errorf("native files or ownership changed for %s: got %s, want %s", key, got, expected[key])
				}
			})
		}
	}
}
