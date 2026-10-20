package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

func codexConsumeTestManifest() codexConsumeManifest {
	return codexConsumeManifest{SchemaVersion: 1, Phase: "convert", Result: "passed", ACRSHA: strings.Repeat("a", 40), CentralSHA: strings.Repeat("b", 40), RunID: "123", RunAttempt: "2", Platform: "linux-amd64", Fixtures: []codexConsumeProducer{
		{Key: "GOC", UpstreamSHA: "f21fda887815af815979a4fea43a66eb5174ee3e", ProducerSHA: strings.Repeat("c", 40), TreeSHA: strings.Repeat("d", 40), Repository: "jbaruch/acr-156-goc-validation", Version: "1.1.11"},
		{Key: "FFA", UpstreamSHA: "142babbb1e2bebc798eb42128ac2466f21b5131d", ProducerSHA: strings.Repeat("e", 40), TreeSHA: strings.Repeat("f", 40), Repository: "jbaruch/acr-156-ffa-validation", Version: "0.9.38"},
	}}
}

func TestCodexConsumeRequiredInputs(t *testing.T) {
	for _, scenario := range []string{"valid", "missing_manifest", "missing_evidence", "missing_goc", "missing_ffa", "one_fixture", "wrong_order", "regenerated_source", "wrong_tag", "wrong_build", "upstream_not_generated", "stale_receipt", "symlink_manifest"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			input := codexConsumeTestManifest()
			env := map[string]string{"ACR_CODEX_CONSUME_MANIFEST": filepath.Join(root, "manifest.json"), "ACR_CODEX_CONSUME_EVIDENCE": filepath.Join(root, "evidence"), "ACR_CODEX_CONSUME_GOC_SOURCE": "github:jbaruch/acr-156-goc-validation@v1.1.11", "ACR_CODEX_CONSUME_FFA_SOURCE": "github:jbaruch/acr-156-ffa-validation@v0.9.38"}
			switch scenario {
			case "one_fixture":
				input.Fixtures = input.Fixtures[:1]
			case "wrong_order":
				input.Fixtures[0], input.Fixtures[1] = input.Fixtures[1], input.Fixtures[0]
			case "regenerated_source":
				env["ACR_CODEX_CONSUME_GOC_SOURCE"] = "github:jbaruch/acr-156-goc-validation@" + input.Fixtures[0].ProducerSHA
			case "wrong_tag":
				env["ACR_CODEX_CONSUME_FFA_SOURCE"] = "github:jbaruch/acr-156-ffa-validation@v0.9.39"
			case "wrong_build":
				input.ACRSHA = "main"
			case "upstream_not_generated":
				input.Fixtures[0].ProducerSHA = input.Fixtures[0].UpstreamSHA
			case "stale_receipt":
				if err := codexConsumeWriteNew(filepath.Join(env["ACR_CODEX_CONSUME_EVIDENCE"], "consumer-result.json"), []byte("old")); err != nil {
					t.Fatal(err)
				}
			}
			data, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(env["ACR_CODEX_CONSUME_MANIFEST"], data, 0600); err != nil {
				t.Fatal(err)
			}
			if scenario == "symlink_manifest" {
				link := filepath.Join(root, "linked.json")
				if err = os.Symlink(env["ACR_CODEX_CONSUME_MANIFEST"], link); err != nil {
					t.Fatal(err)
				}
				env["ACR_CODEX_CONSUME_MANIFEST"] = link
			}
			missing := map[string]string{"missing_manifest": "MANIFEST", "missing_evidence": "EVIDENCE", "missing_goc": "GOC_SOURCE", "missing_ffa": "FFA_SOURCE"}
			if name := missing[scenario]; name != "" {
				delete(env, "ACR_CODEX_CONSUME_"+name)
			}
			got, _, err := codexConsumeInputs(func(name string) string { return env[name] })
			if scenario == "valid" {
				if err != nil || !reflect.DeepEqual(got, input) {
					t.Fatalf("valid input: %+v %v", got, err)
				}
			} else if err == nil {
				t.Fatal("invalid required input accepted")
			}
		})
	}
}

func TestCodexConsumeLiveMissingInputFails(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestCodexLivePublishedConsumption$", "-test.v")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "ACR_CODEX_CONSUME_") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "ACR_CODEX_CONSUME_REQUIRED=1")
	output, err := command.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("requires ACR_CODEX_CONSUME_MANIFEST")) || bytes.Contains(output, []byte("--- SKIP:")) {
		t.Fatalf("missing required input did not fail before execution: %v\n%s", err, output)
	}
}

