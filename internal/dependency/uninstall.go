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
