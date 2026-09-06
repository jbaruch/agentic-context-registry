package migrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

// TesslMCPKey is the only server-map key finalization will ever retire. Name
// alone never authorises deletion — see canonicalTesslMCP — but a differently
// named user server is never even a candidate, whatever command it runs.
const TesslMCPKey = "tessl"

// MCP entry dispositions.
const (
	// MCPCanonical is an entry that matches Tessl's published shape exactly
	// and may be retired.
	MCPCanonical = "canonical"
	// MCPAmbiguous is an entry keyed tessl whose shape ACR cannot prove.
	MCPAmbiguous = "ambiguous"
	// MCPForeign is a server ACR does not own and never inspects further.
	MCPForeign = "foreign"
)

// MCP reason codes. Each names the field that failed the predicate, never its
// value.
const (
	reasonMCPCommandPath  = "command-is-not-the-bare-name"
	reasonMCPArgs         = "args-are-not-mcp-start"
	reasonMCPType         = "type-is-not-stdio"
	reasonMCPExtraKeys    = "entry-carries-extra-keys"
	reasonMCPNotAnObject  = "entry-is-not-an-object"
	reasonMCPUnsupported  = "unsupported-agent-config"
	reasonMCPMalformed    = "malformed-config"
	reasonMCPRetiredShape = "canonical-tessl-entry"
	reasonMCPEntryAbsent  = "entry-absent-since-inventory"
	reasonMCPEntryChanged = "entry-changed-since-inventory"
)

// mcpRetirementConfigs are the supported agents' server maps, and the only
// files finalization ever edits for MCP. It is deliberately a mutation list,
// not derived from mcpPaths, which is a detection list: splicing an
// unsupported agent's config would edit a surface ACR does not own and cannot
// replace.
var mcpRetirementConfigs = []struct {
	path      string
	format    adapter.ConfigFormat
	container []string
}{
	{path: ".mcp.json", format: adapter.ConfigJSON, container: []string{"mcpServers"}},
	{path: ".cursor/mcp.json", format: adapter.ConfigJSON, container: []string{"mcpServers"}},
	{path: ".codex/config.toml", format: adapter.ConfigTOML, container: []string{"mcp_servers"}},
}

// MCPEntry is one classified MCP server entry. It carries identifiers, field
// names and parser coordinates only: no command string, no argument value, no
// environment value ever reaches this struct, because a server entry can hold
// a credential and this record is printed and serialized into the JSON
// envelope. Detail is a sanitized parse position for a malformed config.
type MCPEntry struct {
	Path        string   `json:"path"`
	Container   string   `json:"container"`
	Key         string   `json:"key"`
	Fields      []string `json:"fields,omitempty"`
	Digest      string   `json:"digest,omitempty"`
	Disposition string   `json:"disposition"`
	Reason      string   `json:"reason,omitempty"`
	Detail      string   `json:"detail,omitempty"`
}

// MCPParseError reports a config ACR could not read. Detail names the file and
// the parser's position, never a source line.
type MCPParseError struct {
	Path   string
	Detail string
}

func (err *MCPParseError) Error() string {
	return fmt.Sprintf("parse %s: %s", err.Path, err.Detail)
}

// classifyMCPEntries records the Tessl MCP integration in every supported
// agent's config. An unsupported agent's config is recorded as untouchable
// evidence and never parsed for retirement.
func classifyMCPEntries(snapshot adapter.Snapshot, report *Report) error {
	for _, config := range mcpRetirementConfigs {
		content, present, err := readOptional(snapshot, config.path)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		entry, found, err := classifyOneMCPConfig(config.path, config.format, config.container, content)
		if err != nil {
			var parseErr *MCPParseError
			if !errors.As(err, &parseErr) {
				return err
			}
			report.MCP = append(report.MCP, MCPEntry{
				Path: config.path, Container: strings.Join(config.container, "."), Key: TesslMCPKey,
				Disposition: MCPAmbiguous, Reason: reasonMCPMalformed, Detail: parseErr.Detail,
			})
			continue
		}
		if found {
			report.MCP = append(report.MCP, entry)
		}
	}
	for _, filename := range mcpPaths {
		if isRetirementConfig(filename) {
			continue
		}
		_, present, err := readOptional(snapshot, filename)
		if err != nil {
			return err
		}
		if present {
			report.MCP = append(report.MCP, MCPEntry{
				Path: filename, Disposition: MCPForeign, Reason: reasonMCPUnsupported,
			})
		}
	}
	sort.Slice(report.MCP, func(left, right int) bool {
		if report.MCP[left].Path != report.MCP[right].Path {
			return report.MCP[left].Path < report.MCP[right].Path
		}
		return report.MCP[left].Key < report.MCP[right].Key
	})
	return nil
}

