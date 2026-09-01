package adapter

import (
	"fmt"
	"sort"
)

// BoundaryVersionError reports an adapter compiled against a boundary
// version this package does not implement.
type BoundaryVersionError struct {
	AdapterID string
	Want      BoundaryVersion
	Got       BoundaryVersion
}

func (err *BoundaryVersionError) Error() string {
	return fmt.Sprintf("adapter %q was built for boundary version %d; this build implements boundary version %d and cannot register it",
		err.AdapterID, err.Got, err.Want)
}

// Register validates a set of adapters before they can be coordinated:
// unique kebab-case IDs, semantic adapter versions, sorted and
// duplicate-free capability lists, and a boundary version this package
// implements. It returns the adapters sorted by ID for deterministic
// processing.
func Register(adapters ...Adapter) ([]Adapter, error) {
	seen := make(map[string]struct{}, len(adapters))
	registered := append([]Adapter(nil), adapters...)
	for _, candidate := range registered {
		descriptor := candidate.Descriptor()
		if !adapterIDPattern.MatchString(descriptor.ID) {
			return nil, fmt.Errorf("adapter ID %q must be lowercase kebab-case", descriptor.ID)
		}
		if _, duplicate := seen[descriptor.ID]; duplicate {
			return nil, fmt.Errorf("adapter %q is registered more than once", descriptor.ID)
		}
		seen[descriptor.ID] = struct{}{}
		if !semverPattern.MatchString(descriptor.Version) {
			return nil, fmt.Errorf("adapter %q has invalid semantic version %q", descriptor.ID, descriptor.Version)
		}
		if descriptor.Boundary != CurrentBoundaryVersion {
			return nil, &BoundaryVersionError{AdapterID: descriptor.ID, Want: CurrentBoundaryVersion, Got: descriptor.Boundary}
		}
		if !sortedAndUnique(candidate.SupportedArtifacts()) {
			return nil, fmt.Errorf("adapter %q SupportedArtifacts() must be sorted and duplicate-free", descriptor.ID)
		}
		if !sortedAndUnique(candidate.SupportedEvents()) {
			return nil, fmt.Errorf("adapter %q SupportedEvents() must be sorted and duplicate-free", descriptor.ID)
		}
	}
	sort.Slice(registered, func(left, right int) bool {
		return registered[left].Descriptor().ID < registered[right].Descriptor().ID
	})
	return registered, nil
}
