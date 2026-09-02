package publish

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

const (
	MetadataVersion    = 1
	MetadataAssetName  = "acr-package.json"
	ChecksumsAssetName = "checksums.txt"
)

// ArchiveMetadata identifies the deterministic archive and its raw tar
// stream.
type ArchiveMetadata struct {
	Name      string `json:"name"`
	SHA256    string `json:"sha256"`
	TarSHA256 string `json:"tarSha256"`
}

// AdapterMetadata records the exact realization boundary exercised before
// the release was published.
type AdapterMetadata struct {
	ID       string                  `json:"id"`
	Version  string                  `json:"version"`
	Boundary adapter.BoundaryVersion `json:"boundary"`
}

// Metadata is the schema-versioned evidence uploaded with every package
// release.
type Metadata struct {
	MetadataVersion int               `json:"metadataVersion"`
	Name            string            `json:"name"`
	Version         string            `json:"version"`
	SchemaVersion   int               `json:"schemaVersion"`
	Tag             string            `json:"tag"`
	Commit          string            `json:"commit"`
	Repository      string            `json:"repository"`
	ContentHash     string            `json:"contentHash"`
	Files           []FileMetadata    `json:"files"`
	Archive         ArchiveMetadata   `json:"archive"`
	Adapters        []AdapterMetadata `json:"adapters"`
	Generator       string            `json:"generator"`
}

// Asset is one GitHub release upload.
type Asset struct {
	Name        string
	ContentType string
	Bytes       []byte
}

// ReleaseAssets is the complete immutable upload set.
type ReleaseAssets struct {
	Archive   Asset
	Metadata  Asset
	Checksums Asset
	Evidence  Metadata
}

// BuildReleaseAssets creates the archive, schema-versioned metadata, and
// sha256sum-compatible checksums file.
func BuildReleaseAssets(value manifest.Manifest, identity Identity, files []File, descriptors []adapter.Descriptor, generator string) (ReleaseAssets, error) {
	slash := strings.LastIndexByte(value.Name, '/')
	if slash < 0 || slash == len(value.Name)-1 {
		return ReleaseAssets{}, fmt.Errorf("derive archive repository name from %q", value.Name)
	}
	repository := value.Name[slash+1:]
	archive, err := BuildArchive(repository, value.Version, files)
	if err != nil {
		return ReleaseAssets{}, err
	}
	contentHash, fileMetadata, err := describeContent(value, files)
	if err != nil {
		return ReleaseAssets{}, err
	}
	archiveDigest := sha256.Sum256(archive.Bytes)
	adapters := make([]AdapterMetadata, len(descriptors))
	for index, descriptor := range descriptors {
		adapters[index] = AdapterMetadata{ID: descriptor.ID, Version: descriptor.Version, Boundary: descriptor.Boundary}
	}
	sort.Slice(adapters, func(left, right int) bool { return adapters[left].ID < adapters[right].ID })
	evidence := Metadata{
		MetadataVersion: MetadataVersion,
		Name:            value.Name,
		Version:         value.Version,
		SchemaVersion:   value.SchemaVersion,
		Tag:             identity.Tag,
		Commit:          identity.Commit,
		Repository:      value.Source.Repository,
		ContentHash:     contentHash,
		Files:           fileMetadata,
		Archive: ArchiveMetadata{
			Name: archive.Name, SHA256: hex.EncodeToString(archiveDigest[:]), TarSHA256: archive.TarSHA256,
		},
		Adapters:  adapters,
		Generator: generator,
	}
	metadataBytes, err := json.Marshal(evidence)
	if err != nil {
		return ReleaseAssets{}, fmt.Errorf("encode package release metadata: %w", err)
	}
	metadataBytes = append(metadataBytes, '\n')
	archiveAsset := Asset{Name: archive.Name, ContentType: "application/gzip", Bytes: archive.Bytes}
	metadataAsset := Asset{Name: MetadataAssetName, ContentType: "application/json", Bytes: metadataBytes}
	checksums := checksumFile(archiveAsset, metadataAsset)
	return ReleaseAssets{
		Archive: archiveAsset, Metadata: metadataAsset,
		Checksums: Asset{Name: ChecksumsAssetName, ContentType: "text/plain; charset=utf-8", Bytes: checksums},
		Evidence:  evidence,
	}, nil
}

func checksumFile(assets ...Asset) []byte {
	ordered := append([]Asset(nil), assets...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].Name < ordered[right].Name })
	result := make([]byte, 0, len(ordered)*128)
	for _, asset := range ordered {
		digest := sha256.Sum256(asset.Bytes)
		result = fmt.Appendf(result, "%s  %s\n", hex.EncodeToString(digest[:]), asset.Name)
	}
	return result
}
