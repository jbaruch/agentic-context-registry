package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

// FinalizeEdit is a positively evidenced deletion or byte splice. Hash covers
// the complete Before bytes for regular files and the link target bytes for a
// symlink; structured-entry RemovalRecord hashes cover each removed raw entry.
type FinalizeEdit struct {
	Path       string
	Kind       string
	ID         string
	Operation  string
	Before     []byte
	After      []byte
	Mode       fs.FileMode
	Hash       string
	LinkTarget string
	Removed    []RemovalRecord
}

// FinalizePlan is the pure, fingerprint-bound Tessl removal plan.
type FinalizePlan struct {
	Edits    []FinalizeEdit
	Retained []RetentionRecord
	Unowned  []UnownedNative
}

// reasonNativeUnowned names a per-agent tessl__ entry whose stored
// destination is not the installed package's Tessl plugin tree.
const reasonNativeUnowned = "unproven-native-link-target"

// UnownedNative is a per-agent tessl__ symlink planning refused to retire.
// The matching basename is the name Tessl writes, not evidence about the
// bytes the deletion would take: a repointed link carries a user's own skill
// under that name. The entry is retained exactly as it is, and
// migrateapp.danglingSharedLinkBlockers proves whether it survives the
// removals actually planned.
type UnownedNative struct {
	Path   string
	Kind   string
	ID     string
	Target string
	Reason string
}

