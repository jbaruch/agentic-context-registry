package migrateapp

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

var applyFinalizationFileTransaction = realize.ApplyFileTransactionWithFinalizer
var readFinalizationFile = os.ReadFile

func emptyMigrationReport(options Options) migrate.MigrationReport {
	mode := "coexistence"
	if options.Finalize {
		mode = "finalize"
	}
	return migrate.MigrationReport{
		SchemaVersion: 1, DryRun: options.DryRun, Mode: mode,
		Mappings: []migrate.Mapping{}, Project: dependency.Project{}, Lock: dependency.Lockfile{},
		Plan: migrate.MigrationPlan{Operations: []migrate.MigrationOperation{}}, ToolOwned: []migrate.OwnershipRecord{},
		TesslOwned: []migrate.OwnershipRecord{}, Unmanaged: []migrate.OwnershipRecord{}, EffectiveDiffs: []migrate.EffectiveDiff{},
		Notes: []migrate.CoexistenceNote{}, Vendored: []migrate.VendoredPackage{}, Removed: []migrate.RemovalRecord{},
		Retained: []migrate.RetentionRecord{}, Reanchored: []migrate.ReanchoredTarget{}, StaleReferences: []migrate.StaleReference{},
	}
}

func planFinalization(projectDirectory string, inventory migrate.Report, ledger realize.Ledger) (plan migrate.FinalizePlan, err error) {
	snapshot, err := adapter.NewRootSnapshot(projectDirectory)
	if err != nil {
		return migrate.FinalizePlan{}, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	managed := make(map[string][]string, len(ledger.Targets))
	for _, target := range ledger.Targets {
		for _, entry := range target.Entries {
			managed[target.Path] = append(managed[target.Path], entry.ManagedHash)
		}
	}
	plan, err = migrate.PlanFinalization(snapshot, inventory)
	if err != nil {
		return migrate.FinalizePlan{}, err
	}
	for _, record := range inventory.Unsupported {
		plan.Retained = append(plan.Retained, migrate.RetentionRecord{Path: record.Path, Reason: record.Reason})
	}
	configs := []struct {
		path   string
		format adapter.ConfigFormat
	}{
		{path: ".claude/settings.json", format: adapter.ConfigJSON},
		{path: ".cursor/hooks.json", format: adapter.ConfigJSON},
		{path: ".gemini/settings.json", format: adapter.ConfigJSON},
		{path: ".github/hooks/tessl.json", format: adapter.ConfigJSON},
		{path: ".codex/config.toml", format: adapter.ConfigTOML},
	}
	for _, candidate := range configs {
		observed, readErr := snapshot.ReadFile(candidate.path)
		if errors.Is(readErr, fs.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return migrate.FinalizePlan{}, readErr
		}
		emptyHooks, findErr := preserve.FindEmptyForeignArrays(candidate.format, candidate.path, observed.Content, []string{"tessl", "hooks"})
		if findErr != nil {
			return migrate.FinalizePlan{}, findErr
		}
		for _, empty := range emptyHooks {
			plan.Retained = append(plan.Retained, migrate.RetentionRecord{
				Path: candidate.path, Kind: "structured-container", ID: "tessl.hooks." + empty.Key, Reason: "empty Tessl hook container",
			})
		}
		selectors, findErr := preserve.FindForeignConfigElementsContaining(candidate.format, candidate.path, observed.Content, []byte("tessl hook run"))
		if findErr != nil {
			return migrate.FinalizePlan{}, findErr
		}
		for _, pkg := range inventory.Packages {
			more, findErr := preserve.FindForeignConfigElementsContaining(candidate.format, candidate.path, observed.Content, []byte(".tessl/plugins/"+pkg.TesslIdentity+"/"))
			if findErr != nil {
				return migrate.FinalizePlan{}, findErr
			}
			selectors = appendForeignSelectors(selectors, more...)
		}
		if len(selectors) == 0 {
			continue
		}
		after, removed, removeErr := preserve.RemoveForeignConfigEntries(candidate.format, candidate.path, observed.Content, selectors, managed[candidate.path])
		if removeErr != nil {
			return migrate.FinalizePlan{}, removeErr
		}
		edit := migrate.FinalizeEdit{
			Path: candidate.path, Kind: "structured-entry", ID: "tessl-dispatcher", Operation: "splice",
			Before: append([]byte(nil), observed.Content...), After: append([]byte(nil), after...), Mode: observed.Mode.Perm(), Hash: migrate.HashFinalizationContent(observed.Content),
		}
		for _, item := range removed {
			edit.Removed = append(edit.Removed, migrate.RemovalRecord{
				Path: candidate.path, Kind: "structured-entry", ID: tesslSpliceID(item, inventory),
				Operation: "splice", Hash: migrate.HashFinalizationContent(item.Raw),
			})
		}
		plan.Edits = append(plan.Edits, edit)
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

func tesslSpliceID(splice preserve.ForeignSplice, inventory migrate.Report) string {
	for _, pkg := range inventory.Packages {
		if bytes.Contains(splice.Raw, []byte(".tessl/plugins/"+pkg.TesslIdentity)) {
			return "tessl.hooks." + pkg.TesslIdentity
		}
	}
	return "tessl-dispatcher"
}

func appendForeignSelectors(values []preserve.ForeignSelector, additions ...preserve.ForeignSelector) []preserve.ForeignSelector {
	for _, addition := range additions {
		duplicate := false
		for _, value := range values {
			if value.Kind == addition.Kind && strings.Join(value.Container, "\x00") == strings.Join(addition.Container, "\x00") && bytes.Equal(value.Raw, addition.Raw) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			values = append(values, addition)
		}
	}
	return values
}

func ensureFinalizationTracked(projectDirectory string, state dependency.State) (bool, error) {
	inside := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	inside.Dir = projectDirectory
	output, err := inside.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 && bytes.Contains(output, []byte("not a git repository")) {
			return false, nil
		}
		return false, fmt.Errorf("inspect Git work tree: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if strings.TrimSpace(string(output)) != "true" {
		return false, fmt.Errorf("inspect Git work tree: unexpected git rev-parse output %q", strings.TrimSpace(string(output)))
	}
	command := exec.Command("git", "ls-files", "-z", "--", "tessl.json", ".agents/vendor")
	command.Dir = projectDirectory
	output, err = command.Output()
	if err != nil {
		return true, fmt.Errorf("inspect tracked finalization state: %w", err)
	}
	tracked := make(map[string]bool)
	for _, filename := range bytes.Split(output, []byte{0}) {
		if len(filename) != 0 {
			tracked[filepath.ToSlash(string(filename))] = true
		}
	}
	if !tracked["tessl.json"] {
		return true, &Error{Code: "finalization_blocked", Message: "tessl.json is not tracked by Git; run `git add tessl.json && git commit` first — the committed manifest is the only undo for finalization (`git checkout tessl.json && tessl install`).", Remedy: "git add tessl.json && git commit"}
	}
	var untracked []string
	for _, locked := range state.Lock.Dependencies {
		if locked.Kind != dependency.ResolutionVendor {
			continue
		}
		identity, err := dependency.ParseVendorSource(locked.Source)
		if err != nil {
			return true, err
		}
		root := filepath.Join(projectDirectory, ".agents", "vendor", identity.Workspace, identity.Package)
		err = filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			relative, relErr := filepath.Rel(projectDirectory, filename)
			if relErr != nil {
				return relErr
			}
			relative = filepath.ToSlash(relative)
			if !tracked[relative] {
				untracked = append(untracked, relative)
			}
			return nil
		})
		if err != nil {
			return true, err
		}
	}
	if len(untracked) != 0 {
		sort.Strings(untracked)
		ignoredBy, err := survivingAgentsIgnore(projectDirectory)
		if err != nil {
			return true, fmt.Errorf("inspect surviving .gitignore: %w", err)
		}
		message := "vendored package files are not tracked by Git: " + strings.Join(untracked, ", ")
		remedy := "git add .agents/vendor && git commit"
		if ignoredBy != "" {
			message += "; the surviving ignore pattern " + ignoredBy + " hides the vendor tree"
			remedy = "replace " + ignoredBy + " with '/.agents/*' and '!/.agents/vendor/', then run git add .agents/vendor && git commit"
		} else {
			message += "; track the vendor tree before finalization"
		}
		return true, &Error{Code: "finalization_blocked", Message: message, Remedy: remedy}
	}
	return true, nil
}

func survivingAgentsIgnore(projectDirectory string) (string, error) {
	content, err := readFinalizationFile(filepath.Join(projectDirectory, ".gitignore"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	inTesslBlock := false
	for index, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# === Tessl-generated artifacts") {
			inTesslBlock = true
			continue
		}
		if inTesslBlock && strings.HasPrefix(trimmed, "# === end Tessl-generated artifacts") {
			inTesslBlock = false
			continue
		}
		if inTesslBlock || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "!") {
			continue
		}
		switch trimmed {
		case ".agents", ".agents/", "/.agents", "/.agents/", ".agents/*", "/.agents/*":
			return fmt.Sprintf(".gitignore:%d %q", index+1, trimmed), nil
		}
	}
	return "", nil
}

