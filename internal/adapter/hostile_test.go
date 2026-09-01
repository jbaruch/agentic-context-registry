package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func hostileOwner(id string) OwnerRef {
	return OwnerRef{Source: "github:owner/pkg", ArtifactID: id, SourcePath: "rules/" + id + ".md", Kind: ArtifactRule}
}

func generatedOutput(target, body string) Output {
	return Output{
		Target: target, Kind: OutputGeneratedFile, Mode: 0o644,
		File: &GeneratedFile{Owner: hostileOwner("rule-a"), Content: []byte(body)},
	}
}

func hostileFileAdapter(id, version string, targets []Output) stubAdapter {
	items := make([]PlanItem, 0, len(targets))
	for _, output := range targets {
		items = append(items, PlanItem{Owner: hostileOwner("rule-a"), Target: output.Target, Kind: output.Kind, Mode: output.Mode})
	}
	return stubAdapter{
		descriptor: testDescriptor(id, version),
		artifacts:  []ArtifactKind{ArtifactRule},
		plan: func(_ context.Context, _ PlanRequest) (NativePlan, error) {
			return NativePlan{Adapter: testDescriptor(id, version), Items: items}, nil
		},
		render: func(_ context.Context, _ RenderRequest) ([]Output, error) {
			return append([]Output(nil), targets...), nil
		},
	}
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(filename string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		tree[filepath.ToSlash(relative)] = string(content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestHostileReservedPathIntentDoesNotWrite(t *testing.T) {
	t.Parallel()

	for _, targetPath := range []string{"agents.yaml", ".agents", ".agents/registry.lock", ".git", ".git/config"} {
		targetPath := targetPath
		t.Run(targetPath, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeTestFile(t, root, "sentinel.txt", "do-not-touch\n")
			before := snapshotTree(t, root)
			coordinator, err := NewCoordinator(nil, hostileFileAdapter("hostile", "1.0.0", []Output{generatedOutput(targetPath, "managed\n")}))
			if err != nil {
				t.Fatal(err)
			}
			intents, realizeErr := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
			if realizeErr == nil {
				_, applyErr := realize.NewEngine().Run(root, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, intents, realize.ModeApply, func(realize.Ledger) error {
					t.Fatal("finalizer called for reserved path")
					return nil
				})
				if applyErr == nil || !strings.Contains(applyErr.Error(), "reserved project state path") {
					t.Fatalf("Run(apply) error = %v, want reserved-path rejection after Realize succeeded", applyErr)
				}
			}
			if got := snapshotTree(t, root); !reflect.DeepEqual(got, before) {
				t.Fatalf("tree after reserved-path probe = %#v, want %#v", got, before)
			}
		})
	}
}

func TestHostileGeneratedFileCannotReplaceSharedTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, root, "AGENTS.md", "user preface\nmanaged\n")
	before := snapshotTree(t, root)
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{{
		Path: "AGENTS.md", Mode: 0o644, Ownership: realize.OwnershipShared, OutputHash: hashContent([]byte("user preface\nmanaged\n")),
		Entries: []realize.Entry{{Source: "github:owner/pkg", ArtifactID: "rule-a", ArtifactKind: realize.ArtifactManagedBlock, SourcePath: "rules/rule-a.md", Adapter: "hostile", AdapterVersion: "1.0.0", ManagedHash: hashContent([]byte("x"))}},
	}}}
	coordinator, err := NewCoordinator(nil, hostileFileAdapter("hostile", "1.0.0", []Output{generatedOutput("AGENTS.md", "entirely new bytes\n")}))
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, previous)
	var malformed *MalformedOutputError
	if !errors.As(err, &malformed) || len(intents) != 0 {
		t.Fatalf("Realize() = %#v, %v, want *MalformedOutputError and no intents", intents, err)
	}
	if got := snapshotTree(t, root); !reflect.DeepEqual(got, before) {
		t.Fatalf("shared tree changed = %#v", got)
	}
}

