package migrate

import (
	"testing"
	"testing/fstest"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestWrapperDifferenceIsNotEffectiveDiff(t *testing.T) {
	pkg := adapter.Package{
		Root: fstest.MapFS{
			"rules/always.md": {Data: []byte("---\nalwaysApply: true\n---\nSame body\n")},
			"hooks/start.sh":  {Data: []byte("#!/bin/sh\necho ready\n"), Mode: 0o755},
		},
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{
			Rules: []manifest.RuleArtifact{{ID: "always", Path: "rules/always.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}},
			Hooks: []manifest.HookArtifact{{ID: "start", Path: "hooks/start.sh", Event: manifest.HookSessionStart}},
		}},
	}
	acr, err := FromPackage("example/pkg", pkg)
	if err != nil {
		t.Fatal(err)
	}
	tessl := EffectiveSet{
		{EffectiveKey: EffectiveKey{Package: "example/pkg", Kind: kindRule, ID: "always"}, Digest: contentDigest([]byte("Same body\n")), Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}},
		{EffectiveKey: EffectiveKey{Package: "example/pkg", Kind: kindHook, ID: "start"}, Digest: hookDigest([]byte("#!/bin/sh\necho ready\n"), nil), Event: manifest.HookSessionStart},
	}
	if diffs := CompareEffective(tessl, acr); len(diffs) != 0 {
		t.Fatalf("wrapper-only differences = %#v, want none", diffs)
	}
}

func TestCompareEffectiveExhaustiveReasons(t *testing.T) {
	key := func(id string) EffectiveKey { return EffectiveKey{Package: "example/pkg", Kind: kindRule, ID: id} }
	tessl := EffectiveSet{
		{EffectiveKey: key("activation"), Digest: "same", Activation: manifest.RuleActivation{Mode: manifest.ActivationPaths, Paths: []string{"a"}}},
		{EffectiveKey: key("body"), Digest: "old"},
		{EffectiveKey: key("event"), Digest: "same", Event: manifest.HookSessionStart},
		{EffectiveKey: key("lossy"), Digest: "same", Lossy: []string{"description"}},
		{EffectiveKey: key("missing-acr"), Digest: "same"},
	}
	acr := EffectiveSet{
		{EffectiveKey: key("activation"), Digest: "same", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}},
		{EffectiveKey: key("body"), Digest: "new"},
		{EffectiveKey: key("event"), Digest: "same", Event: manifest.HookStop},
		{EffectiveKey: key("lossy"), Digest: "same"},
		{EffectiveKey: key("missing-tessl"), Digest: "same"},
	}
	diffs := CompareEffective(tessl, acr)
	want := map[string]string{
		"activation":    DiffActivation,
		"body":          DiffBody,
		"event":         DiffEvent,
		"lossy":         DiffLossy,
		"missing-acr":   DiffMissingInACR,
		"missing-tessl": DiffMissingInTessl,
	}
	if len(diffs) != len(want) {
		t.Fatalf("diffs = %#v", diffs)
	}
	for _, diff := range diffs {
		if diff.Reason != want[diff.ID] {
			t.Errorf("%s reason = %q, want %q", diff.ID, diff.Reason, want[diff.ID])
		}
	}
}

func TestCanonicalHookDigestIgnoresLauncher(t *testing.T) {
	files := adapter.NewFSSnapshot(fstest.MapFS{".tessl/plugins/acme/pkg/hooks/start.sh": {Data: []byte("echo ready\n"), Mode: 0o755}})
	install := PackageInstall{Root: ".tessl/plugins/acme/pkg", TesslIdentity: "acme/pkg", Hooks: []DeclaredHook{{ID: "start", NativeEvent: "SessionStart", Command: "bash", Args: []string{"${TESSL_PLUGIN_DIR}/hooks/start.sh", "--verbose"}}}}
	hooks, err := NormalizeHooks(files, install)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hooks[0].Digest, hookDigest([]byte("echo ready\n"), []string{"--verbose"}); got != want {
		t.Fatalf("digest = %q, want %q", got, want)
	}
}