func applyFinalization(projectDirectory string, state *dependency.State, plan migrate.FinalizePlan) ([]migrate.ReanchoredTarget, error) {
	ledger, err := realize.DecodeLedger(state.Lock.Realization)
	if err != nil {
		return nil, err
	}
	var reanchored []migrate.ReanchoredTarget
	for index := range ledger.Targets {
		target := &ledger.Targets[index]
		for _, edit := range plan.Edits {
			if edit.Path != target.Path || edit.Operation != "splice" {
				continue
			}
			before := target.OutputHash
			target.OutputHash = migrate.HashFinalizationContent(edit.After)
			target.Mode = uint32(edit.Mode.Perm())
			if before != target.OutputHash {
				reanchored = append(reanchored, migrate.ReanchoredTarget{Path: target.Path, BeforeHash: before, AfterHash: target.OutputHash})
			}
		}
	}
	encoded, err := realize.EncodeLedger(ledger)
	if err != nil {
		return nil, err
	}
	next := *state
	next.Lock.Realization = encoded
	projectData, lockData, err := dependency.MarshalState(next)
	if err != nil {
		return nil, err
	}
	transactionEdits := make([]realize.FileTransactionEdit, 0, len(plan.Edits)+2)
	for _, edit := range plan.Edits {
		operation := edit.Operation
		if operation == "delete" {
			operation = "remove"
		}
		beforeMode := uint32(edit.Mode.Perm())
		if edit.LinkTarget != "" {
			beforeMode = 0
		}
		transactionEdits = append(transactionEdits, realize.FileTransactionEdit{
			Path: edit.Path, Operation: operation, Before: append([]byte(nil), edit.Before...), After: append([]byte(nil), edit.After...),
			BeforeMode: beforeMode, AfterMode: uint32(edit.Mode.Perm()), LinkTarget: edit.LinkTarget,
		})
	}
	for _, stateFile := range []struct {
		path    string
		content []byte
	}{{path: dependency.ProjectFilename, content: projectData}, {path: dependency.LockFilename, content: lockData}} {
		edit, changed, err := stateFileTransactionEdit(projectDirectory, stateFile.path, stateFile.content)
		if err != nil {
			return nil, err
		}
		if changed {
			transactionEdits = append(transactionEdits, edit)
		}
	}
	if err := applyFinalizationFileTransaction(projectDirectory, transactionEdits, func() error {
		return removeEmptyTesslDirectories(projectDirectory)
	}); err != nil {
		var conflict *realize.FileTransactionConflictError
		if errors.As(err, &conflict) {
			return nil, namedError(cli.CodeFinalizationConflict, err.Error(), err)
		}
		return nil, err
	}
	*state = next
	return reanchored, nil
}

