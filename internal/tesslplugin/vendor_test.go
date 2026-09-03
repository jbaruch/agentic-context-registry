package tesslplugin

import (
	"errors"
	"io/fs"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

type vendorReadDirErrorFS struct {
	fs.FS
	fail string
}

func (filesystem vendorReadDirErrorFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == filesystem.fail {
		return nil, fs.ErrPermission
	}
	return fs.ReadDir(filesystem.FS, name)
}

func TestSynthesizeVendorManifestPropagatesRuleDirectoryErrors(t *testing.T) {
	packageFS := vendorReadDirErrorFS{FS: fstest.MapFS{
		pluginManifestRel: &fstest.MapFile{Data: []byte(`{"rules":["rules"]}`)},
	}, fail: "rules"}
	_, err := SynthesizeVendorManifest(packageFS, "example/plugin", "1.2.3")
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("rule directory error = %v", err)
	}
}

func TestSynthesizeVendorManifestPropagatesSkillDirectoryErrors(t *testing.T) {
	packageFS := vendorReadDirErrorFS{FS: fstest.MapFS{
		pluginManifestRel: &fstest.MapFile{Data: []byte(`{"skills":["skills"]}`)},
	}, fail: "skills"}
	_, err := SynthesizeVendorManifest(packageFS, "example/plugin", "1.2.3")
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("skill directory error = %v", err)
	}
}

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
		{
			name: "empty plugin does not fall back to tile",
			files: fstest.MapFS{
				pluginManifestRel: &fstest.MapFile{Data: []byte(`{}`)},
				tileManifestName:  &fstest.MapFile{Data: []byte(`{"rules":{"always":{"rules":"rules/always.md"}}}`)},
				"rules/always.md": &fstest.MapFile{Data: []byte("Always.\n")},
			},
			want: manifest.Artifacts{},
		},
		{
			name: "empty plugin paths do not select the package root",
			files: fstest.MapFS{
				pluginManifestRel:        &fstest.MapFile{Data: []byte(`{"rules":[" ","rules/"],"skills":["","skills/"]}`)},
				"root.md":                &fstest.MapFile{Data: []byte("Do not select.\n")},
				"SKILL.md":               &fstest.MapFile{Data: []byte("# Do not select\n")},
				"rules/always.md":        &fstest.MapFile{Data: []byte("Always.\n")},
				"skills/review/SKILL.md": &fstest.MapFile{Data: []byte("# Review\n")},
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
