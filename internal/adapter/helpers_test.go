package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
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

// reconcilingCompiler is a real (if minimal) reconciling SharedCompiler test
// double: it parses its own explicit begin/end Markdown block markers back
// out of the observed file, and locates JSON array elements by their
// recorded managedHash rather than by position, so tests can drive genuine
// partial removal, final removal, tampered-entry detection, and
// hash-located array-element updates through the real #10 seam. It is not
// production-representative preservation logic — #6 owns that — but unlike
// a create-only fake, it is not allowed to silently mis-handle removal.
type reconcilingCompiler struct{}

// testCompiler returns the shared reconciling test double used across this
// package's tests, including the cherry-picked hostile suite.
func testCompiler() SharedCompiler {
	return reconcilingCompiler{}
}

func (reconcilingCompiler) CompileMarkdown(_ context.Context, request MarkdownCompileRequest) (SharedCompilation, error) {
	var observedContent []byte
	if request.Target.Observed != nil {
		observedContent = request.Target.Observed.Content
	}
	blocks, err := parseMarkdownBlocks(observedContent)
	if err != nil {
		return SharedCompilation{}, err
	}
	blockByID := make(map[string]markdownBlock, len(blocks))
	for _, block := range blocks {
		blockByID[block.id] = block
	}
	desiredByID := make(map[string]MarkdownInsertion, len(request.Desired))
	for _, insertion := range request.Desired {
		desiredByID[insertion.BlockID] = insertion
	}

	managedIntact := true
	if request.Target.Previous != nil {
		previousHash := previousHashByOwner(request.Target.Previous)
		for _, insertion := range request.Desired {
			block, present := blockByID[insertion.BlockID]
			want, tracked := previousHash[ownerKey(insertion.Owner)]
			if present && tracked && want != hashContent(block.body) {
				managedIntact = false
			}
		}
	}

	var out bytes.Buffer
	var preserved [][]byte
	cursor := 0
	for _, block := range blocks {
		if block.start > cursor {
			gap := observedContent[cursor:block.start]
			out.Write(gap)
			preserved = append(preserved, append([]byte(nil), gap...))
		}
		if insertion, keep := desiredByID[block.id]; keep {
			writeMarkdownBlock(&out, insertion.BlockID, insertion.Body)
		}
		cursor = block.end
	}
	if cursor < len(observedContent) {
		gap := observedContent[cursor:]
		out.Write(gap)
		preserved = append(preserved, append([]byte(nil), gap...))
	}
	var newIDs []string
	for id := range desiredByID {
		if _, existed := blockByID[id]; !existed {
			newIDs = append(newIDs, id)
		}
	}
	sort.Strings(newIDs)
	for _, id := range newIDs {
		writeMarkdownBlock(&out, id, desiredByID[id].Body)
	}

	sortedDesired := append([]MarkdownInsertion(nil), request.Desired...)
	sort.Slice(sortedDesired, func(left, right int) bool { return sortedDesired[left].BlockID < sortedDesired[right].BlockID })
	managed := make([]ManagedResult, 0, len(sortedDesired))
	for _, insertion := range sortedDesired {
		managed = append(managed, ManagedResult{Owner: insertion.Owner, Kind: realize.ArtifactManagedBlock, ManagedHash: hashContent(insertion.Body), Identity: insertion.BlockID})
	}

	if request.Target.Observed == nil && len(request.Desired) == 0 {
		return SharedCompilation{Action: realize.ActionRemove}, nil
	}
	action := realize.ActionEnsure
	ownership := realize.OwnershipGenerated
	if request.Target.Observed != nil {
		ownership = realize.OwnershipShared
		// Demote only when the caller explicitly asked (never on the
		// compiler's own initiative) and nothing unmanaged would be left
		// behind, matching #6's demotion contract: zero unmanaged bytes,
		// intact managed hashes, exact observed hash.
		if request.Target.ExplicitDemotion && len(preserved) == 0 && managedIntact {
			ownership = realize.OwnershipGenerated
		}
	}
	if len(request.Desired) == 0 {
		action = realize.ActionRemove
		ownership = realize.OwnershipUnmanaged
	}
	proof := PreservationProof{ManagedIntact: managedIntact, PreservedContent: preserved}
	if request.Target.Observed != nil {
		proof.ObservedHash = request.Target.Observed.Hash
	}
	candidate := &CandidateFile{Path: request.Target.Path, Content: out.Bytes(), Mode: 0o644, Ownership: ownership}
	return SharedCompilation{Action: action, Candidate: candidate, Managed: managed, Proof: proof}, nil
}

