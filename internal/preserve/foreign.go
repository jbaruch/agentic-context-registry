package preserve

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

// ForeignSelector identifies a non-ACR config entry by positive structural
// evidence. Fields bind by key; array elements bind by their exact raw value.
type ForeignSelector struct {
	Container []string
	Kind      adapter.ConfigEntryKind
	Key       string
	Raw       []byte
}

// ForeignSplice records the byte range and identity removed from a config.
type ForeignSplice struct {
	Container []string
	Kind      adapter.ConfigEntryKind
	Key       string
	Raw       []byte
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
	var edits []configEdit
	var removed []ForeignSplice
	used := make(map[*configLocation]struct{})
	for _, selector := range selectors {
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
		edits = append(edits, configEdit{start: location.removeStart, end: location.removeEnd})
		removed = append(removed, ForeignSplice{Container: append([]string(nil), location.container...), Kind: location.kind, Key: location.key, Raw: append([]byte(nil), location.raw...)})
	}
	result, err := applyConfigEdits(content, edits)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(removed, func(i, j int) bool { return foreignSpliceKey(removed[i]) < foreignSpliceKey(removed[j]) })
	return result, removed, nil
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
