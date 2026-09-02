package preserve

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

var bareTOMLKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

var nativeTOMLHookHeaderPattern = regexp.MustCompile(`^\[\[hooks\.([A-Za-z0-9_-]+)\]\]$`)

type tomlSection struct {
	path     []string
	insertAt int
}

type tomlField struct {
	location *configLocation
	value    *unstable.Node
	array    *tomlArray
}

type tomlArray struct {
	container  []string
	openStart  int
	closeStart int
	members    []*tomlArrayMember
}

type tomlArrayMember struct {
	location    *configLocation
	start       int
	end         int
	commaBefore int
	commaAfter  int
	parent      *tomlArray
	index       int
}

type tomlNativeHookGroup struct {
	location *configLocation
	start    int
	end      int
}

type tomlUnmanagedFragment struct {
	offset int
	raw    []byte
}

type tomlDocument struct {
	path             string
	content          []byte
	entries          []*configLocation
	fields           []*tomlField
	arrays           map[string]*tomlArray
	sections         map[string]*tomlSection
	dottedContainers map[string]bool
	comments         []tomlUnmanagedFragment
	nativeHookGroups []*tomlNativeHookGroup
	eol              []byte
	missing          bool
}

func scanNativeTOMLHookGroups(documentPath string, content []byte) ([]*tomlNativeHookGroup, error) {
	lines := markerLineSpans(content)
	var groups []*tomlNativeHookGroup
	var current *tomlNativeHookGroup
	finish := func() {
		if current == nil {
			return
		}
		current.location.raw = append([]byte(nil), content[current.start:current.end]...)
		current.location.valueStart = current.start
		current.location.valueEnd = current.end
		current.location.removeStart = current.start
		current.location.removeEnd = current.end
		current.location.formatData = current
		groups = append(groups, current)
		current = nil
	}
	for _, line := range lines {
		physical := bytes.TrimSpace(content[line.start:line.contentEnd])
		if match := nativeTOMLHookHeaderPattern.FindSubmatch(physical); match != nil {
			finish()
			current = &tomlNativeHookGroup{start: line.start, end: line.end}
			current.location = &configLocation{container: []string{"hooks", string(match[1])}, kind: adapter.ConfigElement}
			continue
		}
		if current == nil {
			continue
		}
		nestedHeader := []byte("[[hooks." + current.location.container[1] + ".hooks]]")
		if len(physical) != 0 && physical[0] == '[' && !bytes.Equal(physical, nestedHeader) {
			finish()
			continue
		}
		if len(physical) != 0 && physical[0] != '#' {
			current.end = line.end
		}
	}
	finish()
	for _, group := range groups {
		if group.end <= group.start {
			return nil, conflict(CodeConfigConflict, documentPath, "native hook array table has no body")
		}
	}
	return groups, nil
}

func nativeTOMLGroupAt(groups []*tomlNativeHookGroup, offset int) *tomlNativeHookGroup {
	for _, group := range groups {
		if offset >= group.start && offset < group.end {
			return group
		}
	}
	return nil
}

func isNativeTOMLHookEntry(entry adapter.ConfigEntry) bool {
	return entry.Representation == adapter.ConfigEntryTOMLHookTables
}

func renderNativeTOMLHookEntry(entry adapter.ConfigEntry, eol []byte) ([]byte, error) {
	if !isNativeTOMLHookEntry(entry) {
		return nil, fmt.Errorf("entry %q is not a native hook array-table element", entry.Key)
	}
	if entry.Kind != adapter.ConfigElement || len(entry.Container) != 2 || entry.Container[0] != "hooks" {
		return nil, fmt.Errorf("native hook value %q must declare a hooks event container", entry.Key)
	}
	var wrapper struct {
		Value struct {
			Hooks []struct {
				Type    string `toml:"type"`
				Command string `toml:"command"`
			} `toml:"hooks"`
		} `toml:"value"`
	}
	encoded := append([]byte("value = "), entry.EncodedValue...)
	decoder := toml.NewDecoder(bytes.NewReader(encoded)).DisallowUnknownFields()
	if err := decoder.Decode(&wrapper); err != nil {
		return nil, fmt.Errorf("decode native hook value %q: %w", entry.Key, err)
	}
	if len(wrapper.Value.Hooks) != 1 || wrapper.Value.Hooks[0].Type != "command" || wrapper.Value.Hooks[0].Command == "" {
		return nil, fmt.Errorf("native hook value %q must contain one command handler", entry.Key)
	}
	var out bytes.Buffer
	out.WriteString("[[")
	out.WriteString(encodeTOMLPath(entry.Container))
	out.WriteString("]]")
	out.Write(eol)
	out.WriteString("[[")
	out.WriteString(encodeTOMLPath(append(append([]string(nil), entry.Container...), "hooks")))
	out.WriteString("]]")
	out.Write(eol)
	out.WriteString("type = \"command\"")
	out.Write(eol)
	out.WriteString("command = ")
	out.WriteString(strconv.Quote(wrapper.Value.Hooks[0].Command))
	out.Write(eol)
	return out.Bytes(), nil
}

