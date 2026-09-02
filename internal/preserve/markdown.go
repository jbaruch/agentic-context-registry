package preserve

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func compileMarkdown(request adapter.MarkdownCompileRequest) (adapter.SharedCompilation, error) {
	target := request.Target
	content := []byte(nil)
	if target.Observed != nil {
		content = target.Observed.Content
	}
	blocks, err := parseMarkdownBlocks(target.Path, content)
	if err != nil {
		return adapter.SharedCompilation{}, err
	}
	_, err = bindMarkdownOwnership(target, content, blocks)
	if err != nil {
		return adapter.SharedCompilation{}, err
	}
	fragments := markdownUnmanagedFragments(content, blocks)
	ownership, promoted, err := classifyTarget(target, fragments)
	if err != nil {
		return adapter.SharedCompilation{}, err
	}

	desired := make(map[string]adapter.MarkdownInsertion, len(request.Desired))
	for _, insertion := range request.Desired {
		if insertion.AdapterID == "" {
			return adapter.SharedCompilation{}, conflict(CodeMarkerConflict, target.Path, "managed insertion has no trusted adapter identity")
		}
		expected := adapter.CanonicalMarkdownBlockID(insertion.Owner, insertion.AdapterID)
		if insertion.BlockID != expected {
			return adapter.SharedCompilation{}, conflict(CodeMarkerConflict, target.Path, fmt.Sprintf("block ID %q does not match canonical ID %q", insertion.BlockID, expected))
		}
		if _, duplicate := desired[insertion.BlockID]; duplicate {
			return adapter.SharedCompilation{}, conflict(CodeMarkerConflict, target.Path, fmt.Sprintf("desired marker %s occurs more than once", insertion.BlockID))
		}
		if err := validateManagedBody(insertion.Body); err != nil {
			return adapter.SharedCompilation{}, conflict(CodeMarkerConflict, target.Path, fmt.Sprintf("block %s: %v", insertion.BlockID, err))
		}
		desired[insertion.BlockID] = insertion
	}

	var candidate bytes.Buffer
	managed := make([]adapter.ManagedResult, 0, len(desired))
	used := make(map[string]bool, len(desired))
	cursor := 0
	for _, block := range blocks {
		candidate.Write(content[cursor:block.start])
		insertion, keep := desired[block.id]
		if keep {
			raw, renderErr := renderMarkdownBlock(insertion, block.prefix, block.eol)
			if renderErr != nil {
				return adapter.SharedCompilation{}, renderErr
			}
			candidate.Write(raw)
			managed = append(managed, managedMarkdownResult(insertion, raw))
			used[block.id] = true
		}
		cursor = block.end
	}
	candidate.Write(content[cursor:])

	var additions []adapter.MarkdownInsertion
	for id, insertion := range desired {
		if !used[id] {
			additions = append(additions, insertion)
		}
	}
	sort.Slice(additions, func(left, right int) bool { return additions[left].BlockID < additions[right].BlockID })
	eol := firstEOL(content)
	for _, insertion := range additions {
		prefix := "none"
		if candidate.Len() != 0 {
			current := candidate.Bytes()
			if current[len(current)-1] != '\n' {
				if bytes.Equal(eol, []byte("\r\n")) {
					prefix = "crlf"
				} else {
					prefix = "lf"
				}
			}
		}
		raw, renderErr := renderMarkdownBlock(insertion, prefix, eol)
		if renderErr != nil {
			return adapter.SharedCompilation{}, renderErr
		}
		candidate.Write(raw)
		managed = append(managed, managedMarkdownResult(insertion, raw))
	}
	sort.Slice(managed, func(left, right int) bool { return managed[left].Identity < managed[right].Identity })

	if err := verifyPreservedOrder(candidate.Bytes(), fragments); err != nil {
		return adapter.SharedCompilation{}, conflict(CodeOwnershipConflict, target.Path, err.Error())
	}
	proof := adapter.PreservationProof{ManagedIntact: true, PreservedContent: cloneFragments(fragments)}
	mode := uint32(0o644)
	if target.Observed != nil {
		proof.ObservedHash = target.Observed.Hash
		mode = uint32(target.Observed.Mode.Perm())
	}
	action := realize.ActionEnsure
	if len(request.Desired) == 0 && target.Previous != nil {
		action = realize.ActionRemove
	}
	var candidateFile *adapter.CandidateFile
	if action != realize.ActionRemove || candidate.Len() != 0 {
		candidateFile = &adapter.CandidateFile{Path: target.Path, Content: candidate.Bytes(), Mode: fs.FileMode(mode), Ownership: ownership}
	}
	compilation := adapter.SharedCompilation{Action: action, Candidate: candidateFile, Managed: managed, Proof: proof}
	if promoted {
		compilation.Notices = []adapter.Notice{{
			Code: "shared_file_requires_commit", Path: target.Path,
			Message: "commit the now-authoritative shared file; ACR did not stage it",
		}}
	}
	return compilation, nil
}

