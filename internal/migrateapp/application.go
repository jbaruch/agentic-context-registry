package migrateapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/diagnostic"
	"github.com/jbaruch/agentic-context-registry/internal/freshnessapp"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/producerconvert"
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
// Freshness options travel down the fallback chain untouched; migration
// interprets none of them itself.
func NewApplication(client dependency.Remote, version string, freshnessOptions ...freshnessapp.Option) *Application {
	service := NewService()
	if client != nil {
		service = newService(client)
	}
	return &Application{service: service, fallback: publishapp.NewApplication(client, version, freshnessOptions...)}
}

// Execute dispatches Tessl migration commands and preserves the shared CLI contract.
func (application *Application) Execute(ctx context.Context, invocation cli.Invocation) (cli.Result, error) {
	if invocation.Command != cli.CommandMigrate {
		return application.fallback.Execute(ctx, invocation)
	}
	if invocation.Subcommand == "tessl-plugin" {
		packageRoot := invocation.PublicationPath
		if packageRoot == "" {
			packageRoot = "."
		}
		if !filepath.IsAbs(packageRoot) {
			projectRoot, err := filepath.Abs(invocation.ProjectDirectory)
			if err != nil {
				return cli.Result{}, migrateError(err)
			}
			// Resolve the base without cleaning the positional path: clean
			// conversion must still see and refuse its original .. components.
			packageRoot = projectRoot + string(filepath.Separator) + packageRoot
		}
		if invocation.ACROnly {
			report, err := application.service.ConvertClean(producerconvert.Options{PackageRoot: packageRoot, Repository: invocation.Repository, PackageVersion: invocation.PackageVersion, DryRun: invocation.DryRun})
			result := cli.Result{Value: report, Message: producerconvert.FormatText(report)}
			if err != nil {
				if len(report.Blockers) == 0 {
					result.Message = ""
				}
				var refusal *producerconvert.Error
				if errors.As(err, &refusal) {
					failure := cli.Error{ExitCode: cli.ExitOperational, Code: refusal.Code, Field: refusal.Path, Message: refusal.Reason, Cause: err}
					if refusal.Code == "unsupported_semantic_conversion" {
						return result, cli.NewActionableError(failure)
					}
					return result, &failure
				}
				result.Message = ""
				return result, migrateError(err)
			}
			return result, nil
		}
		report, err := application.service.Convert(tesslplugin.Options{
			PackageRoot: packageRoot,
			Repository:  invocation.Repository,
			DryRun:      invocation.DryRun,
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
		DryRun: invocation.DryRun, Finalize: invocation.Finalize, VendorUnmapped: invocation.VendorUnmapped, FileMappings: fileMappings, CLIMappings: cliMappings,
		AcceptReviewedChanges: invocation.AcceptReviewedChanges,
	})
	if err != nil {
		// The detailed report is what names the gate that fired. Dropping it
		// here left exit 4 with a bare code and an operator diffing two runs
		// to learn why; the CLI already renders result.Value as the JSON
		// envelope's result and result.Message after the text diagnostic.
		if report.SchemaVersion == 0 {
			return cli.Result{}, migrateCLIError(err)
		}
		return cli.Result{Message: migrate.FormatCoexistenceText(report), Value: report}, migrateCLIError(err)
	}
	return cli.Result{Message: migrate.FormatCoexistenceText(report), Value: report}, nil
}

// diagnosticCause escapes controls while preserving complete composed explanations.
func diagnosticCause(value string) string {
	var builder strings.Builder
	consumed := 0
	for consumed < len(value) {
		r, size := utf8.DecodeRuneInString(value[consumed:])
		consumed += size
		if unicode.IsPrint(r) {
			builder.WriteRune(r)
		} else if r <= 0xffff {
			fmt.Fprintf(&builder, `\u%04x`, r)
		} else {
			fmt.Fprintf(&builder, `\U%08x`, r)
		}
	}
	return builder.String()
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
		detail := fmt.Sprintf("%q", diagnostic.OpaqueError(err).Error())
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			detail = fmt.Sprintf("%s %s: %s", diagnosticCause(pathErr.Op), migrate.DiagnosticToken(pathErr.Path), diagnosticCause(diagnostic.OpaqueError(pathErr.Err).Error()))
		}
		return nil, &Error{Code: "mapping_file_invalid", Message: fmt.Sprintf("read migration mapping file %s: %s; create or correct the mapping file and ensure it is readable, then rerun with the same --mapping-file value", migrate.DiagnosticToken(mappingFile), detail), Cause: err, Actionable: true}
	}
	mappings, err := migrate.DecodeMappingFile(content)
	if err != nil {
		// An invalid source or target is classified from the typed parse error.
		// A missing file, a YAML decode, and a bad package identity are not.
		var operator *migrate.OperatorSourceError
		if errors.As(err, &operator) {
			return nil, err
		}
		var conflict *migrate.MappingConflictError
		code := "mapping_file_invalid"
		message := diagnosticCause(err.Error())
		if errors.As(err, &conflict) {
			code = cli.CodeMappingConflict
			message = fmt.Sprintf("mapping %s is declared more than once with different values in %s; keep one mapping for that package", migrate.DiagnosticToken(conflict.From), conflict.Origin)
		} else {
			message += "; correct the mapping file to use schemaVersion: 1 with a packages list, workspace/package identities, and valid github:owner/repository sources, then rerun with the same --mapping-file value"
		}
		return nil, &Error{Code: code, Message: message, Cause: err, Actionable: true}
	}
	return mappings, nil
}