func parseTOMLDocument(path string, content []byte, missing bool) (*tomlDocument, error) {
	document := &tomlDocument{
		path: path, content: append([]byte(nil), content...), arrays: make(map[string]*tomlArray),
		sections: make(map[string]*tomlSection), dottedContainers: make(map[string]bool),
		eol: firstEOL(content), missing: missing,
	}
	if missing {
		return document, nil
	}
	if len(content) == 0 {
		return nil, conflict(CodeOwnershipConflict, path, "existing zero-byte target cannot provide a preservation proof")
	}
	nativeGroups, err := scanNativeTOMLHookGroups(path, content)
	if err != nil {
		return nil, err
	}
	document.nativeHookGroups = nativeGroups
	document.sections[tomlPathKey(nil)] = &tomlSection{insertAt: len(content)}
	parser := unstable.Parser{KeepComments: true}
	parserContent := maskNativeTOMLGroups(content, nativeGroups)
	parser.Reset(parserContent)
	currentTable := []string(nil)
	seenKeys := make(map[string]bool)
	seenTables := make(map[string]bool)
	var currentSection *tomlSection = document.sections[tomlPathKey(nil)]
	for parser.NextExpression() {
		expression := parser.Expression()
		if nativeTOMLGroupAt(nativeGroups, int(expression.Raw.Offset)) != nil {
			continue
		}
		switch expression.Kind {
		case unstable.Comment:
			document.comments = append(document.comments, tomlUnmanagedFragment{
				offset: int(expression.Raw.Offset), raw: append([]byte(nil), parser.Raw(expression.Raw)...),
			})
		case unstable.Table, unstable.ArrayTable:
			tablePath := tomlNodeKey(expression)
			key := tomlPathKey(tablePath)
			if seenTables[key] || tomlPathOrPrefixSeen(tablePath, seenKeys) {
				return nil, conflict(CodeDuplicateConfigEntry, path, fmt.Sprintf("TOML table %q is defined more than once", strings.Join(tablePath, ".")))
			}
			seenTables[key] = true
			headerStart := tomlLineStart(content, firstTOMLKeyOffset(expression))
			currentSection.insertAt = headerStart
			currentTable = tablePath
			currentSection = &tomlSection{path: append([]string(nil), tablePath...), insertAt: len(content)}
			document.sections[key] = currentSection
		case unstable.KeyValue:
			keyParts := tomlNodeKey(expression)
			for prefixLength := 1; prefixLength < len(keyParts); prefixLength++ {
				dotted := append(append([]string(nil), currentTable...), keyParts[:prefixLength]...)
				document.dottedContainers[tomlPathKey(dotted)] = true
			}
			fullPath := append(append([]string(nil), currentTable...), keyParts...)
			fullKey := tomlPathKey(fullPath)
			if seenKeys[fullKey] || seenTables[fullKey] || tomlStrictPrefixSeen(fullPath, seenKeys) {
				return nil, conflict(CodeDuplicateConfigEntry, path, fmt.Sprintf("TOML key %q is defined more than once", strings.Join(fullPath, ".")))
			}
			seenKeys[fullKey] = true
			value := expression.Value()
			valueStart, err := tomlAssignmentValueStart(content, expression)
			if err != nil {
				return nil, conflict(CodeConfigConflict, path, err.Error())
			}
			valueEnd := int(expression.Raw.Offset + expression.Raw.Length)
			location := &configLocation{
				container: append([]string(nil), fullPath[:len(fullPath)-1]...), kind: adapter.ConfigField,
				key: fullPath[len(fullPath)-1], raw: append([]byte(nil), content[valueStart:valueEnd]...),
				valueStart: valueStart, valueEnd: valueEnd,
				removeStart: int(expression.Raw.Offset), removeEnd: valueEnd,
			}
			field := &tomlField{location: location, value: value}
			location.formatData = field
			document.entries = append(document.entries, location)
			document.fields = append(document.fields, field)
			if value.Kind == unstable.Array {
				array, err := document.indexTOMLArray(&parser, fullPath, valueStart, valueEnd, value)
				if err != nil {
					return nil, err
				}
				field.array = array
			}
			if err := checkTOMLInlineDuplicates(&parser, value, nil, path); err != nil {
				return nil, err
			}
		}
	}
	if err := parser.Error(); err != nil {
		return nil, conflict(CodeConfigConflict, path, fmt.Sprintf("invalid TOML: %v", err))
	}
	for _, group := range nativeGroups {
		document.entries = append(document.entries, group.location)
	}
	return document, nil
}

