package realize

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
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

// Run plans complete adapter intents and executes the selected mode.
func (engine *Engine) Run(projectDirectory string, current Ledger, intents []Intent, mode Mode, finalize Finalizer) (Plan, error) {
	plan, err := engine.planner.Plan(projectDirectory, current, intents)
	if err != nil {
		return Plan{}, err
	}
	if plan.HasConflicts() {
		return plan, conflictError(plan)
	}
	switch mode {
	case ModeDryRun:
		return plan, nil
	case ModeCheck:
		if plan.HasChanges() {
			return plan, &ChangesError{Plan: plan}
		}
		return plan, nil
	case ModeApply:
		if !plan.HasChanges() {
			return plan, nil
		}
		if finalize == nil {
			return plan, errors.New("apply mode requires a transactional ledger finalizer")
		}
		if err := applyPlan(projectDirectory, plan, finalize); err != nil {
			return plan, err
		}
		return plan, nil
	default:
		return Plan{}, fmt.Errorf("unsupported realization mode %q", mode)
	}
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

func applyPlan(projectDirectory string, plan Plan, finalize Finalizer) error {
	return applyPlanWith(projectDirectory, plan, finalize, writeOperation)
}

func applyPlanWith(projectDirectory string, plan Plan, finalize Finalizer, writer operationWriter) error {
	if plan.HasConflicts() {
		return conflictError(plan)
	}
	projectRoot, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return fmt.Errorf("open project directory %q: %w", projectDirectory, err)
	}
	defer projectRoot.Close()

	snapshots := make(map[string]fileSnapshot)
	createdDirectories := make(map[string]struct{})
	var mutations []Operation
	for _, operation := range plan.Operations {
		if operation.Kind == OperationPreserve {
			continue
		}
		if operation.Path != gitExcludePath {
			if err := validateTargetPath(operation.Path); err != nil {
				return fmt.Errorf("planned operation path: %w", err)
			}
		}
		snapshot, err := snapshotFile(projectRoot, operation.Path)
		if err != nil {
			return err
		}
		if snapshot.exists != operation.beforeExists || snapshot.exists && snapshot.hash != operation.BeforeHash {
			return fmt.Errorf("target %q changed after planning; rerun realization to produce a fresh plan", operation.Path)
		}
		if !operation.remove && contentHash(operation.content) != operation.AfterHash {
			return fmt.Errorf("planned content hash for %q is inconsistent; discard the plan and retry", operation.Path)
		}
		snapshots[operation.Path] = snapshot
		if !operation.remove && needsWrite(snapshot, operation) {
			for _, directory := range absentParentDirectories(projectRoot, operation.Path) {
				createdDirectories[directory] = struct{}{}
			}
			mutations = append(mutations, operation)
		} else if operation.remove && snapshot.exists {
			mutations = append(mutations, operation)
		}
	}

	var applied []Operation
	for _, operation := range mutations {
		replaced, writeErr := writer(projectRoot, operation)
		if replaced {
			applied = append(applied, operation)
		}
		if writeErr != nil {
			return rollbackFailure(projectRoot, applied, snapshots, createdDirectories, fmt.Errorf("apply %s %q: %w", operation.Kind, operation.Path, writeErr))
		}
	}
	if err := finalize(plan.NextLedger); err != nil {
		return rollbackFailure(projectRoot, applied, snapshots, createdDirectories, fmt.Errorf("persist realization ledger: %w", err))
	}
	return nil
}

func needsWrite(snapshot fileSnapshot, operation Operation) bool {
	return !snapshot.exists || snapshot.hash != operation.AfterHash || uint32(snapshot.mode.Perm()) != operation.Mode
}

func writeOperation(root *os.Root, operation Operation) (bool, error) {
	current, err := snapshotFile(root, operation.Path)
	if err != nil {
		return false, err
	}
	if current.exists != operation.beforeExists || current.exists && current.hash != operation.BeforeHash {
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

func writeFileAtomic(root *os.Root, filename string, content []byte, mode os.FileMode) error {
	if err := validateParentDirectories(root, filename); err != nil {
		return err
	}
	directory := path.Dir(filename)
	if directory != "." {
		if err := root.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("create parent directory for %q: %w", filename, err)
		}
		if err := validateParentDirectories(root, path.Join(directory, "placeholder")); err != nil {
			return err
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

func absentParentDirectories(root *os.Root, filename string) []string {
	var result []string
	directory := path.Dir(filename)
	if directory == "." {
		return nil
	}
	current := ""
	for _, component := range strings.Split(directory, "/") {
		if current == "" {
			current = component
		} else {
			current = path.Join(current, component)
		}
		if _, err := root.Lstat(current); errors.Is(err, os.ErrNotExist) {
			result = append(result, current)
		}
	}
	return result
}

func rollbackFailure(root *os.Root, applied []Operation, snapshots map[string]fileSnapshot, createdDirectories map[string]struct{}, applyErr error) error {
	var rollbackErrors []error
	for index := len(applied) - 1; index >= 0; index-- {
		operation := applied[index]
		snapshot := snapshots[operation.Path]
		if snapshot.exists {
			if err := writeFileAtomic(root, operation.Path, snapshot.content, snapshot.mode); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("restore %q: %w", operation.Path, err))
			}
		} else if err := root.Remove(operation.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("remove newly created %q: %w", operation.Path, err))
		}
	}
	directories := make([]string, 0, len(createdDirectories))
	for directory := range createdDirectories {
		directories = append(directories, directory)
	}
	sort.Slice(directories, func(left, right int) bool {
		return strings.Count(directories[left], "/") > strings.Count(directories[right], "/")
	})
	for _, directory := range directories {
		if err := root.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			// A non-empty directory contains pre-existing or concurrently created
			// content and must be preserved; other failures are actionable.
			if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
				continue
			}
			rollbackErrors = append(rollbackErrors, fmt.Errorf("remove newly created directory %q: %w", directory, err))
		}
	}
	if len(rollbackErrors) != 0 {
		return fmt.Errorf("%w; rollback also failed: %v; restore affected files from version control before retrying", applyErr, errors.Join(rollbackErrors...))
	}
	return fmt.Errorf("%w; all filesystem changes were rolled back", applyErr)
}