// PlanFinalization identifies whole files and marked host-file spans without
// writing. Native files are removed only when inventory tied them to a
// non-ambiguous artifact. Host selection and realization validation prevent
// ACR ledger targets from occupying Tessl-owned whole-file paths.
//
// accepted names the artifacts whose reviewed differences an operator
// accepted by token. It reaches exactly one decision: a lossy artifact the
// acceptance covers becomes retirable instead of retained. It never widens
// classification, ownership, or link-target proof.
func PlanFinalization(snapshot adapter.Snapshot, inventory Report, accepted AcceptedSet) (FinalizePlan, error) {
	plan := FinalizePlan{}
	ambiguous := make(map[string]bool)
	for _, record := range inventory.Ambiguous {
		ambiguous[record.Path] = true
		plan.Retained = append(plan.Retained, RetentionRecord{Path: record.Path, Reason: record.Reason})
	}
	seen := make(map[string]bool)
	addDelete := func(filename, kind, id string) error {
		if seen[filename] || ambiguous[filename] {
			return nil
		}
		observed, err := snapshot.ReadFile(filename)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			links, ok := snapshot.(adapter.LinkSnapshot)
			if !ok {
				return err
			}
			target, linkErr := links.ReadLink(filename)
			if linkErr != nil {
				return err
			}
			seen[filename] = true
			plan.Edits = append(plan.Edits, FinalizeEdit{Path: filename, Kind: kind, ID: id, Operation: "delete", Mode: fs.ModeSymlink | 0o777, Hash: HashFinalizationContent([]byte(target)), LinkTarget: target})
			return nil
		}
		seen[filename] = true
		plan.Edits = append(plan.Edits, FinalizeEdit{Path: filename, Kind: kind, ID: id, Operation: "delete", Before: append([]byte(nil), observed.Content...), Mode: observed.Mode.Perm(), Hash: HashFinalizationContent(observed.Content)})
		return nil
	}
	// addNativeDelete plans one per-agent tessl__ entry. A symlink is retired
	// only on positive evidence about the destination it actually stores:
	// planning reads the raw bytes it is about to place in the transaction's
	// before-image and requires them to name this installed package's Tessl
	// plugin tree, which this same run removes in full. A repointed or
	// unreadable destination, or a stored `name/..` the kernel would follow
	// somewhere else, is retained instead of fingerprinted into ownership.
	addNativeDelete := func(filename, kind, id, identity string) error {
		if seen[filename] || ambiguous[filename] {
			return nil
		}
		links, hasLinks := snapshot.(adapter.LinkSnapshot)
		if hasLinks {
			target, linkErr := links.ReadLink(filename)
			switch {
			case linkErr == nil && !nativeRetirementOwned(path.Dir(filename), target, identity):
				plan.Unowned = append(plan.Unowned, UnownedNative{Path: filename, Kind: kind, ID: id, Target: target, Reason: reasonNativeUnowned})
				plan.Retained = append(plan.Retained, RetentionRecord{Path: filename, Kind: kind, ID: id, Reason: reasonNativeUnowned})
				return nil
			case errors.Is(linkErr, fs.ErrNotExist):
				return nil
			}
			// Every other ReadLink outcome means the entry is not a leaf
			// symlink; addDelete classifies it from its own read.
		}
		return addDelete(filename, kind, id)
	}
	if directories, ok := snapshot.(adapter.DirectorySnapshot); ok {
		entries, err := adapter.WalkSnapshot(directories, ".tessl")
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return FinalizePlan{}, err
		}
		for _, entry := range entries {
			if entry.Mode.IsDir() {
				continue
			}
			if err := addDelete(entry.Path, "tessl-state", ""); err != nil {
				return FinalizePlan{}, err
			}
		}
	}
	for _, pkg := range inventory.Packages {
		for _, artifact := range pkg.Artifacts {
			if artifact.Classification != classMigratable || (len(artifact.Lossy) != 0 && !accepted.Covers(pkg.TesslIdentity, artifact.Kind, artifact.ID)) {
				for _, native := range artifact.Natives {
					if onSharedSurface(native) {
						continue
					}
					plan.Retained = append(plan.Retained, RetentionRecord{Path: native, Kind: artifact.Kind, ID: artifact.ID, Reason: artifact.Classification})
				}
				continue
			}
			for _, native := range artifact.Natives {
				// The shared surface is decided per link against the
				// realization ledger, not per artifact: a Tessl link is retired
				// only once ACR owns an equivalent entry, so the decision needs
				// evidence PlanFinalization does not have. See
				// migrateapp.sharedSurfacePlan.
				if onSharedSurface(native) {
					continue
				}
				if err := addNativeDelete(native, artifact.Kind, artifact.ID, pkg.TesslIdentity); err != nil {
					return FinalizePlan{}, err
				}
			}
		}
	}
	for _, filename := range []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"} {
		observed, err := snapshot.ReadFile(filename)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return FinalizePlan{}, err
		}
		spans := tesslManagedSpans(observed.Content)
		if len(spans) == 0 || ambiguous[filename] {
			continue
		}
		after := removeByteSpans(observed.Content, spans)
		plan.Edits = append(plan.Edits, FinalizeEdit{Path: filename, Kind: "managed-span", ID: "tessl-managed", Operation: "splice", Before: append([]byte(nil), observed.Content...), After: after, Mode: observed.Mode.Perm(), Hash: HashFinalizationContent(observed.Content)})
	}
	if observed, err := snapshot.ReadFile(".gitignore"); err == nil {
		if after, ok := removeGitignoreBlock(observed.Content); ok {
			plan.Edits = append(plan.Edits, FinalizeEdit{Path: ".gitignore", Kind: "managed-span", ID: "tessl-gitignore", Operation: "splice", Before: append([]byte(nil), observed.Content...), After: after, Mode: observed.Mode.Perm(), Hash: HashFinalizationContent(observed.Content)})
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return FinalizePlan{}, err
	}
	if err := addDelete("tessl.json", "manifest", ""); err != nil {
		return FinalizePlan{}, err
	}
	sort.Slice(plan.Edits, func(i, j int) bool {
		if plan.Edits[i].Path == "tessl.json" {
			return false
		}
		if plan.Edits[j].Path == "tessl.json" {
			return true
		}
		return plan.Edits[i].Path < plan.Edits[j].Path
	})
	return plan, nil
}

// nativeRetirementOwned reports whether one per-agent tessl__ link's stored
// target is this installed package's Tessl plugin tree. Everything under that
// tree is removed by the same plan, so an accepted link would dangle if it
// were kept; a target anywhere else is a user's own destination the run never
// deletes.
func nativeRetirementOwned(directory, target, identity string) bool {
	if identity == "" || retirementTargetErasesComponent(directory, target) {
		return false
	}
	resolved, inside := resolveNativeRetirementTarget(directory, target)
	if !inside {
		return false
	}
	return strings.HasPrefix(resolved, ".tessl/plugins/"+identity+"/")
}

