package preserve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

type jsonKind byte

const (
	jsonScalar jsonKind = iota
	jsonObject
	jsonArray
)

type jsonNode struct {
	kind       jsonKind
	start      int
	end        int
	openEnd    int
	closeStart int
	members    []*jsonMember
}

type jsonMember struct {
	key         string
	start       int
	end         int
	commaBefore int
	commaAfter  int
	value       *jsonNode
	location    *configLocation
	parent      *jsonNode
	index       int
}

type jsonDocument struct {
	path       string
	content    []byte
	root       *jsonNode
	entries    []*configLocation
	containers map[string]*jsonNode
}

func parseJSONDocument(path string, content []byte, missing bool) (*jsonDocument, error) {
	document := &jsonDocument{path: path, content: append([]byte(nil), content...), containers: make(map[string]*jsonNode)}
	if missing {
		return document, nil
	}
	if len(content) == 0 {
		return nil, conflict(CodeOwnershipConflict, path, "existing zero-byte target cannot provide a preservation proof")
	}
	if !json.Valid(content) {
		return nil, conflict(CodeConfigConflict, path, "existing JSON is invalid")
	}
	parser := jsonOffsetParser{data: content, path: path}
	parser.skipWhitespace()
	root, err := parser.parseValue()
	if err != nil {
		return nil, err
	}
	parser.skipWhitespace()
	if parser.offset != len(content) {
		return nil, conflict(CodeConfigConflict, path, fmt.Sprintf("unexpected JSON bytes at offset %d", parser.offset))
	}
	document.root = root
	document.indexNode(root, nil)
	return document, nil
}

func (document *jsonDocument) locations() []*configLocation {
	return document.entries
}

func (document *jsonDocument) validateDesired(entry adapter.ConfigEntry) error {
	if !json.Valid(entry.EncodedValue) {
		return fmt.Errorf("entry %q is not exactly one valid JSON value", entry.Key)
	}
	return nil
}

func (document *jsonDocument) indexNode(node *jsonNode, container []string) {
	if node == nil {
		return
	}
	if node.kind == jsonObject || node.kind == jsonArray {
		document.containers[jsonContainerKey(container)] = node
	}
	for _, member := range node.members {
		location := &configLocation{
			container: append([]string(nil), container...), raw: append([]byte(nil), document.content[member.value.start:member.value.end]...),
			valueStart: member.value.start, valueEnd: member.value.end,
			removeStart: member.start, removeEnd: member.end, formatData: member,
		}
		if node.kind == jsonObject {
			location.kind = adapter.ConfigField
			location.key = member.key
		} else {
			location.kind = adapter.ConfigElement
		}
		member.location = location
		document.entries = append(document.entries, location)
		if node.kind == jsonObject && (member.value.kind == jsonObject || member.value.kind == jsonArray) {
			document.indexNode(member.value, append(append([]string(nil), container...), member.key))
		}
	}
}

