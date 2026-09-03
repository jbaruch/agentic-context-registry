package migrateapp

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// verify8Round3WriteTwoPackageConsumer seeds a consumer holding example/orphan
// and example/alpha, so a single structured config file can carry one Tessl
// splice per package. The splices are written orphan-first while the identities
// sort alpha-first, which is what makes the removal order assertion below
// discriminating rather than a restatement of the discovery order.
func verify8Round3WriteTwoPackageConsumer(t *testing.T) string {
	t.Helper()
	root := writeUnmappedConsumer(t)
	packageRoot := filepath.Join(root, ".tessl/plugins/example/alpha")
	for _, directory := range []string{
		filepath.Join(packageRoot, ".tessl-plugin"),
		filepath.Join(packageRoot, "rules"),
		filepath.Join(packageRoot, "skills", "audit"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{
		filepath.Join(packageRoot, ".tessl-plugin/plugin.json"): []byte(`{"name":"example/alpha","version":"legacy","rules":["rules"],"skills":["skills"]}`),
		filepath.Join(packageRoot, "rules/always.md"):           []byte("Always alpha.\n"),
		filepath.Join(packageRoot, "skills/audit/SKILL.md"):     []byte("# Audit\n"),
	}
	for filename, content := range files {
		if err := os.WriteFile(filename, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../../.tessl/plugins/example/alpha/skills/audit",
		filepath.Join(root, ".claude/skills/tessl__audit")); err != nil {
		t.Fatal(err)
	}
	document := map[string]any{"name": "consumer", "dependencies": map[string]any{
		"example/orphan": map[string]string{"version": "legacy"},
		"example/alpha":  map[string]string{"version": "legacy"},
	}}
	tesslJSON, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tessl.json"), tesslJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestVerify8Round3RemovedIsSortedNotDiscoveryOrdered pins tester advisory 3 at
// the report the operator reads: two splices in one file, written orphan-first
// while the identities sort alpha-first.
//
// Probed and recorded: with SortMigrationReport's Removed sort deleted this row
// still passes, because plan.Edits is already path-ordered and no reachable
// removal path sorts after the forced-last "tessl.json". The sort itself is
// therefore pinned directly in internal/migrate; this row pins that whatever
// produces the order, the emitted report is sorted.
func TestVerify8Round3RemovedIsSortedNotDiscoveryOrdered(t *testing.T) {
	root := verify8Round3WriteTwoPackageConsumer(t)
	settings := []byte(`{"hooks":{"SessionStart":[` +
		`{"hooks":[{"type":"command","command":"tessl hook run --plugin-path=\".tessl/plugins/example/orphan\" --event=\"SessionStart\""}]},` +
		`{"hooks":[{"type":"command","command":"tessl hook run --plugin-path=\".tessl/plugins/example/alpha\" --event=\"SessionStart\""}]}` +
		`]}}`)
	if err := os.WriteFile(filepath.Join(root, ".claude/settings.json"), settings, 0o644); err != nil {
		t.Fatal(err)
	}
	service := newService(vendorPanicRemote{})
	if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
		t.Fatal(err)
	}
	report, err := service.Migrate(context.Background(), root, Options{Finalize: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}

	keys := make([]string, 0, len(report.Removed))
	for _, removal := range report.Removed {
		keys = append(keys, removal.Path+"\x00"+removal.Kind+"\x00"+removal.ID)
	}
	if !sortedAscending(keys) {
		t.Fatalf("removed[] is not sorted by (path, kind, id): %#v", keys)
	}
	splices := make([]string, 0, 2)
	for _, removal := range report.Removed {
		if removal.Path == ".claude/settings.json" {
			splices = append(splices, removal.ID)
		}
	}
	// The file lists orphan first; the canonical order must invert that.
	want := []string{"tessl.hooks.example/alpha", "tessl.hooks.example/orphan"}
	if !reflect.DeepEqual(splices, want) {
		t.Fatalf("splice removal order = %#v, want %#v", splices, want)
	}
}

func sortedAscending(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index] < values[index-1] {
			return false
		}
	}
	return true
}

// TestVerify8Round3GitignoreBytesAfterFinalization pins tester advisory 4 on the
// finalized file rather than on removeGitignoreBlock in isolation, and covers
// the CRLF shape no unit test reaches.
func TestVerify8Round3GitignoreBytesAfterFinalization(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		before string
		want   string
	}{
		{
			name:   "lf",
			before: "# user policy\n/build/\n\n# === Tessl-generated artifacts (managed by tessl) ===\n.tessl/cache/\n# === end Tessl-generated artifacts ===\n\n*.tmp\n",
			want:   "# user policy\n/build/\n\n*.tmp\n",
		},
		{
			name:   "crlf",
			before: "# user policy\r\n/build/\r\n\r\n# === Tessl-generated artifacts (managed by tessl) ===\r\n.tessl/cache/\r\n# === end Tessl-generated artifacts ===\r\n\r\n*.tmp\r\n",
			want:   "# user policy\r\n/build/\r\n\r\n*.tmp\r\n",
		},
		{
			name:   "block at end of file",
			before: "/build/\n# === Tessl-generated artifacts (managed by tessl) ===\n.tessl/cache/\n# === end Tessl-generated artifacts ===\n",
			want:   "/build/\n",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := writeUnmappedConsumer(t)
			gitignore := filepath.Join(root, ".gitignore")
			if err := os.WriteFile(gitignore, []byte(testCase.before), 0o644); err != nil {
				t.Fatal(err)
			}
			service := newService(vendorPanicRemote{})
			if _, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Migrate(context.Background(), root, Options{Finalize: true}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(gitignore)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != testCase.want {
				t.Fatalf(".gitignore after finalization = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestVerify8Round3UnsupportedHookLeafKeepsInventoryUsable pins NEW-1 on the
// whole report rather than on one artifact row: a non-regular hook leaf must
// classify unsupported without taking the package's other artifacts down with
// it, and vendor synthesis must still refuse before writing.
//
// The hook's internal reason (hook-script-type) is deliberately not asserted:
// migrate.Report.Unsupported is populated only for MCP-server paths, so no hook
// reason reaches the report at all. Recorded as an advisory, not a test.
func TestVerify8Round3UnsupportedHookLeafKeepsInventoryUsable(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		hookPath string
		refusal  string
	}{
		{name: "directory leaf", hookPath: "rules", refusal: `"rules" must be a regular file`},
		{name: "dot leaf", hookPath: ".", refusal: "outside the closed Tessl grammar"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := writeUnmappedConsumer(t)
			packageRoot := filepath.Join(root, ".tessl/plugins/example/orphan")
			pluginJSON := []byte(`{"name":"example/orphan","version":"legacy","rules":["rules"],"skills":["skills"],` +
				`"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/` + testCase.hookPath + `"]}]}]}}`)
			if err := os.WriteFile(filepath.Join(packageRoot, ".tessl-plugin/plugin.json"), pluginJSON, 0o644); err != nil {
				t.Fatal(err)
			}
			before := hashTree(t, root)

			stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl", "--dry-run", "--json", "--project", root)
			if exitCode != cli.ExitSuccess || stderr != "" {
				t.Fatalf("inventory exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
			}
			var envelope struct {
				Result struct {
					Packages []struct {
						Artifacts []struct {
							Kind           string `json:"kind"`
							Classification string `json:"classification"`
						} `json:"artifacts"`
					} `json:"packages"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatalf("decode inventory %q: %v", stdout, err)
			}
			hooks, kinds := 0, map[string]int{}
			for _, pkg := range envelope.Result.Packages {
				for _, artifact := range pkg.Artifacts {
					kinds[artifact.Kind]++
					if artifact.Kind != "hook" {
						continue
					}
					hooks++
					if artifact.Classification != "unsupported" {
						t.Fatalf("hook artifact = %+v, want classification unsupported", artifact)
					}
				}
			}
			if hooks != 1 {
				t.Fatalf("hook artifacts = %d, want 1: %#v", hooks, envelope.Result.Packages)
			}
			// The unsupported hook must not take the package's other artifacts
			// down with it: the inventory is still usable for the rest.
			if kinds["rule"] == 0 || kinds["skill"] == 0 {
				t.Fatalf("inventory dropped the package's other artifacts: %#v", kinds)
			}

			_, err := newService(vendorPanicRemote{}).Migrate(context.Background(), root, Options{VendorUnmapped: true})
			if err == nil || !strings.Contains(err.Error(), testCase.refusal) {
				t.Fatalf("vendor refusal = %v, want %q", err, testCase.refusal)
			}
			if after := hashTree(t, root); !mapsEqual(before, after) {
				t.Fatalf("the refusal changed the project: before=%v after=%v", before, after)
			}
			if _, statErr := os.Stat(filepath.Join(root, ".agents")); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("the refusal created .agents: %v", statErr)
			}
		})
	}
}

// TestVerify8Round3ValidationCodesReachTheCLITyped pins NEW-3 beyond the single
// no_artifacts case: a second, structurally different validation failure must
// also arrive as its own manifest code with a nonempty remedy naming the field,
// which is what proves the mapping is generic rather than special-cased.
func TestVerify8Round3ValidationCodesReachTheCLITyped(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		plugin string
		extra  map[string]string
		code   manifest.ErrorCode
	}{
		{
			name:   "artifact-free manifest",
			plugin: `{"name":"example/orphan","version":"legacy"}`,
			code:   manifest.CodeNoArtifacts,
		},
		{
			name: "one ID maps to two artifact paths",
			// Two rule files whose IDs collapse to the same identifier, so the
			// synthesized manifest fails a structurally different check than the
			// artifact-free case above.
			plugin: `{"name":"example/orphan","version":"legacy","rules":["rules"],"skills":["skills"]}`,
			extra: map[string]string{
				"rules/Error_Handling.md": "Handle errors.\n",
				"rules/error-handling.md": "Handle errors.\n",
			},
			code: manifest.CodeDuplicateArtifactID,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := writeUnmappedConsumer(t)
			plugin := filepath.Join(root, ".tessl/plugins/example/orphan/.tessl-plugin/plugin.json")
			if err := os.WriteFile(plugin, []byte(testCase.plugin), 0o644); err != nil {
				t.Fatal(err)
			}
			for relative, content := range testCase.extra {
				name := filepath.Join(root, ".tessl/plugins/example/orphan", filepath.FromSlash(relative))
				if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
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
					Code   string `json:"code"`
					Remedy string `json:"remedy"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
				t.Fatalf("decode CLI error %q: %v", stderr, err)
			}
			if envelope.OK || envelope.Error.Code != string(testCase.code) {
				t.Fatalf("envelope = %#v, want code %s", envelope, testCase.code)
			}
			if envelope.Error.Remedy == "" || strings.Contains(stderr, `"code":"migrate_failed"`) {
				t.Fatalf("envelope = %#v, stderr = %q", envelope, stderr)
			}
			if after := hashTree(t, root); !mapsEqual(before, after) {
				t.Fatalf("the refusal changed the project: before=%v after=%v", before, after)
			}
			if _, statErr := os.Stat(filepath.Join(root, ".agents")); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("the refusal created .agents: %v", statErr)
			}
		})
	}
}
