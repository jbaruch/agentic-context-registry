package migrateapp

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// Shared-surface blocker codes.
const (
	blockerSharedOrphan      = "shared-skill-orphan"
	blockerSharedRoot        = "shared-skill-surface-symlink"
	blockerSharedReplacement = "shared-skill-replacement-missing"
)

// sharedSurfacePlan decides, per Tessl link on the shared skill surface,
// whether finalization retires it, leaves it, or refuses.
//
// A link is retired only on positive per-entry evidence: it is a symlink, its
// own target resolves lexically inside an installed package's Tessl plugin
// tree, the skill it names is migratable with nothing lossy, and ACR already
// owns an equivalent entry in the realization ledger. Anything else is
// retained with a reason, or blocks.
//
// A link naming .tessl state this run deletes can never be retained: the
// target goes away and the link dangles, which is exactly the stale reference
// the finalization contract exists to prevent. It blocks with a remedy
// instead.
func sharedSurfacePlan(snapshot adapter.Snapshot, inventory migrate.Report, ledger realize.Ledger) ([]migrate.FinalizeEdit, []migrate.RetentionRecord, []migrate.Blocker, error) {
	replacements := sharedReplacementRoots(ledger)
	var edits []migrate.FinalizeEdit
	var retained []migrate.RetentionRecord
	var blockers []migrate.Blocker
	for _, entry := range inventory.SharedSkills {
		switch entry.Disposition {
		case migrate.SharedSkillUser:
			continue
		case migrate.SharedSkillRetained:
			retained = append(retained, migrate.RetentionRecord{Path: entry.Path, Kind: "skill", ID: entry.SkillID, Reason: entry.Reason})
		case migrate.SharedSkillBlocked:
			code := blockerSharedOrphan
			remedy := fmt.Sprintf("remove or repoint %s, then re-run 'acr migrate tessl --finalize'", entry.Path)
			if entry.Reason == "shared-surface-symlink" {
				code = blockerSharedRoot
				remedy = fmt.Sprintf("replace the %s symbolic link with a real directory, then re-run 'acr migrate tessl --finalize'", migrate.SharedSkillsRoot)
			}
			blockers = append(blockers, migrate.Blocker{Code: code, Path: entry.Path, Kind: "skill", ID: entry.SkillID, Detail: entry.Reason, Remedy: remedy})
		case migrate.SharedSkillRemovable:
			replacement, owned := replacements[entry.SkillID]
			if !owned {
				blockers = append(blockers, migrate.Blocker{
					Code: blockerSharedReplacement, Path: entry.Path, Kind: "skill", ID: entry.SkillID,
					Detail: "ACR owns no shared-surface entry for this skill",
					Remedy: "run 'acr migrate tessl --vendor-unmapped' and commit its output so ACR realizes the shared skill surface, then finalize",
				})
				continue
			}
			edit, err := sharedLinkDeletion(snapshot, entry)
			if err != nil {
				return nil, nil, nil, err
			}
			if edit == nil {
				continue
			}
			edit.Removed = []migrate.RemovalRecord{{
				Path: entry.Path, Kind: "skill", ID: entry.SkillID, Hash: edit.Hash,
				Operation: "delete", Replacement: replacement,
			}}
			edits = append(edits, *edit)
		}
	}
	sort.Slice(edits, func(left, right int) bool { return edits[left].Path < edits[right].Path })
	return edits, retained, blockers, nil
}

// sharedLinkDeletion snapshots one symlink as a reversible deletion. The
// journal restores the link from LinkTarget, so the removal is recoverable
// byte for byte.
func sharedLinkDeletion(snapshot adapter.Snapshot, entry migrate.SharedSkillEntry) (*migrate.FinalizeEdit, error) {
	links, ok := snapshot.(adapter.LinkSnapshot)
	if !ok {
		return nil, fmt.Errorf("project snapshot does not support link inspection")
	}
	target, err := links.ReadLink(entry.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect %q: %w", entry.Path, err)
	}
	return &migrate.FinalizeEdit{
		Path: entry.Path, Kind: "skill", ID: entry.SkillID, Operation: "delete",
		Mode: fs.ModeSymlink | 0o777, Hash: migrate.HashFinalizationContent([]byte(target)), LinkTarget: target,
	}, nil
}

// sharedReplacementRoots maps a skill ID to the ACR shared-surface directory
// that replaces its Tessl link. Only a coordinator-owned target counts: an
// adapter's per-agent copy is not a replacement for the shared surface.
func sharedReplacementRoots(ledger realize.Ledger) map[string]string {
	roots := make(map[string]string)
	for _, target := range ledger.Targets {
		if target.Owner != realize.OwnerCoordinator {
			continue
		}
		root := sharedEntryRoot(target.Path)
		if root == "" {
			continue
		}
		for _, entry := range target.Entries {
			if current, seen := roots[entry.ArtifactID]; !seen || root < current {
				roots[entry.ArtifactID] = root
			}
		}
	}
	return roots
}

// sharedEntryRoot returns the acr__ entry directory one shared-surface target
// file belongs to.
func sharedEntryRoot(target string) string {
	rest, inside := strings.CutPrefix(target, migrate.SharedSkillsRoot+"/")
	if !inside {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	if name == "" {
		return ""
	}
	return path.Join(migrate.SharedSkillsRoot, name)
}
