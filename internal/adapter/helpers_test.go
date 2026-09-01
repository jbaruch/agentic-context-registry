package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// stubAdapter is a minimal, purpose-configurable Adapter for boundary tests.
// Concrete rendering behavior belongs in issue #12; this package only proves
// the generic contract and its guards.
type stubAdapter struct {
	descriptor Descriptor
	artifacts  []ArtifactKind
	events     []manifest.HookEvent
	detect     func(context.Context, DetectRequest) (Detection, error)
	plan       func(context.Context, PlanRequest) (NativePlan, error)
	render     func(context.Context, RenderRequest) ([]Output, error)
	validate   func(context.Context, ValidateRequest) error
}

func (stub stubAdapter) Descriptor() Descriptor { return stub.descriptor }

func (stub stubAdapter) Detect(ctx context.Context, request DetectRequest) (Detection, error) {
	if stub.detect != nil {
		return stub.detect(ctx, request)
	}
	return Detection{}, nil
}

func (stub stubAdapter) SupportedArtifacts() []ArtifactKind { return stub.artifacts }

func (stub stubAdapter) SupportedEvents() []manifest.HookEvent { return stub.events }

func (stub stubAdapter) Plan(ctx context.Context, request PlanRequest) (NativePlan, error) {
	if stub.plan != nil {
		return stub.plan(ctx, request)
	}
	return NativePlan{Adapter: stub.descriptor}, nil
}

func (stub stubAdapter) Render(ctx context.Context, request RenderRequest) ([]Output, error) {
	if stub.render != nil {
		return stub.render(ctx, request)
	}
	return nil, nil
}

func (stub stubAdapter) Validate(ctx context.Context, request ValidateRequest) error {
	if stub.validate != nil {
		return stub.validate(ctx, request)
	}
	return nil
}

func testDescriptor(id, version string) Descriptor {
	return Descriptor{ID: id, Version: version, Boundary: CurrentBoundaryVersion}
}

// mapSnapshot is a Snapshot backed by an in-memory map, for tests that do not
// need a real filesystem.
type mapSnapshot map[string][]byte

func (snapshot mapSnapshot) ReadFile(path string) (ObservedFile, error) {
	content, exists := snapshot[path]
	if !exists {
		return ObservedFile{}, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return ObservedFile{Path: path, Content: content, Mode: 0o644, Hash: hashContent(content)}, nil
}

// fakeCompiler is a test-only SharedCompiler with per-test overridable merge
// functions.
type fakeCompiler struct {
	mergeMarkdown func(ObservedFile, bool, []MarkdownInsertion) (MergedDocument, error)
	mergeConfig   func(ObservedFile, bool, ConfigFormat, []ConfigEntry) (MergedDocument, error)
}

func (fake fakeCompiler) MergeMarkdown(observed ObservedFile, exists bool, insertions []MarkdownInsertion) (MergedDocument, error) {
	return fake.mergeMarkdown(observed, exists, insertions)
}

func (fake fakeCompiler) MergeConfig(observed ObservedFile, exists bool, format ConfigFormat, entries []ConfigEntry) (MergedDocument, error) {
	return fake.mergeConfig(observed, exists, format, entries)
}

// testCompiler is a minimal, deliberately unsophisticated SharedCompiler used
// only to prove the #10 seam: appending managed Markdown blocks after the
// verbatim observed bytes (so the whole observed file is always a correctly
// preserved fragment), and creating brand-new JSON documents. Merging
// structural entries into an existing on-disk document is #6's job; this
// fake reports an explicit error rather than guessing at preservation.
func testCompiler() SharedCompiler {
	return fakeCompiler{
		mergeMarkdown: func(observed ObservedFile, exists bool, insertions []MarkdownInsertion) (MergedDocument, error) {
			var out bytes.Buffer
			var preserved [][]byte
			if exists && len(observed.Content) != 0 {
				out.Write(observed.Content)
				if observed.Content[len(observed.Content)-1] != '\n' {
					out.WriteByte('\n')
				}
				preserved = append(preserved, append([]byte(nil), observed.Content...))
			}
			sorted := append([]MarkdownInsertion(nil), insertions...)
			sort.Slice(sorted, func(left, right int) bool { return sorted[left].BlockID < sorted[right].BlockID })
			for _, insertion := range sorted {
				fmt.Fprintf(&out, "<!-- ACR:%s -->\n", insertion.BlockID)
				out.Write(insertion.Body)
				if len(insertion.Body) == 0 || insertion.Body[len(insertion.Body)-1] != '\n' {
					out.WriteByte('\n')
				}
			}
			return MergedDocument{Content: out.Bytes(), ManagedIntact: true, Preserved: preserved}, nil
		},
		mergeConfig: func(observed ObservedFile, exists bool, format ConfigFormat, entries []ConfigEntry) (MergedDocument, error) {
			if format != ConfigJSON {
				return MergedDocument{}, fmt.Errorf("test compiler only supports JSON, got %q", format)
			}
			if exists && len(observed.Content) != 0 {
				return MergedDocument{}, errors.New("test compiler only supports creating a new config file; merging into an existing one belongs to issue #6")
			}
			doc := map[string]any{}
			sorted := append([]ConfigEntry(nil), entries...)
			sort.Slice(sorted, func(left, right int) bool { return sorted[left].Key < sorted[right].Key })
			for _, entry := range sorted {
				var value any
				if err := json.Unmarshal(entry.EncodedValue, &value); err != nil {
					return MergedDocument{}, fmt.Errorf("decode entry %q: %w", entry.Key, err)
				}
				doc[entry.Key] = value
			}
			rendered, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				return MergedDocument{}, err
			}
			return MergedDocument{Content: append(rendered, '\n'), ManagedIntact: true}, nil
		},
	}
}

func writeTestFile(t *testing.T, root, relative, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testDirFS(t *testing.T, root string) fs.FS {
	t.Helper()
	return os.DirFS(root)
}

func jsonValue(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
