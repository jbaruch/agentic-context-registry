package migrateapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/publishapp"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"github.com/jbaruch/agentic-context-registry/internal/tesslplugin"
)

// Application adds Tessl inventory and producer conversion to the shipped publish,
// freshness, realization, and dependency commands.
type Application struct {
	service  *Service
	fallback cli.Application
}

// NewApplication constructs the complete shipped application boundary.
func NewApplication(client *dependency.GitHubClient, version string) *Application {
	service := NewService()
	if client != nil {
		service = newService(client)
	}
	return &Application{service: service, fallback: publishapp.NewApplication(client, version)}
}

// Execute dispatches Tessl migration commands and preserves the shared CLI contract.
func (application *Application) Execute(ctx context.Context, invocation cli.Invocation) (cli.Result, error) {
	if invocation.Command != cli.CommandMigrate {
		return application.fallback.Execute(ctx, invocation)
	}
	if invocation.Subcommand == "tessl-plugin" {
		report, err := application.service.Convert(tesslplugin.Options{
			PackageRoot:         invocation.PublicationPath,
			Repository:          invocation.Repository,
			AcceptAgentWidening: invocation.AcceptAgentWidening,
			DryRun:              invocation.DryRun,
		})
		if err != nil {
			result := cli.Result{Message: tesslplugin.FormatFailureText(report)}
			if len(report.Unmapped) != 0 {
				result.Value = report
			}
			return result, migrateError(err)
		}
		return cli.Result{Message: tesslplugin.FormatText(report), Value: report}, nil
	}
	// Preserve the inventory-only seam for tests and embedders that explicitly
	// construct the shipped application without a GitHub client.
	if application.service.github == nil {
		if invocation.DryRun {
			report, err := application.service.Inventory(invocation.ProjectDirectory)
			if err != nil {
				return cli.Result{}, migrateCLIError(err)
			}
			return cli.Result{Message: migrate.FormatText(report), Value: report}, nil
		}
		return cli.Result{}, &cli.Error{
			ExitCode: cli.ExitOperational,
			Code:     "not_implemented",
			Message:  "acr migrate tessl apply is not implemented yet; rerun with --dry-run to inventory the Tessl installation without writing files, or see https://github.com/jbaruch/agentic-context-registry/issues/2",
		}
	}
	fileMappings, err := readMappingFile(invocation.ProjectDirectory, invocation.MappingFile)
	if err != nil {
		return cli.Result{}, migrateCLIError(err)
	}
	cliMappings, err := migrate.ParseInlineMappings(invocation.Mappings)
	if err != nil {
		return cli.Result{}, &cli.Error{ExitCode: cli.ExitUsage, Code: "usage", Message: err.Error(), Cause: err}
	}
	report, err := application.service.Migrate(ctx, invocation.ProjectDirectory, Options{
		DryRun: invocation.DryRun, Finalize: invocation.Finalize, FileMappings: fileMappings, CLIMappings: cliMappings,
	})
	if err != nil {
		return cli.Result{}, migrateCLIError(err)
	}
	return cli.Result{Message: migrate.FormatCoexistenceText(report), Value: report}, nil
}

func readMappingFile(projectDirectory, mappingFile string) ([]migrate.Mapping, error) {
	if mappingFile == "" {
		return nil, nil
	}
	filename := mappingFile
	if !filepath.IsAbs(filename) {
		filename = filepath.Join(projectDirectory, filename)
	}
	content, err := os.ReadFile(filename)
	if err != nil {
		return nil, &Error{Code: "mapping_file_invalid", Message: fmt.Sprintf("read migration mapping file %q: %v", mappingFile, err), Cause: err}
	}
	mappings, err := migrate.DecodeMappingFile(content)
	if err != nil {
		var conflict *migrate.MappingConflictError
		code := "mapping_file_invalid"
		if errors.As(err, &conflict) {
			code = "mapping_conflict"
		}
		return nil, &Error{Code: code, Message: err.Error(), Cause: err}
	}
	return mappings, nil
}

func migrateCLIError(err error) error {
	var migrationErr *Error
	if errors.As(err, &migrationErr) {
		exitCode := cli.ExitOperational
		if migrationErr.Code == "finalization_blocked" {
			exitCode = cli.ExitConflict
		}
		return &cli.Error{ExitCode: exitCode, Code: migrationErr.Code, Message: migrationErr.Message, Cause: err}
	}
	var pending *realize.PendingTransactionError
	var recovery *realize.RecoveryConflictError
	var unsupported *realize.UnsupportedJournalVersionError
	var busy *realize.TransactionBusyError
	var unavailable *realize.TransactionLockUnavailableError
	code := "migrate_failed"
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
	return &cli.Error{ExitCode: cli.ExitOperational, Code: code, Message: err.Error(), Cause: err}
}

func migrateError(err error) error {
	var conv *tesslplugin.Error
	if errors.As(err, &conv) {
		return &cli.Error{ExitCode: cli.ExitOperational, Code: conv.Code, Message: conv.Message, Field: conv.Field, Cause: err}
	}
	return &cli.Error{
		ExitCode: cli.ExitOperational,
		Code:     "migrate_failed",
		Message:  fmt.Sprintf("%v", err),
		Cause:    err,
	}
}
