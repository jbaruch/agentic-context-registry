package realize

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Mode selects dry-run, normal application, or drift-check behavior.
type Mode string

const (
	ModeDryRun Mode = "dry-run"
	ModeApply  Mode = "apply"
	ModeCheck  Mode = "check"
)

// Finalizer persists the planned ledger and any related project state. It must
// be transactional itself: returning an error means it left persistent state
// unchanged. Filesystem changes are rolled back when it fails.
type Finalizer func(Ledger) error

// StateFile is project state prepared before the first transaction rename.
// State paths bypass adapter target validation but remain subject to the same
// before-image journal and atomic writer as native outputs.
type StateFile struct {
	Path    string
	Content []byte
	Mode    uint32
}

// StateFinalizer renders project state from the planned ledger before apply.
type StateFinalizer func(Ledger) ([]StateFile, error)

// Engine owns planning and transactional application.
type Engine struct {
	planner *Planner
}

// NewEngine constructs the production realization engine.
func NewEngine() *Engine {
	return &Engine{planner: NewPlanner()}
}

func newEngine(planner *Planner) *Engine {
	return &Engine{planner: planner}
}

// Run plans complete adapter intents and executes the selected mode. retained
// is forwarded verbatim to Planner.Plan; see its contract for the optional
// ledger of targets owned outside this invocation.
func (engine *Engine) Run(projectDirectory string, current Ledger, intents []Intent, mode Mode, finalize Finalizer, retained ...Ledger) (Plan, error) {
	return engine.run(projectDirectory, current, intents, mode, finalize, nil, retained...)
}

// RunStateFiles prepares state bytes before applying anything so native
// outputs, agents.yaml, and registry.lock share one crash-recoverable journal.
func (engine *Engine) RunStateFiles(projectDirectory string, current Ledger, intents []Intent, mode Mode, finalize StateFinalizer, retained ...Ledger) (Plan, error) {
	return engine.run(projectDirectory, current, intents, mode, nil, finalize, retained...)
}

func (engine *Engine) run(projectDirectory string, current Ledger, intents []Intent, mode Mode, legacy Finalizer, stateFinalizer StateFinalizer, retained ...Ledger) (Plan, error) {
	if mode != ModeDryRun && mode != ModeCheck && mode != ModeApply {
		return Plan{}, fmt.Errorf("unsupported realization mode %q", mode)
	}
	if mode != ModeApply {
		notes, err := inspectTransactions(projectDirectory)
		if err != nil {
			return Plan{}, err
		}
		plan, err := engine.planner.Plan(projectDirectory, current, intents, retained...)
		if err != nil {
			return Plan{}, err
		}
		plan.TransactionNotes = notes
		if plan.HasConflicts() {
			return plan, conflictError(plan)
		}
		if stateFinalizer != nil {
			if err := appendStateOperations(projectDirectory, &plan, stateFinalizer); err != nil {
				return plan, err
			}
		}
		if mode == ModeCheck && plan.HasChanges() {
			return plan, &ChangesError{Plan: plan}
		}
		return plan, nil
	}

	claim, err := claimTransactions(projectDirectory)
	if err != nil {
		return Plan{}, err
	}
	defer claim.Close()
	if err := cleanStagingTransactions(projectDirectory); err != nil {
		return Plan{}, err
	}
	if err := recoverPendingTransaction(projectDirectory); err != nil {
		return Plan{}, err
	}
	plan, err := engine.planner.Plan(projectDirectory, current, intents, retained...)
	if err != nil {
		return Plan{}, err
	}
	if plan.HasConflicts() {
		return plan, conflictError(plan)
	}
	if stateFinalizer != nil {
		if err := appendStateOperations(projectDirectory, &plan, stateFinalizer); err != nil {
			return plan, err
		}
	}
	if !plan.HasChanges() {
		return plan, nil
	}
	if legacy == nil && stateFinalizer == nil {
		return plan, errors.New("apply mode requires a transactional ledger finalizer")
	}
	if err := applyPlanJournaled(projectDirectory, plan, legacy); err != nil {
		return plan, err
	}
	return plan, nil
}

