package migrateapp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// reverifyPut writes one fixture file, creating parents. Independent of the
// developer's fixture builders on purpose.
func reverifyPut(t *testing.T, root, relative, content string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const reverifyAlwaysRule = "---\nalwaysApply: true\n---\n# Always\n"

// reverifyUnmappedRoot refuses with unmapped_field: private: true is a concept
// #4 cannot express and the refusal carries conversion evidence.
func reverifyUnmappedRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	reverifyPut(t, root, ".tessl-plugin/plugin.json",
		`{"name":"example/alpha","version":"1.0.0","description":"alpha plugin",`+
			`"repository":"https://github.com/example/alpha","private":true,"rules":["rules/always.md"]}`+"\n")
	reverifyPut(t, root, "rules/always.md", reverifyAlwaysRule)
	return root
}

// reverifyAmbiguousRoot refuses with ambiguous_manifest before any conversion
// evidence exists.
func reverifyAmbiguousRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	reverifyPut(t, root, ".tessl-plugin/plugin.json",
		`{"name":"example/alpha","version":"1.0.0","description":"plugin side",`+
			`"repository":"https://github.com/example/alpha","rules":["rules/always.md"]}`+"\n")
	reverifyPut(t, root, "tile.json",
		`{"name":"example/alpha","version":"1.0.0","summary":"tile side","rules":["rules/always.md"]}`+"\n")
	reverifyPut(t, root, "rules/always.md", reverifyAlwaysRule)
	return root
}

// reverifyConflictRoot refuses with manifest_conflict against a hand-edited
// manifest the tool must never overwrite.
func reverifyConflictRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	reverifyPut(t, root, ".tessl-plugin/plugin.json",
		`{"name":"example/alpha","version":"1.0.0","description":"alpha plugin",`+
			`"repository":"https://github.com/example/alpha","rules":["rules/always.md"]}`+"\n")
	reverifyPut(t, root, "rules/always.md", reverifyAlwaysRule)
	reverifyPut(t, root, manifest.Filename, "schemaVersion: 1\nname: example/alpha\nversion: 9.9.9\n"+
		"description: hand edited\nsource:\n  repository: https://github.com/example/alpha\n"+
		"artifacts:\n  rules:\n    - id: always\n      path: rules/always.md\n      activation:\n        mode: always\n")
	return root
}

// reverifyUnpublishableRoot refuses with unpublishable_content on bytecode
// inside a declared skill tree.
func reverifyUnpublishableRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	reverifyPut(t, root, ".tessl-plugin/plugin.json",
		`{"name":"example/alpha","version":"1.0.0","description":"alpha plugin",`+
			`"repository":"https://github.com/example/alpha","skills":["skills/demo"]}`+"\n")
	reverifyPut(t, root, "skills/demo/SKILL.md", "# Demo\n")
	reverifyPut(t, root, "skills/demo/__pycache__/mod.cpython-311.pyc", "bytecode\n")
	return root
}

type reverifyEnvelope struct {
	OK      bool            `json:"ok"`
	Command string          `json:"command"`
	Result  json.RawMessage `json:"result"`
	Error   struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Field   string `json:"field"`
	} `json:"error"`
}

// reverifyDecodeRefusal reads the whole stderr envelope with unknown fields
// rejected, so an unexpected property is a failure rather than silently ignored.
func reverifyDecodeRefusal(t *testing.T, stderr string) reverifyEnvelope {
	t.Helper()
	if strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") {
		t.Fatalf("stderr must be exactly one envelope line, got %q", stderr)
	}
	decoder := json.NewDecoder(strings.NewReader(stderr))
	decoder.DisallowUnknownFields()
	var envelope reverifyEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("stderr envelope does not decode fully: %v (%q)", err, stderr)
	}
	if decoder.More() {
		t.Fatalf("stderr carries trailing content: %q", stderr)
	}
	return envelope
}