func (document *jsonDocument) apply(desired []adapter.ConfigEntry, previous map[string]*configLocation) ([]byte, map[string][]byte, error) {
	if document.root == nil {
		return renderNewJSON(desired)
	}
	desiredByOwner := make(map[string]adapter.ConfigEntry, len(desired))
	for _, entry := range desired {
		desiredByOwner[desiredOwnerKey(entry)] = entry
	}
	removed := make(map[*jsonNode]map[int]bool)
	var edits []configEdit
	rawByIdentity := make(map[string][]byte, len(desired))
	occupied := make(map[string]*configLocation)
	for _, location := range document.entries {
		if location.kind == adapter.ConfigField {
			occupied[adapter.CanonicalEntryKey(location.container, location.kind, location.key)] = location
		}
	}
	var additions []adapter.ConfigEntry
	for owner, location := range previous {
		entry, keep := desiredByOwner[owner]
		member := location.formatData.(*jsonMember)
		if !keep {
			markJSONRemoval(removed, member)
			continue
		}
		identity := adapter.CanonicalEntryKey(entry.Container, entry.Kind, entry.Key)
		currentIdentity := adapter.CanonicalEntryKey(location.container, location.kind, location.key)
		sameLocation := identity == currentIdentity || (entry.Kind == adapter.ConfigElement && location.kind == adapter.ConfigElement && sameContainer(entry.Container, location.container))
		if sameLocation {
			raw := append([]byte(nil), entry.EncodedValue...)
			edits = append(edits, configEdit{start: location.valueStart, end: location.valueEnd, data: raw})
			rawByIdentity[identity] = raw
			delete(desiredByOwner, owner)
			continue
		}
		markJSONRemoval(removed, member)
		additions = append(additions, entry)
		delete(desiredByOwner, owner)
	}
	for _, entry := range desiredByOwner {
		additions = append(additions, entry)
	}
	for _, entry := range additions {
		identity := adapter.CanonicalEntryKey(entry.Container, entry.Kind, entry.Key)
		if entry.Kind == adapter.ConfigField {
			if existing := occupied[identity]; existing != nil && !jsonLocationRemoved(existing, removed) {
				return nil, nil, conflict(CodeConfigConflict, document.path, fmt.Sprintf("desired field %q already exists without matching ownership", entry.Key))
			}
		} else {
			for _, location := range document.entries {
				if location.kind == adapter.ConfigElement && sameContainer(location.container, entry.Container) && bytes.Equal(location.raw, entry.EncodedValue) && !jsonLocationRemoved(location, removed) {
					return nil, nil, conflict(CodeDuplicateConfigEntry, document.path, "desired array element is indistinguishable from an existing element")
				}
			}
		}
		rawByIdentity[identity] = append([]byte(nil), entry.EncodedValue...)
	}
	edits = append(edits, jsonRemovalEdits(removed)...)
	insertionEdits, err := document.jsonInsertionEdits(additions, removed)
	if err != nil {
		return nil, nil, err
	}
	edits = append(edits, insertionEdits...)
	candidate, err := applyConfigEdits(document.content, edits)
	if err != nil {
		return nil, nil, conflict(CodeConfigConflict, document.path, err.Error())
	}
	if !json.Valid(candidate) {
		return nil, nil, conflict(CodeConfigConflict, document.path, "surgical JSON edits produced an invalid document")
	}
	return candidate, rawByIdentity, nil
}

func (document *jsonDocument) unmanagedFragments(previous map[string]*configLocation) [][]byte {
	if document.root == nil {
		return nil
	}
	var fragments [][]byte
	var collect func(*jsonNode)
	collect = func(node *jsonNode) {
		for _, member := range node.members {
			if member.location.managed {
				continue
			}
			if (member.value.kind == jsonObject || member.value.kind == jsonArray) && jsonHasManagedDescendant(member.value) {
				collect(member.value)
				continue
			}
			fragments = append(fragments, append([]byte(nil), member.location.raw...))
		}
	}
	collect(document.root)
	return fragments
}

func jsonHasManagedDescendant(node *jsonNode) bool {
	for _, member := range node.members {
		if member.location != nil && member.location.managed {
			return true
		}
		if member.value != nil && jsonHasManagedDescendant(member.value) {
			return true
		}
	}
	return false
}

func markJSONRemoval(removed map[*jsonNode]map[int]bool, member *jsonMember) {
	indexes := removed[member.parent]
	if indexes == nil {
		indexes = make(map[int]bool)
		removed[member.parent] = indexes
	}
	indexes[member.index] = true
}

func jsonLocationRemoved(location *configLocation, removed map[*jsonNode]map[int]bool) bool {
	member := location.formatData.(*jsonMember)
	return removed[member.parent][member.index]
}

func jsonRemovalEdits(removed map[*jsonNode]map[int]bool) []configEdit {
	var edits []configEdit
	for parent, indexes := range removed {
		for start := 0; start < len(parent.members); {
			if !indexes[start] {
				start++
				continue
			}
			end := start
			for end+1 < len(parent.members) && indexes[end+1] {
				end++
			}
			first := parent.members[start]
			last := parent.members[end]
			removeStart := first.start
			removeEnd := last.end
			switch {
			case start == 0 && end < len(parent.members)-1:
				removeEnd = last.commaAfter + 1
			case start > 0 && end < len(parent.members)-1:
				removeStart = first.commaBefore
				removeEnd = parent.members[end+1].commaBefore
			case start > 0:
				removeStart = first.commaBefore
			}
			edits = append(edits, configEdit{start: removeStart, end: removeEnd})
			start = end + 1
		}
	}
	return edits
}

