package release

import (
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
