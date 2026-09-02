package preserve

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const (
	CodeDuplicateConfigEntry = "duplicate_config_entry"
	CodeConfigConflict       = "config_conflict"
)

type configLocation struct {
	container   []string
	kind        adapter.ConfigEntryKind
	key         string
	raw         []byte
	valueStart  int
	valueEnd    int
	removeStart int
	removeEnd   int
	ownerKey    string
	managed     bool
	formatData  any
}

type configEdit struct {
	start int
	end   int
	data  []byte
}

type configDocument interface {
	locations() []*configLocation
	validateDesired(adapter.ConfigEntry) error
	apply(desired []adapter.ConfigEntry, previous map[string]*configLocation) ([]byte, map[string][]byte, error)
	unmanagedFragments(previous map[string]*configLocation, desired []adapter.ConfigEntry) [][]byte
}

func compileConfig(request adapter.ConfigCompileRequest) (adapter.SharedCompilation, error) {
	if request.Format != adapter.ConfigJSON && request.Format != adapter.ConfigTOML {
		return adapter.SharedCompilation{}, conflict(CodeConfigConflict, request.Target.Path, fmt.Sprintf("unsupported config format %q", request.Format))
	}
	if err := validateDesiredConfig(request.Target.Path, request.Format, request.Desired); err != nil {
		return adapter.SharedCompilation{}, err
	}
	content := []byte(nil)
	if request.Target.Observed != nil {
		content = request.Target.Observed.Content
	}
	document, err := parseConfigDocument(request.Format, request.Target.Path, content, request.Target.Observed == nil)
	if err != nil {
		return adapter.SharedCompilation{}, err
	}
	previous, err := bindConfigOwnership(request.Target, request.Format, document.locations())
	if err != nil {
		return adapter.SharedCompilation{}, err
	}
	for _, desired := range request.Desired {
		if err := document.validateDesired(desired); err != nil {
			return adapter.SharedCompilation{}, conflict(CodeConfigConflict, request.Target.Path, err.Error())
		}
	}
	candidate, rawByIdentity, err := document.apply(request.Desired, previous)
	if err != nil {
		return adapter.SharedCompilation{}, err
	}

	unmanaged := document.unmanagedFragments(previous, request.Desired)
	managedIntact := true
	if request.Target.Previous != nil && request.Target.Previous.Ownership == realize.OwnershipGenerated &&
		request.Target.Observed != nil && request.Target.Observed.Hash != request.Target.Previous.OutputHash && !nonEmptyFragments(unmanaged) {
		managedIntact = false
	}
	ownership, promoted, err := classifyTarget(request.Target, unmanaged)
	if err != nil {
		return adapter.SharedCompilation{}, err
	}
	proofFragments := unmanaged
	if ownership == realize.OwnershipGenerated {
		proofFragments = nil
	}
	if err := verifyPreservedOrder(candidate, proofFragments); err != nil {
		return adapter.SharedCompilation{}, conflict(CodeConfigConflict, request.Target.Path, err.Error())
	}

	managed := make([]adapter.ManagedResult, 0, len(request.Desired))
	for _, entry := range request.Desired {
		identity := adapter.CanonicalEntryKey(entry.Container, entry.Kind, entry.Key)
		raw, exists := rawByIdentity[identity]
		if !exists {
			return adapter.SharedCompilation{}, conflict(CodeConfigConflict, request.Target.Path, fmt.Sprintf("compiled config entry %q has no owned raw value", identity))
		}
		managed = append(managed, adapter.ManagedResult{
			Owner: entry.Owner, Kind: realize.ArtifactStructuredEntry,
			ManagedHash: structuredEntryHash(request.Format, entry.Container, entry.Kind, entry.Key, raw),
			Identity:    identity,
		})
	}
	sort.Slice(managed, func(left, right int) bool { return managed[left].Identity < managed[right].Identity })

	action := realize.ActionEnsure
	if len(request.Desired) == 0 && request.Target.Previous != nil {
		action = realize.ActionRemove
	}
	proof := adapter.PreservationProof{ManagedIntact: managedIntact, PreservedContent: cloneFragments(proofFragments)}
	mode := fs.FileMode(0o644)
	if request.Target.Observed != nil {
		proof.ObservedHash = request.Target.Observed.Hash
		mode = request.Target.Observed.Mode.Perm()
	}
	candidateOwnership := ownership
	if action == realize.ActionRemove {
		candidateOwnership = realize.OwnershipUnmanaged
	}
	var candidateFile *adapter.CandidateFile
	if action != realize.ActionRemove || len(candidate) != 0 {
		candidateFile = &adapter.CandidateFile{Path: request.Target.Path, Content: candidate, Mode: mode, Ownership: candidateOwnership}
	}
	compilation := adapter.SharedCompilation{Action: action, Candidate: candidateFile, Managed: managed, Proof: proof}
	if promoted && action == realize.ActionEnsure {
		compilation.Notices = []adapter.Notice{{
			Code: "shared_file_requires_commit", Path: request.Target.Path,
			Message: "commit the now-authoritative shared file; ACR did not stage it",
		}}
	}
	return compilation, nil
}

