// Package realize plans and applies adapter-rendered artifacts transactionally.
package realize

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	// CurrentLedgerSchemaVersion is the newest ownership-ledger format ACR
	// writes. It is not the only version ACR reads: see SupportedLedgerSchemaVersions.
	CurrentLedgerSchemaVersion = 2
	// BaselineLedgerSchemaVersion is written when the ledger owns no
	// coordinator target, so a project that never gains a shared surface
	// stays readable by every older ACR.
	BaselineLedgerSchemaVersion = 1
	// SharedSurfaceLedgerSchemaVersion is the first ledger format that records
	// the target-level owner discriminator the shared skill surface needs.
	SharedSurfaceLedgerSchemaVersion = 2
	// LedgerKey is the registry.lock property containing realization ownership.
	LedgerKey = "realization"
	// OwnerCoordinator marks a target the coordinator compiles directly rather
	// than any single agent adapter. It is the shared skill surface's owner.
	OwnerCoordinator = "coordinator"
	// SharedSurfaceRoot is the shared skill surface a generic consumer reads.
	SharedSurfaceRoot = ".agents/skills"
	// SharedSurfacePrefix is the reserved ACR-owned entry prefix inside it.
	SharedSurfacePrefix = "acr__"
)

// SupportedLedgerSchemaVersions is the set of ownership-ledger formats this
// ACR reads. A ledger written before the shared surface existed loads
// unchanged; the target-level owner is absent and defaults to the per-adapter
// meaning.
func SupportedLedgerSchemaVersions() []int {
	return []int{BaselineLedgerSchemaVersion, SharedSurfaceLedgerSchemaVersion}
}

var (
	ledgerSourcePattern = regexp.MustCompile(`^(?:github|vendor):[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?/[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$`)
	artifactIDPattern   = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	adapterIDPattern    = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
)

// Ownership describes how ACR-managed and user-managed content compose a target.
type Ownership string

const (
	OwnershipGenerated Ownership = "generated-only"
	OwnershipShared    Ownership = "shared"
	OwnershipUnmanaged Ownership = "unmanaged"
)

// ArtifactKind identifies the ownership granularity supplied by an adapter.
type ArtifactKind string

const (
	ArtifactFile            ArtifactKind = "file"
	ArtifactManagedBlock    ArtifactKind = "managed-block"
	ArtifactStructuredEntry ArtifactKind = "structured-entry"
)

// Entry records one source artifact owned inside a realized target.
type Entry struct {
	Source         string       `yaml:"source" json:"source"`
	ArtifactID     string       `yaml:"artifactId" json:"artifactId"`
	ArtifactKind   ArtifactKind `yaml:"artifactKind" json:"artifactKind"`
	SourcePath     string       `yaml:"sourcePath" json:"sourcePath"`
	Adapter        string       `yaml:"adapter" json:"adapter"`
	AdapterVersion string       `yaml:"adapterVersion" json:"adapterVersion"`
	ManagedHash    string       `yaml:"managedHash" json:"managedHash"`
}

// Target records ACR ownership of one project-relative realized file. Owner is
// empty for the ordinary per-adapter target whose entries name the owning
// agents, and OwnerCoordinator for a shared-surface target the coordinator
// compiles for every selected agent at once.
type Target struct {
	Path       string    `yaml:"path" json:"path"`
	Owner      string    `yaml:"owner,omitempty" json:"owner,omitempty"`
	Mode       uint32    `yaml:"mode" json:"mode"`
	Ownership  Ownership `yaml:"ownership" json:"ownership"`
	OutputHash string    `yaml:"outputHash" json:"outputHash"`
	Excluded   bool      `yaml:"excluded,omitempty" json:"excluded,omitempty"`
	Entries    []Entry   `yaml:"entries" json:"entries"`
}

// Ledger is persisted under realization in .agents/registry.lock.
type Ledger struct {
	SchemaVersion int      `yaml:"schemaVersion" json:"schemaVersion"`
	Targets       []Target `yaml:"targets,omitempty" json:"targets,omitempty"`
}

// IntentAction selects whether a target is ensured, explicitly preserved, or removed.
type IntentAction string

const (
	ActionEnsure   IntentAction = "ensure"
	ActionPreserve IntentAction = "preserve"
	ActionRemove   IntentAction = "remove"
)

