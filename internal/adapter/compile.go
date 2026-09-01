package adapter

import (
	"fmt"
	"io/fs"
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
func compileOutputs(project Snapshot, previous realize.Ledger, compiler SharedCompiler, sources []adapterRender) ([]realize.Intent, error) {
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
	sort.Strings(targets)

	intents := make([]realize.Intent, 0, len(targets))
	for _, target := range targets {
		group := byTarget[target]
		kind := group[0].output.Kind
		for _, tagged := range group[1:] {
			if tagged.output.Kind != kind {
				return nil, &MalformedOutputError{Target: target, Reason: "adapters disagree on output kind for the same native target"}
			}
		}
		var intent realize.Intent
		var err error
		switch kind {
		case OutputGeneratedFile:
			intent, err = compileGeneratedFile(project, previous, target, group)
		case OutputMarkdownInclude:
			intent, err = compileMarkdown(project, compiler, target, group)
		case OutputConfigMerge:
			intent, err = compileConfig(project, compiler, target, group)
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

func validateOutputShape(output Output) error {
	if output.Target == "" {
		return &MalformedOutputError{Target: output.Target, Reason: "target must not be empty"}
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

func compileMarkdown(project Snapshot, compiler SharedCompiler, target string, group []taggedOutput) (realize.Intent, error) {
	if compiler == nil {
		return realize.Intent{}, fmt.Errorf("target %q needs a markdown-include SharedCompiler but none is registered", target)
	}
	seenBlocks := make(map[string]struct{})
	var insertions []MarkdownInsertion
	var entries []realize.Entry
	for _, tagged := range group {
		for _, insertion := range tagged.output.Markdown {
			if _, duplicate := seenBlocks[insertion.BlockID]; duplicate {
				return realize.Intent{}, &DuplicateEntryError{Target: target, Identifier: insertion.BlockID}
			}
			seenBlocks[insertion.BlockID] = struct{}{}
			insertions = append(insertions, insertion)
			entries = append(entries, realize.Entry{
				Source: insertion.Owner.Source, ArtifactID: insertion.Owner.ArtifactID, ArtifactKind: realize.ArtifactManagedBlock,
				SourcePath: insertion.Owner.SourcePath, Adapter: tagged.descriptor.ID, AdapterVersion: tagged.descriptor.Version,
				ManagedHash: hashContent(insertion.Body),
			})
		}
	}
	sort.Slice(insertions, func(left, right int) bool { return insertions[left].BlockID < insertions[right].BlockID })

	observed, exists, err := readOptional(project, target)
	if err != nil {
		return realize.Intent{}, err
	}
	document, err := compiler.MergeMarkdown(observed, exists, insertions)
	if err != nil {
		return realize.Intent{}, fmt.Errorf("merge markdown target %q: %w", target, err)
	}
	return sharedOrGeneratedIntent(target, group[0].output.Mode, exists, observed, document, entries), nil
}

func compileConfig(project Snapshot, compiler SharedCompiler, target string, group []taggedOutput) (realize.Intent, error) {
	if compiler == nil {
		return realize.Intent{}, fmt.Errorf("target %q needs a config-merge SharedCompiler but none is registered", target)
	}
	format := group[0].output.Config.Format
	seenKeys := make(map[string]struct{})
	var mergeEntries []ConfigEntry
	var entries []realize.Entry
	for _, tagged := range group {
		if tagged.output.Config.Format != format {
			return realize.Intent{}, &MalformedOutputError{Target: target, Reason: "adapters disagree on config format for the same native target"}
		}
		for _, mergeEntry := range tagged.output.Config.Entries {
			key := canonicalEntryKey(mergeEntry.Container, mergeEntry.Kind, mergeEntry.Key)
			if _, duplicate := seenKeys[key]; duplicate {
				return realize.Intent{}, &DuplicateEntryError{Target: target, Identifier: key}
			}
			seenKeys[key] = struct{}{}
			mergeEntries = append(mergeEntries, mergeEntry)
			entries = append(entries, realize.Entry{
				Source: mergeEntry.Owner.Source, ArtifactID: mergeEntry.Owner.ArtifactID, ArtifactKind: realize.ArtifactStructuredEntry,
				SourcePath: mergeEntry.Owner.SourcePath, Adapter: tagged.descriptor.ID, AdapterVersion: tagged.descriptor.Version,
				ManagedHash: hashContent(mergeEntry.EncodedValue),
			})
		}
	}
	sort.Slice(mergeEntries, func(left, right int) bool {
		return canonicalEntryKey(mergeEntries[left].Container, mergeEntries[left].Kind, mergeEntries[left].Key) <
			canonicalEntryKey(mergeEntries[right].Container, mergeEntries[right].Kind, mergeEntries[right].Key)
	})

	observed, exists, err := readOptional(project, target)
	if err != nil {
		return realize.Intent{}, err
	}
	document, err := compiler.MergeConfig(observed, exists, format, mergeEntries)
	if err != nil {
		return realize.Intent{}, fmt.Errorf("merge config target %q: %w", target, err)
	}
	return sharedOrGeneratedIntent(target, group[0].output.Mode, exists, observed, document, entries), nil
}

func sharedOrGeneratedIntent(target string, mode fs.FileMode, exists bool, observed ObservedFile, document MergedDocument, entries []realize.Entry) realize.Intent {
	intent := realize.Intent{
		Action: realize.ActionEnsure, Path: target, Content: document.Content,
		Mode: uint32(mode.Perm()), Entries: entries,
	}
	if exists {
		intent.Ownership = realize.OwnershipShared
		intent.ObservedHash = observed.Hash
		intent.ManagedIntact = document.ManagedIntact
		intent.PreservedContent = document.Preserved
	} else {
		intent.Ownership = realize.OwnershipGenerated
	}
	return intent
}

// canonicalEntryKey encodes a (container, kind, key) tuple as a
// length-prefixed string: every segment, the kind, and the key are each
// preceded by their own byte length, so no separator byte inside any segment
// can make two structurally different tuples collide. It is used for both
// duplicate detection and sorting, so both share one unambiguous total
// order; the previous ad hoc "join with \x00" key omitted a separator
// between the joined container and Key and omitted Kind entirely, so
// (["ab"], "c") and (["a"], "bc") compared equal.
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