func plannedReanchors(ledger realize.Ledger, plan migrate.FinalizePlan) []migrate.ReanchoredTarget {
	var result []migrate.ReanchoredTarget
	for _, target := range ledger.Targets {
		for _, edit := range plan.Edits {
			if edit.Path != target.Path || edit.Operation != "splice" {
				continue
			}
			after := migrate.HashFinalizationContent(edit.After)
			if target.OutputHash != after {
				result = append(result, migrate.ReanchoredTarget{Path: target.Path, BeforeHash: target.OutputHash, AfterHash: after})
			}
		}
	}
	return result
}

func stateFileTransactionEdit(projectDirectory, relative string, after []byte) (realize.FileTransactionEdit, bool, error) {
	snapshot, err := adapter.NewRootSnapshot(projectDirectory)
	if err != nil {
		return realize.FileTransactionEdit{}, false, err
	}
	observed, readErr := snapshot.ReadFile(relative)
	closeErr := snapshot.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return realize.FileTransactionEdit{}, false, err
	}
	if bytes.Equal(observed.Content, after) && observed.Mode.Perm() == 0o644 {
		return realize.FileTransactionEdit{}, false, nil
	}
	return realize.FileTransactionEdit{Path: relative, Operation: "splice", Before: observed.Content, After: append([]byte(nil), after...), BeforeMode: uint32(observed.Mode.Perm()), AfterMode: 0o644}, true, nil
}