// Intent is a rendered desired target. Owner is empty for adapter-rendered
// targets and OwnerCoordinator for the shared skill surface. ObservedHash binds a merge to
// the exact current file inspected by the adapter. PreservedContent lists
// unmanaged byte sequences from that observed file that must survive the
// rendered result.
type Intent struct {
	Action           IntentAction
	Path             string
	Owner            string
	Content          []byte
	Mode             uint32
	Ownership        Ownership
	Entries          []Entry
	ObservedHash     string
	ManagedIntact    bool
	ExplicitDemotion bool
	PreservedContent [][]byte
}

// OperationKind is a reviewable realization action.
type OperationKind string

const (
	OperationCreate   OperationKind = "create"
	OperationUpdate   OperationKind = "update"
	OperationMerge    OperationKind = "merge"
	OperationPreserve OperationKind = "preserve"
	OperationPromote  OperationKind = "promote"
	OperationDemote   OperationKind = "demote"
	OperationConflict OperationKind = "conflict"
	OperationRemove   OperationKind = "remove"
)

// Operation describes one filesystem, ownership, or Git-exclusion change.
type Operation struct {
	Kind            OperationKind `json:"kind"`
	Path            string        `json:"path"`
	OwnershipBefore Ownership     `json:"ownershipBefore,omitempty"`
	OwnershipAfter  Ownership     `json:"ownershipAfter,omitempty"`
	BeforeHash      string        `json:"beforeHash,omitempty"`
	AfterHash       string        `json:"afterHash,omitempty"`
	Reason          string        `json:"reason,omitempty"`
	GitExclusion    bool          `json:"gitExclusion,omitempty"`
	Mode            uint32        `json:"mode,omitempty"`
	content         []byte
	remove          bool
	beforeExists    bool
	beforeMode      uint32
	physicalRoot    string
	physicalPath    string
	stateFile       bool
}

// Plan is deterministic and safe to render for dry-run output. Content is kept
// private so JSON output reports changes without leaking generated file bodies.
type Plan struct {
	Operations       []Operation       `json:"operations"`
	NextLedger       Ledger            `json:"ledger"`
	LedgerChanged    bool              `json:"ledgerChanged"`
	TransactionNotes []TransactionNote `json:"transactionNotes,omitempty"`
}

// TransactionNote reports recoverability residue that does not block a
// read-only plan.
type TransactionNote struct {
	Code string `json:"code"`
	Path string `json:"path"`
}

// HasChanges reports whether applying the plan would alter files or the ledger.
func (plan Plan) HasChanges() bool {
	if plan.LedgerChanged {
		return true
	}
	for _, operation := range plan.Operations {
		if operation.Kind != OperationPreserve {
			return true
		}
	}
	return false
}

// HasConflicts reports whether any operation blocks application.
func (plan Plan) HasConflicts() bool {
	for _, operation := range plan.Operations {
		if operation.Kind == OperationConflict {
			return true
		}
	}
	return false
}

// ConflictError reports all paths that require human or adapter intervention.
type ConflictError struct {
	Operations []Operation
}

func (err *ConflictError) Error() string {
	paths := make([]string, 0, len(err.Operations))
	for _, operation := range err.Operations {
		paths = append(paths, operation.Path+": "+operation.Reason)
	}
	return "realization conflicts: " + strings.Join(paths, "; ")
}

// ChangesError is returned by check mode when a conflict-free plan is non-empty.
type ChangesError struct {
	Plan Plan
}

// maxReportedChangePaths bounds the paths one refusal names. A drifted project
// can plan hundreds of operations, and a diagnostic nobody reads to the end is
// no more actionable than a bare count.
const maxReportedChangePaths = 5

func (err *ChangesError) Error() string {
	changes := 0
	if err.Plan.LedgerChanged {
		changes++
	}
	paths := make([]string, 0, len(err.Plan.Operations))
	for _, operation := range err.Plan.Operations {
		if operation.Kind != OperationPreserve {
			changes++
			paths = append(paths, operation.Path)
		}
	}
	message := fmt.Sprintf("realization has %d unapplied change(s)", changes)
	if len(paths) == 0 {
		return message
	}
	sort.Strings(paths)
	named := paths
	suffix := ""
	if len(named) > maxReportedChangePaths {
		named = named[:maxReportedChangePaths]
		suffix = fmt.Sprintf(" and %d more", len(paths)-maxReportedChangePaths)
	}
	// The ledger is one of the counted changes and has no path, so a mixed
	// drift that named only the files would report a count larger than the
	// list it is followed by.
	if err.Plan.LedgerChanged {
		suffix += " and the ownership ledger"
	}
	return message + " for " + strings.Join(named, ", ") + suffix
}

