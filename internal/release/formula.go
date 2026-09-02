package release

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

var releaseVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// ArchiveDigest binds one release target to its published SHA-256 digest.
type ArchiveDigest struct {
	Target Target
	SHA256 string
}

// RenderFormula renders the tested Homebrew formula for one stable release.
func RenderFormula(version string, digests []ArchiveDigest) ([]byte, error) {
	if !releaseVersionPattern.MatchString(version) {
		return nil, fmt.Errorf("render Homebrew formula: version %q is not canonical MAJOR.MINOR.PATCH; use the stable release version without v", version)
	}
	want := make(map[Target]struct{}, len(Targets()))
	for _, target := range Targets() {
		want[target] = struct{}{}
	}
	values := make(map[Target]string, len(digests))
	for _, digest := range digests {
		if _, supported := want[digest.Target]; !supported {
			return nil, fmt.Errorf("render Homebrew formula: target %s/%s is not supported", digest.Target.GOOS, digest.Target.GOARCH)
		}
		decoded, err := hex.DecodeString(digest.SHA256)
		if err != nil || len(decoded) != 32 || strings.ToLower(digest.SHA256) != digest.SHA256 {
			return nil, fmt.Errorf("render Homebrew formula: SHA-256 for %s/%s must be 64 lowercase hexadecimal characters", digest.Target.GOOS, digest.Target.GOARCH)
		}
		if _, duplicate := values[digest.Target]; duplicate {
			return nil, fmt.Errorf("render Homebrew formula: target %s/%s appears more than once", digest.Target.GOOS, digest.Target.GOARCH)
		}
		values[digest.Target] = digest.SHA256
	}
	for _, target := range Targets() {
		if _, exists := values[target]; !exists {
			return nil, fmt.Errorf("render Homebrew formula: missing SHA-256 for %s/%s; render only after every archive is published", target.GOOS, target.GOARCH)
		}
	}

	url := func(target Target) string {
		return "https://github.com/jbaruch/agentic-context-registry/releases/download/v" + version + "/" + target.Name()
	}
	darwinAMD64 := Target{GOOS: "darwin", GOARCH: "amd64"}
	darwinARM64 := Target{GOOS: "darwin", GOARCH: "arm64"}
	linuxAMD64 := Target{GOOS: "linux", GOARCH: "amd64"}
	linuxARM64 := Target{GOOS: "linux", GOARCH: "arm64"}
	formula := fmt.Sprintf(`class Acr < Formula
  desc "GitHub-native package manager for coding-agent context"
  homepage "https://github.com/jbaruch/agentic-context-registry"
  version %q
  license "Apache-2.0"

  livecheck do
    url "https://github.com/jbaruch/agentic-context-registry/releases/latest"
    strategy :github_latest
  end

  on_macos do
    if Hardware::CPU.arm?
      url %q
      sha256 %q
    else
      url %q
      sha256 %q
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url %q
      sha256 %q
    else
      url %q
      sha256 %q
    end
  end

  def install
    bin.install "acr"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/acr version")
  end
end
`, version,
		url(darwinARM64), values[darwinARM64],
		url(darwinAMD64), values[darwinAMD64],
		url(linuxARM64), values[linuxARM64],
		url(linuxAMD64), values[linuxAMD64],
	)
	return []byte(formula), nil
}
