package release

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// Checksums encodes archive digests in lexicographically sorted sha256sum form.
func Checksums(assets []Asset) []byte {
	ordered := append([]Asset(nil), assets...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].Name < ordered[right].Name })
	result := make([]byte, 0, len(ordered)*96)
	for _, asset := range ordered {
		digest := sha256.Sum256(asset.Bytes)
		result = fmt.Appendf(result, "%s  %s\n", hex.EncodeToString(digest[:]), asset.Name)
	}
	return result
}

// ParseArchiveDigests validates a complete, sorted four-archive manifest.
func ParseArchiveDigests(manifest []byte) ([]ArchiveDigest, error) {
	result := make([]ArchiveDigest, 0, len(Targets()))
	var canonical bytes.Buffer
	for _, target := range Targets() {
		digest, err := checksumFor(target.Name(), manifest)
		if err != nil {
			return nil, err
		}
		result = append(result, ArchiveDigest{Target: target, SHA256: digest})
		fmt.Fprintf(&canonical, "%s  %s\n", digest, target.Name())
	}
	if !bytes.Equal(manifest, canonical.Bytes()) {
		return nil, fmt.Errorf("verify release checksums: manifest must contain exactly the four sorted release archives; download checksums.txt again")
	}
	return result, nil
}
