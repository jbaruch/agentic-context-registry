package dependency

import "fmt"

// NotDeclaredError reports a SOURCE argument naming a dependency that
// agents.yaml does not declare. An undeclared source is an invalid argument,
// so callers map it to a usage refusal rather than an operational failure.
type NotDeclaredError struct {
	Source string
}

// Error names the command that lists what is declared.
func (err *NotDeclaredError) Error() string {
	return fmt.Sprintf("dependency %s is not declared; run 'acr list' to see the declared dependencies", err.Source)
}

// PruneDependency removes one declaration, its rollback hold, its lock row, and
// that row's hold from state, returning the pruned state and the lock row it
// dropped. It performs no I/O and mutates nothing the caller passed in: the
// caller writes the result through the ordinary two-file transaction, where a
// half-prune cannot land because an orphaned lock row fails validation.
func PruneDependency(state State, source string) (State, *LockedDependency, error) {
	if _, err := SourceScheme(source); err != nil {
		return State{}, nil, err
	}
	index, declared := findDeclaration(state.Project.Dependencies, source)
	if !declared {
		return State{}, nil, &NotDeclaredError{Source: source}
	}
	pruned := cloneState(state)
	pruned.Project.Dependencies = append(pruned.Project.Dependencies[:index], pruned.Project.Dependencies[index+1:]...)
	var removed *LockedDependency
	if lockIndex, locked := findLock(pruned.Lock.Dependencies, source); locked {
		row := pruned.Lock.Dependencies[lockIndex]
		removed = &row
		pruned.Lock.Dependencies = append(pruned.Lock.Dependencies[:lockIndex], pruned.Lock.Dependencies[lockIndex+1:]...)
	}
	if err := validateState(pruned.Project, pruned.Lock); err != nil {
		return State{}, nil, fmt.Errorf("pruning %s leaves invalid dependency state: %w; run 'acr install' to regenerate %s", source, err, LockFilename)
	}
	return pruned, removed, nil
}
