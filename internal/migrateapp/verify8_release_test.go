package migrateapp

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestVendorMissingDeclaredRuleDirectoryRefusesWithoutWriting(t *testing.T) {
	root := writeUnmappedConsumer(t)
	declared := "rules"
	if err := os.RemoveAll(filepath.Join(root, ".tessl/plugins/example/orphan", declared)); err != nil {
		t.Fatal(err)
	}
	before := hashTree(t, root)
	application := &Application{service: newService(vendorPanicRemote{}), fallback: cli.UnavailableApplication{}}

	stdout, stderr, exitCode := runCLI(t, application, "migrate", "tessl", "--vendor-unmapped", "--json", "--project", root)
	if exitCode != cli.ExitOperational || stdout != "" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	var envelope struct {
		OK    bool `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Remedy  string `json:"remedy"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatalf("decode CLI error %q: %v", stderr, err)
	}
	if envelope.OK || envelope.Error.Code != string(manifest.CodePathNotFound) {
		t.Fatalf("envelope = %#v, want code %s", envelope, manifest.CodePathNotFound)
	}
	if !strings.Contains(envelope.Error.Message, declared) || !strings.Contains(envelope.Error.Remedy, declared) {
		t.Fatalf("refusal does not name declared path %q: %#v", declared, envelope.Error)
	}
	if after := hashTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("missing-path refusal changed project: before=%v after=%v", before, after)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing-path refusal created .agents: %v", err)
	}
}
