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
	"github.com/jbaruch/agentic-context-registry/internal/tesslplugin"
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

func TestVendorUnknownHookEventRefusesWithoutWriting(t *testing.T) {
	root := writeUnmappedConsumer(t)
	packageRoot := filepath.Join(root, ".tessl/plugins/example/orphan")
	if err := os.MkdirAll(filepath.Join(packageRoot, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "hooks/check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	event := "BeforeCoffee"
	pluginJSON := []byte(`{"name":"example/orphan","version":"legacy","rules":["rules"],"skills":["skills"],"hooks":{"` + event + `":[{"hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/check.sh"]}]}]}}`)
	if err := os.WriteFile(filepath.Join(packageRoot, ".tessl-plugin/plugin.json"), pluginJSON, 0o644); err != nil {
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
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatalf("decode CLI error %q: %v", stderr, err)
	}
	if envelope.OK || envelope.Error.Code != tesslplugin.CodeUnmappedField || !strings.Contains(envelope.Error.Message, event) {
		t.Fatalf("unknown-event refusal = %#v, want typed error naming %q", envelope, event)
	}
	if after := hashTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("unknown-event refusal changed project: before=%v after=%v", before, after)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unknown-event refusal created .agents: %v", err)
	}
}
