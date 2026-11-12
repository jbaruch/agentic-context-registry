package adapter

import (
	"fmt"
	"io/fs"
	"sort"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// DroppedFile is a file the previous ownership ledger records as one
// generated-only artifact that no current intent renders: an upgraded package
// stopped shipping it.
type DroppedFile struct {
	// Path is the project-relative target.
	Path string
	// ManagedHash is the managedHash the ledger recorded for the file.
	ManagedHash string
	// Removable is true when the file on disk still has exactly the recorded
	// content and mode, so the realization plan removes it as tool-owned
	// output. A modified file is not removable and stays visible to validation.
	Removable bool
}

// droppedManagedFiles classifies every generated-only whole-file ledger target
// that no current intent renders. Shared targets, the coordinator-owned skill
// surface, and targets that are still rendered are never dropped files.
func droppedManagedFiles(project Snapshot, previous realize.Ledger, intents map[string]realize.Intent) []DroppedFile {
	if project == nil {
		return nil
	}
	var dropped []DroppedFile
	for _, target := range previous.Targets {
		if _, rendered := intents[target.Path]; rendered {
			continue
		}
		if target.Ownership != realize.OwnershipGenerated || target.Owner == realize.OwnerCoordinator {
			continue
		}
		if len(target.Entries) != 1 || target.Entries[0].ArtifactKind != realize.ArtifactFile {
			continue
		}
		managedHash := target.Entries[0].ManagedHash
		observed, err := project.ReadFile(target.Path)
		if err != nil {
			// Missing, symlinked, or unreadable: the planner and the tree
			// walk keep their own handling of it.
			continue
		}
		dropped = append(dropped, DroppedFile{
			Path: target.Path, ManagedHash: managedHash,
			Removable: observed.Hash == managedHash && observed.Hash == target.OutputHash && uint32(observed.Mode.Perm()) == target.Mode,
		})
	}
	sort.Slice(dropped, func(left, right int) bool { return dropped[left].Path < dropped[right].Path })
	return dropped
}

// postRemovalSnapshot presents the project as it will be once the plan removes
// the dropped files that are safe to remove, so adapter validation judges the
// planned state rather than the state before the removal.
type postRemovalSnapshot struct {
	inner  Snapshot
	hidden map[string]bool
}

func newPostRemovalSnapshot(inner Snapshot, dropped []DroppedFile) Snapshot {
	hidden := make(map[string]bool)
	for _, file := range dropped {
		if file.Removable {
			hidden[file.Path] = true
		}
	}
	if len(hidden) == 0 || inner == nil {
		return inner
	}
	return &postRemovalSnapshot{inner: inner, hidden: hidden}
}

func (snapshot *postRemovalSnapshot) ReadFile(name string) (ObservedFile, error) {
	if snapshot.hidden[name] {
		return ObservedFile{}, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}
	return snapshot.inner.ReadFile(name)
}

func (snapshot *postRemovalSnapshot) ReadDir(directory string) ([]ObservedEntry, error) {
	directorySnapshot, ok := snapshot.inner.(DirectorySnapshot)
	if !ok {
		return nil, fmt.Errorf("snapshot does not support directory inspection")
	}
	entries, err := directorySnapshot.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	kept := entries[:0:0]
	for _, entry := range entries {
		if !snapshot.hidden[entry.Path] && !(entry.Mode.IsDir() && snapshot.emptiedByRemoval(directorySnapshot, entry.Path)) {
			kept = append(kept, entry)
		}
	}
	return kept, nil
}

// emptiedByRemoval reports whether a directory holds only files the plan
// removes, which the plan then prunes. A directory that is empty on disk today
// is not the plan's doing and stays visible.
func (snapshot *postRemovalSnapshot) emptiedByRemoval(directorySnapshot DirectorySnapshot, directory string) bool {
	entries, err := directorySnapshot.ReadDir(directory)
	if err != nil || len(entries) == 0 {
		return false
	}
	for _, entry := range entries {
		if snapshot.hidden[entry.Path] {
			continue
		}
		if entry.Mode.IsDir() && snapshot.emptiedByRemoval(directorySnapshot, entry.Path) {
			continue
		}
		return false
	}
	return true
}

func (snapshot *postRemovalSnapshot) ReadLink(name string) (string, error) {
	linkSnapshot, ok := snapshot.inner.(LinkSnapshot)
	if !ok {
		return "", fmt.Errorf("snapshot does not support link inspection")
	}
	if snapshot.hidden[name] {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrNotExist}
	}
	return linkSnapshot.ReadLink(name)
}
