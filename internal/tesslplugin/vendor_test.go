package tesslplugin

import (
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestSynthesizeVendorManifestForms(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files fstest.MapFS
		want  manifest.Artifacts
	}{
		{
			name: "plugin hooks and native hooks",
			files: fstest.MapFS{
				pluginManifestRel: &fstest.MapFile{Data: []byte(`{
					"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/start.sh","--fast"]}]}]},
					"nativeHooks":{"claude-code":{"PostToolUse":[{"hooks":[{"command":"bash \"${TESSL_PLUGIN_DIR}/hooks/after.sh\""}]}]}}
				}`)},
			},
			want: manifest.Artifacts{Hooks: []manifest.HookArtifact{
				{ID: "after", Path: "hooks/after.sh", Event: manifest.HookPostToolUse},
				{ID: "start", Path: "hooks/start.sh", Event: manifest.HookSessionStart, Args: []string{"--fast"}},
			}},
		},
		{
			name: "path scoped rule and expanded skill",
			files: fstest.MapFS{
				pluginManifestRel:             &fstest.MapFile{Data: []byte(`{"rules":"rules/","skills":["skills/"]}`)},
				"rules/Error_Handling.md":     &fstest.MapFile{Data: []byte("---\nalwaysApply: false\napplyTo: internal/**/*.go, cmd/** -- Go sources\n---\n\nHandle errors.\n")},
				"skills/Review/SKILL.md":      &fstest.MapFile{Data: []byte("# Review\n")},
				"skills/ignored/not-skill.md": &fstest.MapFile{Data: []byte("ignored\n")},
			},
			want: manifest.Artifacts{
				Rules:  []manifest.RuleArtifact{{ID: "error-handling", Path: "rules/Error_Handling.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationPaths, Paths: []string{"internal/**/*.go", "cmd/**"}}}},
				Skills: []manifest.SkillArtifact{{ID: "review", Path: "skills/Review"}},
			},
		},
		{
			name: "tile only",
			files: fstest.MapFS{
				tileManifestName: &fstest.MapFile{Data: []byte(`{"rules":{"always":{"rules":"rules/always.md"}},"skills":{"review":{"path":"skills/review/SKILL.md"}}}`)},
			},
			want: manifest.Artifacts{
				Rules:  []manifest.RuleArtifact{{ID: "always", Path: "rules/always.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}},
				Skills: []manifest.SkillArtifact{{ID: "review", Path: "skills/review"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := SynthesizeVendorManifest(test.files, "example/plugin", "1.2.3")
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != "example/plugin" || got.Version != "1.2.3" || got.SchemaVersion != manifest.CurrentSchemaVersion {
				t.Fatalf("manifest header = %#v", got)
			}
			if !reflect.DeepEqual(got.Artifacts, test.want) {
				t.Fatalf("artifacts = %#v, want %#v", got.Artifacts, test.want)
			}
		})
	}
}
