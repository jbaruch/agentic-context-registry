package migrateapp

import (
	"errors"
	"fmt"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/tesslplugin"
)

// Service inventories Tessl consumers and converts Tessl plugin packages.
type Service struct{}

// NewService constructs the production Tessl inventory service.
func NewService() *Service {
	return &Service{}
}

// Inventory opens a root-confined snapshot and returns the dry-run report.
func (service *Service) Inventory(projectDirectory string) (report migrate.Report, err error) {
	snapshot, err := adapter.NewRootSnapshot(projectDirectory)
	if err != nil {
		return migrate.Report{}, fmt.Errorf("open project %q: %w", projectDirectory, err)
	}
	defer func() {
		if closeErr := snapshot.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close project %q: %w", projectDirectory, closeErr))
		}
	}()
	return migrate.Inventory(snapshot)
}

// Convert runs producer conversion for one package root.
func (service *Service) Convert(opts tesslplugin.Options) (tesslplugin.Report, error) {
	if opts.PackageRoot == "" {
		opts.PackageRoot = "."
	}
	return tesslplugin.Convert(opts)
}
