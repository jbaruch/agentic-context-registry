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
	case cli.CommandPrepareExclusion:
		if err := realize.PrepareGitExclusion(ctx, invocation.ProjectDirectory); err != nil {
			return cli.Result{}, realizationError(err, invocation.Command)
		}
		const message = "Prepared the owning worktree for removal. You can now remove it with Git. The shared exclusion fence remains until reconciliation from a surviving worktree."
		return cli.Result{Message: message, Value: map[string]string{"message": message}}, nil
	case cli.CommandReconcileExclusion:
		if err := realize.ReconcileGitExclusion(ctx, invocation.ProjectDirectory, invocation.ExclusionFenceHash); err != nil {
			return cli.Result{}, realizationError(err, invocation.Command)
		}
		return cli.Result{Message: "Retired the prepared Git exclusion fence. Shared files and project outputs were left unchanged."}, nil
	default:
		return application.fallback.Execute(ctx, invocation)
	}
	result, err := application.service.Run(ctx, invocation.ProjectDirectory, invocation.Agents, mode)
	if err != nil {
		return cli.Result{}, realizationError(err, invocation.Command)
	}
	return cli.Result{Message: realizationMessage(mode, result), Value: result, Notices: realizationNotices(result.Notices)}, nil
}

// uninstall refuses a malformed SOURCE at the command boundary, where an
// invalid argument belongs, and hands everything else to the service.
// SourceSchemeError is that argument's typed refusal. Any other error from
// the check stays operational, as a source read from stored state does.
func (application *Application) uninstall(ctx context.Context, invocation cli.Invocation) (cli.Result, error) {
	if _, err := dependency.SourceScheme(invocation.Source); err != nil {
		var invalid *dependency.SourceSchemeError
		if errors.As(err, &invalid) {
			return cli.Result{}, &cli.Error{ExitCode: cli.ExitUsage, Code: "usage", Message: invalid.Error(), Cause: err}
		}
		return cli.Result{}, realizationError(err, cli.CommandUninstall)
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
	return realizationError(err, cli.CommandUninstall)
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
		removed += "@" + result.Removed.Reference()
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

func realizationError(err error, command cli.Command) error {
	if local := dependency.LocalCLIError(err); local != nil {
		return local
	}
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
		code = cli.CodePendingTransaction
	case errors.As(err, &recovery):
		code = cli.CodeRecoveryConflict
	case errors.As(err, &unsupported):
		code = cli.CodeUnsupportedJournalVersion
	case errors.As(err, &busy):
		code = cli.CodeTransactionBusy
	case errors.As(err, &unavailable):
		code = cli.CodeTransactionLockUnavailable
	}
	if code != "" {
		return &cli.Error{ExitCode: cli.ExitOperational, Code: code, Message: err.Error(), Cause: err}
	}
	var changes *realize.ChangesError
	if errors.As(err, &changes) {
		return &cli.Error{ExitCode: cli.ExitChanges, Code: "realization_changes", Message: err.Error(), Cause: err}
	}
	var capability *adapter.UnsupportedError
	if errors.As(err, &capability) {
		return &cli.Error{ExitCode: cli.ExitConflict, Code: adapter.CodeUnsupportedAdapterCapability, Message: err.Error(), Cause: err}
	}
	var graphConflict *preserve.GraphError
	if errors.As(err, &graphConflict) {
		conflict := cli.Error{ExitCode: cli.ExitConflict, Code: "realization_conflict", Message: err.Error(), Cause: err}
		var unresolved, invalid, otherConflict bool
		for _, diagnostic := range graphConflict.Diagnostics {
			switch {
			case diagnostic.Line != 0 && diagnostic.Code == preserve.CodeUnresolvedInclude:
				unresolved = true
			case diagnostic.Line != 0 && diagnostic.Code == preserve.CodeInvalidInclude:
				invalid = true
			default:
				// Other codes and file-level findings also need repair.
				otherConflict = true
			}
		}
		if unresolved {
			conflict.Message += "; create the missing file or any missing directories in its path, or correct or remove the unresolved import"
		}
		if invalid {
			conflict.Message += "; point the invalid import at a regular file inside the project or remove it"
		}
		if unresolved || invalid {
			if otherConflict {
				conflict.Message += "; resolve the conflicting managed content"
			}
			if command == cli.CommandRealize || command == cli.CommandUninstall {
				conflict.Message += ", then retry your original 'acr " + string(command) + "' command"
			}
			conflict.Message += ", then run 'acr check'"
			conflict.Actionable = true
		}
		return &conflict
	}
	var engineConflict *realize.ConflictError
	var preserveConflict *preserve.ConflictError
	var malformed *adapter.MalformedOutputError
	var duplicate *adapter.DuplicateEntryError
	var native *adapter.NativeValidationError
	var mixed *MixedAdapterTargetError
	var fileConflict *realize.FileTransactionConflictError
	if errors.As(err, &engineConflict) || errors.As(err, &preserveConflict) || errors.As(err, &malformed) || errors.As(err, &duplicate) || errors.As(err, &native) || errors.As(err, &mixed) || errors.As(err, &fileConflict) {
		return &cli.Error{ExitCode: cli.ExitConflict, Code: "realization_conflict", Message: err.Error(), Cause: err}
	}
	return &cli.Error{ExitCode: cli.ExitOperational, Code: "realization_failed", Message: err.Error(), Cause: err}
}
