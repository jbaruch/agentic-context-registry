package preserve

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

// ForeignSelector identifies a non-ACR config entry by positive structural
// evidence. Fields bind by key; array elements bind by their exact raw value.
// Table selects one whole TOML table named by Container — its header line and
// every field it declares — because removing the fields alone would leave an
// orphan header behind; Kind, Key and Raw are unused then.
type ForeignSelector struct {
	Container []string
	Kind      adapter.ConfigEntryKind
	Key       string
	Raw       []byte
	Table     bool
}

// ForeignSplice records the byte range and identity removed from a config.
type ForeignSplice struct {
	Container []string
	Kind      adapter.ConfigEntryKind
	Key       string
	Raw       []byte
}

// EmptyForeignArray identifies a retained empty array field under a known
// foreign-owned container.
type EmptyForeignArray struct {
	Container []string
	Key       string
}

// FindEmptyForeignArrays returns empty array fields beneath one exact
// container. They are evidence worth reporting, but not enough evidence to
// delete a container that may also carry operator-owned structure.
func FindEmptyForeignArrays(format adapter.ConfigFormat, filename string, content []byte, container []string) ([]EmptyForeignArray, error) {
	document, err := parseConfigDocument(format, filename, content, false)
	if err != nil {
		return nil, err
	}
	var result []EmptyForeignArray
	for _, location := range document.locations() {
		if location.kind != adapter.ConfigField || !sameContainer(location.container, container) {
			continue
		}
		compact := bytes.Map(func(character rune) rune {
			if character == ' ' || character == '\t' || character == '\r' || character == '\n' {
				return -1
			}
			return character
		}, location.raw)
		if bytes.Equal(compact, []byte("[]")) {
			result = append(result, EmptyForeignArray{Container: append([]string(nil), location.container...), Key: location.key})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.Join(result[i].Container, "\x00")+"\x00"+result[i].Key < strings.Join(result[j].Container, "\x00")+"\x00"+result[j].Key
	})
	return result, nil
}

// FindForeignConfigElementsContaining returns exact element selectors whose
// raw value contains a required ownership literal. Container fields are not
// selected because their raw value can include unrelated descendants.
func FindForeignConfigElementsContaining(format adapter.ConfigFormat, filename string, content, literal []byte) ([]ForeignSelector, error) {
	document, err := parseConfigDocument(format, filename, content, false)
	if err != nil {
		return nil, err
	}
	var result []ForeignSelector
	for _, location := range document.locations() {
		if location.kind != adapter.ConfigElement || !bytes.Contains(location.raw, literal) {
			continue
		}
		result = append(result, ForeignSelector{Container: append([]string(nil), location.container...), Kind: location.kind, Raw: append([]byte(nil), location.raw...)})
	}
	return result, nil
}

// ConfigRetainsUnmanagedContent reports whether content still carries any
// entry, element or comment outside the supplied ACR-managed hashes.
//
// Finalization asks this after a splice. A shared target is shared because it
// held content ACR does not own; once the splice removes the last of it the
// target is wholly ACR-owned, and leaving the ledger at shared ownership makes
// every later realization refuse the merge for want of unmanaged content to
// preserve.
func ConfigRetainsUnmanagedContent(format adapter.ConfigFormat, filename string, content []byte, managedHashes []string) (bool, error) {
	document, err := parseConfigDocument(format, filename, content, false)
	if err != nil {
		return false, err
	}
	managed := make(map[string]struct{}, len(managedHashes))
	for _, digest := range managedHashes {
		managed[digest] = struct{}{}
	}
	for _, location := range document.locations() {
		digest := structuredEntryHash(format, location.container, location.kind, location.key, location.raw)
		if _, owned := managed[digest]; owned {
			location.managed = true
		}
	}
	for _, fragment := range document.unmanagedFragments(nil, nil) {
		if len(bytes.TrimSpace(fragment)) != 0 {
			return true, nil
		}
	}
	return false, nil
}