func appendStateOperations(projectDirectory string, plan *Plan, finalize StateFinalizer) error {
	files, err := finalize(plan.NextLedger)
	if err != nil {
		return fmt.Errorf("prepare realization state: %w", err)
	}
	root, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return fmt.Errorf("open project directory %q: %w", projectDirectory, err)
	}
	defer root.Close()
	seen := make(map[string]struct{}, len(files))
	for _, state := range files {
		if state.Path != "agents.yaml" && state.Path != ".agents/registry.lock" {
			return fmt.Errorf("unsupported transactional state path %q", state.Path)
		}
		if _, exists := seen[state.Path]; exists {
			return fmt.Errorf("transactional state path %q is repeated", state.Path)
		}
		seen[state.Path] = struct{}{}
		if state.Mode == 0 || state.Mode > 0o777 {
			return fmt.Errorf("transactional state path %q has invalid mode %04o", state.Path, state.Mode)
		}
		snapshot, err := snapshotFile(root, state.Path)
		if err != nil {
			return err
		}
		afterHash := contentHash(state.Content)
		if snapshot.exists && snapshot.hash == afterHash && uint32(snapshot.mode.Perm()) == state.Mode {
			continue
		}
		kind := OperationCreate
		if snapshot.exists {
			kind = OperationUpdate
		}
		plan.Operations = append(plan.Operations, Operation{
			Kind: kind, Path: state.Path, BeforeHash: snapshot.hash, AfterHash: afterHash,
			Mode: state.Mode, content: append([]byte(nil), state.Content...),
			beforeExists: snapshot.exists, beforeMode: uint32(snapshot.mode.Perm()), stateFile: true,
		})
	}
	sort.SliceStable(plan.Operations, func(i, j int) bool {
		if plan.Operations[i].Path == plan.Operations[j].Path {
			return plan.Operations[i].Kind < plan.Operations[j].Kind
		}
		return plan.Operations[i].Path < plan.Operations[j].Path
	})
	return nil
}

func conflictError(plan Plan) error {
	var conflicts []Operation
	for _, operation := range plan.Operations {
		if operation.Kind == OperationConflict {
			conflicts = append(conflicts, operation)
		}
	}
	return &ConflictError{Operations: conflicts}
}

type operationWriter func(*os.Root, Operation) (bool, error)

type preparedOperation struct {
	operation Operation
	root      *os.Root
	path      string
	snapshot  fileSnapshot
}

type rootedDirectory struct {
	root *os.Root
	path string
	info os.FileInfo
}

type parentDirectoryCreator func(*os.Root, string) ([]rootedDirectory, error)

func resolveOperationLocation(projectRoot *os.Root, externalRoots map[string]*os.Root, operation Operation) (*os.Root, string, error) {
	if !operation.GitExclusion || operation.physicalRoot == "" {
		return projectRoot, operation.Path, nil
	}
	if !filepath.IsAbs(operation.physicalRoot) || filepath.Clean(operation.physicalRoot) != operation.physicalRoot {
		return nil, "", fmt.Errorf("planned Git exclusion root %q is not a clean absolute path", operation.physicalRoot)
	}
	if err := ValidateTargetPath(operation.physicalPath); err != nil {
		return nil, "", fmt.Errorf("planned Git exclusion path: %w", err)
	}
	root := externalRoots[operation.physicalRoot]
	if root == nil {
		var err error
		root, err = os.OpenRoot(operation.physicalRoot)
		if err != nil {
			return nil, "", fmt.Errorf("open planned Git exclusion directory %q: %w", operation.physicalRoot, err)
		}
		externalRoots[operation.physicalRoot] = root
	}
	return root, operation.physicalPath, nil
}

func needsWrite(snapshot fileSnapshot, operation Operation) bool {
	return !snapshot.exists || snapshot.hash != operation.AfterHash || uint32(snapshot.mode.Perm()) != operation.Mode
}

