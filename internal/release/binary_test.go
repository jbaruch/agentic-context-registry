package release

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

func TestReleaseBinaryIsReproducible(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	output := t.TempDir()
	first := filepath.Join(output, "acr-first")
	second := filepath.Join(output, "acr-second")
	const version = "1.2.3"
	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ldflags := "-s -w -X main.version=" + version + " -X main.commit=" + commit
	for _, path := range []string{first, second} {
		command := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", path, "./cmd/acr")
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build stamped acr: %v\n%s", err, output)
		}
	}
	firstBytes, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatal("two release builds from the same source and flags produced different bytes")
	}

	command := exec.Command(first, "version", "--json")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("run stamped acr: %v, stderr = %q", err, stderr.String())
	}
	var envelope struct {
		OK      bool   `json:"ok"`
		Command string `json:"command"`
		Result  struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode stamped version JSON %q: %v", stdout.String(), err)
	}
	if !envelope.OK || envelope.Command != "version" || !dependency.TagMatchesVersion("v"+version, envelope.Result.Version) || envelope.Result.Commit != commit || stderr.Len() != 0 {
		t.Fatalf("stamped version = %#v, stderr = %q", envelope, stderr.String())
	}
}