func (reconcilingCompiler) CompileConfig(_ context.Context, request ConfigCompileRequest) (SharedCompilation, error) {
	if request.Format != ConfigJSON {
		return SharedCompilation{}, fmt.Errorf("test compiler only supports JSON, got %q", request.Format)
	}
	if request.Target.Observed == nil && len(request.Desired) == 0 {
		return SharedCompilation{Action: realize.ActionRemove}, nil
	}
	doc := map[string]any{}
	if request.Target.Observed != nil && len(request.Target.Observed.Content) != 0 {
		if err := json.Unmarshal(request.Target.Observed.Content, &doc); err != nil {
			return SharedCompilation{}, fmt.Errorf("parse observed config: %w", err)
		}
	}

	byContainer := make(map[string][]ConfigEntry)
	var containerOrder []string
	for _, entry := range request.Desired {
		key := strings.Join(entry.Container, "\x00")
		if _, seen := byContainer[key]; !seen {
			containerOrder = append(containerOrder, key)
		}
		byContainer[key] = append(byContainer[key], entry)
	}
	sort.Strings(containerOrder)

	managedIntact := true
	previousHash := map[string]string{}
	if request.Target.Previous != nil {
		previousHash = previousHashByOwner(request.Target.Previous)
	}
	var managed []ManagedResult
	for _, containerKey := range containerOrder {
		entries := byContainer[containerKey]
		container := entries[0].Container
		switch entries[0].Kind {
		case ConfigField:
			parent := ensureObjectAt(doc, container)
			sorted := append([]ConfigEntry(nil), entries...)
			sort.Slice(sorted, func(left, right int) bool { return sorted[left].Key < sorted[right].Key })
			for _, entry := range sorted {
				var value any
				if err := json.Unmarshal(entry.EncodedValue, &value); err != nil {
					return SharedCompilation{}, fmt.Errorf("decode entry %q: %w", entry.Key, err)
				}
				parent[entry.Key] = value
				managed = append(managed, ManagedResult{Owner: entry.Owner, Kind: realize.ArtifactStructuredEntry, ManagedHash: hashContent(entry.EncodedValue), Identity: CanonicalEntryKey(entry.Container, entry.Kind, entry.Key)})
			}
		case ConfigElement:
			intact, elementResults := reconcileConfigArray(doc, container, entries, previousHash)
			if !intact {
				managedIntact = false
			}
			managed = append(managed, elementResults...)
		default:
			return SharedCompilation{}, fmt.Errorf("test compiler: unsupported config entry kind %q", entries[0].Kind)
		}
	}
	sort.Slice(managed, func(left, right int) bool { return ownerKey(managed[left].Owner) < ownerKey(managed[right].Owner) })

	rendered, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return SharedCompilation{}, err
	}
	action := realize.ActionEnsure
	ownership := realize.OwnershipGenerated
	if request.Target.Observed != nil {
		ownership = realize.OwnershipShared
	}
	if len(request.Desired) == 0 {
		action = realize.ActionRemove
		ownership = realize.OwnershipUnmanaged
	}
	proof := PreservationProof{ManagedIntact: managedIntact}
	if request.Target.Observed != nil {
		proof.ObservedHash = request.Target.Observed.Hash
		proof.PreservedContent = [][]byte{append([]byte(nil), request.Target.Observed.Content...)}
	}
	candidate := &CandidateFile{Path: request.Target.Path, Content: append(rendered, '\n'), Mode: 0o644, Ownership: ownership}
	return SharedCompilation{Action: action, Candidate: candidate, Managed: managed, Proof: proof}, nil
}