func writeOperation(root *os.Root, operation Operation) (bool, error) {
	current, err := snapshotFile(root, operation.Path)
	if err != nil {
		return false, err
	}
	if !matchesBeforeState(operation, current) {
		return false, fmt.Errorf("target changed immediately before %s; rerun realization", operation.Kind)
	}
	if operation.remove {
		if err := root.Remove(operation.Path); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := writeFileAtomic(root, operation.Path, operation.content, os.FileMode(operation.Mode)); err != nil {
		return false, err
	}
	return true, nil
}

func matchesBeforeState(operation Operation, snapshot fileSnapshot) bool {
	return snapshot.exists == operation.beforeExists && (!snapshot.exists || snapshot.hash == operation.BeforeHash && uint32(snapshot.mode.Perm()) == operation.beforeMode)
}

func writeFileAtomic(root *os.Root, filename string, content []byte, mode os.FileMode) error {
	if err := ValidateParentDirectories(root, filename); err != nil {
		return err
	}
	directory := path.Dir(filename)
	if directory != "." {
		info, err := root.Lstat(directory)
		if err != nil {
			return fmt.Errorf("inspect parent directory for %q: %w", filename, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("parent %q must be a directory, not a symlink or special file", directory)
		}
	}
	if info, err := root.Lstat(filename); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("destination %q must be a regular file, not a symlink or special file", filename)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect destination %q: %w", filename, err)
	}
	temporaryName, err := transactionTemporaryName(directory)
	if err != nil {
		return err
	}
	temporary, err := root.OpenFile(temporaryName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary file for %q: %w", filename, err)
	}
	defer root.Remove(temporaryName)
	if err := temporary.Chmod(mode.Perm()); err != nil {
		temporary.Close()
		return fmt.Errorf("set temporary file permissions for %q: %w", filename, err)
	}
	written, err := temporary.Write(content)
	if err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary file for %q: %w", filename, err)
	}
	if written != len(content) {
		temporary.Close()
		return fmt.Errorf("write temporary file for %q: %w", filename, io.ErrShortWrite)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary file for %q: %w", filename, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file for %q: %w", filename, err)
	}
	if err := root.Rename(temporaryName, filename); err != nil {
		return fmt.Errorf("replace destination %q: %w", filename, err)
	}
	return nil
}

func transactionTemporaryName(directory string) (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate transaction temporary name: %w", err)
	}
	return path.Join(directory, ".acr-realize-"+hex.EncodeToString(random[:])), nil
}

func ensureParentDirectories(root *os.Root, filename string) ([]rootedDirectory, error) {
	return ensureParentDirectoriesWith(root, filename, root.Mkdir)
}

func ensureParentDirectoriesWith(root *os.Root, filename string, mkdir func(string, os.FileMode) error) ([]rootedDirectory, error) {
	var created []rootedDirectory
	directory := path.Dir(filename)
	if directory == "." {
		return nil, nil
	}
	current := ""
	for _, component := range strings.Split(directory, "/") {
		if current == "" {
			current = component
		} else {
			current = path.Join(current, component)
		}
		info, err := root.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return created, fmt.Errorf("parent %q must be a directory, not a symlink or special file", current)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return created, fmt.Errorf("inspect parent %q: %w", current, err)
		}
		if err := mkdir(current, 0o755); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return created, fmt.Errorf("create parent %q: %w", current, err)
			}
			info, err = root.Lstat(current)
			if err != nil {
				return created, fmt.Errorf("inspect concurrently created parent %q: %w", current, err)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return created, fmt.Errorf("concurrently created parent %q must be a directory, not a symlink or special file", current)
			}
			continue
		}
		created = append(created, rootedDirectory{root: root, path: current})
		info, err = root.Lstat(current)
		if err != nil {
			return created, fmt.Errorf("inspect created parent %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return created, fmt.Errorf("created parent %q changed before inspection", current)
		}
		created[len(created)-1].info = info
	}
	return created, nil
}