// resolveNativeRetirementTarget places a relative stored target against the
// directory that holds the link. An absolute target is not placed here: the
// pure layer has no project pathname to place it against, so it stays
// unproven rather than assumed.
func resolveNativeRetirementTarget(directory, target string) (string, bool) {
	if target == "" || strings.HasPrefix(target, "/") {
		return "", false
	}
	resolved := path.Clean(path.Join(directory, filepath.ToSlash(target)))
	if resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "", false
	}
	return resolved, true
}

// retirementTargetErasesComponent reports a stored target whose `..` would
// discard a named component after leaving the directory that holds the link.
// The kernel inspects that name before applying `..`; cleaning it away is how
// a distinct user skill is mistaken for the declared package skill.
func retirementTargetErasesComponent(directory, target string) bool {
	if target == "" || strings.HasPrefix(target, "/") {
		return false
	}
	surface := strings.Split(directory, "/")
	var parts []string
	left := false
	for _, component := range strings.Split(directory+"/"+filepath.ToSlash(target), "/") {
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			if len(parts) == 0 {
				left = true
				continue
			}
			if left {
				return true
			}
			parts = parts[:len(parts)-1]
			if len(parts) == 0 {
				left = true
			}
			continue
		}
		parts = append(parts, component)
		if !componentsPrefix(parts, surface) {
			left = true
		}
	}
	return false
}

func componentsPrefix(parts, surface []string) bool {
	if len(parts) > len(surface) {
		return false
	}
	for index, part := range parts {
		if surface[index] != part {
			return false
		}
	}
	return true
}

// onSharedSurface reports whether a native path lives on the shared skill
// surface, which finalization plans separately.
func onSharedSurface(native string) bool {
	return strings.HasPrefix(native, SharedSkillsRoot+"/")
}

// HashFinalizationContent returns the ledger-compatible content digest.
func HashFinalizationContent(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func removeByteSpans(content []byte, spans []byteSpan) []byte {
	result := append([]byte(nil), content...)
	for index := len(spans) - 1; index >= 0; index-- {
		result = append(result[:spans[index].start], result[spans[index].end:]...)
	}
	return result
}

func removeGitignoreBlock(content []byte) ([]byte, bool) {
	lines := physicalLines(content)
	start := -1
	for _, line := range lines {
		text := strings.TrimSpace(string(content[line.start:line.contentEnd]))
		if start < 0 && strings.HasPrefix(text, gitignoreBeginPrefix) {
			start = line.start
			continue
		}
		if start >= 0 && text == gitignoreEnd {
			return joinRemovedBlock(content[:start], content[line.end:]), true
		}
	}
	return content, false
}

func joinRemovedBlock(prefix, suffix []byte) []byte {
	left := append([]byte(nil), prefix...)
	right := suffix
	for boundaryLineBreaks(left, right) > 2 {
		if width := leadingLineBreakWidth(right); width != 0 {
			right = right[width:]
			continue
		}
		left = left[:len(left)-trailingLineBreakWidth(left)]
	}
	return append(left, right...)
}

func boundaryLineBreaks(left, right []byte) int {
	count := 0
	for width := trailingLineBreakWidth(left); width != 0; width = trailingLineBreakWidth(left) {
		count++
		left = left[:len(left)-width]
	}
	for width := leadingLineBreakWidth(right); width != 0; width = leadingLineBreakWidth(right) {
		count++
		right = right[width:]
	}
	return count
}

func leadingLineBreakWidth(content []byte) int {
	if len(content) >= 2 && content[0] == '\r' && content[1] == '\n' {
		return 2
	}
	if len(content) != 0 && content[0] == '\n' {
		return 1
	}
	return 0
}

func trailingLineBreakWidth(content []byte) int {
	if len(content) >= 2 && content[len(content)-2] == '\r' && content[len(content)-1] == '\n' {
		return 2
	}
	if len(content) != 0 && content[len(content)-1] == '\n' {
		return 1
	}
	return 0
}
