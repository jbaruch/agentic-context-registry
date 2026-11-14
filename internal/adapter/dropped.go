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
	// Removable is true only when the realization plan will delete the file:
	// it still has exactly the recorded content and mode and Git does not track
	// it. Every other dropped file stays visible to validation, which refuses
	// it before any ledger write.
	Removable bool
	// Tracked is true when the file still matches its recorded content and
	// mode but Git tracks it. The plan keeps a tracked target on disk and
	// drops ownership instead of deleting it, so it is not Removable.
	Tracked bool
	// ModeOnly is true when the content still hashes to ManagedHash and only
	// the mode differs from RecordedMode.
	ModeOnly     bool
	RecordedMode uint32
	ObservedMode uint32
}

// Refusal says why validation will not let a dropped file stand: it names the
// file and the recorded managedHash, and the way forward fits the cause.
func (file DroppedFile) Refusal() string {
	switch {
	case file.Tracked:
		return fmt.Sprintf("%q is tracked by Git and was not modified: it still matches its recorded managedHash %s, but an upgraded package dropped it and ACR never deletes a tracked file; run `git rm %s` (or delete it and commit the removal) and retry", file.Path, file.ManagedHash, file.Path)
	case file.ModeOnly:
		return fmt.Sprintf("%q was dropped by an upgraded package; its content still matches its recorded managedHash %s but its mode changed from %04o to %04o; run `chmod %04o %s` to restore the mode, or delete the file yourself, and retry", file.Path, file.ManagedHash, file.RecordedMode, file.ObservedMode, file.RecordedMode, file.Path)
	default:
		return fmt.Sprintf("%q was dropped by an upgraded package but no longer matches its recorded managedHash %s; review it, then restore the recorded content or delete the file yourself and retry", file.Path, file.ManagedHash)
	}
}

// droppedManagedFiles classifies every generated-only whole-file ledger target
// that no current intent renders. Shared targets, the coordinator-owned skill
// surface, and targets that are still rendered are never dropped files. A
// Git-tracked file that still matches the ledger is not removable, because the
// planner keeps a tracked target on disk.
func droppedManagedFiles(project Snapshot, previous realize.Ledger, intents map[string]realize.Intent) ([]DroppedFile, error) {
	if project == nil {
		return nil, nil
	}
	var dropped []DroppedFile
	var matching []string
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
		contentMatches := observed.Hash == managedHash && observed.Hash == target.OutputHash
		modeMatches := uint32(observed.Mode.Perm()) == target.Mode
		file := DroppedFile{
			Path: target.Path, ManagedHash: managedHash,
			ModeOnly: contentMatches && !modeMatches, RecordedMode: target.Mode, ObservedMode: uint32(observed.Mode.Perm()),
		}
		if contentMatches && modeMatches {
			file.Removable = true
			matching = append(matching, target.Path)
		}
		dropped = append(dropped, file)
	}
	if len(matching) != 0 {
		tracker, ok := project.(TrackedSnapshot)
		if ok {
			tracked, err := tracker.TrackedPaths(matching)
			if err != nil {
				return nil, fmt.Errorf("inspect Git-tracked dropped files: %w", err)
			}
			for index := range dropped {
				if dropped[index].Removable && tracked[dropped[index].Path] {
					dropped[index].Removable = false
					dropped[index].Tracked = true
				}
			}
		}
	}
	sort.Slice(dropped, func(left, right int) bool { return dropped[left].Path < dropped[right].Path })
	return dropped, nil
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
