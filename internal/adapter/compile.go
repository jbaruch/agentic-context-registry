package adapter

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// CodeDuplicateConfigEntry is DuplicateEntryError's stable, machine-readable
// code.
const CodeDuplicateConfigEntry = "duplicate_config_entry"

// DuplicateEntryError reports a duplicate managed-block or structural-entry
// identifier across adapters rendering the same native target.
type DuplicateEntryError struct {
	Target     string
	Identifier string
}

func (err *DuplicateEntryError) Error() string {
	return fmt.Sprintf("%s: target %q has more than one managed entry identified by %q", CodeDuplicateConfigEntry, err.Target, err.Identifier)
}

// MalformedOutputError reports an Output whose payload does not match its
// Kind, a target rendered with incompatible kinds by different adapters, or
// a generated-file output that would replace shared or unproven content.
type MalformedOutputError struct {
	Target string
	Reason string
}

func (err *MalformedOutputError) Error() string {
	return fmt.Sprintf("malformed adapter output for %q: %s", err.Target, err.Reason)
}

// adapterRender pairs one adapter's rendered outputs with its descriptor, so
// compileOutputs can stamp ledger identity without trusting adapter payload
// data.
type adapterRender struct {
	Descriptor Descriptor
	Outputs    []Output
}

type taggedOutput struct {
	descriptor Descriptor
	output     Output
}

// compileOutputs is the single trusted bridge from adapter-rendered Output
// values to realize.Intent. It groups every adapter's outputs by native
// target, rejects malformed tagged unions and duplicate managed identifiers,
// and derives every realize.Intent merge-binding field (ObservedHash,
// ManagedIntact, PreservedContent) from the project snapshot, the previous
// ledger, and a registered SharedCompiler's proof — never from adapter
// payload data. Markdown and config-merge outputs fail closed when no
// SharedCompiler is registered.
//
// It also visits every previously shared target that has no current output
// at all, so a Markdown/config SharedCompiler can express a safe partial or
// final removal instead of the engine's plain generated-only delete path,
// which would silently drop any surviving unmanaged or still-owned content.
// targetOptions is variadic so every existing caller keeps compiling
// unchanged: omit it entirely for the default (no per-target overrides), or
// pass exactly one map keyed by native target path. Options are always
// caller-supplied, coordinator-owned state — compileOutputs never derives
// them from adapter output.
func compileOutputs(project Snapshot, previous realize.Ledger, compiler SharedCompiler, sources []adapterRender, targetOptions ...map[string]TargetOptions) ([]realize.Intent, error) {
	options := firstTargetOptions(targetOptions)
	byTarget := make(map[string][]taggedOutput)
	var targets []string
	for _, source := range sources {
		for _, output := range source.Outputs {
			if err := validateOutputShape(output); err != nil {
				return nil, err
			}
			if _, seen := byTarget[output.Target]; !seen {
				targets = append(targets, output.Target)
			}
			byTarget[output.Target] = append(byTarget[output.Target], taggedOutput{descriptor: source.Descriptor, output: output})
		}
	}
	for _, previousTarget := range previous.Targets {
		if previousTarget.Ownership != realize.OwnershipShared {
			continue
		}
		if _, present := byTarget[previousTarget.Path]; present {
			continue
		}
		targets = append(targets, previousTarget.Path)
	}
	sort.Strings(targets)

	intents := make([]realize.Intent, 0, len(targets))
	for _, target := range targets {
		group := byTarget[target]
		previousTarget, owned := findLedgerTarget(previous, target)
		var previousTargetPtr *realize.Target
		if owned {
			previousTargetPtr = &previousTarget
		}

		var kind OutputKind
		var err error
		if len(group) != 0 {
			kind = group[0].output.Kind
			for _, tagged := range group[1:] {
				if tagged.output.Kind != kind {
					return nil, &MalformedOutputError{Target: target, Reason: "adapters disagree on output kind for the same native target"}
				}
			}
		} else {
			kind, err = revisitKind(previousTarget)
			if err != nil {
				return nil, err
			}
		}

		var intent realize.Intent
		switch kind {
		case OutputGeneratedFile:
			intent, err = compileGeneratedFile(project, previous, target, group)
		case OutputMarkdownInclude:
			intent, err = compileMarkdown(project, compiler, target, group, previousTargetPtr, options[target])
		case OutputConfigMerge:
			intent, err = compileConfig(project, compiler, target, group, previousTargetPtr, options[target])
		default:
			return nil, &MalformedOutputError{Target: target, Reason: fmt.Sprintf("unsupported output kind %q", kind)}
		}
		if err != nil {
			return nil, err
		}
		intents = append(intents, intent)
	}
	return intents, nil
}