// MCPRetirementContract is one supported agent's server map: the only files
// finalization edits for MCP.
type MCPRetirementContract struct {
	Path      string
	Format    adapter.ConfigFormat
	Container []string
}

// VerifyCanonicalMCPEntry re-classifies the Tessl server entry in content and
// reports whether it is still exactly the canonical object recorded under
// digest.
//
// Inventory and planning read the file separately, and planning's bytes are
// what the transaction accepts as its before-image. Without this check a user
// could replace the canonical object with a wrapper command or an env block
// between the two reads and have finalization delete it as proven Tessl
// evidence. reason names the field that differs, never its value.
func VerifyCanonicalMCPEntry(contract MCPRetirementContract, content []byte, digest string) (bool, string, error) {
	entry, found, err := classifyOneMCPConfig(contract.Path, contract.Format, contract.Container, content)
	if err != nil {
		return false, reasonMCPMalformed, err
	}
	if !found {
		return false, reasonMCPEntryAbsent, nil
	}
	if entry.Disposition != MCPCanonical {
		return false, entry.Reason, nil
	}
	if entry.Digest != digest {
		return false, reasonMCPEntryChanged, nil
	}
	return true, "", nil
}

// MCPRetirementConfig returns the retirement contract for one config path.
// A path with no contract is never edited.
func MCPRetirementConfig(filename string) (MCPRetirementContract, bool) {
	for _, config := range mcpRetirementConfigs {
		if config.path == filename {
			return MCPRetirementContract{Path: config.path, Format: config.format, Container: append([]string(nil), config.container...)}, true
		}
	}
	return MCPRetirementContract{}, false
}

func isRetirementConfig(filename string) bool {
	for _, config := range mcpRetirementConfigs {
		if config.path == filename {
			return true
		}
	}
	return false
}

// classifyOneMCPConfig decodes one supported config and classifies its tessl
// server entry, if it has one.
func classifyOneMCPConfig(filename string, format adapter.ConfigFormat, container []string, content []byte) (MCPEntry, bool, error) {
	document, err := decodeMCPDocument(filename, format, content)
	if err != nil {
		return MCPEntry{}, false, err
	}
	servers, ok := nestedObject(document, container)
	if !ok {
		return MCPEntry{}, false, nil
	}
	value, present := servers[TesslMCPKey]
	if !present {
		return MCPEntry{}, false, nil
	}
	entry := MCPEntry{Path: filename, Container: strings.Join(container, "."), Key: TesslMCPKey}
	object, isObject := value.(map[string]any)
	if !isObject {
		entry.Disposition = MCPAmbiguous
		entry.Reason = reasonMCPNotAnObject
		return entry, true, nil
	}
	entry.Fields = objectFields(object)
	digest, err := canonicalMCPDigest(object)
	if err != nil {
		return MCPEntry{}, false, err
	}
	entry.Digest = digest
	if reason := canonicalTesslMCP(object); reason != "" {
		entry.Disposition = MCPAmbiguous
		entry.Reason = reason
		return entry, true, nil
	}
	entry.Disposition = MCPCanonical
	entry.Reason = reasonMCPRetiredShape
	return entry, true, nil
}

