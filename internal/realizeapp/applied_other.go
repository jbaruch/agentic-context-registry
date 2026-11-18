//go:build !darwin && !linux

package realizeapp

import "os"

// Unsupported metadata supplies no receipt evidence; committed comparison
// remains available on these platforms.
func appliedMetadata(os.FileInfo) (appliedFile, bool) {
	return appliedFile{}, false
}