func firstTargetOptions(variadic []map[string]TargetOptions) map[string]TargetOptions {
	if len(variadic) == 0 {
		return nil
	}
	return variadic[0]
}

// revisitKind determines the Markdown/config family of a previously shared
// target that has no current adapter output, from its ledger entries' kind.
func revisitKind(previousTarget realize.Target) (OutputKind, error) {
	seen := make(map[realize.ArtifactKind]struct{}, 1)
	for _, entry := range previousTarget.Entries {
		seen[entry.ArtifactKind] = struct{}{}
	}
	if len(seen) == 1 {
		if _, ok := seen[realize.ArtifactManagedBlock]; ok {
			return OutputMarkdownInclude, nil
		}
		if _, ok := seen[realize.ArtifactStructuredEntry]; ok {
			return OutputConfigMerge, nil
		}
	}
	return "", &MalformedOutputError{
		Target: previousTarget.Path,
		Reason: "previously shared target has no current output and its ledger entries are not homogeneously managed-block or structured-entry",
	}
}

func validateOutputShape(output Output) error {
	if output.Target == "" {
		return &MalformedOutputError{Target: output.Target, Reason: "target must not be empty"}
	}
	// Defense in depth: reject reserved, parent-traversal, and absolute
	// targets at the interface as well as the engine, which remains the
	// authoritative enforcement (realize.Planner rejects every intent path
	// the same way, whatever produced it).
	if err := realize.ValidateTargetPath(output.Target); err != nil {
		return &MalformedOutputError{Target: output.Target, Reason: err.Error()}
	}
	markdown, config, file := len(output.Markdown) != 0, output.Config != nil, output.File != nil
	switch output.Kind {
	case OutputMarkdownInclude:
		if !markdown || config || file {
			return &MalformedOutputError{Target: output.Target, Reason: "markdown-include output must carry only Markdown insertions"}
		}
	case OutputConfigMerge:
		if markdown || !config || file {
			return &MalformedOutputError{Target: output.Target, Reason: "config-merge output must carry only a Config payload"}
		}
	case OutputGeneratedFile:
		if markdown || config || !file {
			return &MalformedOutputError{Target: output.Target, Reason: "generated-file output must carry only a File payload"}
		}
	default:
		return &MalformedOutputError{Target: output.Target, Reason: fmt.Sprintf("unsupported output kind %q", output.Kind)}
	}
	return nil
}

func compileGeneratedFile(project Snapshot, previous realize.Ledger, target string, group []taggedOutput) (realize.Intent, error) {
	if len(group) != 1 {
		return realize.Intent{}, &MalformedOutputError{Target: target, Reason: "more than one adapter rendered a generated-file output for the same target"}
	}
	tagged := group[0]
	file := tagged.output.File

	_, exists, err := readOptional(project, target)
	if err != nil {
		return realize.Intent{}, err
	}
	if exists {
		ledgerTarget, owned := findLedgerTarget(previous, target)
		if !owned || ledgerTarget.Ownership != realize.OwnershipGenerated {
			return realize.Intent{}, &MalformedOutputError{
				Target: target,
				Reason: "a whole-file generated-file output can never replace shared or unproven content; render it through a markdown-include or config-merge output instead",
			}
		}
	}

	entry := realize.Entry{
		Source: file.Owner.Source, ArtifactID: file.Owner.ArtifactID, ArtifactKind: realize.ArtifactFile,
		SourcePath: file.Owner.SourcePath, Adapter: tagged.descriptor.ID, AdapterVersion: tagged.descriptor.Version,
		ManagedHash: hashContent(file.Content),
	}
	return realize.Intent{
		Action: realize.ActionEnsure, Path: target, Content: file.Content,
		Mode: uint32(tagged.output.Mode.Perm()), Ownership: realize.OwnershipGenerated,
		Entries: []realize.Entry{entry},
	}, nil
}

