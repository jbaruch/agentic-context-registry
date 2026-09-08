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
//
// Reviewed-change acceptance does not reach this surface. Only a rule can be
// lossy, and the shared surface carries skills, so an accepted artifact never
// changes a link's disposition. A lossy skill would refuse here, which is the
// safe outcome, not a silent removal.
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

// Bounded inspection budgets. A retained route or bundle that exhausts one of
// them is a named uncertainty refusal with a remedy, never a silent success.
const (
	maxRouteComponents = 4096
	maxRouteExpansions = 40
	maxBundleEntries   = 4096
	maxBundleDepth     = 16
	maxTesslTreeDepth  = 64
)

// retainedDependency is one entry finalization keeps whose survival must be
// proved before any mutation: a shared-surface link, a shared-surface real
// directory whose contents can depend on removed state, or a per-agent
// tessl__ entry planning refused to retire.
type retainedDependency struct {
	path      string
	kind      string
	id        string
	target    string
	directory bool
}

// closureEntry binds one planned removal to the identity the filesystem
// reports for it, so a differently spelled route into the same object is
// recognized without transforming any name.
type closureEntry struct {
	path      string
	info      fs.FileInfo
	directory bool
}

type closureVerdict int

const (
	closureUnaffected closureVerdict = iota
	closureRemoved
	closureUncertain
)

// removalClosure is everything this finalization destroys: every planned
// delete edit plus every .tessl directory the pruning pass empties, including
// one that was already empty and every ancestor emptied with it.
//
// Membership is answered by path when the retained route spells it the way
// the plan does, and otherwise by the identity the filesystem itself reports:
// a failed spelling match is not proof that a route is outside. A directory
// cannot be hard-linked, so identity settles it for one. A non-directory can
// carry several entries on one inode, and unlinking the planned entry then
// does not unlink the other, so identity alone is uncertainty there rather
// than either proof or safety.
type removalClosure struct {
	paths       map[string]bool
	directories []closureEntry
	files       []closureEntry
}

func newRemovalClosure(root *os.Root, plan migrate.FinalizePlan) (*removalClosure, error) {
	closure := &removalClosure{paths: make(map[string]bool, len(plan.Edits))}
	for _, edit := range plan.Edits {
		if edit.Operation == "delete" {
			closure.paths[edit.Path] = false
		}
	}
	pruned, err := prunedTesslDirectories(root, closure.paths)
	if err != nil {
		return nil, err
	}
	for _, directory := range pruned {
		closure.paths[directory] = true
	}
	ordered := make([]string, 0, len(closure.paths))
	for candidate := range closure.paths {
		ordered = append(ordered, candidate)
	}
	sort.Strings(ordered)
	for _, candidate := range ordered {
		info, err := root.Lstat(candidate)
		if errors.Is(err, fs.ErrNotExist) {
			// Planning read it moments ago. Something else unlinked it since,
			// so it is no longer an identity a retained route can reach.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect planned removal %q: %w", candidate, err)
		}
		entry := closureEntry{path: candidate, info: info, directory: closure.paths[candidate]}
		if entry.directory {
			closure.directories = append(closure.directories, entry)
			continue
		}
		closure.files = append(closure.files, entry)
	}
	return closure, nil
}

// inspect reports how the plan affects one live object. relative is empty for
// a path outside the project, where only identity can answer.
func (closure *removalClosure) inspect(relative string, info fs.FileInfo) (closureVerdict, string) {
	if relative != "" {
		if _, removed := closure.paths[relative]; removed {
			return closureRemoved, relative
		}
	}
	if info.IsDir() {
		for _, candidate := range closure.directories {
			if os.SameFile(candidate.info, info) {
				return closureRemoved, candidate.path
			}
		}
		return closureUnaffected, ""
	}
	for _, candidate := range closure.files {
		if os.SameFile(candidate.info, info) {
			return closureUncertain, candidate.path
		}
	}
	return closureUnaffected, ""
}

