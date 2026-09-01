package adaptertest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// NewCompiler returns a minimal, deliberately unsophisticated SharedCompiler:
// it appends managed Markdown blocks after the verbatim observed bytes (so
// the whole observed file is always a correctly preserved fragment) and
// creates brand-new JSON documents. A target with no Desired entries is
// compiled as a full removal, handing back any observed bytes verbatim as
// unmanaged rather than performing #6's byte-precise partial removal. It
// exists only to prove the #10 seam; preservation-aware merging into an
// existing structured document, and precise partial removal, are #6's job.
func NewCompiler() adapter.SharedCompiler {
	return compiler{}
}

type compiler struct{}

func (compiler) CompileMarkdown(_ context.Context, request adapter.MarkdownCompileRequest) (adapter.SharedCompilation, error) {
	if len(request.Desired) == 0 {
		return removalCompilation(request.Target), nil
	}
	var out bytes.Buffer
	var preserved [][]byte
	if request.Target.Observed != nil && len(request.Target.Observed.Content) != 0 {
		content := request.Target.Observed.Content
		out.Write(content)
		if content[len(content)-1] != '\n' {
			out.WriteByte('\n')
		}
		preserved = append(preserved, append([]byte(nil), content...))
	}
	sorted := append([]adapter.MarkdownInsertion(nil), request.Desired...)
	sort.Slice(sorted, func(left, right int) bool { return sorted[left].BlockID < sorted[right].BlockID })
	managed := make([]adapter.ManagedResult, 0, len(sorted))
	for _, insertion := range sorted {
		fmt.Fprintf(&out, "<!-- ACR:%s -->\n", insertion.BlockID)
		out.Write(insertion.Body)
		if len(insertion.Body) == 0 || insertion.Body[len(insertion.Body)-1] != '\n' {
			out.WriteByte('\n')
		}
		managed = append(managed, adapter.ManagedResult{Owner: insertion.Owner, Kind: realize.ArtifactManagedBlock, ManagedHash: hashBytes(insertion.Body)})
	}
	ownership := realize.OwnershipGenerated
	proof := adapter.PreservationProof{ManagedIntact: true, PreservedContent: preserved}
	if request.Target.Observed != nil {
		ownership = realize.OwnershipShared
		proof.ObservedHash = request.Target.Observed.Hash
	}
	candidate := &adapter.CandidateFile{Path: request.Target.Path, Content: out.Bytes(), Mode: 0o644, Ownership: ownership}
	return adapter.SharedCompilation{Action: realize.ActionEnsure, Candidate: candidate, Managed: managed, Proof: proof}, nil
}

func (compiler) CompileConfig(_ context.Context, request adapter.ConfigCompileRequest) (adapter.SharedCompilation, error) {
	if request.Format != adapter.ConfigJSON {
		return adapter.SharedCompilation{}, fmt.Errorf("reference compiler only supports JSON, got %q", request.Format)
	}
	if len(request.Desired) == 0 {
		return removalCompilation(request.Target), nil
	}
	if request.Target.Observed != nil && len(request.Target.Observed.Content) != 0 {
		return adapter.SharedCompilation{}, errors.New("reference compiler only supports creating a new config file; merging into an existing one belongs to issue #6")
	}
	doc := map[string]any{}
	sorted := append([]adapter.ConfigEntry(nil), request.Desired...)
	sort.Slice(sorted, func(left, right int) bool { return sorted[left].Key < sorted[right].Key })
	managed := make([]adapter.ManagedResult, 0, len(sorted))
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
			return adapter.SharedCompilation{}, fmt.Errorf("decode entry %q: %w", entry.Key, err)
		}
		container[entry.Key] = value
		managed = append(managed, adapter.ManagedResult{Owner: entry.Owner, Kind: realize.ArtifactStructuredEntry, ManagedHash: hashBytes(entry.EncodedValue)})
	}
	rendered, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return adapter.SharedCompilation{}, err
	}
	candidate := &adapter.CandidateFile{Path: request.Target.Path, Content: append(rendered, '\n'), Mode: 0o644, Ownership: realize.OwnershipGenerated}
	return adapter.SharedCompilation{Action: realize.ActionEnsure, Candidate: candidate, Managed: managed, Proof: adapter.PreservationProof{ManagedIntact: true}}, nil
}

// removalCompilation handles a target with no Desired entries: nothing
// remains to own, so every previously managed span is dropped and any
// observed bytes are handed back verbatim as unmanaged content.
func removalCompilation(target adapter.SharedTarget) adapter.SharedCompilation {
	if target.Observed == nil {
		return adapter.SharedCompilation{Action: realize.ActionRemove}
	}
	content := append([]byte(nil), target.Observed.Content...)
	candidate := &adapter.CandidateFile{Path: target.Path, Content: content, Mode: 0o644, Ownership: realize.OwnershipUnmanaged}
	return adapter.SharedCompilation{
		Action:    realize.ActionRemove,
		Candidate: candidate,
		Proof: adapter.PreservationProof{
			ObservedHash: target.Observed.Hash, ManagedIntact: true,
			PreservedContent: [][]byte{content},
		},
	}
}

func hashBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}