// NEW-1: a refusal carries result only when conversion identified unmapped
// input, and that partial report is the shipped schema version naming the field.
func TestReverifyRefusalResultTracksUnmappedEvidence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		root         func(*testing.T) string
		code         string
		field        string
		wantUnmapped bool
		manifestKept bool
	}{
		{name: "ambiguousManifest", root: reverifyAmbiguousRoot, code: "ambiguous_manifest", field: "description"},
		{name: "manifestConflict", root: reverifyConflictRoot, code: "manifest_conflict", field: manifest.Filename, manifestKept: true},
		{name: "unpublishableContent", root: reverifyUnpublishableRoot, code: "unpublishable_content", field: "skills/demo/__pycache__/mod.cpython-311.pyc"},
		{name: "unmappedField", root: reverifyUnmappedRoot, code: "unmapped_field", field: "private", wantUnmapped: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := test.root(t)
			var before []byte
			if test.manifestKept {
				content, err := os.ReadFile(filepath.Join(root, manifest.Filename))
				if err != nil {
					t.Fatal(err)
				}
				before = content
			}

			stdout, stderr, exitCode := runCLI(t, NewApplication(nil), "migrate", "tessl-plugin", root, "--json")

			if exitCode != cli.ExitOperational {
				t.Fatalf("exit = %d, want %d (stderr %q)", exitCode, cli.ExitOperational, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout must stay empty on refusal, got %q", stdout)
			}
			envelope := reverifyDecodeRefusal(t, stderr)
			if envelope.OK || envelope.Command != "migrate" {
				t.Fatalf("envelope = %+v", envelope)
			}
			if envelope.Error.Code != test.code || envelope.Error.Field != test.field {
				t.Fatalf("error = %+v, want code %q field %q", envelope.Error, test.code, test.field)
			}
			if envelope.Error.Message == "" {
				t.Fatal("refusal carries no message")
			}
			if !test.wantUnmapped {
				if envelope.Result != nil {
					t.Fatalf("refusal with no unmapped input carries result: %s", envelope.Result)
				}
			} else {
				if envelope.Result == nil {
					t.Fatal("refusal that identified unmapped input carries no result")
				}
				var report struct {
					ReportVersion int `json:"reportVersion"`
					Unmapped      []struct {
						Field  string `json:"field"`
						Reason string `json:"reason"`
					} `json:"unmapped"`
				}
				if err := json.Unmarshal(envelope.Result, &report); err != nil {
					t.Fatalf("result does not decode: %v (%s)", err, envelope.Result)
				}
				if report.ReportVersion != 1 {
					t.Fatalf("result.reportVersion = %d, want 1 (%s)", report.ReportVersion, envelope.Result)
				}
				if len(report.Unmapped) != 1 || report.Unmapped[0].Field != test.field {
					t.Fatalf("result.unmapped = %+v, want one entry naming %q", report.Unmapped, test.field)
				}
				if report.Unmapped[0].Reason == "" {
					t.Fatal("result.unmapped entry carries no reason")
				}
			}
			if test.manifestKept {
				after, err := os.ReadFile(filepath.Join(root, manifest.Filename))
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != string(before) {
					t.Fatalf("refused conversion rewrote %s", manifest.Filename)
				}
				return
			}
			if _, err := os.Stat(filepath.Join(root, manifest.Filename)); !os.IsNotExist(err) {
				t.Fatalf("refused conversion wrote %s: %v", manifest.Filename, err)
			}
		})
	}
}

// N2: text mode renders the same unmapped section the JSON envelope carries,
// and neither mode leaks the other's bytes onto the wrong stream.
func TestReverifyTextRefusalRendersUnmappedSection(t *testing.T) {
	t.Parallel()

	root := reverifyUnmappedRoot(t)
	stdout, stderr, exitCode := runCLI(t, NewApplication(nil), "migrate", "tessl-plugin", root)

	if exitCode != cli.ExitOperational {
		t.Fatalf("exit = %d, want %d (stderr %q)", exitCode, cli.ExitOperational, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must stay empty on refusal, got %q", stdout)
	}
	if strings.Contains(stderr, "{") || strings.Contains(stderr, `"ok"`) {
		t.Fatalf("text mode leaked JSON onto stderr: %q", stderr)
	}
	if !strings.HasPrefix(stderr, "acr migrate: private: true cannot be published") {
		t.Fatalf("stderr does not open with the diagnostic: %q", stderr)
	}
	if !strings.Contains(stderr, "\nunmapped:\n") {
		t.Fatalf("stderr carries no unmapped section: %q", stderr)
	}
	if !strings.Contains(stderr, "  - private: ") {
		t.Fatalf("unmapped section does not name the field: %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(root, manifest.Filename)); !os.IsNotExist(err) {
		t.Fatalf("refused conversion wrote %s: %v", manifest.Filename, err)
	}
}

// A refusal with no unmapped evidence appends no section in text mode either.
func TestReverifyTextRefusalWithoutUnmappedIsDiagnosticOnly(t *testing.T) {
	t.Parallel()

	root := reverifyAmbiguousRoot(t)
	stdout, stderr, exitCode := runCLI(t, NewApplication(nil), "migrate", "tessl-plugin", root)

	if exitCode != cli.ExitOperational {
		t.Fatalf("exit = %d, want %d (stderr %q)", exitCode, cli.ExitOperational, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must stay empty on refusal, got %q", stdout)
	}
	if strings.Contains(stderr, "unmapped:") {
		t.Fatalf("refusal with no unmapped input rendered a section: %q", stderr)
	}
	if strings.Count(stderr, "\n") != 1 {
		t.Fatalf("stderr must be one diagnostic line, got %q", stderr)
	}
}

// A3 as behavior: --non-interactive has no meaning for producer conversion and
// is refused as a usage error rather than accepted and ignored.
func TestReverifyNonInteractiveIsRefusedOnProducerConversion(t *testing.T) {
	t.Parallel()

	root := reverifyUnmappedRoot(t)
	stdout, stderr, exitCode := runCLI(t, NewApplication(nil), "migrate", "tessl-plugin", root, "--non-interactive")

	if exitCode != cli.ExitUsage {
		t.Fatalf("exit = %d, want %d (stderr %q)", exitCode, cli.ExitUsage, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must stay empty on a usage error, got %q", stdout)
	}
	if !strings.Contains(stderr, "--non-interactive is not supported by acr migrate tessl-plugin") {
		t.Fatalf("stderr = %q, want the producer usage diagnostic", stderr)
	}
}
