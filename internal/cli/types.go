// Package cli defines the acr command-line contract and dispatch boundary.
package cli

import (
	"context"
	"errors"
	"fmt"
)

// Command identifies a top-level acr command.
type Command string

const (
	CommandInit      Command = "init"
	CommandInstall   Command = "install"
	CommandRealize   Command = "realize"
	CommandList      Command = "list"
	CommandOutdated  Command = "outdated"
	CommandUpdate    Command = "update"
	CommandUninstall Command = "uninstall"
	CommandCheck     Command = "check"
	CommandPublish   Command = "publish"
	CommandMigrate   Command = "migrate"
)

// OutputFormat selects human-readable or machine-readable output.
type OutputFormat string

const (
	OutputText OutputFormat = "text"
	OutputJSON OutputFormat = "json"
)

// FreshnessPolicy controls project session-start update behavior.
type FreshnessPolicy string

const (
	FreshnessOutdated FreshnessPolicy = "outdated"
	FreshnessInstall  FreshnessPolicy = "install"
	FreshnessNone     FreshnessPolicy = "none"
)

// Invocation is the parsed, shell-independent command contract passed to the
// application layer.
type Invocation struct {
	Command           Command
	Subcommand        string
	ProjectDirectory  string
	Output            OutputFormat
	DryRun            bool
	NonInteractive    bool
	Agents            []string
	Freshness         FreshnessPolicy
	FreshnessExplicit bool
	Source            string
	RequestedVersion  string
	Reconcile         bool
	PublicationPath   string
}

// Result is returned by the application layer for rendering by the CLI.
type Result struct {
	Message string
	Value   any
}

// Application owns domain behavior outside command parsing and rendering.
type Application interface {
	Execute(context.Context, Invocation) (Result, error)
}

// ApplicationFunc adapts a function to Application.
type ApplicationFunc func(context.Context, Invocation) (Result, error)

// Execute calls f with the parsed invocation.
func (f ApplicationFunc) Execute(ctx context.Context, invocation Invocation) (Result, error) {
	return f(ctx, invocation)
}

// Exit codes form the stable process contract for acr.
const (
	ExitSuccess     = 0
	ExitOperational = 1
	ExitUsage       = 2
	ExitChanges     = 3
	ExitConflict    = 4
)

// Error carries a machine-readable code and stable process exit code.
type Error struct {
	ExitCode int
	Code     string
	Message  string
	Cause    error
}

// Error returns the user-facing diagnostic.
func (e *Error) Error() string {
	return e.Message
}

// Unwrap exposes the underlying cause when one exists.
func (e *Error) Unwrap() error {
	return e.Cause
}

func commandError(err error) *Error {
	var commandErr *Error
	if errors.As(err, &commandErr) {
		return commandErr
	}
	return &Error{
		ExitCode: ExitOperational,
		Code:     "operation_failed",
		Message:  err.Error(),
		Cause:    err,
	}
}

func usageError(format string, args ...any) *Error {
	return &Error{
		ExitCode: ExitUsage,
		Code:     "usage",
		Message:  fmt.Sprintf(format, args...),
	}
}

// UnavailableApplication keeps the command surface executable until domain
// services are wired by their owning implementation issues.
type UnavailableApplication struct{}

// Execute reports that the selected command has not been wired yet.
func (UnavailableApplication) Execute(_ context.Context, invocation Invocation) (Result, error) {
	return Result{}, &Error{
		ExitCode: ExitOperational,
		Code:     "not_implemented",
		Message:  fmt.Sprintf("%s is not implemented yet", invocation.Command),
	}
}
