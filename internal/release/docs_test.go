package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocsDoNotAdvertiseWindowsBinaries(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	paths := []string{"README.md", filepath.Join("docs", "cli.md"), filepath.Join("docs", "install.md")}
	combined := ""
	for _, path := range paths {
		contents, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		combined += string(contents)
	}
	for _, asset := range []string{
		"acr-darwin-amd64.tar.gz",
		"acr-darwin-arm64.tar.gz",
		"acr-linux-amd64.tar.gz",
		"acr-linux-arm64.tar.gz",
	} {
		if !strings.Contains(combined, asset) {
			t.Errorf("installation documentation omits %q", asset)
		}
	}
	for _, required := range []string{"brew install jbaruch/agentic-context-registry/acr", "go install github.com/jbaruch/agentic-context-registry/cmd/acr@v1.2.3", "cosign verify-blob", "gh attestation verify"} {
		if !strings.Contains(combined, required) {
			t.Errorf("installation documentation omits %q", required)
		}
	}
	for _, forbidden := range []string{"acr-windows-", "acr-macos-", "acr-linux-x86_64", "acr-linux-aarch64", "v1.2.3-rc"} {
		if strings.Contains(combined, forbidden) {
			t.Errorf("installation documentation advertises unsupported or unstable asset %q", forbidden)
		}
	}
}