// prunedTesslDirectories names every directory under .tessl that
// removeEmptyTesslDirectories deletes once the planned edits are applied: one
// whose entries are all removed or themselves pruned. A directory that was
// already empty is pruned too, which is why a retained route can traverse it
// today and lose it to this run.
func prunedTesslDirectories(root *os.Root, removed map[string]bool) ([]string, error) {
	info, err := root.Lstat(".tessl")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect .tessl: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return nil, nil
	}
	var pruned []string
	var visit func(directory string, depth int) (bool, error)
	visit = func(directory string, depth int) (bool, error) {
		if depth > maxTesslTreeDepth {
			return false, fmt.Errorf("%q nests deeper than %d directories; flatten the Tessl state and re-run 'acr migrate tessl --finalize'", directory, maxTesslTreeDepth)
		}
		entries, err := readRootDir(root, directory)
		if err != nil {
			return false, err
		}
		empties := true
		for _, entry := range entries {
			child := path.Join(directory, entry.Name())
			if entry.IsDir() {
				childEmpties, err := visit(child, depth+1)
				if err != nil {
					return false, err
				}
				if !childEmpties {
					empties = false
				}
				continue
			}
			if _, gone := removed[child]; !gone {
				empties = false
			}
		}
		if empties {
			pruned = append(pruned, directory)
		}
		return empties, nil
	}
	if _, err := visit(".tessl", 0); err != nil {
		return nil, err
	}
	return pruned, nil
}

func readRootDir(root *os.Root, directory string) (entries []os.DirEntry, err error) {
	handle, err := root.Open(directory)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", directory, err)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	entries, err = handle.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", directory, err)
	}
	return entries, nil
}

// routeLocation is where an inspection currently stands: a project-relative
// path while inside the opened root, an absolute slash path once the route
// has climbed out of it.
type routeLocation struct {
	inProject bool
	relative  string
	absolute  string
}

func (location routeLocation) display() string {
	if location.inProject {
		return location.relative
	}
	return location.absolute
}

// projectPath is the spelling the closure can match by path. It is empty
// outside the project, where only identity answers.
func (location routeLocation) projectPath() string {
	if location.inProject {
		return location.relative
	}
	return ""
}

type routeVerdict int

const (
	routeSafe routeVerdict = iota
	// routeBroken is a demonstrated missing component: the entry was already
	// broken before finalization, and repairing it is not this run's to do.
	routeBroken
	routeDangling
	routeUnproven
)

type routeResult struct {
	verdict  routeVerdict
	cause    string
	endpoint routeLocation
}

func unprovenRoute(cause string) routeResult {
	return routeResult{verdict: routeUnproven, cause: cause}
}

// routeWalker proves what a retained entry depends on. It inspects every
// stored component in filesystem order, before later `..` can discard it, and
// expands an external link's own stored target rather than accepting the
// endpoint that link evaluates to: equality with the project today proves
// nothing about the directories the route passes through.
type routeWalker struct {
	root        *os.Root
	project     string
	projectInfo fs.FileInfo
	closure     *removalClosure
	components  int
}

// prove walks one retained entry's route and then inspects the skill it
// actually opens.
func (walker *routeWalker) prove(entry retainedDependency) (routeResult, error) {
	endpoint := routeLocation{inProject: true, relative: entry.path}
	if !entry.directory {
		start := routeLocation{inProject: true, relative: path.Dir(entry.path)}
		if filepath.IsAbs(entry.target) {
			start = routeLocation{absolute: "/"}
		}
		result, err := walker.walk(start, strings.Split(filepath.ToSlash(entry.target), "/"))
		if err != nil || result.verdict != routeSafe {
			return result, err
		}
		endpoint = result.endpoint
	}
	if endpoint.inProject {
		if removed := removalReachesBeneath(walker.closure, endpoint.relative); removed {
			return routeResult{verdict: routeDangling, cause: endpoint.relative}, nil
		}
	}
	return walker.bundle(endpoint)
}