func TestCodexConsumeLockAndNativeRefusals(t *testing.T) {
	producer := codexConsumeTestManifest().Fixtures[0]
	release := codexConsumeRelease{ID: 7, Tag: "v1.1.11", Commit: producer.ProducerSHA, ContentHash: "sha256:" + strings.Repeat("1", 64)}
	state := dependency.State{Project: dependency.Project{Agents: []string{"codex"}, Freshness: "none", Dependencies: []dependency.Declaration{{Source: "github:" + producer.Repository, Requested: release.Tag}}}, Lock: dependency.Lockfile{Dependencies: []dependency.LockedDependency{{Source: "github:" + producer.Repository, Requested: release.Tag, Kind: dependency.ResolutionRelease, ReleaseID: release.ID, Tag: release.Tag, Commit: producer.ProducerSHA, PackageVersion: producer.Version, ContentHash: release.ContentHash}}}}
	if _, err := codexConsumeLock(state, producer, release, "codex", false); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"wrong_commit", "wrong_hash", "wrong_release", "commit_kind", "extra_dependency", "freshness", "different_adapter"} {
		t.Run(scenario, func(t *testing.T) {
			changed := state
			changed.Project.Dependencies = append([]dependency.Declaration(nil), state.Project.Dependencies...)
			changed.Lock.Dependencies = append([]dependency.LockedDependency(nil), state.Lock.Dependencies...)
			switch scenario {
			case "wrong_commit":
				changed.Lock.Dependencies[0].Commit = strings.Repeat("9", 40)
			case "wrong_hash":
				changed.Lock.Dependencies[0].ContentHash = "sha256:" + strings.Repeat("9", 64)
			case "wrong_release":
				changed.Lock.Dependencies[0].ReleaseID++
			case "commit_kind":
				changed.Lock.Dependencies[0].Kind = dependency.ResolutionCommit
			case "extra_dependency":
				changed.Project.Dependencies = append(changed.Project.Dependencies, changed.Project.Dependencies[0])
			case "freshness":
				changed.Project.Freshness = "outdated"
			case "different_adapter":
				changed.Project.Agents = []string{"cursor"}
			}
			if _, err := codexConsumeLock(changed, producer, release, "codex", false); err == nil {
				t.Fatal("incorrect consumer state accepted")
			}
		})
	}
	root := t.TempDir()
	path := filepath.Join(root, ".codex", "skills", "sample", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	declared := []codexConsumeFile{{Path: ".codex/skills/sample/SKILL.md", SHA256: codexConsumeHash([]byte("original")), Mode: "100644"}}
	observed, err := codexConsumeNativeInventory(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = codexConsumeCompare(declared, observed); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"bytes", "mode", "extra", "missing", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			if err := os.WriteFile(path, []byte("original"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "bytes":
				if err := os.WriteFile(path, []byte("tampered"), 0644); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(path, 0755); err != nil {
					t.Fatal(err)
				}
			case "extra":
				extra := filepath.Join(filepath.Dir(path), "extra.md")
				if err := os.WriteFile(extra, []byte("extra"), 0644); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Remove(extra) })
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("absent", path); err != nil {
					t.Fatal(err)
				}
			}
			observed, err := codexConsumeNativeInventory(root)
			if err == nil && codexConsumeCompare(declared, observed) == nil {
				t.Fatal("native inventory mismatch accepted")
			}
		})
	}
}

