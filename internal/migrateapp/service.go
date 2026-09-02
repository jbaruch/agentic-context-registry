package migrateapp

import (
	"errors"
	"fmt"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
)

// Service inventories a Tessl consumer project through a read-only snapshot.
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
