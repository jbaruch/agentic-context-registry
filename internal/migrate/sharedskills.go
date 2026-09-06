package migrate

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

// SharedSkillsRoot is the shared surface Tessl writes in addition to the
// per-agent trees. It has no config file and no client of its own, so it is
// classified per entry rather than as an agent.
const SharedSkillsRoot = ".agents/skills"

// tesslSharedPrefix is the Tessl-owned entry prefix on the shared surface.
const tesslSharedPrefix = "tessl__"

// Shared-surface dispositions.
const (
	// SharedSkillRemovable is a Tessl link finalization may retire once ACR
	// has materialized an equivalent entry.
	SharedSkillRemovable = "removable"
	// SharedSkillRetained is evidence finalization leaves exactly as it is.
	SharedSkillRetained = "retained"
	// SharedSkillUser is a non-Tessl entry ACR never takes ownership of. Its
	// link target is still recorded, so finalization can prove the entry
	// survives the removals it plans.
	SharedSkillUser = "user"
	// SharedSkillBlocked is a Tessl link finalization can neither retire nor
	// safely leave behind.
	SharedSkillBlocked = "blocked"
)

// SharedSurfaceSymlinkError refuses a project whose shared skill surface is
// itself a symbolic link. ACR writes real files there and never writes through
// a link it does not own, so the whole inventory refuses rather than
// classifying entries reached through one.
type SharedSurfaceSymlinkError struct {
	Path string
}

func (err *SharedSurfaceSymlinkError) Error() string {
	return err.Path + " is a symbolic link; replace it with a real directory so ACR can own the shared skill entries it writes there"
}

// RefuseSymlinkedSharedSurface fails closed before any surface entry is read.
func RefuseSymlinkedSharedSurface(snapshot adapter.Snapshot) error {
	directories, err := directorySnapshot(snapshot)
	if err != nil {
		return err
	}
	entries, err := readDir(directories, ".agents")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Path == SharedSkillsRoot && entry.Mode&fs.ModeSymlink != 0 {
			return &SharedSurfaceSymlinkError{Path: SharedSkillsRoot}
		}
	}
	return nil
}

// Shared-surface reasons.
const (
	reasonSharedNonSymlink     = "non-symlink-shared-entry"
	reasonSharedForeign        = "foreign-shared-link"
	reasonSharedUserEntry      = "user-shared-entry"
	reasonSharedNotMigrating   = "artifact-not-migratable"
	reasonSharedUnprovenTarget = "unproven-shared-link-target"
)