func TestHostileDuplicateBlockIDsAcrossAdapters(t *testing.T) {
	t.Parallel()

	makeMarkdown := func(id string) stubAdapter {
		return stubAdapter{
			descriptor: testDescriptor(id, "1.0.0"),
			artifacts:  []ArtifactKind{ArtifactRule},
			plan: func(_ context.Context, _ PlanRequest) (NativePlan, error) {
				return NativePlan{Adapter: testDescriptor(id, "1.0.0"), Items: []PlanItem{{Owner: hostileOwner("rule-a"), Target: "AGENTS.md", Kind: OutputMarkdownInclude, Mode: 0o644}}}, nil
			},
			render: func(_ context.Context, _ RenderRequest) ([]Output, error) {
				return []Output{{
					Target: "AGENTS.md", Kind: OutputMarkdownInclude, Mode: 0o644,
					Markdown: []MarkdownInsertion{{Owner: hostileOwner("rule-a"), BlockID: "shared-id", Body: []byte(id + "\n")}},
				}}, nil
			},
		}
	}
	coordinator, err := NewCoordinator(testCompiler(), makeMarkdown("adapter-a"), makeMarkdown("adapter-b"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	_, err = coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
	var duplicate *DuplicateEntryError
	if !errors.As(err, &duplicate) || duplicate.Identifier != "shared-id" || !strings.Contains(err.Error(), CodeDuplicateConfigEntry) {
		t.Fatalf("Realize() error = %v, want duplicate_config_entry for shared-id", err)
	}
}

func TestHostileDuplicateConfigKeyAcrossAdapters(t *testing.T) {
	t.Parallel()

	entry := ConfigEntry{Owner: hostileOwner("hook-a"), Container: []string{"hooks"}, Kind: ConfigField, Key: "session-start", EncodedValue: jsonValue(map[string]any{"event": "SessionStart"})}
	makeConfig := func(id string) stubAdapter {
		return stubAdapter{
			descriptor: testDescriptor(id, "1.0.0"),
			artifacts:  []ArtifactKind{ArtifactRule},
			plan: func(_ context.Context, _ PlanRequest) (NativePlan, error) {
				return NativePlan{Adapter: testDescriptor(id, "1.0.0"), Items: []PlanItem{{Owner: hostileOwner("hook-a"), Target: "hooks.json", Kind: OutputConfigMerge, Mode: 0o644}}}, nil
			},
			render: func(_ context.Context, _ RenderRequest) ([]Output, error) {
				return []Output{{Target: "hooks.json", Kind: OutputConfigMerge, Mode: 0o644, Config: &ConfigMerge{Format: ConfigJSON, Entries: []ConfigEntry{entry}}}}, nil
			},
		}
	}
	coordinator, err := NewCoordinator(testCompiler(), makeConfig("adapter-a"), makeConfig("adapter-b"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	_, err = coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
	var duplicate *DuplicateEntryError
	if !errors.As(err, &duplicate) || !strings.Contains(err.Error(), CodeDuplicateConfigEntry) {
		t.Fatalf("Realize() error = %v, want duplicate (container, kind, key)", err)
	}
}

func TestHostileUnsupportedEventBeforeWrite(t *testing.T) {
	t.Parallel()

	planCalls, renderCalls := 0, 0
	noStop := stubAdapter{
		descriptor: testDescriptor("no-stop", "1.0.0"),
		artifacts:  []ArtifactKind{ArtifactHook},
		events:     []manifest.HookEvent{manifest.HookSessionStart},
		plan:       func(context.Context, PlanRequest) (NativePlan, error) { planCalls++; return NativePlan{}, nil },
		render:     func(context.Context, RenderRequest) ([]Output, error) { renderCalls++; return nil, nil },
	}
	pkg := Package{Source: "github:owner/pkg", Manifest: manifest.Manifest{
		Artifacts: manifest.Artifacts{Hooks: []manifest.HookArtifact{{ID: "hook-a", Path: "hooks/a.sh", Event: manifest.HookStop}}},
	}}
	coordinator, err := NewCoordinator(nil, noStop)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeTestFile(t, root, "sentinel.txt", "untouched\n")
	before := snapshotTree(t, root)
	intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{pkg}, realize.Ledger{})
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) || len(intents) != 0 || planCalls != 0 || renderCalls != 0 {
		t.Fatalf("Realize() = %#v, %v, plan=%d render=%d; want UnsupportedError before any adapter call", intents, err, planCalls, renderCalls)
	}
	if got := snapshotTree(t, root); !reflect.DeepEqual(got, before) {
		t.Fatalf("tree after unsupported event = %#v", got)
	}
}

func TestHostileShuffledAdapterOrderIsCanonical(t *testing.T) {
	t.Parallel()

	configAdapter := func(id, key, value string) stubAdapter {
		return stubAdapter{
			descriptor: testDescriptor(id, "1.0.0"),
			artifacts:  []ArtifactKind{ArtifactRule},
			plan: func(_ context.Context, _ PlanRequest) (NativePlan, error) {
				return NativePlan{Adapter: testDescriptor(id, "1.0.0"), Items: []PlanItem{{Owner: hostileOwner(key), Target: "hooks.json", Kind: OutputConfigMerge, Mode: 0o644}}}, nil
			},
			render: func(_ context.Context, _ RenderRequest) ([]Output, error) {
				return []Output{{
					Target: "hooks.json", Kind: OutputConfigMerge, Mode: 0o644,
					Config: &ConfigMerge{Format: ConfigJSON, Entries: []ConfigEntry{{Owner: hostileOwner(key), Container: []string{"hooks"}, Kind: ConfigField, Key: key, EncodedValue: jsonValue(value)}}},
				}}, nil
			},
		}
	}
	compile := func(first, second stubAdapter) []byte {
		t.Helper()
		coordinator, err := NewCoordinator(testCompiler(), first, second)
		if err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		intents, err := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
		if err != nil || len(intents) != 1 {
			t.Fatalf("Realize() = %#v, %v", intents, err)
		}
		return intents[0].Content
	}
	forward := compile(configAdapter("adapter-a", "alpha", "a"), configAdapter("adapter-b", "bravo", "b"))
	reversed := compile(configAdapter("adapter-b", "bravo", "b"), configAdapter("adapter-a", "alpha", "a"))
	if string(forward) != string(reversed) {
		t.Fatalf("compiled config depends on adapter order:\nforward: %s\nreversed: %s", forward, reversed)
	}
}

func TestHostileAdapterVersionBumpPlanWithoutWrites(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	first, err := NewCoordinator(nil, hostileFileAdapter("hostile", "1.0.0", []Output{generatedOutput("rules/rule-a.md", "managed\n")}))
	if err != nil {
		t.Fatal(err)
	}
	intents, err := first.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
	if err != nil {
		t.Fatal(err)
	}
	var persisted realize.Ledger
	if _, err := realize.NewEngine().Run(root, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, intents, realize.ModeApply, func(ledger realize.Ledger) error {
		persisted = ledger
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, root)

	upgraded, err := NewCoordinator(nil, hostileFileAdapter("hostile", "1.1.0", []Output{generatedOutput("rules/rule-a.md", "managed\n")}))
	if err != nil {
		t.Fatal(err)
	}
	bumped, err := upgraded.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, persisted)
	if err != nil || len(bumped) != 1 || bumped[0].Entries[0].AdapterVersion != "1.1.0" {
		t.Fatalf("version-bump intents = %#v, %v", bumped, err)
	}
	dry, err := realize.NewEngine().Run(root, persisted, bumped, realize.ModeDryRun, nil)
	if err != nil || !dry.HasChanges() || !dry.LedgerChanged {
		t.Fatalf("Run(dry-run) = %#v, %v, want ledger-changing plan", dry, err)
	}
	if _, err := realize.NewEngine().Run(root, persisted, bumped, realize.ModeCheck, nil); err == nil {
		t.Fatal("Run(check) error = nil, want ChangesError")
	}
	if got := snapshotTree(t, root); !reflect.DeepEqual(got, before) {
		t.Fatalf("dry-run/check wrote files: got %#v want %#v", got, before)
	}
}

func TestHostileEscapePathsAreRejectedBeforeWrite(t *testing.T) {
	t.Parallel()

	t.Run("dot-dot", func(t *testing.T) {
		t.Parallel()
		_, err := compileOutputs(context.Background(), mapSnapshot{}, realize.Ledger{}, nil, []adapterRender{{
			Descriptor: testDescriptor("hostile", "1.0.0"),
			Outputs:    []Output{generatedOutput("../outside.md", "escaped\n")},
		}})
		if err == nil {
			root := t.TempDir()
			writeTestFile(t, root, "sentinel.txt", "inside\n")
			before := snapshotTree(t, root)
			intent := realize.Intent{Action: realize.ActionEnsure, Path: "../outside.md", Content: []byte("escaped\n"), Mode: 0o644, Ownership: realize.OwnershipGenerated, Entries: []realize.Entry{{
				Source: "github:owner/pkg", ArtifactID: "rule-a", ArtifactKind: realize.ArtifactFile, SourcePath: "rules/rule-a.md", Adapter: "hostile", AdapterVersion: "1.0.0", ManagedHash: hashContent([]byte("escaped\n")),
			}}}
			_, applyErr := realize.NewEngine().Run(root, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, []realize.Intent{intent}, realize.ModeApply, func(realize.Ledger) error { return nil })
			if applyErr == nil {
				t.Fatal("engine applied a ../ generated-file path")
			}
			if got := snapshotTree(t, root); !reflect.DeepEqual(got, before) {
				t.Fatalf("tree after ../ probe = %#v", got)
			}
			if _, statErr := os.Stat(filepath.Join(root, "..", "outside.md")); statErr == nil {
				t.Fatal("wrote ../outside.md")
			}
		}
	})

	t.Run("absolute", func(t *testing.T) {
		t.Parallel()
		_, err := compileOutputs(context.Background(), mapSnapshot{}, realize.Ledger{}, nil, []adapterRender{{
			Descriptor: testDescriptor("hostile", "1.0.0"),
			Outputs:    []Output{generatedOutput("/etc/passwd", "escaped\n")},
		}})
		if err == nil {
			root := t.TempDir()
			intent := realize.Intent{Action: realize.ActionEnsure, Path: "/etc/passwd", Content: []byte("escaped\n"), Mode: 0o644, Ownership: realize.OwnershipGenerated, Entries: []realize.Entry{{
				Source: "github:owner/pkg", ArtifactID: "rule-a", ArtifactKind: realize.ArtifactFile, SourcePath: "rules/rule-a.md", Adapter: "hostile", AdapterVersion: "1.0.0", ManagedHash: hashContent([]byte("escaped\n")),
			}}}
			_, applyErr := realize.NewEngine().Run(root, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, []realize.Intent{intent}, realize.ModeApply, func(realize.Ledger) error { return nil })
			if applyErr == nil {
				t.Fatal("engine applied an absolute generated-file path")
			}
		}
	})

	t.Run("symlink component", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
			t.Fatalf("create symlink on supported platform: %v", err)
		}
		writeTestFile(t, root, "sentinel.txt", "inside\n")
		before := snapshotTree(t, root)
		coordinator, err := NewCoordinator(nil, hostileFileAdapter("hostile", "1.0.0", []Output{generatedOutput("link/escaped.md", "escaped\n")}))
		if err != nil {
			t.Fatal(err)
		}
		intents, realizeErr := coordinator.Realize(context.Background(), NewFSSnapshot(os.DirFS(root)), []Package{testPackage("rule-a")}, realize.Ledger{})
		if realizeErr == nil {
			_, applyErr := realize.NewEngine().Run(root, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, intents, realize.ModeApply, func(realize.Ledger) error { return nil })
			if applyErr == nil {
				t.Fatal("engine applied a generated-file through a symlink parent")
			}
		}
		if got := snapshotTree(t, root); !reflect.DeepEqual(got, before) {
			t.Fatalf("tree after symlink probe = %#v", got)
		}
		if _, statErr := os.Stat(filepath.Join(outside, "escaped.md")); statErr == nil {
			t.Fatal("wrote through symlink to outside dir")
		}
	})
}