func maskNativeTOMLGroups(content []byte, groups []*tomlNativeHookGroup) []byte {
	masked := append([]byte(nil), content...)
	for _, group := range groups {
		for index := group.start; index < group.end; index++ {
			if masked[index] != '\n' && masked[index] != '\r' {
				masked[index] = ' '
			}
		}
	}
	return masked
}

func (document *tomlDocument) locations() []*configLocation {
	return document.entries
}

func (document *tomlDocument) validateDesired(entry adapter.ConfigEntry) error {
	prefix := []byte("acr_value = ")
	wrapper := append(append(append([]byte(nil), prefix...), entry.EncodedValue...), '\n')
	parser := unstable.Parser{}
	parser.Reset(wrapper)
	if !parser.NextExpression() || parser.Expression().Kind != unstable.KeyValue {
		if parser.Error() != nil {
			return fmt.Errorf("entry %q is not a valid TOML value: %v", entry.Key, parser.Error())
		}
		return fmt.Errorf("entry %q is not a valid TOML value", entry.Key)
	}
	expression := parser.Expression()
	valueStart, err := tomlAssignmentValueStart(wrapper, expression)
	if err != nil {
		return err
	}
	valueEnd := int(expression.Raw.Offset + expression.Raw.Length)
	if valueStart != len(prefix) || valueEnd != len(prefix)+len(entry.EncodedValue) || parser.NextExpression() || parser.Error() != nil {
		return fmt.Errorf("entry %q must contain exactly one TOML value without outer whitespace", entry.Key)
	}
	return nil
}

