// Package release assembles and validates the immutable acr CLI release assets.
package release

import (
	"fmt"
	"sort"

	"github.com/jbaruch/agentic-context-registry/internal/tarball"
)

const (
	// ChecksumsAssetName is the sha256sum-compatible archive manifest.
	ChecksumsAssetName = "checksums.txt"
	// SignatureAssetName is the keyless cosign bundle for the checksum manifest.
	SignatureAssetName = "checksums.txt.sigstore.json"
	// SBOMAssetName is the module-level CycloneDX software bill of materials.
	SBOMAssetName = "acr.cdx.json"
)

// Target identifies one supported release platform.
type Target struct {
	GOOS   string
	GOARCH string
}

// Name returns the stable archive asset name for the target.
func (target Target) Name() string {
	return "acr-" + target.GOOS + "-" + target.GOARCH + ".tar.gz"
}

// Targets returns the four MVP targets in asset-name order.
func Targets() []Target {
	return []Target{
		{GOOS: "darwin", GOARCH: "amd64"},
		{GOOS: "darwin", GOARCH: "arm64"},
		{GOOS: "linux", GOARCH: "amd64"},
		{GOOS: "linux", GOARCH: "arm64"},
	}
}

// Binary is one compiled acr executable awaiting packaging.
type Binary struct {
	Target Target
	Bytes  []byte
}

// Asset is one GitHub release upload.
type Asset struct {
	Name        string
	ContentType string
	Bytes       []byte
}

// Bundle contains all deterministic assets produced from compiled binaries.
type Bundle struct {
	Archives  []Asset
	Checksums Asset
}

// Pack creates the four normalized archives and their checksum manifest.
func Pack(binaries []Binary, license []byte) (Bundle, error) {
	if len(license) == 0 {
		return Bundle{}, fmt.Errorf("pack acr release: LICENSE is empty; package the repository license and retry")
	}
	expected := make(map[Target]struct{}, len(Targets()))
	for _, target := range Targets() {
		expected[target] = struct{}{}
	}
	seen := make(map[Target]struct{}, len(binaries))
	archives := make([]Asset, 0, len(binaries))
	for _, binary := range binaries {
		if _, supported := expected[binary.Target]; !supported {
			return Bundle{}, fmt.Errorf("pack acr release: target %s/%s is outside the macOS/Linux amd64/arm64 release set", binary.Target.GOOS, binary.Target.GOARCH)
		}
		if _, duplicate := seen[binary.Target]; duplicate {
			return Bundle{}, fmt.Errorf("pack acr release: target %s/%s appears more than once", binary.Target.GOOS, binary.Target.GOARCH)
		}
		if len(binary.Bytes) == 0 {
			return Bundle{}, fmt.Errorf("pack acr release: binary for %s/%s is empty; rebuild the target and retry", binary.Target.GOOS, binary.Target.GOARCH)
		}
		seen[binary.Target] = struct{}{}
		archive, err := packArchive(binary.Bytes, license)
		if err != nil {
			return Bundle{}, fmt.Errorf("pack acr release target %s/%s: %w", binary.Target.GOOS, binary.Target.GOARCH, err)
		}
		archives = append(archives, Asset{Name: binary.Target.Name(), ContentType: "application/gzip", Bytes: archive})
	}
	for _, target := range Targets() {
		if _, exists := seen[target]; !exists {
			return Bundle{}, fmt.Errorf("pack acr release: missing binary for %s/%s; build all four targets before publishing", target.GOOS, target.GOARCH)
		}
	}
	sort.Slice(archives, func(left, right int) bool { return archives[left].Name < archives[right].Name })
	checksums := Checksums(archives)
	return Bundle{
		Archives:  archives,
		Checksums: Asset{Name: ChecksumsAssetName, ContentType: "text/plain; charset=utf-8", Bytes: checksums},
	}, nil
}

func packArchive(binary, license []byte) ([]byte, error) {
	var writer tarball.Writer
	if err := writer.Add("acr", 0o755, binary); err != nil {
		return nil, err
	}
	if err := writer.Add("LICENSE", 0o644, license); err != nil {
		return nil, err
	}
	compressed, _, err := writer.Bytes()
	return compressed, err
}
