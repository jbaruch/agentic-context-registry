package adapter

import (
	"context"
	"io/fs"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

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

// TargetOptions carries coordinator/user-facing, per-target options for
// shared compilation: ExplicitDemotion, requesting that a target move back
// to generated-only ownership; Force, per the SharedCompiler contract (it
// cannot weaken ownership or preservation checks); and ConfigFormat, the
// trusted structured-config encoding for a target compileOutputs must
// revisit with no current adapter output at all (Desired empty). A target's
// on-disk file extension is not trustworthy evidence of its format — a
// caller that already compiled the target once knows the real format and
// must supply it here for a later run that revisits it with nothing left
// contributing; compileOutputs fails closed rather than guessing from the
// path. Callers pass TargetOptions into Coordinator.Realize/compileOutputs
// keyed by native target path; it is never derived from adapter output.
type TargetOptions struct {
	ExplicitDemotion bool
	Force            bool
	ConfigFormat     ConfigFormat
}

// SharedTarget is the trusted state of one native target compileOutputs asks
// a SharedCompiler to reconcile. Observed and Previous are nil when the
// native file or the ledger target is absent, respectively. ExplicitDemotion
// and Force are coordinator/user options compileOutputs sets from its own
// caller-facing surface (TargetOptions), never from adapter-supplied data.
type SharedTarget struct {
	Path             string
	Observed         *ObservedFile
	Previous         *realize.Target
	ExplicitDemotion bool
	Force            bool
}

// MarkdownCompileRequest is the input to SharedCompiler.CompileMarkdown.
// Desired is the complete set of managed-block insertions every currently
// selected adapter wants for Target this run; it is empty when a target
// previously owned via Markdown has no current contributor, so the compiler
// can express a safe partial or final removal.
type MarkdownCompileRequest struct {
	Target  SharedTarget
	Desired []MarkdownInsertion
}

// ConfigCompileRequest is the input to SharedCompiler.CompileConfig. Desired
// is empty under the same no-current-contributor condition as
// MarkdownCompileRequest.Desired.
type ConfigCompileRequest struct {
	Target  SharedTarget
	Format  ConfigFormat
	Desired []ConfigEntry
}

// ManagedResult is one ledger entry a SharedCompilation leaves owned after
// reconciling Previous against Desired. compileOutputs stamps Adapter and
// AdapterVersion onto the realize.Entry it builds from this; the compiler
// itself never sets adapter identity.
type ManagedResult struct {
	Owner       OwnerRef
	Kind        realize.ArtifactKind
	ManagedHash string
}

// PreservationProof is a SharedCompiler's evidence that a merge or removal
// is safe. compileOutputs copies it verbatim onto realize.Intent's
// ObservedHash, ManagedIntact, and PreservedContent; adapters never see this
// type and cannot set those authority fields themselves.
type PreservationProof struct {
	ObservedHash     string
	ManagedIntact    bool
	PreservedContent [][]byte
}

// Notice is one non-fatal, caller-facing diagnostic a SharedCompilation
// returns alongside its result (for example, a promotion that now requires
// a Git commit).
type Notice struct {
	Code    string
	Path    string
	Message string
}

// SharedCompilation is a SharedCompiler's complete, trusted answer for one
// target. Candidate is nil only for a proven whole-target removal (nothing
// remains to preserve); every other Action carries a Candidate compileOutputs
// copies onto the resulting realize.Intent.
type SharedCompilation struct {
	Action    realize.IntentAction
	Candidate *CandidateFile
	Managed   []ManagedResult
	Proof     PreservationProof
	Notices   []Notice
}

// SharedCompiler reconciles adapter-desired Markdown include bodies and
// structural config entries against the previously owned ledger entries and
// the observed native file, and proves the result safe. Issue #6 supplies
// the production, preservation-aware implementation; this package defines
// only the request/response seam and its fail-closed guard when no compiler
// is registered. The compiler receives trusted snapshot/ledger state, not
// adapter assertions, and alone computes every PreservationProof field;
// adapters cannot supply a candidate document, hashes, intactness,
// preservation fragments, demotion, or force.
type SharedCompiler interface {
	CompileMarkdown(ctx context.Context, request MarkdownCompileRequest) (SharedCompilation, error)
	CompileConfig(ctx context.Context, request ConfigCompileRequest) (SharedCompilation, error)
}
