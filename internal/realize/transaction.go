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

func applyPlan(projectDirectory string, plan Plan, finalize Finalizer) error {
	return applyPlanWith(projectDirectory, plan, finalize, writeOperation)
}

func applyPlanWith(projectDirectory string, plan Plan, finalize Finalizer, writer operationWriter) error {
	return applyPlanWithDirectories(projectDirectory, plan, finalize, writer, ensureParentDirectories)
}

func applyPlanWithDirectories(projectDirectory string, plan Plan, finalize Finalizer, writer operationWriter, createParents parentDirectoryCreator) error {
	if plan.HasConflicts() {
		return conflictError(plan)
	}
	projectRoot, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return fmt.Errorf("open project directory %q: %w", projectDirectory, err)
	}
	defer projectRoot.Close()

	externalRoots := make(map[string]*os.Root)
	defer func() {
		for _, root := range externalRoots {
			root.Close()
		}
	}()
	var createdDirectories []rootedDirectory
	var mutations []preparedOperation
	for _, operation := range plan.Operations {
		if operation.Kind == OperationPreserve {
			continue
		}
		if !operation.GitExclusion {
			if err := validateTargetPath(operation.Path); err != nil {
				return fmt.Errorf("planned operation path: %w", err)
			}
		}
		operationRoot, operationPath, err := resolveOperationLocation(projectRoot, externalRoots, operation)
		if err != nil {
			return err
		}
		snapshot, err := snapshotFile(operationRoot, operationPath)
		if err != nil {
			return err
		}
		if !matchesBeforeState(operation, snapshot) {
			return fmt.Errorf("target %q changed after planning; rerun realization to produce a fresh plan", operation.Path)
		}
		if !operation.remove && contentHash(operation.content) != operation.AfterHash {
			return fmt.Errorf("planned content hash for %q is inconsistent; discard the plan and retry", operation.Path)
		}
		prepared := preparedOperation{operation: operation, root: operationRoot, path: operationPath, snapshot: snapshot}
		if !operation.remove && needsWrite(snapshot, operation) {
			mutations = append(mutations, prepared)
		} else if operation.remove && snapshot.exists {
			mutations = append(mutations, prepared)
		}
	}

	var applied []preparedOperation
	for _, prepared := range mutations {
		if !prepared.operation.remove {
			created, err := createParents(prepared.root, prepared.path)
			createdDirectories = append(createdDirectories, created...)
			if err != nil {
				return rollbackFailure(applied, createdDirectories, fmt.Errorf("create parents for %q: %w", prepared.operation.Path, err))
			}
		}
		physical := prepared.operation
		physical.Path = prepared.path
		replaced, writeErr := writer(prepared.root, physical)
		if replaced {
			applied = append(applied, prepared)
		}
		if writeErr != nil {
			return rollbackFailure(applied, createdDirectories, fmt.Errorf("apply %s %q: %w", prepared.operation.Kind, prepared.operation.Path, writeErr))
		}
	}
	if err := finalize(plan.NextLedger); err != nil {
		return rollbackFailure(applied, createdDirectories, fmt.Errorf("persist realization ledger: %w", err))
	}
	return nil
}

func resolveOperationLocation(projectRoot *os.Root, externalRoots map[string]*os.Root, operation Operation) (*os.Root, string, error) {
	if !operation.GitExclusion || operation.physicalRoot == "" {
		return projectRoot, operation.Path, nil
	}
	if !filepath.IsAbs(operation.physicalRoot) || filepath.Clean(operation.physicalRoot) != operation.physicalRoot {
		return nil, "", fmt.Errorf("planned Git exclusion root %q is not a clean absolute path", operation.physicalRoot)
	}
	if err := validateTargetPath(operation.physicalPath); err != nil {
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
	if err := validateParentDirectories(root, filename); err != nil {
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

func rollbackFailure(applied []preparedOperation, createdDirectories []rootedDirectory, applyErr error) error {
	var rollbackErrors []error
	for index := len(applied) - 1; index >= 0; index-- {
		prepared := applied[index]
		current, err := snapshotFile(prepared.root, prepared.path)
		if err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("inspect %q before rollback: %w; current content was preserved", prepared.operation.Path, err))
			continue
		}
		if err := verifyAppliedState(prepared.operation, current); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("preserve %q: %w", prepared.operation.Path, err))
			continue
		}
		if prepared.snapshot.exists {
			if err := writeFileAtomic(prepared.root, prepared.path, prepared.snapshot.content, prepared.snapshot.mode); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("restore %q: %w", prepared.operation.Path, err))
			}
		} else if err := prepared.root.Remove(prepared.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("remove newly created %q: %w", prepared.operation.Path, err))
		}
	}
	directories := append([]rootedDirectory(nil), createdDirectories...)
	sort.Slice(directories, func(left, right int) bool {
		return strings.Count(directories[left].path, "/") > strings.Count(directories[right].path, "/")
	})
	for _, directory := range directories {
		current, err := directory.root.Lstat(directory.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("inspect created directory %q before rollback: %w", directory.path, err))
			continue
		}
		if directory.info == nil || !os.SameFile(directory.info, current) || directory.info.Mode().Perm() != current.Mode().Perm() {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("preserve created directory %q because it changed after creation", directory.path))
			continue
		}
		if err := directory.root.Remove(directory.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			// A non-empty directory contains pre-existing or concurrently created
			// content and must be preserved; other failures are actionable.
			if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
				continue
			}
			rollbackErrors = append(rollbackErrors, fmt.Errorf("remove newly created directory %q: %w", directory.path, err))
		}
	}
	if len(rollbackErrors) != 0 {
		return fmt.Errorf("%w; rollback incomplete: %v; concurrent content was preserved, inspect affected files and reconcile them before retrying", applyErr, errors.Join(rollbackErrors...))
	}
	return fmt.Errorf("%w; all filesystem changes were rolled back", applyErr)
}

func verifyAppliedState(operation Operation, current fileSnapshot) error {
	if operation.remove {
		if current.exists {
			return errors.New("target was recreated after this transaction removed it")
		}
		return nil
	}
	if !current.exists {
		return errors.New("target was removed after this transaction wrote it")
	}
	if current.hash != operation.AfterHash || uint32(current.mode.Perm()) != operation.Mode {
		return errors.New("target changed after this transaction wrote it")
	}
	return nil
}
