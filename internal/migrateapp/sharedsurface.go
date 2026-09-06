package migrateapp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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
	blockerSharedUnproven    = "shared-skill-unproven-dependency"
)

// sharedSurfacePlan decides, per Tessl link on the shared skill surface,
// whether finalization retires it, leaves it, or refuses.
//
// A link is retired only on positive per-entry evidence: it is a symlink, its
// own target names an installed package's Tessl plugin tree without erasing
// an uninspected `name/..` component, the skill it names is migratable with
// nothing lossy, and ACR already owns an equivalent entry in the realization
// ledger. Anything else is retained with a reason, or blocks.
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
// The stored target is placed against the project without collapsing
// components, relative or absolute, and never followed: an absolute pathname
// can name a file inside this very project. A failed prefix match is not
// treated as an escape. A link whose stored path never enters the project
// names something finalization cannot reach, so it stays safe.
func danglingSharedLinkBlockers(projectDirectory string, inventory migrate.Report, plan migrate.FinalizePlan) ([]migrate.Blocker, error) {
	removed := make(map[string]struct{}, len(plan.Edits))
	for _, edit := range plan.Edits {
		if edit.Operation == "delete" {
			removed[edit.Path] = struct{}{}
		}
	}
	root, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return nil, fmt.Errorf("open project directory %q: %w", projectDirectory, err)
	}
	defer root.Close()
	var blockers []migrate.Blocker
	for _, entry := range inventory.SharedSkills {
		if entry.Target == "" {
			continue
		}
		if entry.Disposition != migrate.SharedSkillUser && entry.Disposition != migrate.SharedSkillRetained {
			continue
		}
		resolved, inside := migrate.ResolveSharedLinkDependency(projectDirectory, entry.Target)
		if !inside {
			continue
		}
		blocker, dependent, err := sharedLinkDependency(root, projectDirectory, removed, entry, resolved)
		if err != nil {
			return nil, err
		}
		if dependent {
			blockers = append(blockers, blocker)
		}
	}
	return blockers, nil
}

// sharedLinkDependency decides whether one retained link survives this
// finalization, walking its stored target one component at a time from the
// project root.
//
// Two things break a link, and neither is visible in a lexical comparison of
// the target alone. A Tessl skill tree is a symlink, and the plan deletes that
// one link rather than each path beneath it, so an alias into a nested
// directory loses an *ancestor* rather than its own target. And a component
// that is itself a symlink ACR does not own leads somewhere this walk cannot
// establish: following it to find out would be exactly the traversal that
// grants no ownership and could leave the project, so the dependency is
// unproven and the run refuses instead of guessing.
//
// Stored `..` and `.` are path motion, not names. They are applied only after
// the component they would discard has been inspected, so `shortcut/..`
// cannot drop an unowned link the kernel would follow. `..` through a real
// directory, and leading `../` out of `.agents/skills`, stay ordinary. `..`
// that would leave the project does not end the walk: the remaining
// components can name this same project again. A later component that is
// this project's own name at that depth re-enters it and inspection resumes.
// A name that is not an ancestor stays outside; `..` through such a name is
// unproven because the walk cannot Lstat it.
//
// The walk stops at the first symlink it meets, whether it is the target
// itself or an ancestor of it, so no unowned link is ever traversed. A
// component that does not exist ends it too: the link was already broken
// before this run, and refusing would blame finalization for it.
func sharedLinkDependency(root *os.Root, projectDirectory string, removed map[string]struct{}, entry migrate.SharedSkillEntry, resolved string) (migrate.Blocker, bool, error) {
	dangling := func(cause string) migrate.Blocker {
		return migrate.Blocker{
			Code: blockerSharedDangling, Path: entry.Path, Kind: "skill", ID: entry.SkillID,
			Detail: "the retained link depends on " + cause + ", which this finalization removes",
			Remedy: fmt.Sprintf("remove or repoint %s, then re-run 'acr migrate tessl --finalize'", entry.Path),
		}
	}
	unproven := func(cause string) migrate.Blocker {
		return migrate.Blocker{
			Code: blockerSharedUnproven, Path: entry.Path, Kind: "skill", ID: entry.SkillID,
			Detail: "the retained link reaches its target through " + cause + ", a link ACR does not own, so its survival cannot be proved without following it",
			Remedy: fmt.Sprintf("repoint %s at a path this project owns, or remove it, then re-run 'acr migrate tessl --finalize'", entry.Path),
		}
	}
	identity := identifyProject(projectDirectory)
	var parts []string
	depthAbove := 0
	var offChain []string
	for _, component := range strings.Split(resolved, "/") {
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
				continue
			}
			if len(offChain) > 0 {
				return unproven(offChain[len(offChain)-1]), true, nil
			}
			depthAbove++
			continue
		}
		if depthAbove > 0 || len(offChain) > 0 {
			if len(offChain) == 0 && identity.reenters(depthAbove, component) {
				depthAbove--
				continue
			}
			offChain = append(offChain, component)
			continue
		}
		parts = append(parts, component)
		prefix := strings.Join(parts, "/")
		if _, exact := removed[prefix]; exact {
			return dangling(prefix), true, nil
		}
		info, err := root.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			return migrate.Blocker{}, false, nil
		}
		if err != nil {
			return migrate.Blocker{}, false, fmt.Errorf("inspect %q for retained link %q: %w", prefix, entry.Path, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return unproven(prefix), true, nil
		}
	}
	if depthAbove > 0 || len(offChain) > 0 {
		return migrate.Blocker{}, false, nil
	}
	if len(parts) == 0 {
		return migrate.Blocker{}, false, nil
	}
	prefix := strings.Join(parts, "/")
	if removalReachesBeneath(removed, prefix) {
		return dangling(prefix), true, nil
	}
	return migrate.Blocker{}, false, nil
}

type projectIdentity struct {
	names     []string
	evalNames []string
}

func identifyProject(projectDirectory string) projectIdentity {
	identity := projectIdentity{names: absolutePathNames(filepath.Clean(projectDirectory))}
	evaluated, err := filepath.EvalSymlinks(projectDirectory)
	if err != nil {
		return identity
	}
	evalNames := absolutePathNames(filepath.Clean(evaluated))
	if strings.Join(evalNames, "/") != strings.Join(identity.names, "/") {
		identity.evalNames = evalNames
	}
	return identity
}

func (identity projectIdentity) reenters(depth int, name string) bool {
	return pathNameAt(identity.names, depth) == name || (len(identity.evalNames) > 0 && pathNameAt(identity.evalNames, depth) == name)
}

func pathNameAt(names []string, depth int) string {
	index := len(names) - depth
	if index < 0 || index >= len(names) {
		return ""
	}
	return names[index]
}

func absolutePathNames(projectDirectory string) []string {
	slashed := strings.Trim(filepath.ToSlash(projectDirectory), "/")
	if slashed == "" || slashed == "." {
		return nil
	}
	return strings.Split(slashed, "/")
}

// removalReachesBeneath reports whether the plan deletes anything below
// resolved. A link naming a directory dangles once the last file under it is
// gone, and finalization then removes the emptied directory as well. The
// comparison is on path components, so a sibling whose name merely starts with
// the same characters is not mistaken for a child.
func removalReachesBeneath(removed map[string]struct{}, resolved string) bool {
	prefix := resolved + "/"
	for candidate := range removed {
		if strings.HasPrefix(candidate, prefix) {
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
