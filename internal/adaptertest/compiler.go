package adaptertest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

// NewCompiler returns a minimal, deliberately unsophisticated SharedCompiler:
// it appends managed Markdown blocks after the verbatim observed bytes (so
// the whole observed file is always a correctly preserved fragment) and
// creates brand-new JSON documents. It exists only to prove the #10 seam;
// preservation-aware merging into an existing structured document is #6's
// job.
func NewCompiler() adapter.SharedCompiler {
	return compiler{}
}

type compiler struct{}

func (compiler) MergeMarkdown(observed adapter.ObservedFile, exists bool, insertions []adapter.MarkdownInsertion) (adapter.MergedDocument, error) {
	var out bytes.Buffer
	var preserved [][]byte
	if exists && len(observed.Content) != 0 {
		out.Write(observed.Content)
		if observed.Content[len(observed.Content)-1] != '\n' {
			out.WriteByte('\n')
		}
		preserved = append(preserved, append([]byte(nil), observed.Content...))
	}
	sorted := append([]adapter.MarkdownInsertion(nil), insertions...)
	sort.Slice(sorted, func(left, right int) bool { return sorted[left].BlockID < sorted[right].BlockID })
	for _, insertion := range sorted {
		fmt.Fprintf(&out, "<!-- ACR:%s -->\n", insertion.BlockID)
		out.Write(insertion.Body)
		if len(insertion.Body) == 0 || insertion.Body[len(insertion.Body)-1] != '\n' {
			out.WriteByte('\n')
		}
	}
	return adapter.MergedDocument{Content: out.Bytes(), ManagedIntact: true, Preserved: preserved}, nil
}

func (compiler) MergeConfig(observed adapter.ObservedFile, exists bool, format adapter.ConfigFormat, entries []adapter.ConfigEntry) (adapter.MergedDocument, error) {
	if format != adapter.ConfigJSON {
		return adapter.MergedDocument{}, fmt.Errorf("reference compiler only supports JSON, got %q", format)
	}
	if exists && len(observed.Content) != 0 {
		return adapter.MergedDocument{}, errors.New("reference compiler only supports creating a new config file; merging into an existing one belongs to issue #6")
	}
	doc := map[string]any{}
	sorted := append([]adapter.ConfigEntry(nil), entries...)
	sort.Slice(sorted, func(left, right int) bool { return sorted[left].Key < sorted[right].Key })
	for _, entry := range sorted {
		container := doc
		for _, segment := range entry.Container {
			next, ok := container[segment].(map[string]any)
			if !ok {
				next = map[string]any{}
				container[segment] = next
			}
			container = next
		}
		var value any
		if err := json.Unmarshal(entry.EncodedValue, &value); err != nil {
			return adapter.MergedDocument{}, fmt.Errorf("decode entry %q: %w", entry.Key, err)
		}
		container[entry.Key] = value
	}
	rendered, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return adapter.MergedDocument{}, err
	}
	return adapter.MergedDocument{Content: append(rendered, '\n'), ManagedIntact: true}, nil
}