// This exercises the complete consumer orchestration against the existing
// local GitHub transport fixture. It is not hosted publication evidence.
func TestCodexConsumePublishedFixture(t *testing.T) {
	github := newJourneyGitHub(t)
	pkg := newJourneyPackage(t, "example/consume", "1.0.0")
	repo, commit := journeyPublisher(t, pkg)
	github.PublishSource(pkg.fullName, pkg.tag, commit, journeyGitSourceArchive(t, repo, pkg.tag, commit))
	publisher := newJourneyProject(t, github)
	publisher.runOnPath(repo, 0, "publish", "--json")
	runner := func(root, state string, args ...string) journeyRun {
		if os.Getenv("ACR_TEST_CODEX_CONSUME_FAIL") != "" && args[0] == "realize" {
			return journeyRun{args: args, stderr: "injected realization failure", exit: 1}
		}
		t.Setenv("ACR_STATE_HOME", state)
		full := append(append([]string(nil), args...), "--project", root, "--json")
		var stdout, stderr bytes.Buffer
		exit := runWith(publisher.composedClient(), strings.NewReader(""), &stdout, &stderr, full)
		return journeyRun{args: full, stdout: stdout.String(), stderr: stderr.String(), exit: exit}
	}
	producer := codexConsumeProducer{Key: "GOC", Repository: pkg.fullName, Version: pkg.version, ProducerSHA: commit}
	evidence := t.TempDir()
	if failureRoot := os.Getenv("ACR_TEST_CODEX_CONSUME_FAIL"); failureRoot != "" {
		evidence = failureRoot
	}
	result := codexConsumePublished(t, publisher.composedClient(), producer, evidence, runner)
	if len(result.Consumers) != 3 || !result.SHAInstall.Install || result.SHAInstall.Commit != commit {
		t.Fatalf("incomplete fixture: %+v", result)
	}
	for i, id := range []string{"claude-code", "codex", "cursor"} {
		if result.Consumers[i].Adapter != id || len(result.Consumers[i].Native) == 0 {
			t.Fatal("missing adapter evidence")
		}
	}
	// Only the final aggregate may write success, never an individual fixture.
	if _, err := os.Stat(filepath.Join(evidence, "consumer-result.json")); !os.IsNotExist(err) {
		t.Fatal("fixture prematurely wrote final receipt")
	}
	input := codexConsumeTestManifest()
	receipt := codexConsumeReceipt{SchemaVersion: 1, Result: "passed", ACRSHA: input.ACRSHA, CentralSHA: input.CentralSHA, ProducerRunID: input.RunID, ProducerRunAttempt: input.RunAttempt, Host: "linux-amd64", Fixtures: []codexConsumeFixture{result}}
	if err := codexConsumeWriteReceipt(evidence, receipt); err == nil {
		t.Fatal("partial receipt accepted")
	}
	if _, err := os.Stat(filepath.Join(evidence, "consumer-result.json")); !os.IsNotExist(err) {
		t.Fatal("failed aggregate left success receipt")
	}
	second := result
	second.Key = "FFA"
	receipt.Fixtures = append(receipt.Fixtures, second)
	if err := codexConsumeWriteReceipt(evidence, receipt); err != nil {
		t.Fatal(err)
	}
	if err := codexConsumeWriteReceipt(evidence, receipt); err == nil {
		t.Fatal("stale success receipt overwritten")
	}
	data, err := os.ReadFile(filepath.Join(evidence, "consumer-result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err = json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	codexConsumeKeys(t, decoded, "schema_version result acr_sha central_sha producer_run_id producer_run_attempt host fixtures")
	fixture := decoded["fixtures"].([]any)[0].(map[string]any)
	codexConsumeKeys(t, fixture, "key source release consumers sha_install")
	codexConsumeKeys(t, fixture["release"].(map[string]any), "id tag commit contentHash")
	codexConsumeKeys(t, fixture["sha_install"].(map[string]any), "source commit contentHash install")
	native := fixture["consumers"].([]any)[0].(map[string]any)
	codexConsumeKeys(t, native, "adapter kind commit release_id tag contentHash install realize check declared_inventory native_inventory")
	codexConsumeKeys(t, native["native_inventory"].([]any)[0].(map[string]any), "path sha256 mode")
	github.AssertNoUnknownRequests(t)
}
func codexConsumeKeys(t *testing.T, value map[string]any, names string) {
	t.Helper()
	keys := strings.Fields(names)
	if len(value) != len(keys) {
		t.Fatalf("receipt fields=%v want %s", value, names)
	}
	for _, key := range keys {
		if _, ok := value[key]; !ok {
			t.Fatalf("missing receipt field %s", key)
		}
	}
}

func TestCodexConsumeFailureLeavesNoReceipt(t *testing.T) {
	evidence := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestCodexConsumePublishedFixture$", "-test.v")
	command.Env = append(os.Environ(), "ACR_TEST_CODEX_CONSUME_FAIL="+evidence)
	output, err := command.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("published consumer realize failed")) {
		t.Fatalf("wrong failure: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(evidence, "consumer-result.json")); !os.IsNotExist(err) {
		t.Fatal("failed command left a success receipt")
	}
	data, err := os.ReadFile(filepath.Join(evidence, "claude-code", "realize.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Exit int `json:"exit"`
	}
	if err := json.Unmarshal(data, &result); err != nil || result.Exit != 1 {
		t.Fatalf("missing command failure evidence: %s %v", data, err)
	}
}

func TestCodexConsumeIsolatesCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-github-token")
	t.Setenv("GITHUB_TOKEN", "test-actions-token")
	t.Setenv("CODEX_API_KEY", "test-model-token")
	t.Setenv("OPENAI_API_KEY", "test-openai-token")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_0", "inherited-helper")
	oldHome := os.Getenv("HOME")
	codexConsumeIsolate(t)
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "CODEX_API_KEY", "OPENAI_API_KEY", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"} {
		if os.Getenv(name) != "" {
			t.Errorf("credential input %s survived", name)
		}
	}
	if os.Getenv("HOME") == oldHome || os.Getenv("GIT_CONFIG_GLOBAL") != os.DevNull || os.Getenv("GIT_CONFIG_NOSYSTEM") != "1" || os.Getenv("GIT_CONFIG_COUNT") != "0" {
		t.Fatal("credential discovery is not isolated")
	}
	cwd, err := os.Getwd()
	if err != nil || cwd != os.Getenv("HOME") {
		t.Fatal("credential discovery still runs in the caller checkout")
	}
}