// reconcileConfigArray locates each desired array element by the ledger's
// recorded managedHash for its owner, not by array position: an element
// still tracked by Previous is replaced wherever it currently sits (even if
// the array was reordered by something else), an untracked one is appended,
// and a tracked owner no longer in Desired has its old element dropped.
func reconcileConfigArray(doc map[string]any, container []string, desired []ConfigEntry, previousHash map[string]string) (bool, []ManagedResult) {
	raw := getAtPath(doc, container)
	array, _ := raw.([]any)

	hashAt := make(map[string]int, len(array))
	for index, element := range array {
		encoded, _ := json.Marshal(element)
		hashAt[hashContent(encoded)] = index
	}
	keep := make([]bool, len(array))
	for index := range keep {
		keep[index] = true
	}
	desiredOwners := make(map[string]struct{}, len(desired))
	for _, entry := range desired {
		desiredOwners[ownerKey(entry.Owner)] = struct{}{}
	}
	for owner, oldHash := range previousHash {
		if _, stillDesired := desiredOwners[owner]; stillDesired {
			continue
		}
		if index, found := hashAt[oldHash]; found {
			keep[index] = false
		}
	}
	rebuilt := make([]any, 0, len(array))
	for index, element := range array {
		if keep[index] {
			rebuilt = append(rebuilt, element)
		}
	}

	managedIntact := true
	managed := make([]ManagedResult, 0, len(desired))
	for _, entry := range desired {
		var value any
		if err := json.Unmarshal(entry.EncodedValue, &value); err == nil {
			replaced := false
			if oldHash, tracked := previousHash[ownerKey(entry.Owner)]; tracked {
				if index := indexByElementHash(rebuilt, oldHash); index >= 0 {
					rebuilt[index] = value
					replaced = true
				} else {
					managedIntact = false
				}
			}
			if !replaced {
				rebuilt = append(rebuilt, value)
			}
		}
		managed = append(managed, ManagedResult{Owner: entry.Owner, Kind: realize.ArtifactStructuredEntry, ManagedHash: hashContent(entry.EncodedValue), Identity: CanonicalEntryKey(entry.Container, entry.Kind, entry.Key)})
	}
	setAtPath(doc, container, rebuilt)
	return managedIntact, managed
}

func indexByElementHash(array []any, hash string) int {
	for index, element := range array {
		encoded, _ := json.Marshal(element)
		if hashContent(encoded) == hash {
			return index
		}
	}
	return -1
}

func getAtPath(doc map[string]any, path []string) any {
	current := any(doc)
	for _, segment := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[segment]
	}
	return current
}

func setAtPath(doc map[string]any, path []string, value any) {
	current := doc
	for index, segment := range path {
		if index == len(path)-1 {
			current[segment] = value
			return
		}
		next, ok := current[segment].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[segment] = next
		}
		current = next
	}
}

func ensureObjectAt(doc map[string]any, path []string) map[string]any {
	current := doc
	for _, segment := range path {
		next, ok := current[segment].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[segment] = next
		}
		current = next
	}
	return current
}

func previousHashByOwner(target *realize.Target) map[string]string {
	byOwner := make(map[string]string, len(target.Entries))
	for _, entry := range target.Entries {
		byOwner[entry.Source+"\x00"+entry.ArtifactID] = entry.ManagedHash
	}
	return byOwner
}

type markdownBlock struct {
	id         string
	start, end int
	body       []byte
}

func parseMarkdownBlocks(content []byte) ([]markdownBlock, error) {
	var blocks []markdownBlock
	offset := 0
	beginPrefix := []byte("<!-- ACR:BEGIN ")
	for {
		relative := bytes.Index(content[offset:], beginPrefix)
		if relative < 0 {
			break
		}
		start := offset + relative
		lineEnd := bytes.IndexByte(content[start:], '\n')
		if lineEnd < 0 {
			return nil, fmt.Errorf("unterminated begin marker at byte %d", start)
		}
		beginLine := string(content[start : start+lineEnd+1])
		id := strings.TrimSuffix(strings.TrimPrefix(beginLine, "<!-- ACR:BEGIN "), " -->\n")
		bodyStart := start + lineEnd + 1
		endMarker := []byte("<!-- ACR:END " + id + " -->\n")
		endRelative := bytes.Index(content[bodyStart:], endMarker)
		if endRelative < 0 {
			return nil, fmt.Errorf("block %q missing END marker", id)
		}
		bodyEnd := bodyStart + endRelative
		blockEnd := bodyEnd + len(endMarker)
		blocks = append(blocks, markdownBlock{id: id, start: start, end: blockEnd, body: append([]byte(nil), content[bodyStart:bodyEnd]...)})
		offset = blockEnd
	}
	return blocks, nil
}

func writeMarkdownBlock(out *bytes.Buffer, id string, body []byte) {
	out.WriteString("<!-- ACR:BEGIN " + id + " -->\n")
	out.Write(body)
	if len(body) == 0 || body[len(body)-1] != '\n' {
		out.WriteByte('\n')
	}
	out.WriteString("<!-- ACR:END " + id + " -->\n")
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