func removeEmptyTesslDirectories(projectDirectory string) error {
	root := filepath.Join(projectDirectory, ".tessl")
	var directories []string
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories = append(directories, filename)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
	for _, directory := range directories {
		if err := os.Remove(directory); err != nil && !errors.Is(err, fs.ErrNotExist) {
			// A retained file makes its ancestors intentionally non-empty.
			if entries, readErr := os.ReadDir(directory); readErr == nil && len(entries) != 0 {
				continue
			}
			return err
		}
	}
	return nil
}

func finalizationRecords(plan migrate.FinalizePlan, ledger realize.Ledger) ([]migrate.RemovalRecord, []migrate.RetentionRecord) {
	replacements := make(map[string]string)
	for _, target := range ledger.Targets {
		replacement := acrReplacementPath(target.Path)
		if replacement == "" {
			continue
		}
		for _, entry := range target.Entries {
			key := string(entry.ArtifactKind) + "\x00" + entry.ArtifactID
			if current := replacements[key]; current == "" || replacement < current {
				replacements[key] = replacement
			}
		}
	}
	removed := make([]migrate.RemovalRecord, 0, len(plan.Edits))
	for _, edit := range plan.Edits {
		if len(edit.Removed) != 0 {
			removed = append(removed, edit.Removed...)
			continue
		}
		replacement := replacements[edit.Kind+"\x00"+edit.ID]
		if replacement == "" {
			for key, candidate := range replacements {
				if strings.HasSuffix(key, "\x00"+edit.ID) && (replacement == "" || candidate < replacement) {
					replacement = candidate
				}
			}
		}
		removed = append(removed, migrate.RemovalRecord{Path: edit.Path, Kind: edit.Kind, ID: edit.ID, Hash: edit.Hash, Operation: edit.Operation, Replacement: replacement})
	}
	return removed, append([]migrate.RetentionRecord(nil), plan.Retained...)
}

func acrReplacementPath(target string) string {
	components := strings.Split(target, "/")
	for index, component := range components {
		if strings.Contains(component, "acr__") {
			return strings.Join(components[:index+1], "/")
		}
	}
	return ""
}

func findStaleReferences(projectDirectory string, removed []migrate.RemovalRecord) ([]migrate.StaleReference, error) {
	removedPaths := make(map[string]bool)
	replacements := make(map[string]string)
	for _, item := range removed {
		removedPaths[item.Path] = true
		if item.ID != "" && item.Replacement != "" {
			replacements["tessl__"+item.ID] = item.Replacement
		}
	}
	command := exec.Command("git", "ls-files", "-z")
	command.Dir = projectDirectory
	output, err := command.Output()
	if err != nil {
		// The tracking gate runs first, so this exec failure is unreachable for a tracked project; the branch preserves the accepted no-Git case.
		inside := exec.Command("git", "rev-parse", "--is-inside-work-tree")
		inside.Dir = projectDirectory
		if insideOutput, insideErr := inside.Output(); insideErr != nil || strings.TrimSpace(string(insideOutput)) != "true" {
			return []migrate.StaleReference{}, nil
		}
		return nil, fmt.Errorf("list tracked files for stale-reference report: %w", err)
	}
	var result []migrate.StaleReference
	for _, tracked := range bytes.Split(output, []byte{0}) {
		if len(tracked) == 0 {
			continue
		}
		relative := filepath.ToSlash(string(tracked))
		if removedPaths[relative] {
			continue
		}
		filename := filepath.Join(projectDirectory, filepath.FromSlash(relative))
		info, statErr := os.Lstat(filename)
		if errors.Is(statErr, fs.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return nil, fmt.Errorf("inspect tracked file %q for stale-reference report: %w", relative, statErr)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		content, readErr := readFinalizationFile(filename)
		if errors.Is(readErr, fs.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("read tracked file %q for stale-reference report: %w", relative, readErr)
		}
		if bytes.IndexByte(content, 0) >= 0 {
			continue
		}
		for index, line := range strings.Split(string(content), "\n") {
			if strings.Contains(line, "tessl mcp start") {
				continue
			}
			if !strings.Contains(line, ".tessl/") && !strings.Contains(line, "tessl__") {
				continue
			}
			replacement := ""
			for old, candidate := range replacements {
				if strings.Contains(line, old) && (replacement == "" || candidate < replacement) {
					replacement = candidate
				}
			}
			result = append(result, migrate.StaleReference{Path: relative, Line: index + 1, Text: line, Replacement: replacement})
		}
	}
	return result, nil
}
