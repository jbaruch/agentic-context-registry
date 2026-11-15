package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type codexSuiteEvidence struct {
	Phase    string   `json:"phase"`
	Argv     []string `json:"argv"`
	ExitCode int      `json:"exit_code"`
	Output   string   `json:"output"`
	SHA256   string   `json:"sha256"`
	Counts   []int    `json:"counts"`
}

type codexTreeEntry struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
}

func codexEvidenceHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func codexGitInventory(t *testing.T, root, revision string) []codexTreeEntry {
	t.Helper()
	command := exec.Command("git", "-C", root, "ls-tree", "-rz", "--full-tree", revision)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	entries := []codexTreeEntry{}
	for _, entry := range strings.Split(string(output), "\x00") {
		if entry == "" {
			continue
		}
		meta, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[1] != "blob" {
			t.Fatal("unsupported inventory entry")
		}
		command = exec.Command("git", "-C", root, "cat-file", "blob", fields[2])
		body, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, codexTreeEntry{Path: name, Mode: fields[0], SHA256: codexEvidenceHash(body)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries
}

// This projection is written only after every actual operation and original
// suite has passed. Boundary records are copied from checked CLI reports.
func (lane *codexLiveLane) writeFixtureReceipt(root string, fixture codexLiveFixture, binary, upstream, generated, repository string, baseline []codexTreeEntry, runs []journeyRun) {
	t := lane.t
	t.Helper()
	key := strings.ToLower(fixture.key)
	prefix := "evidence/" + key + "/"
	names := []string{"deterministic-dry-run", "dry-run", "apply", "validate", "rerun"}
	if len(runs) != len(names) {
		t.Fatal("missing actual operation evidence")
	}
	operations := []map[string]any{}
	boundaries := []map[string]any{}
	var version string
	for i, run := range runs {
		body := run.stdout
		if i == 0 {
			body = run.stderr
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if run.exit != 1 || journeyError(t, body)["code"] != "unsupported_semantic_conversion" {
				t.Fatal("missing deterministic refusal")
			}
		} else if run.exit != 0 || envelope["ok"] != true {
			t.Fatal("unsuccessful operation in receipt")
		}
		lane.record(names[i]+".json", body)
		actual, err := os.ReadFile(filepath.Join(lane.evidence, names[i]+".json"))
		if err != nil {
			t.Fatal(err)
		}
		operations = append(operations, map[string]any{"name": names[i], "argv": append([]string{binary}, run.args...), "exit_code": run.exit, "output": prefix + names[i] + ".json", "sha256": codexEvidenceHash(actual)})
		if i == 1 || i == 2 {
			result := journeyResult(t, body)
			lane.assertCodexRuns(result)
			version, _ = result["version"].(string)
			for index, agent := range codexRuns(t, result) {
				boundaries = append(boundaries, map[string]any{"phase": names[i], "index": index, "boundary": agent["credentialBoundary"]})
			}
		}
	}
	if len(lane.commands) != 2*len(fixture.tests) {
		t.Fatal("missing original command receipts")
	}
	for i := range fixture.tests {
		before, after := lane.commands[i], lane.commands[i+len(fixture.tests)]
		if len(before.Counts) != len(after.Counts) {
			t.Fatal("original suite summaries differ")
		}
		for j, n := range before.Counts {
			if after.Counts[j] < n {
				t.Fatal("original suite coverage diminished")
			}
		}
	}
	inventories := map[string]any{}
	for phase, inventory := range map[string][]codexTreeEntry{"baseline": baseline, "converted": codexGitInventory(t, root, generated)} {
		data, err := json.Marshal(inventory)
		if err != nil {
			t.Fatal(err)
		}
		lane.record(phase+"-inventory.json", string(data))
		stored, err := os.ReadFile(filepath.Join(lane.evidence, phase+"-inventory.json"))
		if err != nil {
			t.Fatal(err)
		}
		inventories[phase] = map[string]any{"path": prefix + phase + "-inventory.json", "sha256": codexEvidenceHash(stored)}
	}
	acrRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	acrSHA := journeyGit(t, acrRoot, "rev-parse", "HEAD")
	if expected := os.Getenv("ACR_ACCEPT_ACR_SHA"); expected != "" && acrSHA != expected {
		t.Fatal("candidate SHA differs from central binding")
	}
	receipt := map[string]any{
		"schema_version": 1, "key": fixture.key, "result": "passed", "acr_sha": acrSHA, "upstream_sha": upstream, "producer_sha": generated, "tree_sha": journeyGit(t, root, "rev-parse", generated+"^{tree}"), "repository": strings.TrimPrefix(repository, "https://github.com/"), "version": version, "source_root": root, "operations": operations, "commands": lane.commands, "inventories": inventories,
		"checks":              map[string]bool{"deterministic_refusal": true, "source_preserved": true, "delta_validated": true, "validate": true, "rerun": true, "clean": true},
		"credential_boundary": map[string]any{"contract": "acr-credential-boundary/v1", "plan_checked": true, "runs": boundaries},
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	lane.record("fixture-result.json", string(data)+"\n")
}

// Exercise the actual projection writer without a provider or network. These
// synthetic observations verify serialization only, never live acceptance.
func TestCodexConversionReceiptProjection(t *testing.T) {
	if scenario := os.Getenv("ACR_TEST_CONVERT_RECEIPT"); scenario != "" {
		evidence := os.Getenv("ACR_TEST_CONVERT_EVIDENCE")
		root := filepath.Join(t.TempDir(), "fixtures", "ffa")
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		journeyGit(t, root, "init")
		journeyGit(t, root, "config", "user.email", "receipt@example.invalid")
		journeyGit(t, root, "config", "user.name", "Receipt test")
		if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("decoded blob\n"), 0644); err != nil {
			t.Fatal(err)
		}
		journeyGit(t, root, "add", ".")
		journeyGit(t, root, "commit", "-m", "fixture")
		revision := journeyGit(t, root, "rev-parse", "HEAD")
		lane := &codexLiveLane{t: t, evidence: evidence, version: "codex-cli synthetic"}
		fixture := codexLiveFixtures[1]
		for _, phase := range []string{"baseline", "converted"} {
			body := "pyright 1.1.411\n0 errors, 0 warnings, 0 informations\n19/19 passed\n114/114 passed\n51/51 passed\nAll gates passed.\n"
			name := phase + "-test-1.log"
			lane.record(name, body)
			lane.commands = append(lane.commands, codexSuiteEvidence{Phase: phase, Argv: fixture.tests[0], Output: "evidence/ffa/" + name, SHA256: codexEvidenceHash([]byte(body)), Counts: []int{19, 114, 51}})
		}
		report := func(wrote bool) string {
			boundary := map[string]any{"contract": "acr-credential-boundary/v1", "authInspected": true, "proposalChecked": true, "reportSanitized": true, "isolatedHomeRemoved": true, "refreshObserved": false}
			if scenario == "missing_boundary" {
				delete(boundary, "authInspected")
			}
			data, err := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"version": "0.9.38", "wrote": wrote, "credentialBoundary": map[string]any{"contract": "acr-credential-boundary/v1", "planChecked": true, "applicationChecked": wrote, "reportSanitized": true}, "agentRuns": []any{map[string]any{"provider": "codex", "runtimeVersion": lane.version, "isolation": "synthetic", "arguments": []string{"exec"}, "requestDigest": "sha256:" + strings.Repeat("1", 64), "stdout": "{\"type\":\"turn.started\"}\n{\"type\":\"turn.completed\"}\n", "credentialBoundary": boundary}}}})
			if err != nil {
				t.Fatal(err)
			}
			return string(data)
		}
		runs := []journeyRun{{exit: 1, stderr: `{"ok":false,"error":{"code":"unsupported_semantic_conversion"}}`}, {stdout: report(false)}, {stdout: report(true)}, {stdout: `{"ok":true,"result":{}}`}, {stdout: `{"ok":true,"result":{"current":true}}`}}
		base := []string{"migrate", "tessl-plugin", root, "--acr-only", "--repository", "https://github.com/jbaruch/acr-156-ffa-validation", "--json"}
		suffixes := [][]string{{"--dry-run"}, {"--agent", "codex", "--dry-run"}, {"--agent", "codex"}, nil, {"--dry-run"}}
		for i := range runs {
			runs[i].args = append(append([]string{}, base...), suffixes[i]...)
		}
		runs[3].args = []string{"validate", root, "--json"}
		if scenario == "failed_operation" {
			runs[3].exit = 1
		}
		if scenario == "missing_command" {
			lane.commands = lane.commands[:1]
		}
		lane.writeFixtureReceipt(root, fixture, "/synthetic/acr", "142babbb1e2bebc798eb42128ac2466f21b5131d", revision, "https://github.com/jbaruch/acr-156-ffa-validation", codexGitInventory(t, root, revision), runs)
		return
	}
	for _, scenario := range []string{"valid", "missing_boundary", "failed_operation", "missing_command"} {
		t.Run(scenario, func(t *testing.T) {
			evidence := t.TempDir()
			command := exec.Command(os.Args[0], "-test.run=^TestCodexConversionReceiptProjection$")
			command.Env = append(os.Environ(), "ACR_TEST_CONVERT_RECEIPT="+scenario, "ACR_TEST_CONVERT_EVIDENCE="+evidence)
			output, err := command.CombinedOutput()
			data, readErr := os.ReadFile(filepath.Join(evidence, "fixture-result.json"))
			if scenario != "valid" {
				if err == nil || !os.IsNotExist(readErr) {
					t.Fatalf("failed projection exposed success: %v %v\n%s", err, readErr, output)
				}
				return
			}
			if err != nil || readErr != nil {
				t.Fatalf("projection: %v %v\n%s", err, readErr, output)
			}
			var receipt map[string]any
			if err := json.Unmarshal(data, &receipt); err != nil {
				t.Fatal(err)
			}
			keys := []string{}
			for key := range receipt {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			want := []string{"acr_sha", "checks", "commands", "credential_boundary", "inventories", "key", "operations", "producer_sha", "repository", "result", "schema_version", "source_root", "tree_sha", "upstream_sha", "version"}
			if !reflect.DeepEqual(keys, want) {
				t.Fatalf("receipt keys %v", keys)
			}
			for _, raw := range receipt["operations"].([]any) {
				operation := raw.(map[string]any)
				stored, err := os.ReadFile(filepath.Join(evidence, filepath.Base(operation["output"].(string))))
				if err != nil || operation["sha256"] != codexEvidenceHash(stored) {
					t.Fatal("operation hash differs from persisted evidence")
				}
			}
			boundary := receipt["credential_boundary"].(map[string]any)
			if len(boundary["runs"].([]any)) != 2 {
				t.Fatal("incomplete run projection")
			}
			for _, raw := range receipt["commands"].([]any) {
				if !reflect.DeepEqual(raw.(map[string]any)["counts"], []any{float64(19), float64(114), float64(51)}) {
					t.Fatal("suite count components lost")
				}
			}
			for _, raw := range receipt["inventories"].(map[string]any) {
				inventory := raw.(map[string]any)
				stored, err := os.ReadFile(filepath.Join(evidence, filepath.Base(inventory["path"].(string))))
				if err != nil || inventory["sha256"] != codexEvidenceHash(stored) {
					t.Fatal("inventory hash mismatch")
				}
				var entries []codexTreeEntry
				if err := json.Unmarshal(stored, &entries); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(entries, []codexTreeEntry{{Path: "source.txt", Mode: "100644", SHA256: codexEvidenceHash([]byte("decoded blob\n"))}}) {
					t.Fatalf("decoded inventory mismatch: %+v", entries)
				}
			}
		})
	}
}