func (document *jsonDocument) jsonInsertionEdits(additions []adapter.ConfigEntry, removed map[*jsonNode]map[int]bool) ([]configEdit, error) {
	byContainer := make(map[string][]adapter.ConfigEntry)
	for _, entry := range additions {
		if document.containers[jsonContainerKey(entry.Container)] != nil {
			byContainer[jsonContainerKey(entry.Container)] = append(byContainer[jsonContainerKey(entry.Container)], entry)
			continue
		}
		prefixLength := len(entry.Container)
		for prefixLength > 0 && document.containers[jsonContainerKey(entry.Container[:prefixLength])] == nil {
			prefixLength--
		}
		container := document.containers[jsonContainerKey(entry.Container[:prefixLength])]
		if container == nil || container.kind != jsonObject || prefixLength == len(entry.Container) {
			return nil, conflict(CodeConfigConflict, document.path, fmt.Sprintf("JSON container %q does not exist beneath an object", jsonContainerKey(entry.Container)))
		}
		groupKey := jsonMissingGroupKey(entry.Container[:prefixLength], entry.Container[prefixLength])
		byContainer[groupKey] = append(byContainer[groupKey], entry)
	}
	var edits []configEdit
	var keys []string
	for key := range byContainer {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		containerKey, missingKey, isMissing := parseJSONMissingGroupKey(key)
		container := document.containers[containerKey]
		if container == nil {
			return nil, conflict(CodeConfigConflict, document.path, fmt.Sprintf("JSON container %q does not exist", key))
		}
		entries := byContainer[key]
		if isMissing {
			prefixLength := len(entries[0].Container)
			for prefixLength > 0 && jsonContainerKey(entries[0].Container[:prefixLength]) != containerKey {
				prefixLength--
			}
			raw, renderErr := renderJSONMissingValue(entries, prefixLength, missingKey)
			if renderErr != nil {
				return nil, conflict(CodeConfigConflict, document.path, renderErr.Error())
			}
			entries = []adapter.ConfigEntry{{Container: entries[0].Container[:prefixLength], Kind: adapter.ConfigField, Key: missingKey, EncodedValue: raw}}
		}
		sort.Slice(entries, func(left, right int) bool {
			return adapter.CanonicalEntryKey(entries[left].Container, entries[left].Kind, entries[left].Key) < adapter.CanonicalEntryKey(entries[right].Container, entries[right].Kind, entries[right].Key)
		})
		kept := len(container.members) - len(removed[container])
		var inserted bytes.Buffer
		for index, entry := range entries {
			if kept+index > 0 {
				inserted.WriteByte(',')
			}
			if container.kind == jsonObject {
				if entry.Kind != adapter.ConfigField {
					return nil, conflict(CodeConfigConflict, document.path, "array element requested for a JSON object container")
				}
				keyBytes, _ := json.Marshal(entry.Key)
				inserted.Write(keyBytes)
				inserted.WriteByte(':')
			} else if entry.Kind != adapter.ConfigElement {
				return nil, conflict(CodeConfigConflict, document.path, "object field requested for a JSON array container")
			}
			inserted.Write(entry.EncodedValue)
		}
		edits = append(edits, configEdit{start: container.closeStart, end: container.closeStart, data: inserted.Bytes()})
	}
	return edits, nil
}

const jsonMissingSeparator = "\x00missing\x00"

func jsonMissingGroupKey(container []string, key string) string {
	return jsonContainerKey(container) + jsonMissingSeparator + key
}

func parseJSONMissingGroupKey(key string) (string, string, bool) {
	container, missing, found := strings.Cut(key, jsonMissingSeparator)
	return container, missing, found
}

func renderJSONMissingValue(entries []adapter.ConfigEntry, prefixLength int, missingKey string) ([]byte, error) {
	root := &jsonBuildNode{kind: jsonObject, fields: make(map[string]*jsonBuildNode)}
	for _, entry := range entries {
		current := root
		container := entry.Container[prefixLength:]
		for index, segment := range container {
			next := current.fields[segment]
			if next == nil {
				nextKind := jsonObject
				if index == len(container)-1 && entry.Kind == adapter.ConfigElement {
					nextKind = jsonArray
				}
				next = &jsonBuildNode{kind: nextKind, fields: make(map[string]*jsonBuildNode)}
				current.fields[segment] = next
			}
			current = next
		}
		if entry.Kind == adapter.ConfigField {
			if current.kind != jsonObject || current.fields[entry.Key] != nil {
				return nil, fmt.Errorf("conflicting JSON field %q", entry.Key)
			}
			current.fields[entry.Key] = &jsonBuildNode{kind: jsonScalar, raw: append([]byte(nil), entry.EncodedValue...)}
		} else {
			if current.kind != jsonArray {
				return nil, fmt.Errorf("JSON element container %q is not an array", entry.Container)
			}
			current.elements = append(current.elements, entry)
		}
	}
	value := root.fields[missingKey]
	if value == nil {
		return nil, fmt.Errorf("missing JSON subtree %q was not rendered", missingKey)
	}
	var out bytes.Buffer
	renderJSONBuildNode(&out, value)
	return out.Bytes(), nil
}