// RemoveForeignConfigEntries removes positively identified foreign entries
// with the same offset-preserving parser used for ACR ownership. A selector
// that resolves to an ACR-managed entry is always refused.
func RemoveForeignConfigEntries(format adapter.ConfigFormat, filename string, content []byte, selectors []ForeignSelector, managedHashes []string) ([]byte, []ForeignSplice, error) {
	document, err := parseConfigDocument(format, filename, content, false)
	if err != nil {
		return nil, nil, err
	}
	managed := make(map[string]struct{}, len(managedHashes))
	for _, digest := range managedHashes {
		managed[digest] = struct{}{}
	}
	locations := document.locations()
	var removed []ForeignSplice
	var tableEdits []configEdit
	used := make(map[*configLocation]struct{})
	for _, selector := range selectors {
		if selector.Table {
			start, end, fields, ok := document.tableSpan(selector.Container)
			if !ok {
				return nil, nil, fmt.Errorf("foreign config evidence did not match table %s in %q", strings.Join(selector.Container, "."), filename)
			}
			for _, field := range fields {
				digest := structuredEntryHash(format, field.container, field.kind, field.key, field.raw)
				if _, owned := managed[digest]; owned || field.managed {
					return nil, nil, fmt.Errorf("refuse to remove ACR-managed config entry %s in %q", adapter.CanonicalEntryKey(field.container, field.kind, field.key), filename)
				}
				used[field] = struct{}{}
			}
			tableEdits = append(tableEdits, configEdit{start: start, end: end})
			removed = append(removed, ForeignSplice{
				Container: append([]string(nil), selector.Container...), Kind: adapter.ConfigField,
				Raw: append([]byte(nil), content[start:end]...),
			})
			continue
		}
		var matches []*configLocation
		for _, location := range locations {
			if !sameContainer(location.container, selector.Container) || location.kind != selector.Kind {
				continue
			}
			switch selector.Kind {
			case adapter.ConfigField:
				if location.key == selector.Key {
					matches = append(matches, location)
				}
			case adapter.ConfigElement:
				if len(selector.Raw) != 0 && bytes.Equal(bytes.TrimSpace(location.raw), bytes.TrimSpace(selector.Raw)) {
					matches = append(matches, location)
				}
			}
		}
		if len(matches) == 0 {
			return nil, nil, fmt.Errorf("foreign config evidence did not match %s in %q", foreignIdentity(selector), filename)
		}
		if len(matches) != 1 {
			return nil, nil, fmt.Errorf("foreign config evidence ambiguously matched %d entries for %s in %q", len(matches), foreignIdentity(selector), filename)
		}
		location := matches[0]
		if _, duplicate := used[location]; duplicate {
			return nil, nil, fmt.Errorf("foreign config evidence repeats %s in %q", foreignIdentity(selector), filename)
		}
		used[location] = struct{}{}
		digest := structuredEntryHash(format, location.container, location.kind, location.key, location.raw)
		if _, owned := managed[digest]; owned || location.managed {
			return nil, nil, fmt.Errorf("refuse to remove ACR-managed config entry %s in %q", foreignIdentity(selector), filename)
		}
		removed = append(removed, ForeignSplice{Container: append([]string(nil), location.container...), Kind: location.kind, Key: location.key, Raw: append([]byte(nil), location.raw...)})
	}
	edits := foreignRemovalEdits(format, used, tableEdits)
	result, err := applyConfigEdits(content, edits)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(removed, func(i, j int) bool { return foreignSpliceKey(removed[i]) < foreignSpliceKey(removed[j]) })
	return result, removed, nil
}

// foreignRemovalEdits turns selected locations into byte edits. A table span
// already covers the fields inside it, so those fields are dropped from the
// per-field edit set: applyConfigEdits refuses overlapping ranges.
func foreignRemovalEdits(format adapter.ConfigFormat, locations map[*configLocation]struct{}, tables []configEdit) []configEdit {
	if len(tables) != 0 {
		remaining := make(map[*configLocation]struct{}, len(locations))
		for location := range locations {
			covered := false
			for _, table := range tables {
				if location.removeStart >= table.start && location.removeEnd <= table.end {
					covered = true
					break
				}
			}
			if !covered {
				remaining[location] = struct{}{}
			}
		}
		locations = remaining
	}
	switch format {
	case adapter.ConfigJSON:
		removed := make(map[*jsonNode]map[int]bool)
		for location := range locations {
			markJSONRemoval(removed, location.formatData.(*jsonMember))
		}
		return append(jsonRemovalEdits(removed), tables...)
	case adapter.ConfigTOML:
		fields := make(map[*tomlField]bool)
		elements := make(map[*tomlArray]map[int]bool)
		nativeGroups := make(map[*tomlNativeHookGroup]bool)
		for location := range locations {
			markTOMLRemoval(location, fields, elements, nativeGroups)
		}
		var edits []configEdit
		for field := range fields {
			edits = append(edits, configEdit{start: field.location.removeStart, end: field.location.removeEnd})
		}
		edits = append(edits, tomlArrayRemovalEdits(elements)...)
		for group := range nativeGroups {
			edits = append(edits, configEdit{start: group.start, end: group.end})
		}
		return append(edits, tables...)
	default:
		return tables
	}
}

func foreignIdentity(selector ForeignSelector) string {
	if selector.Kind == adapter.ConfigField {
		return adapter.CanonicalEntryKey(selector.Container, selector.Kind, selector.Key)
	}
	return adapter.CanonicalEntryKey(selector.Container, selector.Kind, "")
}

func foreignSpliceKey(value ForeignSplice) string {
	return adapter.CanonicalEntryKey(value.Container, value.Kind, value.Key) + "\x00" + string(value.Raw)
}