// DecodeLedger validates the realization value decoded from registry.lock.
func DecodeLedger(value map[string]any) (Ledger, error) {
	if value == nil {
		return Ledger{SchemaVersion: BaselineLedgerSchemaVersion}, nil
	}
	encoded, err := yaml.Marshal(value)
	if err != nil {
		return Ledger{}, fmt.Errorf("encode realization ledger: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(encoded))
	decoder.KnownFields(true)
	var ledger Ledger
	if err := decoder.Decode(&ledger); err != nil {
		return Ledger{}, fmt.Errorf("decode realization ledger: %w; delete the invalid realization property and run 'acr realize'", err)
	}
	if err := ValidateLedger(ledger); err != nil {
		return Ledger{}, err
	}
	return canonicalLedger(ledger), nil
}

// EncodeLedger returns a generic mapping suitable for registry.lock.
func EncodeLedger(ledger Ledger) (map[string]any, error) {
	ledger = canonicalLedger(ledger)
	if err := ValidateLedger(ledger); err != nil {
		return nil, err
	}
	encoded, err := yaml.Marshal(ledger)
	if err != nil {
		return nil, fmt.Errorf("encode realization ledger: %w", err)
	}
	var result map[string]any
	if err := yaml.Unmarshal(encoded, &result); err != nil {
		return nil, fmt.Errorf("convert realization ledger: %w", err)
	}
	return result, nil
}

// MergeLedgers combines two ledgers owning disjoint targets into one
// canonical, validated ledger. It is how an --agent subset invocation carries
// the omitted agents' ownership through: the planner compares against the
// selected agents' targets alone, and the merge restores the rest before the
// ledger is persisted. A target recorded in both inputs is an error, never a
// silent winner.
func MergeLedgers(base, carried Ledger) (Ledger, error) {
	merged := Ledger{
		SchemaVersion: CurrentLedgerSchemaVersion,
		Targets:       make([]Target, 0, len(base.Targets)+len(carried.Targets)),
	}
	seen := make(map[string]struct{}, cap(merged.Targets))
	for _, ledger := range [2]Ledger{base, carried} {
		for _, target := range ledger.Targets {
			if _, exists := seen[target.Path]; exists {
				return Ledger{}, fmt.Errorf("realization target %q is recorded in both merged ownership ledgers; regenerate the ownership ledger", target.Path)
			}
			seen[target.Path] = struct{}{}
			merged.Targets = append(merged.Targets, target)
		}
	}
	merged = canonicalLedger(merged)
	if err := ValidateLedger(merged); err != nil {
		return Ledger{}, err
	}
	return merged, nil
}

// ValidateLedger checks persisted ownership metadata before it can authorize writes.
func ValidateLedger(ledger Ledger) error {
	if ledger.SchemaVersion != BaselineLedgerSchemaVersion && ledger.SchemaVersion != SharedSurfaceLedgerSchemaVersion {
		return fmt.Errorf("unsupported realization schemaVersion %d; use schemaVersion 1 or 2, or regenerate the lockfile — schemaVersion %d is written by ACR versions that own the shared skill surface", ledger.SchemaVersion, SharedSurfaceLedgerSchemaVersion)
	}
	if required := requiredLedgerSchemaVersion(ledger); ledger.SchemaVersion < required {
		return fmt.Errorf("realization records a coordinator-owned target under schemaVersion %d, which has no target owner; use schemaVersion %d so an older ACR refuses the ledger instead of realizing the shared surface as an adapter target", ledger.SchemaVersion, required)
	}
	seenTargets := make(map[string]struct{}, len(ledger.Targets))
	for index, target := range ledger.Targets {
		if err := ValidateRealizationPath(target.Path); err != nil {
			return fmt.Errorf("realization.targets[%d].path: %w", index, err)
		}
		if err := validateTargetOwner(target); err != nil {
			return fmt.Errorf("realization.targets[%d]: %w", index, err)
		}
		if _, exists := seenTargets[target.Path]; exists {
			return fmt.Errorf("realization target %q is recorded more than once; regenerate the ownership ledger", target.Path)
		}
		seenTargets[target.Path] = struct{}{}
		if target.Mode == 0 || target.Mode > 0o777 {
			return fmt.Errorf("realization target %q has invalid mode %04o; use file permission bits only", target.Path, target.Mode)
		}
		if target.Ownership != OwnershipGenerated && target.Ownership != OwnershipShared {
			return fmt.Errorf("realization target %q has unsupported ownership %q", target.Path, target.Ownership)
		}
		if target.Ownership == OwnershipShared && target.Excluded {
			return fmt.Errorf("shared realization target %q cannot be locally Git-excluded; regenerate the ownership ledger", target.Path)
		}
		if !validHash(target.OutputHash) {
			return fmt.Errorf("realization target %q has invalid output hash %q", target.Path, target.OutputHash)
		}
		if len(target.Entries) == 0 {
			return fmt.Errorf("realization target %q has no owned entries; remove the unmanaged target from the ledger", target.Path)
		}
		seenEntries := make(map[string]struct{}, len(target.Entries))
		for entryIndex, entry := range target.Entries {
			if err := validateEntry(entry); err != nil {
				return fmt.Errorf("realization target %q entries[%d]: %w", target.Path, entryIndex, err)
			}
			key := entry.Source + "\x00" + entry.ArtifactID + "\x00" + entry.Adapter
			if _, exists := seenEntries[key]; exists {
				return fmt.Errorf("realization target %q repeats ownership entry %s/%s/%s", target.Path, entry.Source, entry.ArtifactID, entry.Adapter)
			}
			seenEntries[key] = struct{}{}
		}
	}
	return nil
}

func validateEntry(entry Entry) error {
	if entry.Source == "" || entry.ArtifactID == "" || entry.SourcePath == "" || entry.Adapter == "" || entry.AdapterVersion == "" {
		return errors.New("source, artifactId, sourcePath, adapter, and adapterVersion are required")
	}
	if !ledgerSourcePattern.MatchString(entry.Source) {
		return fmt.Errorf("source %q must use canonical github:owner/repository or vendor:workspace/package syntax", entry.Source)
	}
	if !artifactIDPattern.MatchString(entry.ArtifactID) {
		return fmt.Errorf("artifactId %q must be lowercase kebab-case", entry.ArtifactID)
	}
	if !adapterIDPattern.MatchString(entry.Adapter) {
		return fmt.Errorf("adapter %q must be lowercase kebab-case", entry.Adapter)
	}
	switch entry.ArtifactKind {
	case ArtifactFile, ArtifactManagedBlock, ArtifactStructuredEntry:
	default:
		return fmt.Errorf("unsupported artifactKind %q", entry.ArtifactKind)
	}
	if err := validateRelativePath(entry.SourcePath); err != nil {
		return fmt.Errorf("sourcePath: %w", err)
	}
	if !validHash(entry.ManagedHash) {
		return fmt.Errorf("invalid managedHash %q", entry.ManagedHash)
	}
	return nil
}

// ValidateTargetPath checks that target is a normalized, project-relative
// path outside every reserved project-state location (agents.yaml,
// .agents/**, .git/**). It is exported so callers upstream of the engine —
// such as internal/adapter's compileOutputs — can reject the same paths
// before ever constructing an Intent, as defense in depth alongside the
// engine's own authoritative enforcement.
func ValidateTargetPath(target string) error {
	if err := validateRelativePath(target); err != nil {
		return err
	}
	if target == "agents.yaml" || target == ".agents" || strings.HasPrefix(target, ".agents/") || target == ".git" || strings.HasPrefix(target, ".git/") {
		return fmt.Errorf("reserved project state path %q cannot be an adapter target", target)
	}
	return nil
}

// validateTargetOwner binds the target-level owner to the surface it may
// occupy. A coordinator-owned target lives only on the shared skill surface,
// and no adapter-owned target may occupy it: the discriminator and the path
// prove the same thing, so a ledger that disagrees with itself is refused
// rather than resolved.
func validateTargetOwner(target Target) error {
	shared := ValidateSharedSurfacePath(target.Path) == nil
	switch target.Owner {
	case "":
		if shared {
			return fmt.Errorf("target %q is on the shared skill surface but records no owner; regenerate the ownership ledger", target.Path)
		}
		return nil
	case OwnerCoordinator:
		if !shared {
			return fmt.Errorf("coordinator-owned target %q is outside %s/%s*; regenerate the ownership ledger", target.Path, SharedSurfaceRoot, SharedSurfacePrefix)
		}
		return nil
	default:
		return fmt.Errorf("target %q has unsupported owner %q; use %q or leave it unset", target.Path, target.Owner, OwnerCoordinator)
	}
}

// requiredLedgerSchemaVersion reports the oldest format that can express this
// ledger's state, following the graded precedent in internal/dependency: a
// project that never gains a shared surface never gains a version-2 ledger.
func requiredLedgerSchemaVersion(ledger Ledger) int {
	for _, target := range ledger.Targets {
		if target.Owner != "" {
			return SharedSurfaceLedgerSchemaVersion
		}
	}
	return BaselineLedgerSchemaVersion
}

// ValidateSharedSurfacePath accepts only an ACR-owned entry beneath the shared
// skill surface. It is deliberately separate from ValidateTargetPath, which
// keeps rejecting every .agents path: an adapter still cannot name one, so
// .agents/registry.lock, .agents/vendor/** and .agents/.acr-transactions/**
// stay unreachable from every producer.
func ValidateSharedSurfacePath(target string) error {
	if err := validateRelativePath(target); err != nil {
		return err
	}
	rest, inside := strings.CutPrefix(target, SharedSurfaceRoot+"/")
	if !inside {
		return fmt.Errorf("shared skill surface path %q must start with %s/", target, SharedSurfaceRoot)
	}
	entry, _, _ := strings.Cut(rest, "/")
	if !strings.HasPrefix(entry, SharedSurfacePrefix) || entry == SharedSurfacePrefix {
		return fmt.Errorf("shared skill surface path %q must name an %s entry", target, SharedSurfacePrefix)
	}
	return nil
}

// ValidateRealizationPath accepts every path the realization engine may own:
// an ordinary adapter target, or a coordinator-owned shared-surface entry. The
// engine's ledger, planner, transaction and journal checks use it; the adapter
// boundary keeps using the stricter ValidateTargetPath.
func ValidateRealizationPath(target string) error {
	if err := validateRelativePath(target); err != nil {
		return err
	}
	if ValidateTargetPath(target) == nil || ValidateSharedSurfacePath(target) == nil {
		return nil
	}
	if strings.HasPrefix(target, SharedSurfaceRoot+"/") {
		return fmt.Errorf("shared skill surface path %q must name an %s entry", target, SharedSurfacePrefix)
	}
	return fmt.Errorf("reserved project state path %q cannot be a realization target", target)
}

// ValidateRealizationDirectoryPath additionally accepts the shared surface's
// own ancestors, which a transaction creates on the way to an entry but never
// writes a file at.
func ValidateRealizationDirectoryPath(target string) error {
	if target == ".agents" || target == SharedSurfaceRoot {
		return nil
	}
	return ValidateRealizationPath(target)
}

func validateRelativePath(value string) error {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("use a normalized project-relative slash path, got %q", value)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("project-relative path %q contains a control character", value)
		}
	}
	return nil
}