func validateDesiredConfig(path string, format adapter.ConfigFormat, desired []adapter.ConfigEntry) error {
	seen := make(map[string]bool, len(desired))
	seenOwners := make(map[string]bool, len(desired))
	elementValues := make(map[string]bool)
	for _, entry := range desired {
		if entry.AdapterID == "" {
			return conflict(CodeConfigConflict, path, "structured entry has no trusted adapter identity")
		}
		if entry.Kind != adapter.ConfigField && entry.Kind != adapter.ConfigElement {
			return conflict(CodeConfigConflict, path, fmt.Sprintf("structured entry %q has unsupported kind %q", entry.Key, entry.Kind))
		}
		switch entry.Representation {
		case "":
		case adapter.ConfigEntryTOMLHookTables:
			if format != adapter.ConfigTOML || entry.Kind != adapter.ConfigElement {
				return conflict(CodeConfigConflict, path, fmt.Sprintf("structured entry %q declares TOML hook tables for an incompatible format or kind", entry.Key))
			}
		default:
			return conflict(CodeConfigConflict, path, fmt.Sprintf("structured entry %q has unsupported representation %q", entry.Key, entry.Representation))
		}
		identity := adapter.CanonicalEntryKey(entry.Container, entry.Kind, entry.Key)
		if seen[identity] {
			return conflict(CodeDuplicateConfigEntry, path, fmt.Sprintf("desired structured entry %q occurs more than once", identity))
		}
		seen[identity] = true
		owner := desiredOwnerKey(entry)
		if seenOwners[owner] {
			return conflict(CodeDuplicateConfigEntry, path, "one adapter repeats ownership for the same package artifact")
		}
		seenOwners[owner] = true
		if entry.Kind == adapter.ConfigElement {
			valueKey := fmt.Sprintf("%s\x00%s\x00%s", format, adapter.CanonicalEntryKey(entry.Container, entry.Kind, ""), entry.EncodedValue)
			if elementValues[valueKey] {
				return conflict(CodeDuplicateConfigEntry, path, "identical managed array elements in one container are ambiguous")
			}
			elementValues[valueKey] = true
		}
	}
	return nil
}

func parseConfigDocument(format adapter.ConfigFormat, path string, content []byte, missing bool) (configDocument, error) {
	switch format {
	case adapter.ConfigJSON:
		return parseJSONDocument(path, content, missing)
	case adapter.ConfigTOML:
		return parseTOMLDocument(path, content, missing)
	default:
		return nil, conflict(CodeConfigConflict, path, fmt.Sprintf("unsupported config format %q", format))
	}
}

func bindConfigOwnership(target adapter.SharedTarget, format adapter.ConfigFormat, locations []*configLocation) (map[string]*configLocation, error) {
	if target.Previous == nil {
		return nil, nil
	}
	previous := make(map[string]*configLocation, len(target.Previous.Entries))
	for _, entry := range target.Previous.Entries {
		if entry.ArtifactKind != realize.ArtifactStructuredEntry {
			return nil, conflict(CodeConfigConflict, target.Path, fmt.Sprintf("ledger entry %s is not a structured config entry", entry.ArtifactID))
		}
		var matches []*configLocation
		for _, location := range locations {
			if structuredEntryHash(format, location.container, location.kind, location.key, location.raw) == entry.ManagedHash {
				matches = append(matches, location)
			}
		}
		if len(matches) == 0 {
			return nil, conflict(CodeConfigConflict, target.Path, fmt.Sprintf("ledger-owned structured entry %s was edited or removed", entry.ArtifactID))
		}
		if len(matches) > 1 {
			return nil, conflict(CodeDuplicateConfigEntry, target.Path, fmt.Sprintf("more than one current entry matches managed hash %s", entry.ManagedHash))
		}
		ownerKey := configOwnerKey(entry.Source, entry.ArtifactID, entry.Adapter)
		matches[0].managed = true
		matches[0].ownerKey = ownerKey
		previous[ownerKey] = matches[0]
	}
	return previous, nil
}

func configOwnerKey(source, artifactID, adapterID string) string {
	return source + "\x00" + artifactID + "\x00" + adapterID
}

func desiredOwnerKey(entry adapter.ConfigEntry) string {
	return configOwnerKey(entry.Owner.Source, entry.Owner.ArtifactID, entry.AdapterID)
}

func structuredEntryHash(format adapter.ConfigFormat, container []string, kind adapter.ConfigEntryKind, key string, raw []byte) string {
	var payload bytes.Buffer
	payload.WriteString("acr-config-entry-v1\x00")
	writeHashPart(&payload, []byte(format))
	var containerCount [8]byte
	binary.BigEndian.PutUint64(containerCount[:], uint64(len(container)))
	payload.Write(containerCount[:])
	for _, segment := range container {
		writeHashPart(&payload, []byte(segment))
	}
	writeHashPart(&payload, []byte(kind))
	if kind == adapter.ConfigField {
		writeHashPart(&payload, []byte(key))
	}
	writeHashPart(&payload, raw)
	return hashBytes(payload.Bytes())
}

func writeHashPart(buffer *bytes.Buffer, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	buffer.Write(length[:])
	buffer.Write(value)
}

func applyConfigEdits(content []byte, edits []configEdit) ([]byte, error) {
	sort.Slice(edits, func(left, right int) bool {
		if edits[left].start != edits[right].start {
			return edits[left].start > edits[right].start
		}
		return edits[left].end > edits[right].end
	})
	result := append([]byte(nil), content...)
	lastStart := len(content) + 1
	for _, edit := range edits {
		if edit.start < 0 || edit.end < edit.start || edit.end > len(content) || edit.end > lastStart {
			return nil, fmt.Errorf("overlapping or invalid config edit [%d:%d]", edit.start, edit.end)
		}
		result = append(result[:edit.start], append(append([]byte(nil), edit.data...), result[edit.end:]...)...)
		lastStart = edit.start
	}
	return result, nil
}