func migrateCLIError(err error) error {
	// Only a source or target the operator typed, which the mapping parsers
	// return as OperatorSourceError, is usage. Matching the "invalid source "
	// prefix would also catch a repository URL read from an installed plugin
	// manifest, and that failure stays operational.
	var operator *migrate.OperatorSourceError
	if errors.As(err, &operator) {
		return &cli.Error{ExitCode: cli.ExitUsage, Code: "usage", Message: operator.Error(), Cause: err}
	}
	var conversionErr *tesslplugin.Error
	if errors.As(err, &conversionErr) {
		return migrateError(err)
	}
	var migrationErr *Error
	if errors.As(err, &migrationErr) {
		exitCode := cli.ExitOperational
		if migrationErr.Code == "finalization_blocked" || migrationErr.Code == cli.CodeFinalizationConflict || migrationErr.Code == cli.CodeEffectiveMismatch || migrationErr.Code == cli.CodeVendorCollision {
			exitCode = cli.ExitConflict
		}
		return &cli.Error{ExitCode: exitCode, Code: migrationErr.Code, Message: migrationErr.Message, Cause: err, Remedy: migrationErr.Remedy, Actionable: migrationErr.Actionable}
	}
	var pending *realize.PendingTransactionError
	var recovery *realize.RecoveryConflictError
	var unsupported *realize.UnsupportedJournalVersionError
	var busy *realize.TransactionBusyError
	var unavailable *realize.TransactionLockUnavailableError
	var tesslTarget *realize.TesslOwnedTargetError
	code := "migrate_failed"
	exitCode := cli.ExitOperational
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
	case errors.As(err, &tesslTarget):
		code = "tessl_owned_target"
		exitCode = cli.ExitConflict
	}
	return &cli.Error{ExitCode: exitCode, Code: code, Message: err.Error(), Cause: err}
}

func migrateError(err error) error {
	var conv *tesslplugin.Error
	if errors.As(err, &conv) {
		return &cli.Error{ExitCode: cli.ExitOperational, Code: conv.Code, Message: conv.Message, Field: conv.Field, Cause: err}
	}
	var validation *manifest.ValidationErrors
	if errors.As(err, &validation) && len(validation.Issues) != 0 {
		return &cli.Error{ExitCode: cli.ExitOperational, Code: string(validation.Issues[0].Code), Field: validation.Issues[0].Field, Message: validation.Error(), Cause: err}
	}
	return &cli.Error{
		ExitCode: cli.ExitOperational,
		Code:     "migrate_failed",
		Message:  fmt.Sprintf("%v", err),
		Cause:    err,
	}
}