// walk inspects one stored route component by component.
func (walker *routeWalker) walk(start routeLocation, components []string) (routeResult, error) {
	current := start
	queue := append([]string(nil), components...)
	expansions := 0
	for len(queue) != 0 {
		component := queue[0]
		queue = queue[1:]
		if component == "" || component == "." {
			continue
		}
		walker.components++
		if walker.components > maxRouteComponents {
			return unprovenRoute(fmt.Sprintf("the route inspects more than %d components", maxRouteComponents)), nil
		}
		if component == ".." {
			current = walker.parent(current)
			continue
		}
		candidate := walker.child(current, component)
		info, err := walker.lstat(candidate)
		if errors.Is(err, fs.ErrNotExist) {
			return routeResult{verdict: routeBroken}, nil
		}
		if err != nil {
			return unprovenRoute(fmt.Sprintf("cannot inspect %q: %v", candidate.display(), err)), nil
		}
		verdict, removed := walker.closure.inspect(candidate.projectPath(), info)
		switch verdict {
		case closureRemoved:
			return routeResult{verdict: routeDangling, cause: removed}, nil
		case closureUncertain:
			return unprovenRoute(fmt.Sprintf("%q shares an inode with %q, which this finalization removes, and unlinking one entry does not unlink the other", candidate.display(), removed)), nil
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if candidate.inProject {
				// Following a link ACR does not own is the traversal that
				// grants no ownership and can leave the project.
				return unprovenRoute(fmt.Sprintf("%q is a link ACR does not own", candidate.display())), nil
			}
			expansions++
			if expansions > maxRouteExpansions {
				return unprovenRoute(fmt.Sprintf("the route expands more than %d links, or cycles through them", maxRouteExpansions)), nil
			}
			target, readErr := os.Readlink(filepath.FromSlash(candidate.absolute))
			if readErr != nil {
				return unprovenRoute(fmt.Sprintf("cannot read the link %q: %v", candidate.absolute, readErr)), nil
			}
			target = filepath.ToSlash(target)
			if strings.HasPrefix(target, "/") {
				current = routeLocation{absolute: "/"}
			}
			queue = append(strings.Split(target, "/"), queue...)
			continue
		}
		if !info.IsDir() && len(queue) != 0 {
			// A non-directory carries no remaining component, `.` and `..`
			// included: this route was already broken before finalization.
			return routeResult{verdict: routeBroken}, nil
		}
		if !candidate.inProject && os.SameFile(info, walker.projectInfo) {
			candidate = routeLocation{inProject: true, relative: "."}
		}
		current = candidate
	}
	return routeResult{verdict: routeSafe, endpoint: current}, nil
}