func (document *tomlDocument) apply(desired []adapter.ConfigEntry, previous map[string]*configLocation) ([]byte, map[string][]byte, error) {
	if document.missing {
		candidate, raw, err := renderNewTOML(desired, document.eol)
		if err != nil {
			return nil, nil, conflict(CodeConfigConflict, document.path, err.Error())
		}
		return candidate, raw, nil
	}
	desiredByOwner := make(map[string]adapter.ConfigEntry, len(desired))
	for _, entry := range desired {
		desiredByOwner[desiredOwnerKey(entry)] = entry
	}
	removedFields := make(map[*tomlField]bool)
	removedElements := make(map[*tomlArray]map[int]bool)
	removedNativeGroups := make(map[*tomlNativeHookGroup]bool)
	occupied := make(map[string]*configLocation)
	for _, location := range document.entries {
		if location.kind == adapter.ConfigField {
			occupied[adapter.CanonicalEntryKey(location.container, location.kind, location.key)] = location
		}
	}
	var edits []configEdit
	var additions []adapter.ConfigEntry
	rawByIdentity := make(map[string][]byte, len(desired))
	for owner, location := range previous {
		entry, keep := desiredByOwner[owner]
		if !keep {
			markTOMLRemoval(location, removedFields, removedElements, removedNativeGroups)
			continue
		}
		identity := adapter.CanonicalEntryKey(entry.Container, entry.Kind, entry.Key)
		currentIdentity := adapter.CanonicalEntryKey(location.container, location.kind, location.key)
		sameLocation := identity == currentIdentity || (entry.Kind == adapter.ConfigElement && location.kind == adapter.ConfigElement && sameContainer(entry.Container, location.container))
		if sameLocation {
			raw := append([]byte(nil), entry.EncodedValue...)
			if _, native := location.formatData.(*tomlNativeHookGroup); native {
				var renderErr error
				raw, renderErr = renderNativeTOMLHookEntry(entry, document.eol)
				if renderErr != nil {
					return nil, nil, conflict(CodeConfigConflict, document.path, renderErr.Error())
				}
			}
			edits = append(edits, configEdit{start: location.valueStart, end: location.valueEnd, data: raw})
			rawByIdentity[identity] = raw
			delete(desiredByOwner, owner)
			continue
		}
		markTOMLRemoval(location, removedFields, removedElements, removedNativeGroups)
		additions = append(additions, entry)
		delete(desiredByOwner, owner)
	}
	for _, entry := range desiredByOwner {
		additions = append(additions, entry)
	}
	for _, entry := range additions {
		identity := adapter.CanonicalEntryKey(entry.Container, entry.Kind, entry.Key)
		if entry.Kind == adapter.ConfigField {
			if existing := occupied[identity]; existing != nil && !tomlLocationRemoved(existing, removedFields, removedElements, removedNativeGroups) {
				return nil, nil, conflict(CodeConfigConflict, document.path, fmt.Sprintf("desired TOML field %q already exists without matching ownership", entry.Key))
			}
		} else {
			for _, location := range document.entries {
				if location.kind == adapter.ConfigElement && sameContainer(location.container, entry.Container) && bytes.Equal(location.raw, entry.EncodedValue) && !tomlLocationRemoved(location, removedFields, removedElements, removedNativeGroups) {
					return nil, nil, conflict(CodeDuplicateConfigEntry, document.path, "desired TOML array element is indistinguishable from an existing element")
				}
			}
		}
		rawByIdentity[identity] = append([]byte(nil), entry.EncodedValue...)
	}
	for field := range removedFields {
		edits = append(edits, configEdit{start: field.location.removeStart, end: field.location.removeEnd})
	}
	edits = append(edits, tomlArrayRemovalEdits(removedElements)...)
	for group := range removedNativeGroups {
		edits = append(edits, configEdit{start: group.start, end: group.end})
	}
	var genericAdditions []adapter.ConfigEntry
	var nativeAdditions []adapter.ConfigEntry
	for _, entry := range additions {
		if isNativeTOMLHookEntry(entry) {
			nativeAdditions = append(nativeAdditions, entry)
		} else {
			genericAdditions = append(genericAdditions, entry)
		}
	}
	insertionEdits, err := document.tomlInsertionEdits(genericAdditions, removedElements)
	if err != nil {
		return nil, nil, err
	}
	edits = append(edits, insertionEdits...)
	if len(nativeAdditions) != 0 {
		sort.Slice(nativeAdditions, func(left, right int) bool { return nativeAdditions[left].Key < nativeAdditions[right].Key })
		var inserted bytes.Buffer
		if len(document.content) != 0 && document.content[len(document.content)-1] != '\n' {
			inserted.Write(document.eol)
		}
		for _, entry := range nativeAdditions {
			raw, renderErr := renderNativeTOMLHookEntry(entry, document.eol)
			if renderErr != nil {
				return nil, nil, conflict(CodeConfigConflict, document.path, renderErr.Error())
			}
			inserted.Write(raw)
			rawByIdentity[adapter.CanonicalEntryKey(entry.Container, entry.Kind, entry.Key)] = append([]byte(nil), raw...)
		}
		edits = append(edits, configEdit{start: len(document.content), end: len(document.content), data: inserted.Bytes()})
	}
	candidate, err := applyConfigEdits(document.content, edits)
	if err != nil {
		return nil, nil, conflict(CodeConfigConflict, document.path, err.Error())
	}
	if len(candidate) == 0 && len(desired) == 0 {
		return candidate, rawByIdentity, nil
	}
	if _, err := parseTOMLDocument(document.path, candidate, false); err != nil {
		return nil, nil, conflict(CodeConfigConflict, document.path, fmt.Sprintf("surgical TOML edits produced an invalid document: %v", err))
	}
	return candidate, rawByIdentity, nil
}

func (document *tomlDocument) unmanagedFragments(previous map[string]*configLocation, _ []adapter.ConfigEntry) [][]byte {
	if document.missing {
		return nil
	}
	var located []tomlUnmanagedFragment
	located = append(located, document.comments...)
	for _, field := range document.fields {
		if field.location.managed {
			continue
		}
		if field.array != nil {
			for _, member := range field.array.members {
				if !member.location.managed {
					located = append(located, tomlUnmanagedFragment{offset: member.location.valueStart, raw: append([]byte(nil), member.location.raw...)})
				}
			}
			continue
		}
		located = append(located, tomlUnmanagedFragment{offset: field.location.valueStart, raw: append([]byte(nil), field.location.raw...)})
	}
	for _, group := range document.nativeHookGroups {
		if !group.location.managed {
			located = append(located, tomlUnmanagedFragment{offset: group.start, raw: append([]byte(nil), group.location.raw...)})
		}
	}
	sort.SliceStable(located, func(left, right int) bool { return located[left].offset < located[right].offset })
	fragments := make([][]byte, len(located))
	for index, fragment := range located {
		fragments[index] = fragment.raw
	}
	return fragments
}

