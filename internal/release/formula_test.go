package release

import (
	"strings"
	"testing"
)

func TestFormulaRendersFourPlatformURLs(t *testing.T) {
	t.Parallel()

	digests := fixtureDigests()
	digests[0], digests[3] = digests[3], digests[0]
	formula, err := RenderFormula("1.2.3", digests)
	if err != nil {
		t.Fatal(err)
	}
	const want = `class Acr < Formula
  desc "GitHub-native package manager for coding-agent context"
  homepage "https://github.com/jbaruch/agentic-context-registry"
  version "1.2.3"
  license "Apache-2.0"

  livecheck do
    url "https://github.com/jbaruch/agentic-context-registry/releases/latest"
    strategy :github_latest
  end

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/jbaruch/agentic-context-registry/releases/download/v1.2.3/acr-darwin-arm64.tar.gz"
      sha256 "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    else
      url "https://github.com/jbaruch/agentic-context-registry/releases/download/v1.2.3/acr-darwin-amd64.tar.gz"
      sha256 "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/jbaruch/agentic-context-registry/releases/download/v1.2.3/acr-linux-arm64.tar.gz"
      sha256 "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
    else
      url "https://github.com/jbaruch/agentic-context-registry/releases/download/v1.2.3/acr-linux-amd64.tar.gz"
      sha256 "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
    end
  end

  def install
    bin.install "acr"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/acr version")
  end
end
`
	if string(formula) != want {
		t.Fatalf("RenderFormula() =\n%s\nwant:\n%s", formula, want)
	}
}

func TestFormulaHasNoWindowsBranch(t *testing.T) {
	t.Parallel()

	formula, err := RenderFormula("1.2.3", fixtureDigests())
	if err != nil {
		t.Fatal(err)
	}
	for _, excluded := range []string{"windows", "x86_64", "aarch64", "bottle do"} {
		if strings.Contains(strings.ToLower(string(formula)), excluded) {
			t.Fatalf("formula contains excluded %q:\n%s", excluded, formula)
		}
	}
}

func TestFormulaRefusesIncompleteOrUnstableMetadata(t *testing.T) {
	t.Parallel()

	if _, err := RenderFormula("1.2.3-rc.1", fixtureDigests()); err == nil {
		t.Fatal("RenderFormula() accepted a prerelease")
	}
	if _, err := RenderFormula("1.2.3", fixtureDigests()[:3]); err == nil || !strings.Contains(err.Error(), "linux/arm64") {
		t.Fatalf("RenderFormula() error = %v, want missing linux/arm64", err)
	}
}

func fixtureDigests() []ArchiveDigest {
	characters := []string{"a", "b", "c", "d"}
	targets := Targets()
	result := make([]ArchiveDigest, len(targets))
	for index, target := range targets {
		result[index] = ArchiveDigest{Target: target, SHA256: strings.Repeat(characters[index], 64)}
	}
	return result
}