// bundle inspects the skill a retained entry actually opens: its entrypoint
// and the bundled entries beneath it, descending only through real
// directories inside that subtree. A nested link's own route is proved with
// the same walk and never crawled through. Budget exhaustion and unreadable
// metadata are uncertainty refusals.
func (walker *routeWalker) bundle(endpoint routeLocation) (routeResult, error) {
	info, err := walker.lstat(endpoint)
	if errors.Is(err, fs.ErrNotExist) {
		return routeResult{verdict: routeBroken}, nil
	}
	if err != nil {
		return unprovenRoute(fmt.Sprintf("cannot inspect %q: %v", endpoint.display(), err)), nil
	}
	if !info.IsDir() {
		return routeResult{verdict: routeSafe, endpoint: endpoint}, nil
	}
	type frame struct {
		location routeLocation
		depth    int
	}
	pending := []frame{{location: endpoint}}
	inspected := 0
	for len(pending) != 0 {
		current := pending[0]
		pending = pending[1:]
		entries, readErr := walker.readDir(current.location)
		if readErr != nil {
			return unprovenRoute(fmt.Sprintf("cannot enumerate %q: %v", current.location.display(), readErr)), nil
		}
		for _, entry := range entries {
			inspected++
			if inspected > maxBundleEntries {
				return unprovenRoute(fmt.Sprintf("the retained skill holds more than %d entries to inspect", maxBundleEntries)), nil
			}
			child := walker.child(current.location, entry.Name())
			childInfo, statErr := walker.lstat(child)
			if statErr != nil {
				return unprovenRoute(fmt.Sprintf("cannot inspect %q: %v", child.display(), statErr)), nil
			}
			verdict, removed := walker.closure.inspect(child.projectPath(), childInfo)
			switch verdict {
			case closureRemoved:
				return routeResult{verdict: routeDangling, cause: removed}, nil
			case closureUncertain:
				return unprovenRoute(fmt.Sprintf("%q shares an inode with %q, which this finalization removes, and unlinking one entry does not unlink the other", child.display(), removed)), nil
			}
			if childInfo.Mode()&fs.ModeSymlink != 0 {
				target, readErr := walker.readlink(child)
				if readErr != nil {
					return unprovenRoute(fmt.Sprintf("cannot read the link %q: %v", child.display(), readErr)), nil
				}
				start := current.location
				if filepath.IsAbs(target) {
					start = routeLocation{absolute: "/"}
				}
				result, walkErr := walker.walk(start, strings.Split(filepath.ToSlash(target), "/"))
				if walkErr != nil {
					return routeResult{}, walkErr
				}
				if result.verdict == routeDangling || result.verdict == routeUnproven {
					return result, nil
				}
				continue
			}
			if !childInfo.IsDir() {
				continue
			}
			if current.depth+1 > maxBundleDepth {
				return unprovenRoute(fmt.Sprintf("the retained skill nests deeper than %d directories", maxBundleDepth)), nil
			}
			pending = append(pending, frame{location: child, depth: current.depth + 1})
		}
	}
	return routeResult{verdict: routeSafe, endpoint: endpoint}, nil
}

func (walker *routeWalker) parent(location routeLocation) routeLocation {
	if !location.inProject {
		return routeLocation{absolute: path.Dir(location.absolute)}
	}
	if location.relative == "." {
		return routeLocation{absolute: path.Dir(walker.project)}
	}
	return routeLocation{inProject: true, relative: path.Dir(location.relative)}
}

func (walker *routeWalker) child(location routeLocation, component string) routeLocation {
	if location.inProject {
		return routeLocation{inProject: true, relative: path.Join(location.relative, component)}
	}
	return routeLocation{absolute: path.Join(location.absolute, component)}
}

func (walker *routeWalker) lstat(location routeLocation) (fs.FileInfo, error) {
	if location.inProject {
		return walker.root.Lstat(location.relative)
	}
	return os.Lstat(filepath.FromSlash(location.absolute))
}

func (walker *routeWalker) readlink(location routeLocation) (string, error) {
	if location.inProject {
		return walker.root.Readlink(location.relative)
	}
	return os.Readlink(filepath.FromSlash(location.absolute))
}

func (walker *routeWalker) readDir(location routeLocation) ([]os.DirEntry, error) {
	if location.inProject {
		return readRootDir(walker.root, location.relative)
	}
	return os.ReadDir(filepath.FromSlash(location.absolute))
}

// projectIdentity binds the project pathname to the opened root. An
// inspection that cannot establish the identity is uncertainty, never proof
// that a retained route is safe.
func projectIdentity(root *os.Root, projectDirectory string) (string, fs.FileInfo, string) {
	project, err := filepath.EvalSymlinks(projectDirectory)
	if err != nil {
		return "", nil, fmt.Sprintf("cannot establish project identity %q: %v", projectDirectory, err)
	}
	info, err := os.Stat(project)
	if err != nil {
		return "", nil, fmt.Sprintf("cannot inspect project identity %q: %v", project, err)
	}
	rootInfo, err := root.Stat(".")
	if err != nil {
		return "", nil, fmt.Sprintf("cannot inspect the open project root: %v", err)
	}
	if !os.SameFile(info, rootInfo) {
		return "", nil, "the project path no longer identifies the open project root"
	}
	return filepath.ToSlash(project), info, ""
}