func compileMarkdown(project Snapshot, compiler SharedCompiler, target string, group []taggedOutput, previousTarget *realize.Target, options TargetOptions) (realize.Intent, error) {
	if compiler == nil {
		return realize.Intent{}, fmt.Errorf("target %q needs a markdown-include SharedCompiler but none is registered", target)
	}
	seenBlocks := make(map[string]struct{})
	var desired []MarkdownInsertion
	descriptors := make(map[string]Descriptor)
	for _, tagged := range group {
		for _, insertion := range tagged.output.Markdown {
			if _, duplicate := seenBlocks[insertion.BlockID]; duplicate {
				return realize.Intent{}, &DuplicateEntryError{Target: target, Identifier: insertion.BlockID}
			}
			seenBlocks[insertion.BlockID] = struct{}{}
			desired = append(desired, insertion)
			descriptors[ownerKey(insertion.Owner)] = tagged.descriptor
		}
	}
	sort.Slice(desired, func(left, right int) bool { return desired[left].BlockID < desired[right].BlockID })

	sharedTarget, err := buildSharedTarget(project, target, previousTarget, options)
	if err != nil {
		return realize.Intent{}, err
	}
	compilation, err := compiler.CompileMarkdown(context.Background(), MarkdownCompileRequest{Target: sharedTarget, Desired: desired})
	if err != nil {
		return realize.Intent{}, fmt.Errorf("compile markdown target %q: %w", target, err)
	}
	entries, err := stampManagedEntries(target, compilation.Managed, descriptors)
	if err != nil {
		return realize.Intent{}, err
	}
	return intentFromCompilation(target, sharedTarget, compilation, entries), nil
}

func compileConfig(project Snapshot, compiler SharedCompiler, target string, group []taggedOutput, previousTarget *realize.Target, options TargetOptions) (realize.Intent, error) {
	if compiler == nil {
		return realize.Intent{}, fmt.Errorf("target %q needs a config-merge SharedCompiler but none is registered", target)
	}
	var format ConfigFormat
	if len(group) != 0 {
		format = group[0].output.Config.Format
		if options.ConfigFormat != "" && options.ConfigFormat != format {
			return realize.Intent{}, &MalformedOutputError{
				Target: target,
				Reason: fmt.Sprintf("caller-supplied TargetOptions.ConfigFormat %q does not match the rendered config format %q", options.ConfigFormat, format),
			}
		}
	} else if options.ConfigFormat != "" {
		format = options.ConfigFormat
	} else {
		return realize.Intent{}, &MalformedOutputError{
			Target: target,
			Reason: "target has no current adapter output and no caller-supplied TargetOptions.ConfigFormat; a target's file extension is not trusted evidence of its format",
		}
	}

	seenKeys := make(map[string]struct{})
	var desired []ConfigEntry
	descriptors := make(map[string]Descriptor)
	for _, tagged := range group {
		if tagged.output.Config.Format != format {
			return realize.Intent{}, &MalformedOutputError{Target: target, Reason: "adapters disagree on config format for the same native target"}
		}
		for _, entry := range tagged.output.Config.Entries {
			key := canonicalEntryKey(entry.Container, entry.Kind, entry.Key)
			if _, duplicate := seenKeys[key]; duplicate {
				return realize.Intent{}, &DuplicateEntryError{Target: target, Identifier: key}
			}
			seenKeys[key] = struct{}{}
			desired = append(desired, entry)
			descriptors[ownerKey(entry.Owner)] = tagged.descriptor
		}
	}
	sort.Slice(desired, func(left, right int) bool {
		return canonicalEntryKey(desired[left].Container, desired[left].Kind, desired[left].Key) <
			canonicalEntryKey(desired[right].Container, desired[right].Kind, desired[right].Key)
	})

	sharedTarget, err := buildSharedTarget(project, target, previousTarget, options)
	if err != nil {
		return realize.Intent{}, err
	}
	compilation, err := compiler.CompileConfig(context.Background(), ConfigCompileRequest{Target: sharedTarget, Format: format, Desired: desired})
	if err != nil {
		return realize.Intent{}, fmt.Errorf("compile config target %q: %w", target, err)
	}
	entries, err := stampManagedEntries(target, compilation.Managed, descriptors)
	if err != nil {
		return realize.Intent{}, err
	}
	return intentFromCompilation(target, sharedTarget, compilation, entries), nil
}

