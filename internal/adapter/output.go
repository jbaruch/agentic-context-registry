package adapter

import "io/fs"

// OutputKind selects which single payload field on Output is populated.
type OutputKind string

const (
	OutputMarkdownInclude OutputKind = "markdown-include"
	OutputConfigMerge     OutputKind = "config-merge"
	OutputGeneratedFile   OutputKind = "generated-file"
)

// Output is one adapter-rendered native target. Exactly one payload field
// matching Kind is populated; compileOutputs rejects any other shape.
type Output struct {
	Target   string
	Mode     fs.FileMode
	Kind     OutputKind
	Markdown []MarkdownInsertion
	Config   *ConfigMerge
	File     *GeneratedFile
}

// MarkdownInsertion is one adapter-supplied include block body. The adapter
// never supplies or reconstructs the host document; compileOutputs derives
// the merged content and preservation proof from a registered SharedCompiler.
type MarkdownInsertion struct {
	Owner   OwnerRef
	BlockID string
	Body    []byte
}

// ConfigFormat is the on-disk structured-configuration encoding.
type ConfigFormat string

const (
	ConfigJSON ConfigFormat = "json"
	ConfigTOML ConfigFormat = "toml"
)

// ConfigMerge is a set of adapter-supplied structural entries. The adapter
// never supplies the whole document; compileOutputs derives the merged
// content and preservation proof from a registered SharedCompiler.
type ConfigMerge struct {
	Format  ConfigFormat
	Entries []ConfigEntry
}

// ConfigEntryKind selects whether a ConfigEntry targets an object field or an
// array element.
type ConfigEntryKind string

const (
	ConfigField   ConfigEntryKind = "field"
	ConfigElement ConfigEntryKind = "element"
)

// ConfigEntry is one structural write into a structured-configuration
// document. (Container, Kind, Key) is unique across every adapter rendering
// the same target. EncodedValue is exactly one JSON/TOML value; only the
// trusted SharedCompiler parses it. For an array element, Key is an internal
// ownership key: the compiler locates the previous element by its ledger
// managedHash, never by array position.
type ConfigEntry struct {
	Owner        OwnerRef
	Container    []string
	Kind         ConfigEntryKind
	Key          string
	EncodedValue []byte
}

// GeneratedFile is a whole native file body. Content is valid only for a
// missing target or an unchanged, ledger-proven generated-only target;
// compileOutputs rejects it for any shared target.
type GeneratedFile struct {
	Owner   OwnerRef
	Content []byte
}

// MergedDocument is a SharedCompiler's proof of one safe merge into an
// observed native file. compileOutputs derives realize.Intent's
// ObservedHash, ManagedIntact, and PreservedContent from it; adapters never
// see this type and cannot set those authority fields themselves.
type MergedDocument struct {
	Content       []byte
	ManagedIntact bool
	Preserved     [][]byte
}

// SharedCompiler merges adapter-supplied Markdown include bodies and
// structural config entries into an observed native file. Issue #6 supplies
// the production, preservation-aware implementation; this package defines
// only the seam and its fail-closed guard when no compiler is registered.
type SharedCompiler interface {
	MergeMarkdown(observed ObservedFile, exists bool, insertions []MarkdownInsertion) (MergedDocument, error)
	MergeConfig(observed ObservedFile, exists bool, format ConfigFormat, entries []ConfigEntry) (MergedDocument, error)
}