func (document *tomlDocument) indexTOMLArray(parser *unstable.Parser, container []string, start, end int, node *unstable.Node) (*tomlArray, error) {
	array := &tomlArray{container: append([]string(nil), container...), openStart: start, closeStart: end - 1}
	iterator := node.Children()
	for iterator.Next() {
		child := iterator.Node()
		if child.Kind == unstable.Comment {
			document.comments = append(document.comments, tomlUnmanagedFragment{
				offset: int(child.Raw.Offset), raw: append([]byte(nil), parser.Raw(child.Raw)...),
			})
			continue
		}
		childStart := int(child.Raw.Offset)
		childEnd := int(child.Raw.Offset + child.Raw.Length)
		if child.Kind == unstable.Array || child.Kind == unstable.InlineTable {
			var err error
			childEnd, err = tomlDelimitedValueEnd(document.content, childStart, child.Kind)
			if err != nil {
				return nil, conflict(CodeConfigConflict, document.path, err.Error())
			}
		}
		member := &tomlArrayMember{start: childStart, end: childEnd, commaBefore: -1, commaAfter: -1, parent: array, index: len(array.members)}
		location := &configLocation{
			container: append([]string(nil), container...), kind: adapter.ConfigElement,
			raw:        append([]byte(nil), document.content[childStart:childEnd]...),
			valueStart: childStart, valueEnd: childEnd, removeStart: childStart, removeEnd: childEnd,
			formatData: member,
		}
		member.location = location
		array.members = append(array.members, member)
		document.entries = append(document.entries, location)
	}
	for index, member := range array.members {
		searchEnd := array.closeStart
		if index+1 < len(array.members) {
			searchEnd = array.members[index+1].start
		}
		if comma := bytes.IndexByte(document.content[member.end:searchEnd], ','); comma >= 0 {
			member.commaAfter = member.end + comma
			if index+1 < len(array.members) {
				array.members[index+1].commaBefore = member.commaAfter
			}
		}
	}
	document.arrays[tomlPathKey(container)] = array
	return array, nil
}

func markTOMLRemoval(location *configLocation, fields map[*tomlField]bool, elements map[*tomlArray]map[int]bool, nativeGroups map[*tomlNativeHookGroup]bool) {
	switch typed := location.formatData.(type) {
	case *tomlField:
		fields[typed] = true
	case *tomlArrayMember:
		indexes := elements[typed.parent]
		if indexes == nil {
			indexes = make(map[int]bool)
			elements[typed.parent] = indexes
		}
		indexes[typed.index] = true
	case *tomlNativeHookGroup:
		nativeGroups[typed] = true
	}
}

func tomlLocationRemoved(location *configLocation, fields map[*tomlField]bool, elements map[*tomlArray]map[int]bool, nativeGroups map[*tomlNativeHookGroup]bool) bool {
	switch typed := location.formatData.(type) {
	case *tomlField:
		return fields[typed]
	case *tomlArrayMember:
		return elements[typed.parent][typed.index]
	case *tomlNativeHookGroup:
		return nativeGroups[typed]
	default:
		return false
	}
}

func tomlArrayRemovalEdits(removed map[*tomlArray]map[int]bool) []configEdit {
	var edits []configEdit
	for array, indexes := range removed {
		for start := 0; start < len(array.members); {
			if !indexes[start] {
				start++
				continue
			}
			end := start
			for end+1 < len(array.members) && indexes[end+1] {
				end++
			}
			first := array.members[start]
			last := array.members[end]
			removeStart, removeEnd := first.start, last.end
			switch {
			case start == 0 && end < len(array.members)-1:
				removeEnd = last.commaAfter + 1
			case start > 0 && end < len(array.members)-1:
				removeStart = first.commaBefore
				removeEnd = array.members[end+1].commaBefore
			case start > 0:
				removeStart = first.commaBefore
				if last.commaAfter >= 0 {
					removeEnd = last.commaAfter + 1
				}
			case start == 0 && end == len(array.members)-1 && last.commaAfter >= 0:
				removeEnd = last.commaAfter + 1
			}
			edits = append(edits, configEdit{start: removeStart, end: removeEnd})
			start = end + 1
		}
	}
	return edits
}