// buildSharedTarget reads the observed native file, if any, and assembles
// the trusted SharedTarget compileOutputs hands to a SharedCompiler.
// ExplicitDemotion and Force come only from the caller's own TargetOptions
// (see compileOutputs's targetOptions parameter and
// Coordinator.Realize's), keyed by this target's path — never from adapter
// data, which has no field to carry them in the first place.
func buildSharedTarget(project Snapshot, target string, previousTarget *realize.Target, options TargetOptions) (SharedTarget, error) {
	observed, exists, err := readOptional(project, target)
	if err != nil {
		return SharedTarget{}, err
	}
	sharedTarget := SharedTarget{Path: target, Previous: previousTarget, ExplicitDemotion: options.ExplicitDemotion, Force: options.Force}
	if exists {
		sharedTarget.Observed = &observed
	}
	return sharedTarget, nil
}

func intentFromCompilation(target string, sharedTarget SharedTarget, compilation SharedCompilation, entries []realize.Entry) realize.Intent {
	intent := realize.Intent{
		Action: compilation.Action, Path: target, Entries: entries,
		ExplicitDemotion: sharedTarget.ExplicitDemotion,
		ObservedHash:     compilation.Proof.ObservedHash,
		ManagedIntact:    compilation.Proof.ManagedIntact,
		PreservedContent: compilation.Proof.PreservedContent,
	}
	if compilation.Candidate != nil {
		intent.Content = compilation.Candidate.Content
		intent.Mode = uint32(compilation.Candidate.Mode.Perm())
		intent.Ownership = compilation.Candidate.Ownership
	}
	return intent
}

// stampManagedEntries builds realize.Entry values from a SharedCompilation's
// Managed results, stamping adapter identity from the registered descriptor
// that contributed each owner's Desired entry — never from compiler or
// adapter data.
func stampManagedEntries(target string, managed []ManagedResult, descriptors map[string]Descriptor) ([]realize.Entry, error) {
	entries := make([]realize.Entry, 0, len(managed))
	for _, result := range managed {
		descriptor, ok := descriptors[ownerKey(result.Owner)]
		if !ok {
			return nil, fmt.Errorf("target %q: shared compilation returned a managed entry for %q with no matching desired contribution this run", target, result.Owner.ArtifactID)
		}
		entries = append(entries, realize.Entry{
			Source: result.Owner.Source, ArtifactID: result.Owner.ArtifactID, ArtifactKind: result.Kind,
			SourcePath: result.Owner.SourcePath, Adapter: descriptor.ID, AdapterVersion: descriptor.Version,
			ManagedHash: result.ManagedHash,
		})
	}
	sort.Slice(entries, func(left, right int) bool {
		leftKey := entries[left].Source + "\x00" + entries[left].ArtifactID + "\x00" + entries[left].Adapter
		rightKey := entries[right].Source + "\x00" + entries[right].ArtifactID + "\x00" + entries[right].Adapter
		return leftKey < rightKey
	})
	return entries, nil
}

func ownerKey(owner OwnerRef) string {
	return owner.Source + "\x00" + owner.ArtifactID
}

// canonicalEntryKey encodes a (container, kind, key) tuple as a
// length-prefixed string: every segment, the kind, and the key are each
// preceded by their own byte length, so no separator byte inside any segment
// can make two structurally different tuples collide. It is used for both
// duplicate detection and sorting, so both share one unambiguous total
// order.
func canonicalEntryKey(container []string, kind ConfigEntryKind, key string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%d:", len(container))
	for _, segment := range container {
		fmt.Fprintf(&builder, "%d:%s", len(segment), segment)
	}
	fmt.Fprintf(&builder, "%d:%s", len(string(kind)), string(kind))
	fmt.Fprintf(&builder, "%d:%s", len(key), key)
	return builder.String()
}

func findLedgerTarget(ledger realize.Ledger, path string) (realize.Target, bool) {
	for _, target := range ledger.Targets {
		if target.Path == path {
			return target, true
		}
	}
	return realize.Target{}, false
}
