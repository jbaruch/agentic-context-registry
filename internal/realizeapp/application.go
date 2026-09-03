package realizeapp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// Application adds realization commands to the dependency application.
type Application struct {
	service  *Service
	fallback cli.Application
}

// NewApplication constructs the complete shipped application boundary.
func NewApplication(github dependency.GitHub) *Application {
	resolver := dependency.NewResolver(github)
	return &Application{service: NewService(resolver), fallback: dependency.NewApplication(github)}
}

// Execute dispatches realization and dependency commands.
func (application *Application) Execute(ctx context.Context, invocation cli.Invocation) (cli.Result, error) {
	var mode realize.Mode
	switch invocation.Command {
	case cli.CommandRealize:
		mode = realize.ModeApply
		if invocation.DryRun {
			mode = realize.ModeDryRun
		}
	case cli.CommandCheck:
		mode = realize.ModeCheck
	case cli.CommandUninstall:
		return application.uninstall(ctx, invocation)
	default:
		return application.fallback.Execute(ctx, invocation)
	}
	result, err := application.service.Run(ctx, invocation.ProjectDirectory, invocation.Agents, mode)
	if err != nil {
		return cli.Result{}, realizationError(err)
	}
	return cli.Result{Message: realizationMessage(mode, result), Value: result, Notices: realizationNotices(result.Notices)}, nil
}

// uninstall refuses a malformed SOURCE at the command boundary, where an
// invalid argument belongs, and hands everything else to the service.
func (application *Application) uninstall(ctx context.Context, invocation cli.Invocation) (cli.Result, error) {
	if _, err := dependency.SourceScheme(invocation.Source); err != nil {
		return cli.Result{}, &cli.Error{ExitCode: cli.ExitUsage, Code: "usage", Message: err.Error(), Cause: err}
	}
	result, err := application.service.Uninstall(ctx, invocation.ProjectDirectory, invocation.Source, invocation.DryRun)
	if err != nil {
		return cli.Result{}, uninstallError(err)
	}
	return cli.Result{
		Message: uninstallMessage(result, invocation.DryRun),
		Value:   result,
		Notices: realizationNotices(result.Notices),
	}, nil
}

func uninstallError(err error) error {
	if notDeclared := dependency.NotDeclaredCLIError(err); notDeclared != nil {
		return notDeclared
	}
	var remaining *RemainingPackagesError
	if errors.As(err, &remaining) {
		return &cli.Error{ExitCode: cli.ExitOperational, Code: "remaining_packages_unavailable", Message: err.Error(), Cause: err}
	}
	return realizationError(err)
}

// uninstallMessage names the removed release, how many targets were deleted
// outright, how many kept unmanaged content, and the agents it covered.
func uninstallMessage(result UninstallResult, dryRun bool) string {
	deleted, spliced := 0, 0
	for _, operation := range result.Plan.Operations {
		if operation.Kind != realize.OperationRemove {
			continue
		}
		if operation.AfterHash == "" {
			deleted++
			continue
		}
		spliced++
	}
	removed := result.Source
	if result.Removed != nil {
		removed += "@" + removedReference(*result.Removed)
	}
	verb := "Removed"
	if dryRun {
		verb = "Would remove"
	}
	message := fmt.Sprintf("%s %s; deleted %d target(s) and spliced %d shared target(s)", verb, removed, deleted, spliced)
	if len(result.Agents) != 0 {
		message += " for " + strings.Join(result.Agents, ", ")
	}
	return message + "."
}

func removedReference(locked dependency.LockedDependency) string {
	if locked.Kind == dependency.ResolutionVendor {
		return locked.PackageVersion
	}
	if locked.Tag != "" {
		return locked.Tag
	}
	return locked.Commit
}

func realizationNotices(notices []adapter.Notice) []cli.Notice {
	result := make([]cli.Notice, 0, len(notices))
	for _, notice := range notices {
		message := notice.Message
		if notice.Path != "" {
			message = notice.Path + ": " + message
		}
		result = append(result, cli.Notice{Code: notice.Code, Message: message})
	}
	return result
}

func realizationError(err error) error {
	var tesslTarget *realize.TesslOwnedTargetError
	if errors.As(err, &tesslTarget) {
		return &cli.Error{ExitCode: cli.ExitConflict, Code: "tessl_owned_target", Message: err.Error(), Cause: err}
	}
	var pending *realize.PendingTransactionError
	var recovery *realize.RecoveryConflictError
	var unsupported *realize.UnsupportedJournalVersionError
	var busy *realize.TransactionBusyError
	var unavailable *realize.TransactionLockUnavailableError
	code := ""
	switch {
	case errors.As(err, &pending):
		code = "pending_transaction"
	case errors.As(err, &recovery):
		code = "recovery_conflict"
	case errors.As(err, &unsupported):
		code = "unsupported_journal_version"
	case errors.As(err, &busy):
		code = "transaction_busy"
	case errors.As(err, &unavailable):
		code = "transaction_lock_unavailable"
	}
	if code != "" {
		return &cli.Error{ExitCode: cli.ExitOperational, Code: code, Message: err.Error(), Cause: err}
	}
	var changes *realize.ChangesError
	if errors.As(err, &changes) {
		return &cli.Error{ExitCode: cli.ExitChanges, Code: "realization_changes", Message: err.Error(), Cause: err}
	}
	var engineConflict *realize.ConflictError
	var preserveConflict *preserve.ConflictError
	var graphConflict *preserve.GraphError
	var malformed *adapter.MalformedOutputError
	var duplicate *adapter.DuplicateEntryError
	var native *adapter.NativeValidationError
	var mixed *MixedAdapterTargetError
	var fileConflict *realize.FileTransactionConflictError
	if errors.As(err, &engineConflict) || errors.As(err, &preserveConflict) || errors.As(err, &graphConflict) || errors.As(err, &malformed) || errors.As(err, &duplicate) || errors.As(err, &native) || errors.As(err, &mixed) || errors.As(err, &fileConflict) {
		return &cli.Error{ExitCode: cli.ExitConflict, Code: "realization_conflict", Message: err.Error(), Cause: err}
	}
	return &cli.Error{ExitCode: cli.ExitOperational, Code: "realization_failed", Message: err.Error(), Cause: err}
}
