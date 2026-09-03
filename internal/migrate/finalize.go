package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

// FinalizeEdit is a positively evidenced deletion or byte splice.
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
	Edits       []FinalizeEdit
	Retained    []RetentionRecord
	Fingerprint string
}

// PlanFinalization identifies whole files and marked host-file spans without
// writing. Native files are removed only when inventory tied them to a
// non-ambiguous artifact. Host selection and realization validation prevent
// ACR ledger targets from occupying Tessl-owned whole-file paths.
func PlanFinalization(snapshot adapter.Snapshot, inventory Report) (FinalizePlan, error) {
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
			if artifact.Classification != classMigratable || len(artifact.Lossy) != 0 {
				for _, native := range artifact.Natives {
					plan.Retained = append(plan.Retained, RetentionRecord{Path: native, Kind: artifact.Kind, ID: artifact.ID, Reason: artifact.Classification})
				}
				continue
			}
			for _, native := range artifact.Natives {
				if err := addDelete(native, artifact.Kind, artifact.ID); err != nil {
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
	plan.Fingerprint = FinalizationFingerprint(plan.Edits)
	return plan, nil
}

// FinalizationFingerprint binds paths, modes, and contents before mutation.
func FinalizationFingerprint(edits []FinalizeEdit) string {
	hash := sha256.New()
	for _, edit := range edits {
		fmt.Fprintf(hash, "%s\x00%s\x00%04o\x00%s\x00%s\x00", edit.Path, edit.Operation, edit.Mode.Perm(), edit.Hash, edit.LinkTarget)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
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
			return append(append([]byte(nil), content[:start]...), content[line.end:]...), true
		}
	}
	return content, false
}