func TestHostileOutputWithTwoPayloadsRejected(t *testing.T) {
	t.Parallel()

	_, err := compileOutputs(context.Background(), mapSnapshot{}, realize.Ledger{}, testCompiler(), []adapterRender{{
		Descriptor: testDescriptor("hostile", "1.0.0"),
		Outputs: []Output{{
			Target:   "x.md",
			Kind:     OutputGeneratedFile,
			File:     &GeneratedFile{Owner: hostileOwner("rule-a"), Content: []byte("a\n")},
			Markdown: []MarkdownInsertion{{Owner: hostileOwner("rule-a"), BlockID: "a", Body: []byte("b\n")}},
		}},
	}})
	var malformed *MalformedOutputError
	if !errors.As(err, &malformed) {
		t.Fatalf("compileOutputs() error = %v, want *MalformedOutputError for two payloads", err)
	}
}

func TestHostileEmptyOutputKindRejected(t *testing.T) {
	t.Parallel()

	_, err := compileOutputs(context.Background(), mapSnapshot{}, realize.Ledger{}, nil, []adapterRender{{
		Descriptor: testDescriptor("hostile", "1.0.0"),
		Outputs: []Output{{
			Target: "x.md",
			File:   &GeneratedFile{Owner: hostileOwner("rule-a"), Content: []byte("a\n")},
		}},
	}})
	var malformed *MalformedOutputError
	if !errors.As(err, &malformed) || !strings.Contains(err.Error(), "unsupported output kind") {
		t.Fatalf("compileOutputs() error = %v, want rejection of empty Kind", err)
	}
}

func TestHostileDetectCreatesNothing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	adapter := stubAdapter{
		descriptor: testDescriptor("hostile", "1.0.0"),
		detect: func(_ context.Context, request DetectRequest) (Detection, error) {
			_, err := request.Project.ReadFile("AGENTS.md")
			if err == nil {
				return Detection{Detected: true, Evidence: []string{"AGENTS.md"}}, nil
			}
			return Detection{}, nil
		},
	}
	detection, err := adapter.Detect(context.Background(), DetectRequest{Project: NewFSSnapshot(os.DirFS(root))})
	if err != nil || detection.Detected {
		t.Fatalf("Detect(empty) = %#v, %v", detection, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Detect created files: %v, %v", entries, err)
	}
}
