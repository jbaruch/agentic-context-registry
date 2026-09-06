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
	// SharedSkillUser is a non-Tessl entry ACR never inspects further.
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
	reasonSharedNonSymlink   = "non-symlink-shared-entry"
	reasonSharedForeign      = "foreign-shared-link"
	reasonSharedUserEntry    = "user-shared-entry"
	reasonSharedNotMigrating = "artifact-not-migratable"
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
			// A user entry stays user-owned, but its link target is recorded:
			// finalization removes files this link can point at, and a
			// retained link whose target disappears is the dangling reference
			// the contract exists to prevent. Reading the link grants no
			// deletion ownership and never follows it.
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
// bytes. The target is resolved lexically against the surface directory and
// never followed, so a link escaping the project root is classified without
// reading anything outside it.
func classifySharedLink(record *SharedSkillEntry, target string, installs []PackageInstall, claimed map[string]sharedClaim) {
	resolved, inside := resolveSharedLinkTarget(target)
	if !inside {
		record.Disposition = SharedSkillRetained
		record.Reason = reasonSkillEscape
		return
	}
	if resolved != ".tessl" && !strings.HasPrefix(resolved, ".tessl/") {
		// Finalization removes nothing outside .tessl, so this link keeps
		// pointing at a live target and is safe to leave in place.
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

// ResolveSharedLinkDependency resolves one shared-surface link target to the
// project-relative path it names, whether the link is relative or absolute.
// inside is false only when the target provably lies outside this project, in
// which case finalization cannot reach it and the link is safe by
// construction.
//
// An absolute pathname can name a file inside this very project. Containment
// is decided lexically against the project root, and against the root's own
// resolved form so a project reached through a symlinked ancestor — /tmp
// against /private/tmp, say — is still recognized. No component of the target
// is ever followed: a link ACR does not own must not be traversed, and a
// target it cannot place stays outside and retained.
func ResolveSharedLinkDependency(projectDirectory, target string) (string, bool) {
	if target == "" {
		return "", false
	}
	if !filepath.IsAbs(target) {
		return resolveSharedLinkTarget(target)
	}
	cleaned := filepath.Clean(target)
	roots := []string{filepath.Clean(projectDirectory)}
	if evaluated, err := filepath.EvalSymlinks(projectDirectory); err == nil {
		roots = append(roots, filepath.Clean(evaluated))
	}
	for _, root := range roots {
		relative, err := filepath.Rel(root, cleaned)
		if err != nil {
			continue
		}
		relative = filepath.ToSlash(relative)
		if relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
			continue
		}
		return relative, true
	}
	return "", false
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
