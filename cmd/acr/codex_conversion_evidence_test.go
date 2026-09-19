package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	inventories := map[string]any{}
	for phase, inventory := range map[string][]codexTreeEntry{"baseline": baseline, "converted": codexGitInventory(t, root, generated)} {
		data, err := json.Marshal(inventory)
		if err != nil {
			t.Fatal(err)
		}
		lane.record(phase+"-inventory.json", string(data))
		inventories[phase] = map[string]any{"path": prefix + phase + "-inventory.json", "sha256": codexEvidenceHash(data)}
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
