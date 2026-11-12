package publishapp

import (
	"context"
	"errors"
	"fmt"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/publish"
)

type validationResult struct {
	Valid   bool     `json:"valid"`
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Files   []string `json:"files"`
}

// Validate the authored tree without credentials, remote calls or writes. Git is
// consulted read-only, and only to leave ignored files out of the listing.
func validatePackage(ctx context.Context, root string) (cli.Result, error) {
	if err := ctx.Err(); err != nil {
		return cli.Result{}, err
	}
	// Git is optional here: a directory outside a work tree is walked whole, and
	// one inside it skips what Git ignores so validate lists what publish will read.
	filter, err := publish.PackageFilter(ctx, root)
	var value manifest.Manifest
	var files []string
	if err == nil {
		value, err = manifest.LoadFiltered(root, filter)
	}
	if err == nil {
		files, err = manifest.PackageFilesFiltered(root, value, filter)
	}
	if err != nil {
		failure := &cli.Error{ExitCode: cli.ExitOperational, Code: "operation_failed", Message: fmt.Sprintf("validate package: %v", err), Cause: err}
		var invalid *manifest.ValidationErrors
		if errors.As(err, &invalid) && len(invalid.Issues) > 0 {
			failure.Code = string(invalid.Issues[0].Code)
			failure.Field = invalid.Issues[0].Field
		}
		return cli.Result{}, failure
	}
	if err := ctx.Err(); err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Message: fmt.Sprintf("Package %s@%s is valid with %d distribution files.", value.Name, value.Version, len(files)), Value: validationResult{true, value.Name, value.Version, files}}, nil
}