// SharedSkillEntry is one classified entry on the shared skill surface.
// Target is the link's own bytes, never a followed path.
type SharedSkillEntry struct {
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	Target      string `json:"target,omitempty"`
	SkillID     string `json:"skillId,omitempty"`
	Package     string `json:"package,omitempty"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
}

// classifySharedSkills records every entry on the shared skill surface with
// the disposition finalization must honour. Coverage is computed from these
// entries plus the realization ledger, never declared: see
// migrateapp.sharedSurfaceBlockers.
func classifySharedSkills(snapshot adapter.Snapshot, installs []PackageInstall, report *Report) error {
	directories, err := directorySnapshot(snapshot)
	if err != nil {
		return err
	}
	links, hasLinks := snapshot.(adapter.LinkSnapshot)
	if !hasLinks {
		return fmt.Errorf("project snapshot does not support link inspection; inventory a real project tree")
	}
	entries, err := readDir(directories, SharedSkillsRoot)
	if err != nil {
		return err
	}
	claimed := migratableSkillNatives(installs, report)
	for _, entry := range entries {
		base := path.Base(entry.Path)
		if !strings.HasPrefix(base, tesslSharedPrefix) {
			// A user entry stays user-owned, but its link target is recorded
			// and checked against the removal plan later: finalization removes
			// files this link can point at, and a retained link whose target
			// disappears is the dangling reference the contract exists to
			// prevent. Reading the link grants no deletion ownership and never
			// follows it.
			record := SharedSkillEntry{
				Path: entry.Path, Kind: entryKind(entry.Mode), Disposition: SharedSkillUser, Reason: reasonSharedUserEntry,
			}
			if entry.Mode&fs.ModeSymlink != 0 {
				target, readErr := links.ReadLink(entry.Path)
				if readErr != nil {
					return fmt.Errorf("inspect %q: %w", entry.Path, readErr)
				}
				record.Target = target
			}
			report.SharedSkills = append(report.SharedSkills, record)
			continue
		}
		if entry.Mode&fs.ModeSymlink == 0 {
			report.SharedSkills = append(report.SharedSkills, SharedSkillEntry{
				Path: entry.Path, Kind: entryKind(entry.Mode), Disposition: SharedSkillRetained, Reason: reasonSharedNonSymlink,
			})
			continue
		}
		target, readErr := links.ReadLink(entry.Path)
		if readErr != nil {
			return fmt.Errorf("inspect %q: %w", entry.Path, readErr)
		}
		record := SharedSkillEntry{Path: entry.Path, Kind: "symlink", Target: target}
		classifySharedLink(&record, target, installs, claimed)
		report.SharedSkills = append(report.SharedSkills, record)
	}
	sort.Slice(report.SharedSkills, func(left, right int) bool {
		return report.SharedSkills[left].Path < report.SharedSkills[right].Path
	})
	return nil
}

// classifySharedLink decides one tessl__ link's disposition from its own
// bytes. The target is never followed. A stored `name/..` pair is not
// collapsed: the kernel follows `name` first, so a cleaned destination
// inside `.tessl/**` is not proof the link names the declared skill.
func classifySharedLink(record *SharedSkillEntry, target string, installs []PackageInstall, claimed map[string]sharedClaim) {
	if sharedLinkRetirementErasesComponent(target) {
		record.Disposition = SharedSkillRetained
		record.Reason = reasonSharedUnprovenTarget
		return
	}
	resolved, inside := resolveSharedLinkTarget(target)
	if !inside {
		record.Disposition = SharedSkillRetained
		record.Reason = reasonSkillEscape
		return
	}
	if resolved != ".tessl" && !strings.HasPrefix(resolved, ".tessl/") {
		// Not Tessl state, so this link is not ACR's to retire. Whether its
		// target survives is a separate question the finalization plan answers
		// once it is built: see migrateapp.danglingSharedLinkBlockers, which
		// checks every retained link against the actual removals.
		record.Disposition = SharedSkillRetained
		record.Reason = reasonSharedForeign
		return
	}
	claim, ok := claimed[resolved]
	if !ok {
		// The link names Tessl state finalization deletes, so leaving it would
		// produce exactly the dangling reference #8 exists to prevent.
		record.Disposition = SharedSkillBlocked
		record.Reason = reasonOrphanNative
		record.Package = installOwning(resolved, installs)
		return
	}
	record.SkillID = claim.skillID
	record.Package = claim.identity
	if !claim.migratable {
		record.Disposition = SharedSkillBlocked
		record.Reason = reasonSharedNotMigrating
		return
	}
	record.Disposition = SharedSkillRemovable
}

// ResolveSharedLinkDependency places one shared-surface link target against
// the project so the confined dependency walk can inspect it. inside is false
// only when the stored path never enters this project, in which case
// finalization cannot reach it and the link is safe by construction.
//
// Stored components are preserved in filesystem order. path.Clean and
// filepath.Clean collapse `shortcut/..` before the walk can Lstat `shortcut`,
// and the kernel does not: it follows the unowned link first. Classification
// of tessl__ links refuses that same `name/..` shape rather than treating the
// cleaned destination as equivalent ownership.
//
// An absolute pathname can name a file inside this very project. Containment
// is a prefix of the stored bytes against the project root, the root's own
// resolved form, and the same match after skipping `.` and empty components
// or evaluating a prefix that is an ancestor of the project — /tmp against
// /private/tmp, or parent/./basename, without collapsing `shortcut/..`.
// Failed prefix matching is not proof the target is outside. No component of
// the target is ever followed to gain deletion ownership.
func ResolveSharedLinkDependency(projectDirectory, target string) (string, bool) {
	if target == "" {
		return "", false
	}
	if !filepath.IsAbs(target) {
		return resolveSharedLinkDependencyRelative(target)
	}
	return resolveSharedLinkDependencyAbsolute(projectDirectory, target)
}

// resolveSharedLinkDependencyRelative joins the stored target to the shared
// surface directory without cleaning it. Leading `../` out of `.agents/skills`
// is ordinary path motion; an interior `shortcut/..` must still name
// `shortcut` when the walk inspects it.
func resolveSharedLinkDependencyRelative(target string) (string, bool) {
	if strings.HasPrefix(target, "/") {
		return "", false
	}
	return SharedSkillsRoot + "/" + filepath.ToSlash(target), true
}

// resolveSharedLinkDependencyAbsolute strips the project root from an
// absolute target without cleaning the stored bytes, so an interior
// `shortcut/..` survives into the walk.
func resolveSharedLinkDependencyAbsolute(projectDirectory, target string) (string, bool) {
	slashed := filepath.ToSlash(target)
	for _, root := range projectDependencyRoots(projectDirectory) {
		rest, ok := cutProjectPrefix(root, slashed)
		if !ok || rest == "" || rest == "." {
			continue
		}
		return rest, true
	}
	return placeAbsoluteByComponents(projectDirectory, slashed)
}

// placeAbsoluteByComponents finds the first stored prefix that is this
// project, skipping `.` and empty components and evaluating ancestor
// prefixes, then returns the remaining stored components uncleaned.
func placeAbsoluteByComponents(projectDirectory, target string) (string, bool) {
	roots := projectDependencyRoots(projectDirectory)
	comps := strings.Split(target, "/")
	var built []string
	for i, component := range comps {
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			if absoluteBuiltMatchesRoot(built, roots) {
				return strings.Join(comps[i:], "/"), true
			}
			if len(built) > 0 {
				built = built[:len(built)-1]
			}
			continue
		}
		built = append(built, component)
		if absoluteBuiltMatchesRoot(built, roots) {
			return strings.Join(comps[i+1:], "/"), true
		}
		evaluated, err := filepath.EvalSymlinks("/" + strings.Join(built, "/"))
		if err != nil {
			continue
		}
		evalSlash := strings.TrimSuffix(filepath.ToSlash(filepath.Clean(evaluated)), "/")
		for _, root := range roots {
			if evalSlash == strings.TrimSuffix(root, "/") {
				return strings.Join(comps[i+1:], "/"), true
			}
		}
	}
	return "", false
}

func absoluteBuiltMatchesRoot(built, roots []string) bool {
	if len(built) == 0 {
		return false
	}
	current := "/" + strings.Join(built, "/")
	for _, root := range roots {
		if current == strings.TrimSuffix(root, "/") {
			return true
		}
	}
	return false
}

func projectDependencyRoots(projectDirectory string) []string {
	roots := []string{filepath.ToSlash(filepath.Clean(projectDirectory))}
	if evaluated, err := filepath.EvalSymlinks(projectDirectory); err == nil {
		cleaned := filepath.ToSlash(filepath.Clean(evaluated))
		if cleaned != roots[0] {
			roots = append(roots, cleaned)
		}
	}
	return roots
}

func cutProjectPrefix(root, target string) (string, bool) {
	root = strings.TrimSuffix(root, "/")
	if target == root {
		return "", true
	}
	prefix := root + "/"
	if !strings.HasPrefix(target, prefix) {
		return "", false
	}
	return strings.TrimPrefix(target, prefix), true
}

// sharedLinkRetirementErasesComponent reports a stored tessl__ target whose
// `..` would discard a named component after leaving `.agents/skills`. The
// kernel inspects that name before applying `..`; cleaning it away is how a
// distinct user skill is mistaken for the declared package skill.
func sharedLinkRetirementErasesComponent(target string) bool {
	if target == "" || strings.HasPrefix(target, "/") {
		return false
	}
	var parts []string
	leftSurface := false
	for _, component := range strings.Split(SharedSkillsRoot+"/"+filepath.ToSlash(target), "/") {
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			if len(parts) == 0 {
				leftSurface = true
				continue
			}
			if leftSurface {
				return true
			}
			parts = parts[:len(parts)-1]
			if len(parts) == 0 {
				leftSurface = true
			}
			continue
		}
		parts = append(parts, component)
		if !onSharedSurfacePrefix(parts) {
			leftSurface = true
		}
	}
	return false
}

func onSharedSurfacePrefix(parts []string) bool {
	switch len(parts) {
	case 1:
		return parts[0] == ".agents"
	case 2:
		return parts[0] == ".agents" && parts[1] == "skills"
	default:
		return false
	}
}

// resolveSharedLinkTarget normalizes a link target against the shared surface
// directory without following it. inside is false when the target is absolute
// or climbs out of the project root.
func resolveSharedLinkTarget(target string) (string, bool) {
	if target == "" || strings.HasPrefix(target, "/") {
		return "", false
	}
	resolved := path.Clean(path.Join(SharedSkillsRoot, target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") || resolved == "." {
		return "", false
	}
	return resolved, true
}

type sharedClaim struct {
	skillID    string
	identity   string
	migratable bool
}

// migratableSkillNatives indexes every declared skill's plugin-tree root by
// the path a shared link resolves to, carrying the classification
// finalization keys on. A skill an install does not declare is absent, which
// is what makes an orphan link an orphan.
func migratableSkillNatives(installs []PackageInstall, report *Report) map[string]sharedClaim {
	migratable := make(map[string]bool)
	declared := make(map[string]bool)
	for _, pkg := range report.Packages {
		for _, artifact := range pkg.Artifacts {
			if artifact.Kind != kindSkill {
				continue
			}
			key := pkg.TesslIdentity + "\x00" + artifact.ID
			declared[key] = true
			migratable[key] = artifact.Classification == classMigratable && len(artifact.Lossy) == 0
		}
	}
	claims := make(map[string]sharedClaim)
	for _, install := range installs {
		for _, skill := range install.Skills {
			key := install.TesslIdentity + "\x00" + skill.ID
			if !declared[key] {
				continue
			}
			claims[posixJoin(install.Root, skill.Path)] = sharedClaim{
				skillID: skill.ID, identity: install.TesslIdentity, migratable: migratable[key],
			}
		}
	}
	return claims
}

func entryKind(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	case mode.IsDir():
		return "directory"
	case mode.IsRegular():
		return "file"
	default:
		return "special"
	}
}

func installOwning(resolved string, installs []PackageInstall) string {
	for _, install := range installs {
		root := strings.TrimSuffix(install.Root, "/")
		if resolved == root || strings.HasPrefix(resolved, root+"/") {
			return install.TesslIdentity
		}
	}
	return ""
}