func (document *tomlDocument) tomlInsertionEdits(additions []adapter.ConfigEntry, removedElements map[*tomlArray]map[int]bool) ([]configEdit, error) {
	fieldsByTable := make(map[string][]adapter.ConfigEntry)
	elementsByArray := make(map[string][]adapter.ConfigEntry)
	for _, entry := range additions {
		if entry.Kind == adapter.ConfigField {
			fieldsByTable[tomlPathKey(entry.Container)] = append(fieldsByTable[tomlPathKey(entry.Container)], entry)
		} else {
			elementsByArray[tomlPathKey(entry.Container)] = append(elementsByArray[tomlPathKey(entry.Container)], entry)
		}
	}
	insertions := make(map[int]*bytes.Buffer)
	var missingTables [][]string
	var fieldTableKeys []string
	for tableKey := range fieldsByTable {
		fieldTableKeys = append(fieldTableKeys, tableKey)
	}
	sort.Strings(fieldTableKeys)
	for _, tableKey := range fieldTableKeys {
		entries := fieldsByTable[tableKey]
		section := document.sections[tableKey]
		if section == nil {
			if document.dottedContainers[tableKey] {
				return nil, conflict(CodeConfigConflict, document.path, fmt.Sprintf("TOML table %q exists only through dotted keys; add an explicit table before inserting managed fields", strings.Join(entries[0].Container, ".")))
			}
			missingTables = append(missingTables, append([]string(nil), entries[0].Container...))
			continue
		}
		sort.Slice(entries, func(left, right int) bool { return entries[left].Key < entries[right].Key })
		var inserted bytes.Buffer
		if section.insertAt > 0 && document.content[section.insertAt-1] != '\n' {
			inserted.Write(document.eol)
		}
		for _, entry := range entries {
			inserted.WriteString(encodeTOMLKey(entry.Key))
			inserted.WriteString(" = ")
			inserted.Write(entry.EncodedValue)
			inserted.Write(document.eol)
		}
		appendTOMLInsertion(insertions, section.insertAt, inserted.Bytes())
	}
	if len(missingTables) != 0 {
		sort.Slice(missingTables, func(left, right int) bool {
			return tomlPathKey(missingTables[left]) < tomlPathKey(missingTables[right])
		})
		var inserted bytes.Buffer
		if len(document.content) != 0 && document.content[len(document.content)-1] != '\n' {
			inserted.Write(document.eol)
		}
		for _, table := range missingTables {
			entries := fieldsByTable[tomlPathKey(table)]
			sort.Slice(entries, func(left, right int) bool { return entries[left].Key < entries[right].Key })
			inserted.WriteByte('[')
			inserted.WriteString(encodeTOMLPath(table))
			inserted.WriteByte(']')
			inserted.Write(document.eol)
			for _, entry := range entries {
				inserted.WriteString(encodeTOMLKey(entry.Key))
				inserted.WriteString(" = ")
				inserted.Write(entry.EncodedValue)
				inserted.Write(document.eol)
			}
		}
		appendTOMLInsertion(insertions, len(document.content), inserted.Bytes())
	}
	var arrayKeys []string
	for arrayKey := range elementsByArray {
		arrayKeys = append(arrayKeys, arrayKey)
	}
	sort.Strings(arrayKeys)
	for _, arrayKey := range arrayKeys {
		entries := elementsByArray[arrayKey]
		array := document.arrays[arrayKey]
		if array == nil {
			return nil, conflict(CodeConfigConflict, document.path, fmt.Sprintf("TOML array %q does not exist", strings.Join(entries[0].Container, ".")))
		}
		sort.Slice(entries, func(left, right int) bool { return entries[left].Key < entries[right].Key })
		kept := len(array.members) - len(removedElements[array])
		keptTrailingComma := kept > 0 && !removedElements[array][len(array.members)-1] && array.members[len(array.members)-1].commaAfter >= 0
		var inserted bytes.Buffer
		for index, entry := range entries {
			if kept+index > 0 && !(index == 0 && keptTrailingComma) {
				inserted.WriteString(", ")
			}
			inserted.Write(entry.EncodedValue)
		}
		appendTOMLInsertion(insertions, array.closeStart, inserted.Bytes())
	}
	positions := make([]int, 0, len(insertions))
	for position := range insertions {
		positions = append(positions, position)
	}
	sort.Ints(positions)
	edits := make([]configEdit, 0, len(positions))
	for _, position := range positions {
		edits = append(edits, configEdit{start: position, end: position, data: insertions[position].Bytes()})
	}
	return edits, nil
}