func jsonContainerKey(container []string) string {
	return adapter.CanonicalEntryKey(container, "", "")
}

func sameContainer(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type jsonOffsetParser struct {
	data   []byte
	offset int
	path   string
}

func (parser *jsonOffsetParser) parseValue() (*jsonNode, error) {
	parser.skipWhitespace()
	if parser.offset >= len(parser.data) {
		return nil, conflict(CodeConfigConflict, parser.path, "unexpected end of JSON")
	}
	switch parser.data[parser.offset] {
	case '{':
		return parser.parseObject()
	case '[':
		return parser.parseArray()
	case '"':
		start := parser.offset
		if _, err := parser.parseString(); err != nil {
			return nil, err
		}
		return &jsonNode{kind: jsonScalar, start: start, end: parser.offset}, nil
	default:
		start := parser.offset
		for parser.offset < len(parser.data) && !bytes.ContainsRune([]byte(" \t\r\n,]}"), rune(parser.data[parser.offset])) {
			parser.offset++
		}
		return &jsonNode{kind: jsonScalar, start: start, end: parser.offset}, nil
	}
}

func (parser *jsonOffsetParser) parseObject() (*jsonNode, error) {
	node := &jsonNode{kind: jsonObject, start: parser.offset}
	parser.offset++
	node.openEnd = parser.offset
	parser.skipWhitespace()
	seen := make(map[string]bool)
	commaBefore := -1
	if parser.consume('}') {
		node.closeStart = parser.offset - 1
		node.end = parser.offset
		return node, nil
	}
	for {
		parser.skipWhitespace()
		start := parser.offset
		key, err := parser.parseString()
		if err != nil {
			return nil, err
		}
		if seen[key] {
			return nil, conflict(CodeDuplicateConfigEntry, parser.path, fmt.Sprintf("JSON object key %q is defined more than once", key))
		}
		seen[key] = true
		parser.skipWhitespace()
		if !parser.consume(':') {
			return nil, conflict(CodeConfigConflict, parser.path, fmt.Sprintf("object key %q has no colon", key))
		}
		value, err := parser.parseValue()
		if err != nil {
			return nil, err
		}
		member := &jsonMember{key: key, start: start, end: value.end, commaBefore: commaBefore, commaAfter: -1, value: value, parent: node, index: len(node.members)}
		node.members = append(node.members, member)
		parser.skipWhitespace()
		if parser.consume('}') {
			node.closeStart = parser.offset - 1
			node.end = parser.offset
			return node, nil
		}
		if !parser.consume(',') {
			return nil, conflict(CodeConfigConflict, parser.path, fmt.Sprintf("object key %q is not followed by a comma or close brace", key))
		}
		commaBefore = parser.offset - 1
		member.commaAfter = commaBefore
	}
}

func (parser *jsonOffsetParser) parseArray() (*jsonNode, error) {
	node := &jsonNode{kind: jsonArray, start: parser.offset}
	parser.offset++
	node.openEnd = parser.offset
	parser.skipWhitespace()
	commaBefore := -1
	if parser.consume(']') {
		node.closeStart = parser.offset - 1
		node.end = parser.offset
		return node, nil
	}
	for {
		parser.skipWhitespace()
		start := parser.offset
		value, err := parser.parseValue()
		if err != nil {
			return nil, err
		}
		member := &jsonMember{start: start, end: value.end, commaBefore: commaBefore, commaAfter: -1, value: value, parent: node, index: len(node.members)}
		node.members = append(node.members, member)
		parser.skipWhitespace()
		if parser.consume(']') {
			node.closeStart = parser.offset - 1
			node.end = parser.offset
			return node, nil
		}
		if !parser.consume(',') {
			return nil, conflict(CodeConfigConflict, parser.path, "array element is not followed by a comma or close bracket")
		}
		commaBefore = parser.offset - 1
		member.commaAfter = commaBefore
	}
}

func (parser *jsonOffsetParser) parseString() (string, error) {
	if parser.offset >= len(parser.data) || parser.data[parser.offset] != '"' {
		return "", conflict(CodeConfigConflict, parser.path, fmt.Sprintf("expected JSON string at offset %d", parser.offset))
	}
	start := parser.offset
	parser.offset++
	for parser.offset < len(parser.data) {
		switch parser.data[parser.offset] {
		case '\\':
			parser.offset += 2
		case '"':
			parser.offset++
			var decoded string
			if err := json.Unmarshal(parser.data[start:parser.offset], &decoded); err != nil {
				return "", conflict(CodeConfigConflict, parser.path, err.Error())
			}
			return decoded, nil
		default:
			parser.offset++
		}
	}
	return "", conflict(CodeConfigConflict, parser.path, "unterminated JSON string")
}

func (parser *jsonOffsetParser) skipWhitespace() {
	for parser.offset < len(parser.data) && bytes.ContainsRune([]byte(" \t\r\n"), rune(parser.data[parser.offset])) {
		parser.offset++
	}
}

func (parser *jsonOffsetParser) consume(expected byte) bool {
	if parser.offset < len(parser.data) && parser.data[parser.offset] == expected {
		parser.offset++
		return true
	}
	return false
}

type jsonBuildNode struct {
	kind     jsonKind
	fields   map[string]*jsonBuildNode
	elements []adapter.ConfigEntry
	raw      []byte
	identity string
}

func renderNewJSON(desired []adapter.ConfigEntry) ([]byte, map[string][]byte, error) {
	rootKind := jsonObject
	for _, entry := range desired {
		if len(entry.Container) == 0 && entry.Kind == adapter.ConfigElement {
			rootKind = jsonArray
		}
	}
	root := &jsonBuildNode{kind: rootKind, fields: make(map[string]*jsonBuildNode)}
	rawByIdentity := make(map[string][]byte, len(desired))
	for _, entry := range desired {
		identity := adapter.CanonicalEntryKey(entry.Container, entry.Kind, entry.Key)
		rawByIdentity[identity] = append([]byte(nil), entry.EncodedValue...)
		current := root
		for index, segment := range entry.Container {
			if current.kind != jsonObject {
				return nil, nil, fmt.Errorf("JSON container %q crosses an array", entry.Container)
			}
			next := current.fields[segment]
			if next == nil {
				nextKind := jsonObject
				if index == len(entry.Container)-1 && entry.Kind == adapter.ConfigElement {
					nextKind = jsonArray
				}
				next = &jsonBuildNode{kind: nextKind, fields: make(map[string]*jsonBuildNode)}
				current.fields[segment] = next
			}
			current = next
		}
		if entry.Kind == adapter.ConfigField {
			if current.kind != jsonObject || current.fields[entry.Key] != nil {
				return nil, nil, fmt.Errorf("conflicting JSON field %q", entry.Key)
			}
			current.fields[entry.Key] = &jsonBuildNode{kind: jsonScalar, raw: append([]byte(nil), entry.EncodedValue...), identity: identity}
		} else {
			if current.kind != jsonArray {
				return nil, nil, fmt.Errorf("JSON element container %q is not an array", entry.Container)
			}
			current.elements = append(current.elements, entry)
		}
	}
	var out bytes.Buffer
	renderJSONBuildNode(&out, root)
	out.WriteByte('\n')
	return out.Bytes(), rawByIdentity, nil
}

func renderJSONBuildNode(out *bytes.Buffer, node *jsonBuildNode) {
	switch node.kind {
	case jsonScalar:
		out.Write(node.raw)
	case jsonObject:
		out.WriteByte('{')
		keys := make([]string, 0, len(node.fields))
		for key := range node.fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for index, key := range keys {
			if index != 0 {
				out.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			out.Write(encoded)
			out.WriteByte(':')
			renderJSONBuildNode(out, node.fields[key])
		}
		out.WriteByte('}')
	case jsonArray:
		out.WriteByte('[')
		sort.Slice(node.elements, func(left, right int) bool { return node.elements[left].Key < node.elements[right].Key })
		for index, entry := range node.elements {
			if index != 0 {
				out.WriteByte(',')
			}
			out.Write(entry.EncodedValue)
		}
		out.WriteByte(']')
	}
}
