package dependency

import (
	"errors"
	"fmt"
	"os"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"go.yaml.in/yaml/v3"
)

// AuthorizeLocalRecovery checks both independently readable state documents
// and verified journal before-images. Cross-file validation cannot precede
// this check: interruption can leave two individually valid but mismatched
// local paths. A readable nonlocal after-state can also conceal a local
// before-state that recovery would restore.
func AuthorizeLocalRecovery(project string) (err error) {
	before, err := realize.RecoveryBeforeImages(project, ProjectFilename, LockFilename)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	if _, err := validateStateDirectory(root); err != nil {
		return err
	}
	for _, name := range []string{ProjectFilename, LockFilename} {
		restored, hasBefore := before[name]
		// Check recovered local requirements even when the live file is malformed.
		if hasBefore {
			if err := authorizeRecoveryDocument(project, name, restored); err != nil {
				return err
			}
		}
		contents, _, readErr := readRegularFile(root, name)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return readErr
		}
		declarations, decodeErr := recoveryLocalDeclarations(contents)
		if decodeErr != nil {
			if hasBefore {
				// The checked journal proves these live bytes are an owned before/after
				// image and replaces them. The decoded before-image governs recovery.
				// This preserves nonlocal recovery of a malformed written after-image.
				continue
			}
			return fmt.Errorf("inspect %s before recovery: %w; for a local plugin run 'acr install file:PATH' to authorize and repair explicitly", name, decodeErr)
		}
		if err := authorizeRecoveryDeclarations(project, declarations); err != nil {
			return err
		}
	}
	return nil
}

func authorizeRecoveryDocument(project, filename string, contents []byte) error {
	declarations, err := recoveryLocalDeclarations(contents)
	if err != nil {
		return fmt.Errorf("inspect recovered %s: %w; repair explicitly before realization", filename, err)
	}
	return authorizeRecoveryDeclarations(project, declarations)
}

func recoveryLocalDeclarations(contents []byte) ([]Declaration, error) {
	var document struct {
		Dependencies []struct {
			Source    string         `yaml:"source"`
			Requested string         `yaml:"requested"`
			Kind      ResolutionKind `yaml:"kind"`
			Path      string         `yaml:"path"`
		} `yaml:"dependencies"`
	}
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return nil, err
	}
	var declarations []Declaration
	for _, row := range document.Dependencies {
		if row.Requested == RequestedLocal || row.Kind == ResolutionLocal || row.Path != "" {
			declarations = append(declarations, Declaration{Source: row.Source, Requested: RequestedLocal, Path: row.Path})
		}
	}
	return declarations, nil
}

func authorizeRecoveryDeclarations(project string, declarations []Declaration) error {
	for _, declaration := range declarations {
		if err := validateLocalPath(RequestedLocal, declaration.Path); err != nil {
			return localError(cli.CodeLocalSourceUnauthorized, declaration, err)
		}
		if _, err := authorizeLocal(project, declaration); err != nil {
			return err
		}
	}
	return nil
}