func appendTOMLInsertion(insertions map[int]*bytes.Buffer, position int, content []byte) {
	buffer := insertions[position]
	if buffer == nil {
		buffer = &bytes.Buffer{}
		insertions[position] = buffer
	}
	buffer.Write(content)
}

func renderNewTOML(desired []adapter.ConfigEntry, eol []byte) ([]byte, map[string][]byte, error) {
	fieldsByTable := make(map[string][]adapter.ConfigEntry)
	tablePaths := make(map[string][]string)
	arrayEntries := make(map[string][]adapter.ConfigEntry)
	var nativeHooks []adapter.ConfigEntry
	for _, entry := range desired {
		if isNativeTOMLHookEntry(entry) {
			nativeHooks = append(nativeHooks, entry)
			continue
		}
		if entry.Kind == adapter.ConfigField {
			key := tomlPathKey(entry.Container)
			fieldsByTable[key] = append(fieldsByTable[key], entry)
			tablePaths[key] = append([]string(nil), entry.Container...)
		} else {
			if len(entry.Container) == 0 {
				return nil, nil, fmt.Errorf("root TOML array elements require a named container")
			}
			table := entry.Container[:len(entry.Container)-1]
			key := tomlPathKey(table)
			arrayEntries[tomlPathKey(entry.Container)] = append(arrayEntries[tomlPathKey(entry.Container)], entry)
			tablePaths[key] = append([]string(nil), table...)
		}
	}
	var tableKeys []string
	for key := range tablePaths {
		tableKeys = append(tableKeys, key)
	}
	sort.Strings(tableKeys)
	rawByIdentity := make(map[string][]byte, len(desired))
	var out bytes.Buffer
	for tableIndex, tableKey := range tableKeys {
		table := tablePaths[tableKey]
		if len(table) != 0 {
			if tableIndex != 0 || out.Len() != 0 {
				out.Write(eol)
			}
			out.WriteByte('[')
			out.WriteString(encodeTOMLPath(table))
			out.WriteByte(']')
			out.Write(eol)
		}
		fields := fieldsByTable[tableKey]
		sort.Slice(fields, func(left, right int) bool { return fields[left].Key < fields[right].Key })
		for _, entry := range fields {
			out.WriteString(encodeTOMLKey(entry.Key))
			out.WriteString(" = ")
			out.Write(entry.EncodedValue)
			out.Write(eol)
			rawByIdentity[adapter.CanonicalEntryKey(entry.Container, entry.Kind, entry.Key)] = append([]byte(nil), entry.EncodedValue...)
		}
		var arrays []string
		for arrayKey, entries := range arrayEntries {
			if tomlPathKey(entries[0].Container[:len(entries[0].Container)-1]) == tableKey {
				arrays = append(arrays, arrayKey)
			}
		}
		sort.Strings(arrays)
		for _, arrayKey := range arrays {
			entries := arrayEntries[arrayKey]
			sort.Slice(entries, func(left, right int) bool { return entries[left].Key < entries[right].Key })
			out.WriteString(encodeTOMLKey(entries[0].Container[len(entries[0].Container)-1]))
			out.WriteString(" = [")
			for index, entry := range entries {
				if index != 0 {
					out.WriteString(", ")
				}
				out.Write(entry.EncodedValue)
				rawByIdentity[adapter.CanonicalEntryKey(entry.Container, entry.Kind, entry.Key)] = append([]byte(nil), entry.EncodedValue...)
			}
			out.WriteByte(']')
			out.Write(eol)
		}
	}
	sort.Slice(nativeHooks, func(left, right int) bool { return nativeHooks[left].Key < nativeHooks[right].Key })
	for _, entry := range nativeHooks {
		raw, err := renderNativeTOMLHookEntry(entry, eol)
		if err != nil {
			return nil, nil, err
		}
		if out.Len() != 0 && out.Bytes()[out.Len()-1] != '\n' {
			out.Write(eol)
		}
		out.Write(raw)
		rawByIdentity[adapter.CanonicalEntryKey(entry.Container, entry.Kind, entry.Key)] = append([]byte(nil), raw...)
	}
	return out.Bytes(), rawByIdentity, nil
}