func bindMarkdownOwnership(target adapter.SharedTarget, content []byte, blocks []markdownBlock) (map[string]markdownBlock, error) {
	byID := make(map[string]markdownBlock, len(blocks))
	for _, block := range blocks {
		byID[block.id] = block
	}
	if target.Previous == nil {
		if len(blocks) != 0 {
			return nil, conflict(CodeMarkerConflict, target.Path, "ACR-looking block has no matching ledger ownership")
		}
		return nil, nil
	}
	owned := make(map[string]markdownBlock, len(target.Previous.Entries))
	for _, entry := range target.Previous.Entries {
		if entry.ArtifactKind != realize.ArtifactManagedBlock {
			return nil, conflict(CodeMarkerConflict, target.Path, fmt.Sprintf("ledger entry %s is not a managed Markdown block", entry.ArtifactID))
		}
		owner := adapter.OwnerRef{Source: entry.Source, ArtifactID: entry.ArtifactID}
		id := adapter.CanonicalMarkdownBlockID(owner, entry.Adapter)
		block, exists := byID[id]
		if !exists {
			return nil, conflict(CodeMarkerConflict, target.Path, fmt.Sprintf("ledger-owned marker %s is missing", id))
		}
		if block.source != entry.Source || block.artifactID != entry.ArtifactID || block.adapterID != entry.Adapter {
			return nil, conflict(CodeMarkerConflict, target.Path, fmt.Sprintf("marker %s attribution does not match the ledger", id))
		}
		if hashBytes(block.raw) != entry.ManagedHash {
			return nil, conflict(CodeMarkerConflict, target.Path, fmt.Sprintf("ledger-owned marker %s was edited", id))
		}
		owned[id] = block
	}
	for _, block := range blocks {
		if _, exists := owned[block.id]; !exists {
			return nil, conflict(CodeMarkerConflict, target.Path, fmt.Sprintf("ACR-looking block %s has no matching ledger ownership", block.id))
		}
	}
	return owned, nil
}

func markdownUnmanagedFragments(content []byte, blocks []markdownBlock) [][]byte {
	var fragments [][]byte
	cursor := 0
	for _, block := range blocks {
		if block.start > cursor {
			fragments = append(fragments, append([]byte(nil), content[cursor:block.start]...))
		}
		cursor = block.end
	}
	if cursor < len(content) {
		fragments = append(fragments, append([]byte(nil), content[cursor:]...))
	}
	return fragments
}

func renderMarkdownBlock(insertion adapter.MarkdownInsertion, prefix string, eol []byte) ([]byte, error) {
	var out bytes.Buffer
	switch prefix {
	case "none":
	case "lf":
		out.WriteByte('\n')
	case "crlf":
		out.WriteString("\r\n")
	default:
		return nil, fmt.Errorf("unsupported managed prefix %q", prefix)
	}
	fmt.Fprintf(&out, "<!-- acr:begin id=%s source=%s artifact=%s adapter=%s prefix=%s -->", insertion.BlockID, insertion.Owner.Source, insertion.Owner.ArtifactID, insertion.AdapterID, prefix)
	out.Write(eol)
	body := normalizeManagedBody(insertion.Body, eol)
	out.Write(body)
	if len(body) == 0 || !bytes.HasSuffix(body, eol) {
		out.Write(eol)
	}
	fmt.Fprintf(&out, "<!-- acr:end id=%s -->", insertion.BlockID)
	out.Write(eol)
	return out.Bytes(), nil
}

func validateManagedBody(body []byte) error {
	for _, line := range markerLineSpans(body) {
		physical := body[line.start:line.contentEnd]
		if beginMarkerPattern.Match(physical) || endMarkerPattern.Match(physical) {
			return fmt.Errorf("body contains a complete managed marker line")
		}
	}
	return nil
}

func normalizeManagedBody(body, eol []byte) []byte {
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	normalized = bytes.ReplaceAll(normalized, []byte("\r"), []byte("\n"))
	if bytes.Equal(eol, []byte("\n")) {
		return append([]byte(nil), normalized...)
	}
	return bytes.ReplaceAll(normalized, []byte("\n"), eol)
}

func managedMarkdownResult(insertion adapter.MarkdownInsertion, raw []byte) adapter.ManagedResult {
	return adapter.ManagedResult{
		Owner: insertion.Owner, Kind: realize.ArtifactManagedBlock,
		ManagedHash: hashBytes(raw), Identity: insertion.BlockID,
	}
}

func hashBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func cloneFragments(fragments [][]byte) [][]byte {
	cloned := make([][]byte, len(fragments))
	for index, fragment := range fragments {
		cloned[index] = append([]byte(nil), fragment...)
	}
	return cloned
}

func verifyPreservedOrder(candidate []byte, fragments [][]byte) error {
	offset := 0
	for _, fragment := range fragments {
		if len(fragment) == 0 {
			continue
		}
		index := bytes.Index(candidate[offset:], fragment)
		if index < 0 {
			return fmt.Errorf("candidate does not retain an unmanaged byte fragment")
		}
		offset += index + len(fragment)
	}
	return nil
}