// danglingSharedLinkBlockers refuses a finalization that would leave a
// retained entry pointing at nothing, or leave the skill it opens unreadable.
//
// An entry ACR keeps — a user's own alias, a Tessl link it could not prove, a
// per-agent tessl__ link whose destination it does not own — is only safe
// while the route to it and the bundle beneath it survive. The comparison is
// against the removal plan that was actually built plus the directories
// pruning empties, and it runs before any mutation is staged.
func danglingSharedLinkBlockers(projectDirectory string, inventory migrate.Report, plan migrate.FinalizePlan) (blockers []migrate.Blocker, err error) {
	retained := retainedDependencies(inventory, plan)
	if len(retained) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return nil, fmt.Errorf("open project directory %q: %w", projectDirectory, err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	closure, err := newRemovalClosure(root, plan)
	if err != nil {
		return nil, err
	}
	project, projectInfo, identityFailure := projectIdentity(root, projectDirectory)
	if identityFailure != "" {
		for _, entry := range retained {
			blockers = append(blockers, unprovenBlocker(entry, identityFailure))
		}
		return blockers, nil
	}
	for _, entry := range retained {
		walker := &routeWalker{root: root, project: project, projectInfo: projectInfo, closure: closure}
		result, walkErr := walker.prove(entry)
		if walkErr != nil {
			return nil, walkErr
		}
		switch result.verdict {
		case routeDangling:
			blockers = append(blockers, danglingBlocker(entry, result.cause))
		case routeUnproven:
			blockers = append(blockers, unprovenBlocker(entry, result.cause))
		}
	}
	return blockers, nil
}

// retainedDependencies collects every entry whose survival this run must
// prove: the shared surface's user and retained entries, and the per-agent
// tessl__ entries planning refused to retire.
func retainedDependencies(inventory migrate.Report, plan migrate.FinalizePlan) []retainedDependency {
	var retained []retainedDependency
	for _, entry := range inventory.SharedSkills {
		if entry.Disposition != migrate.SharedSkillUser && entry.Disposition != migrate.SharedSkillRetained {
			continue
		}
		switch {
		case entry.Target != "":
			retained = append(retained, retainedDependency{path: entry.Path, kind: "skill", id: entry.SkillID, target: entry.Target})
		case entry.Kind == "directory":
			retained = append(retained, retainedDependency{path: entry.Path, kind: "skill", id: entry.SkillID, directory: true})
		}
	}
	for _, native := range plan.Unowned {
		retained = append(retained, retainedDependency{path: native.Path, kind: native.Kind, id: native.ID, target: native.Target})
	}
	return retained
}

func danglingBlocker(entry retainedDependency, cause string) migrate.Blocker {
	return migrate.Blocker{
		Code: blockerSharedDangling, Path: entry.path, Kind: entry.kind, ID: entry.id,
		Detail: "the retained entry depends on " + cause + ", which this finalization removes",
		Remedy: fmt.Sprintf("remove or repoint %s, then re-run 'acr migrate tessl --finalize'", entry.path),
	}
}

func unprovenBlocker(entry retainedDependency, cause string) migrate.Blocker {
	return migrate.Blocker{
		Code: blockerSharedUnproven, Path: entry.path, Kind: entry.kind, ID: entry.id,
		Detail: "the retained entry's survival cannot be proved: " + cause,
		Remedy: fmt.Sprintf("repoint %s at an inspectable path without unowned links, or remove it, then re-run 'acr migrate tessl --finalize'", entry.path),
	}
}

// removalReachesBeneath reports whether the plan deletes anything below
// resolved. A link naming a directory dangles once the last file under it is
// gone. The comparison is on path components, so a sibling whose name merely
// starts with the same characters is not mistaken for a child.
func removalReachesBeneath(closure *removalClosure, resolved string) bool {
	prefix := resolved + "/"
	for candidate := range closure.paths {
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