func tomlNodeKey(node *unstable.Node) []string {
	iterator := node.Key()
	var result []string
	for iterator.Next() {
		result = append(result, string(iterator.Node().Data))
	}
	return result
}

func firstTOMLKeyOffset(node *unstable.Node) int {
	iterator := node.Key()
	if iterator.Next() {
		return int(iterator.Node().Raw.Offset)
	}
	return 0
}

func tomlLineStart(content []byte, offset int) int {
	if offset > len(content) {
		offset = len(content)
	}
	if newline := bytes.LastIndexByte(content[:offset], '\n'); newline >= 0 {
		return newline + 1
	}
	return 0
}

func tomlAssignmentValueStart(content []byte, expression *unstable.Node) (int, error) {
	start := int(expression.Raw.Offset)
	end := int(expression.Raw.Offset + expression.Raw.Length)
	quote := byte(0)
	escaped := false
	for offset := start; offset < end; offset++ {
		character := content[offset]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if quote == '"' && character == '\\' {
				escaped = true
				continue
			}
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if character == '=' {
			offset++
			for offset < end && (content[offset] == ' ' || content[offset] == '\t') {
				offset++
			}
			return offset, nil
		}
	}
	return 0, fmt.Errorf("TOML key/value expression at offset %d has no assignment", start)
}

func tomlPathKey(path []string) string {
	return adapter.CanonicalEntryKey(path, "", "")
}

func tomlPathOrPrefixSeen(path []string, seen map[string]bool) bool {
	for length := 1; length <= len(path); length++ {
		if seen[tomlPathKey(path[:length])] {
			return true
		}
	}
	return false
}

func tomlStrictPrefixSeen(path []string, seen map[string]bool) bool {
	for length := 1; length < len(path); length++ {
		if seen[tomlPathKey(path[:length])] {
			return true
		}
	}
	return false
}

func encodeTOMLPath(path []string) string {
	encoded := make([]string, len(path))
	for index, segment := range path {
		encoded[index] = encodeTOMLKey(segment)
	}
	return strings.Join(encoded, ".")
}

func encodeTOMLKey(key string) string {
	if bareTOMLKeyPattern.MatchString(key) {
		return key
	}
	return strconv.Quote(key)
}

func tomlDelimitedValueEnd(content []byte, start int, kind unstable.Kind) (int, error) {
	open, close := byte('['), byte(']')
	if kind == unstable.InlineTable {
		open, close = '{', '}'
	}
	if start >= len(content) || content[start] != open {
		return 0, fmt.Errorf("TOML container at offset %d has no opening delimiter", start)
	}
	depth := 0
	quote := byte(0)
	escaped := false
	comment := false
	for offset := start; offset < len(content); offset++ {
		character := content[offset]
		if comment {
			if character == '\n' {
				comment = false
			}
			continue
		}
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if quote == '"' && character == '\\' {
				escaped = true
				continue
			}
			if character == quote {
				quote = 0
			}
			continue
		}
		switch character {
		case '#':
			comment = true
		case '\'', '"':
			quote = character
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return offset + 1, nil
			}
		}
	}
	return 0, fmt.Errorf("TOML container at offset %d is not closed", start)
}

func checkTOMLInlineDuplicates(parser *unstable.Parser, node *unstable.Node, prefix []string, path string) error {
	if node == nil {
		return nil
	}
	if node.Kind == unstable.Array {
		iterator := node.Children()
		for iterator.Next() {
			if err := checkTOMLInlineDuplicates(parser, iterator.Node(), prefix, path); err != nil {
				return err
			}
		}
		return nil
	}
	if node.Kind != unstable.InlineTable {
		return nil
	}
	seen := make(map[string]bool)
	iterator := node.Children()
	for iterator.Next() {
		child := iterator.Node()
		if child.Kind != unstable.KeyValue {
			continue
		}
		key := append(append([]string(nil), prefix...), tomlNodeKey(child)...)
		encoded := tomlPathKey(key)
		if seen[encoded] {
			return conflict(CodeDuplicateConfigEntry, path, fmt.Sprintf("TOML inline key %q is defined more than once", strings.Join(key, ".")))
		}
		seen[encoded] = true
		if err := checkTOMLInlineDuplicates(parser, child.Value(), key, path); err != nil {
			return err
		}
	}
	return nil
}