// canonicalTesslMCP returns "" when the entry is exactly the object real Tessl
// writes, and otherwise the reason code naming the field that differs. It
// reports the classification of command, never the command string: a wrapper
// command can carry a credential.
//
// The shape is verified from Tessl 0.105.0 output for Claude Code, Codex and
// Cursor: {"type":"stdio","command":"tessl","args":["mcp","start"]}, with no
// other key in any of the three.
func canonicalTesslMCP(object map[string]any) string {
	kind, ok := object["type"].(string)
	if !ok || kind != "stdio" {
		return reasonMCPType
	}
	command, ok := object["command"].(string)
	if !ok || command != "tessl" {
		return reasonMCPCommandPath
	}
	arguments, ok := object["args"].([]any)
	if !ok || len(arguments) != 2 {
		return reasonMCPArgs
	}
	first, firstOK := arguments[0].(string)
	second, secondOK := arguments[1].(string)
	if !firstOK || !secondOK || first != "mcp" || second != "start" {
		return reasonMCPArgs
	}
	if len(object) != 3 {
		return reasonMCPExtraKeys
	}
	return ""
}

// canonicalMCPDigest hashes a deterministic serialization of the whole entry.
// It is the only handle on an entry's content ACR ever emits: an operator can
// compare two digests without ACR rendering a token, a wrapper command or an
// environment value anywhere.
func canonicalMCPDigest(object map[string]any) (string, error) {
	encoded, err := json.Marshal(sortedJSONValue(object))
	if err != nil {
		return "", fmt.Errorf("hash MCP entry: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// sortedJSONValue rewrites maps into ordered key/value pairs so the digest is
// independent of decode order.
func sortedJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		pairs := make([][2]any, 0, len(keys))
		for _, key := range keys {
			pairs = append(pairs, [2]any{key, sortedJSONValue(typed[key])})
		}
		return pairs
	case []any:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, sortedJSONValue(item))
		}
		return items
	default:
		return value
	}
}

func objectFields(object map[string]any) []string {
	fields := make([]string, 0, len(object))
	for key := range object {
		fields = append(fields, key)
	}
	sort.Strings(fields)
	return fields
}

func decodeMCPDocument(filename string, format adapter.ConfigFormat, content []byte) (map[string]any, error) {
	var document map[string]any
	switch format {
	case adapter.ConfigJSON:
		decoder := json.NewDecoder(bytes.NewReader(content))
		if err := decoder.Decode(&document); err != nil {
			return nil, &MCPParseError{Path: filename, Detail: jsonParseDetail(err)}
		}
		// One document, then end of file. Decoding the first value and
		// stopping accepts trailing garbage, and a client that cannot read
		// its own config is exactly the state finalization must refuse — with
		// or without a Tessl member in that first object.
		if err := requireJSONEOF(decoder, content); err != nil {
			return nil, &MCPParseError{Path: filename, Detail: err.Error()}
		}
	case adapter.ConfigTOML:
		if err := toml.Unmarshal(content, &document); err != nil {
			return nil, &MCPParseError{Path: filename, Detail: tomlParseDetail(err)}
		}
	default:
		return nil, fmt.Errorf("unsupported MCP config format %q for %q", format, filename)
	}
	return document, nil
}

// requireJSONEOF rejects any value or non-whitespace byte after the config's
// single document. It reports the offset of the offending byte, never its
// content.
func requireJSONEOF(decoder *json.Decoder, content []byte) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	offset := decoder.InputOffset()
	if err == nil {
		return fmt.Errorf("trailing JSON value at byte offset %d; the config must hold one document", offset)
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		offset = syntax.Offset
	}
	if offset < 0 || offset > int64(len(content)) {
		offset = int64(len(content))
	}
	return fmt.Errorf("trailing content at byte offset %d; the config must hold one document", offset)
}

// jsonParseDetail reports the decoder's own position without echoing the
// source text it failed on.
func jsonParseDetail(err error) string {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Sprintf("invalid JSON at byte offset %d", syntax.Offset)
	}
	var typed *json.UnmarshalTypeError
	if errors.As(err, &typed) {
		return fmt.Sprintf("unexpected JSON value at byte offset %d", typed.Offset)
	}
	return "invalid JSON document"
}

// tomlParseDetail reports the decoder's row and column. go-toml's Error()
// embeds the offending source line, so only the position is taken.
func tomlParseDetail(err error) string {
	var decodeErr *toml.DecodeError
	if !errors.As(err, &decodeErr) {
		return "invalid TOML document"
	}
	row, column := decodeErr.Position()
	return fmt.Sprintf("invalid TOML at line %d, column %d", row, column)
}

func nestedObject(document map[string]any, container []string) (map[string]any, bool) {
	current := document
	for _, key := range container {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}