func validHash(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && strings.ToLower(value) == value
}

func canonicalLedger(ledger Ledger) Ledger {
	ledger.Targets = append([]Target(nil), ledger.Targets...)
	// The version is a property of the state, not of the caller: a ledger that
	// owns no coordinator target is written at 1 whatever supported version it
	// arrived under, so an older ACR keeps reading every project that never
	// gains a shared surface. An unsupported version is left alone for
	// ValidateLedger to refuse rather than normalized into acceptance.
	if ledger.SchemaVersion == BaselineLedgerSchemaVersion || ledger.SchemaVersion == SharedSurfaceLedgerSchemaVersion {
		ledger.SchemaVersion = requiredLedgerSchemaVersion(ledger)
	}
	for index := range ledger.Targets {
		ledger.Targets[index].Entries = append([]Entry(nil), ledger.Targets[index].Entries...)
		sort.Slice(ledger.Targets[index].Entries, func(left, right int) bool {
			leftEntry := ledger.Targets[index].Entries[left]
			rightEntry := ledger.Targets[index].Entries[right]
			leftKey := leftEntry.Source + "\x00" + leftEntry.ArtifactID + "\x00" + leftEntry.Adapter
			rightKey := rightEntry.Source + "\x00" + rightEntry.ArtifactID + "\x00" + rightEntry.Adapter
			return leftKey < rightKey
		})
	}
	sort.Slice(ledger.Targets, func(left, right int) bool {
		return ledger.Targets[left].Path < ledger.Targets[right].Path
	})
	return ledger
}
