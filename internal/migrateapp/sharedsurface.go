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
	blockerSharedReplacement = "shared-skill-replacement-missing"
	blockerSharedOwnership   = "shared-skill-ownership-changed"
	blockerSharedDangling    = "shared-skill-dangling-dependency"
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
			blockers = append(blockers, migrate.Blocker{
				Code: blockerSharedOrphan, Path: entry.Path, Kind: "skill", ID: entry.SkillID, Detail: entry.Reason,
				Remedy: fmt.Sprintf("remove or repoint %s, then re-run 'acr migrate tessl --finalize'", entry.Path),
			})
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
			edit, changed, err := sharedLinkDeletion(snapshot, entry)
			if err != nil {
				return nil, nil, nil, err
			}
			if changed {
				blockers = append(blockers, migrate.Blocker{
					Code: blockerSharedOwnership, Path: entry.Path, Kind: "skill", ID: entry.SkillID,
					Detail: "the link changed after it was inventoried",
					Remedy: fmt.Sprintf("re-run 'acr migrate tessl --vendor-unmapped' to re-inventory %s, then finalize", entry.Path),
				})
				continue
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
//
// The link is read again here and compared with the target the inventory
// classified. Planning's read is what the transaction accepts as its
// before-image, so a link repointed at a user tree between the two reads would
// otherwise be deleted under Tessl's proof. changed is true when the link no
// longer carries the evidence that authorized its removal.
func sharedLinkDeletion(snapshot adapter.Snapshot, entry migrate.SharedSkillEntry) (edit *migrate.FinalizeEdit, changed bool, err error) {
	links, ok := snapshot.(adapter.LinkSnapshot)
	if !ok {
		return nil, false, fmt.Errorf("project snapshot does not support link inspection")
	}
	target, err := links.ReadLink(entry.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect %q: %w", entry.Path, err)
	}
	if target != entry.Target {
		return nil, true, nil
	}
	return &migrate.FinalizeEdit{
		Path: entry.Path, Kind: "skill", ID: entry.SkillID, Operation: "delete",
		Mode: fs.ModeSymlink | 0o777, Hash: migrate.HashFinalizationContent([]byte(target)), LinkTarget: target,
	}, false, nil
}

// danglingSharedLinkBlockers refuses a finalization that would leave a
// retained shared link pointing at nothing.
//
// A link ACR keeps — a user's own alias, a Tessl link it could not prove — is
// only safe while its target survives. The plan removes files well outside
// .tessl: every per-agent tessl__ native it positively owns is a deletion too,
// so a prefix test against .tessl proves nothing. The comparison is against
// the removal plan that was actually built, and it runs before any mutation is
// staged.
//
// The target is resolved lexically and never followed. A link escaping the
// project root names something finalization cannot reach, so it stays safe.
func danglingSharedLinkBlockers(inventory migrate.Report, plan migrate.FinalizePlan) []migrate.Blocker {
	removed := make(map[string]struct{}, len(plan.Edits))
	for _, edit := range plan.Edits {
		if edit.Operation == "delete" {
			removed[edit.Path] = struct{}{}
		}
	}
	var blockers []migrate.Blocker
	for _, entry := range inventory.SharedSkills {
		if entry.Target == "" {
			continue
		}
		if entry.Disposition != migrate.SharedSkillUser && entry.Disposition != migrate.SharedSkillRetained {
			continue
		}
		resolved, inside := migrate.ResolveSharedLinkTarget(entry.Target)
		if !inside || !removalReaches(removed, resolved) {
			continue
		}
		blockers = append(blockers, migrate.Blocker{
			Code: blockerSharedDangling, Path: entry.Path, Kind: "skill", ID: entry.SkillID,
			Detail: "the retained link depends on " + resolved + ", which this finalization removes",
			Remedy: fmt.Sprintf("remove or repoint %s, then re-run 'acr migrate tessl --finalize'", entry.Path),
		})
	}
	return blockers
}

// removalReaches reports whether the plan deletes resolved itself or anything
// beneath it. A link naming a directory dangles once the last file under it is
// gone, and finalization then removes the emptied directory as well.
func removalReaches(removed map[string]struct{}, resolved string) bool {
	if _, exact := removed[resolved]; exact {
		return true
	}
	prefix := resolved + "/"
	for path := range removed {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
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
