package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/publish"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// The central runner authenticates the producer artifact before supplying this
// manifest. Consumption neither generates nor publishes producer content.
type codexConsumeManifest struct {
	SchemaVersion int                    `json:"schema_version"`
	Phase         string                 `json:"phase"`
	Result        string                 `json:"result"`
	ACRSHA        string                 `json:"acr_sha"`
	CentralSHA    string                 `json:"central_sha"`
	RunID         string                 `json:"run_id"`
	RunAttempt    string                 `json:"run_attempt"`
	Platform      string                 `json:"platform"`
	Fixtures      []codexConsumeProducer `json:"fixtures"`
}
type codexConsumeProducer struct {
	Key         string `json:"key"`
	UpstreamSHA string `json:"upstream_sha"`
	ProducerSHA string `json:"producer_sha"`
	TreeSHA     string `json:"tree_sha"`
	Repository  string `json:"repository"`
	Version     string `json:"version"`
}
type codexConsumeFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Mode   string `json:"mode"`
}
type codexConsumeRelease struct {
	ID          int64  `json:"id"`
	Tag         string `json:"tag"`
	Commit      string `json:"commit"`
	ContentHash string `json:"contentHash"`
}
type codexConsumeNative struct {
	Adapter     string                    `json:"adapter"`
	Kind        dependency.ResolutionKind `json:"kind"`
	Commit      string                    `json:"commit"`
	ReleaseID   int64                     `json:"release_id"`
	Tag         string                    `json:"tag"`
	ContentHash string                    `json:"contentHash"`
	Install     bool                      `json:"install"`
	Realize     bool                      `json:"realize"`
	Check       bool                      `json:"check"`
	Declared    []codexConsumeFile        `json:"declared_inventory"`
	Native      []codexConsumeFile        `json:"native_inventory"`
}
type codexConsumePin struct {
	Source      string `json:"source"`
	Commit      string `json:"commit"`
	ContentHash string `json:"contentHash"`
	Install     bool   `json:"install"`
}
type codexConsumeFixture struct {
	Key        string               `json:"key"`
	Source     string               `json:"source"`
	Release    codexConsumeRelease  `json:"release"`
	Consumers  []codexConsumeNative `json:"consumers"`
	SHAInstall codexConsumePin      `json:"sha_install"`
}
type codexConsumeReceipt struct {
	SchemaVersion      int                   `json:"schema_version"`
	Result             string                `json:"result"`
	ACRSHA             string                `json:"acr_sha"`
	CentralSHA         string                `json:"central_sha"`
	ProducerRunID      string                `json:"producer_run_id"`
	ProducerRunAttempt string                `json:"producer_run_attempt"`
	Host               string                `json:"host"`
	Fixtures           []codexConsumeFixture `json:"fixtures"`
}

func codexConsumeInputs(getenv func(string) string) (codexConsumeManifest, string, error) {
	var value codexConsumeManifest
	for _, name := range []string{"MANIFEST", "EVIDENCE", "GOC_SOURCE", "FFA_SOURCE"} {
		if getenv("ACR_CODEX_CONSUME_"+name) == "" {
			return value, "", fmt.Errorf("live consumption requires ACR_CODEX_CONSUME_%s", name)
		}
	}
	filename := getenv("ACR_CODEX_CONSUME_MANIFEST")
	info, err := os.Lstat(filename)
	if err != nil {
		return value, "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return value, "", fmt.Errorf("consume manifest must be a bounded regular file")
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return value, "", err
	}
	if err = json.Unmarshal(data, &value); err != nil {
		return value, "", err
	}
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	decimal := regexp.MustCompile(`^[1-9][0-9]*$`)
	if value.SchemaVersion != 1 || value.Phase != "convert" || value.Result != "passed" || !sha.MatchString(value.ACRSHA) || !sha.MatchString(value.CentralSHA) || !decimal.MatchString(value.RunID) || !decimal.MatchString(value.RunAttempt) || value.Platform != "linux-amd64" || len(value.Fixtures) != 2 {
		return value, "", fmt.Errorf("incomplete producer provenance")
	}
	expected := []codexConsumeProducer{
		{Key: "GOC", UpstreamSHA: "f21fda887815af815979a4fea43a66eb5174ee3e", Repository: "jbaruch/acr-156-goc-validation", Version: "1.1.11"},
		{Key: "FFA", UpstreamSHA: "142babbb1e2bebc798eb42128ac2466f21b5131d", Repository: "jbaruch/acr-156-ffa-validation", Version: "0.9.38"},
	}
	for i, p := range value.Fixtures {
		want := expected[i]
		if p.Key != want.Key || p.UpstreamSHA != want.UpstreamSHA || p.Repository != want.Repository || p.Version != want.Version || !sha.MatchString(p.ProducerSHA) || p.ProducerSHA == p.UpstreamSHA || !sha.MatchString(p.TreeSHA) || getenv("ACR_CODEX_CONSUME_"+p.Key+"_SOURCE") != "github:"+p.Repository+"@v"+p.Version {
			return value, "", fmt.Errorf("producer %s or published source differs from required fixture", want.Key)
		}
	}
	evidence := getenv("ACR_CODEX_CONSUME_EVIDENCE")
	if !filepath.IsAbs(filename) || !filepath.IsAbs(evidence) {
		return value, "", fmt.Errorf("manifest and evidence paths must be absolute")
	}
	if _, err := os.Lstat(filepath.Join(evidence, "consumer-result.json")); !os.IsNotExist(err) {
		return value, "", fmt.Errorf("consumer receipt already exists or cannot be inspected; use fresh evidence")
	}
	return value, evidence, nil
}

// No real Codex invocation or credential is needed in this lane. In particular,
// do not call codexLive or newJourneyProject: both configure authentication.
func TestCodexLivePublishedConsumption(t *testing.T) {
	if os.Getenv("ACR_CODEX_CONSUME_REQUIRED") != "1" {
		t.Skip("published consumption requires ACR_CODEX_CONSUME_REQUIRED=1")
	}
	input, evidence, err := codexConsumeInputs(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	binary := journeyBuiltBinary(t)
	build, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]string{}
	for _, setting := range build.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["vcs.revision"] != input.ACRSHA || settings["vcs.modified"] != "false" {
		t.Fatal("consumer binary does not identify the clean producer-tested ACR commit")
	}
	codexConsumeIsolate(t)
	receipt := codexConsumeReceipt{SchemaVersion: 1, Result: "passed", ACRSHA: input.ACRSHA, CentralSHA: input.CentralSHA, ProducerRunID: input.RunID, ProducerRunAttempt: input.RunAttempt, Host: runtime.GOOS + "-" + runtime.GOARCH}
	remote := dependency.NewGitHubClient()
	for _, producer := range input.Fixtures {
		t.Run(producer.Key, func(t *testing.T) {
			run := func(root, state string, args ...string) journeyRun {
				full := append(append([]string(nil), args...), "--project", root, "--json")
				stdout, stderr, exit := hostileRunBinary(t, binary, state, strings.NewReader(""), full...)
				return journeyRun{args: full, stdout: stdout, stderr: stderr, exit: exit}
			}
			receipt.Fixtures = append(receipt.Fixtures, codexConsumePublished(t, remote, producer, filepath.Join(evidence, strings.ToLower(producer.Key)), run))
		})
	}
	if t.Failed() {
		return
	}
	if err := codexConsumeWriteReceipt(evidence, receipt); err != nil {
		t.Fatal(err)
	}
}

func codexConsumeIsolate(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "CODEX_") || strings.HasPrefix(name, "OPENAI_") || strings.HasPrefix(name, "GH_") || strings.HasPrefix(name, "GITHUB_") || strings.HasPrefix(name, "GIT_") {
			t.Setenv(name, "")
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("GH_CONFIG_DIR", filepath.Join(home, "gh"))
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	// git credential fill must run outside any user's checkout-local helpers.
	t.Chdir(home)
}

func codexConsumeHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func codexConsumeRecord(t *testing.T, directory, name string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := codexConsumeWriteNew(filepath.Join(directory, name), append(data, '\n')); err != nil {
		t.Fatal(err)
	}
}
func codexConsumeWriteNew(name string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
func codexConsumeWriteReceipt(directory string, value codexConsumeReceipt) error {
	if len(value.Fixtures) != 2 {
		return fmt.Errorf("both successful consumers are required before writing a receipt")
	}
	for i, key := range []string{"GOC", "FFA"} {
		f := value.Fixtures[i]
		if f.Key != key || len(f.Consumers) != 3 || !f.SHAInstall.Install {
			return fmt.Errorf("incomplete fixture acceptance")
		}
		for _, c := range f.Consumers {
			if !c.Install || !c.Realize || !c.Check || len(c.Declared) == 0 || !reflect.DeepEqual(c.Declared, c.Native) {
				return fmt.Errorf("incomplete native acceptance")
			}
		}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return codexConsumeWriteNew(filepath.Join(directory, "consumer-result.json"), append(data, '\n'))
}

type codexConsumeRunner func(root, state string, args ...string) journeyRun

func codexConsumePublished(t *testing.T, remote dependency.Remote, producer codexConsumeProducer, evidence string, run codexConsumeRunner) codexConsumeFixture {
	t.Helper()
	ctx := context.Background()
	source := "github:" + producer.Repository
	repository, err := dependency.ParseSource(source)
	if err != nil {
		t.Fatal(err)
	}
	tag := "v" + producer.Version
	release, err := remote.ReleaseByTag(ctx, repository, tag)
	if err != nil {
		t.Fatal(err)
	}
	commit, exists, err := remote.TagCommit(ctx, repository, tag)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || commit != producer.ProducerSHA || release.ID <= 0 || release.Tag != tag || release.Draft || release.Prerelease {
		t.Fatal("published release or peeled tag does not identify the generated producer")
	}
	metadata, packageRoot := codexConsumeAssets(t, remote, repository, release, producer, evidence)
	codexConsumeRecord(t, evidence, "release.json", release)
	codexConsumeRecord(t, evidence, "peeled-tag.json", map[string]string{"tag": tag, "commit": commit})
	result := codexConsumeFixture{Key: producer.Key, Source: source + "@" + tag, Release: codexConsumeRelease{ID: release.ID, Tag: tag, Commit: commit, ContentHash: metadata.ContentHash}}
	value, err := manifest.Load(packageRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, native := range []adapter.Adapter{claudecode.New(), codex.New(), cursor.New()} {
		id := native.Descriptor().ID
		project, state := t.TempDir(), t.TempDir()
		declared, err := codexConsumeDeclared(t.TempDir(), adapter.Package{Source: source, Root: os.DirFS(packageRoot), Manifest: value}, native)
		if err != nil {
			t.Fatal(err)
		}
		commands := [][]string{{"init", "--agent", id, "--freshness", "none", "--non-interactive"}, {"install", result.Source, "--non-interactive"}, {"realize"}, {"check"}}
		for _, args := range commands {
			codexConsumeCommand(t, run, project, state, filepath.Join(evidence, id), args)
		}
		actual, err := dependency.LoadState(project)
		if err != nil {
			t.Fatal(err)
		}
		locked, err := codexConsumeLock(actual, producer, result.Release, id, false)
		if err != nil {
			t.Fatal(err)
		}
		observed, err := codexConsumeNativeInventory(project)
		if err != nil {
			t.Fatal(err)
		}
		if err := codexConsumeCompare(declared, observed); err != nil {
			t.Fatal(err)
		}
		codexConsumeRecord(t, filepath.Join(evidence, id), "state.json", actual)
		result.Consumers = append(result.Consumers, codexConsumeNative{Adapter: id, Kind: locked.Kind, Commit: locked.Commit, ReleaseID: locked.ReleaseID, Tag: locked.Tag, ContentHash: locked.ContentHash, Install: true, Realize: true, Check: true, Declared: declared, Native: observed})
	}
	project, state := t.TempDir(), t.TempDir()
	pin := source + "@" + producer.ProducerSHA
	for _, args := range [][]string{{"init", "--agent", "codex", "--freshness", "none", "--non-interactive"}, {"install", pin, "--non-interactive"}} {
		codexConsumeCommand(t, run, project, state, filepath.Join(evidence, "sha"), args)
	}
	actual, err := dependency.LoadState(project)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := codexConsumeLock(actual, producer, result.Release, "codex", true)
	if err != nil {
		t.Fatal(err)
	}
	codexConsumeRecord(t, filepath.Join(evidence, "sha"), "state.json", actual)
	result.SHAInstall = codexConsumePin{Source: pin, Commit: locked.Commit, ContentHash: locked.ContentHash, Install: true}
	finalCommit, finalExists, err := remote.TagCommit(ctx, repository, tag)
	if err != nil || !finalExists || finalCommit != producer.ProducerSHA {
		t.Fatal("published tag moved during consumer verification")
	}
	codexConsumeRecord(t, evidence, "final-peeled-tag.json", map[string]string{"tag": tag, "commit": finalCommit})
	return result
}

func codexConsumeCommand(t *testing.T, run codexConsumeRunner, root, state, evidence string, args []string) {
	t.Helper()
	got := run(root, state, args...)
	codexConsumeRecord(t, evidence, args[0]+".json", map[string]any{"argv": got.args, "exit": got.exit, "stdout": got.stdout, "stderr": got.stderr})
	if got.exit != 0 {
		t.Fatalf("published consumer %s failed; inspect %s", args[0], evidence)
	}
}

func codexConsumeLock(state dependency.State, p codexConsumeProducer, r codexConsumeRelease, id string, pinned bool) (dependency.LockedDependency, error) {
	source := "github:" + p.Repository
	requested, kind := r.Tag, dependency.ResolutionRelease
	if pinned {
		requested, kind = p.ProducerSHA, dependency.ResolutionCommit
	}
	if len(state.Project.Dependencies) != 1 || len(state.Lock.Dependencies) != 1 || !reflect.DeepEqual(state.Project.Agents, []string{id}) || state.Project.Freshness != "none" {
		return dependency.LockedDependency{}, fmt.Errorf("consumer declaration or dependency inventory differs")
	}
	d, l := state.Project.Dependencies[0], state.Lock.Dependencies[0]
	if d.Source != source || d.Requested != requested || d.Path != "" || d.Hold != nil || l.Source != source || l.Requested != requested || l.Kind != kind || l.Commit != p.ProducerSHA || l.ContentHash != r.ContentHash || l.PackageVersion != p.Version || l.Path != "" || l.Hold != nil {
		return l, fmt.Errorf("consumer lock does not identify the generated published content")
	}
	if (!pinned && (l.ReleaseID != r.ID || l.Tag != r.Tag)) || (pinned && (l.ReleaseID != 0 || l.Tag != "")) {
		return l, fmt.Errorf("consumer release identity differs")
	}
	return l, nil
}

func codexConsumeAssets(t *testing.T, remote dependency.Remote, repository dependency.Repository, release dependency.Release, p codexConsumeProducer, evidence string) (publish.Metadata, string) {
	t.Helper()
	var metadata publish.Metadata
	assets := map[string][]byte{}
	if len(release.Assets) != 3 {
		t.Fatal("published producer must have exactly archive, metadata and checksums")
	}
	for _, asset := range release.Assets {
		if filepath.Base(asset.Name) != asset.Name || asset.Name == "." || asset.Name == ".." || assets[asset.Name] != nil {
			t.Fatal("unsafe or duplicate release asset")
		}
		data, err := remote.DownloadReleaseAsset(context.Background(), repository, asset)
		if err != nil {
			t.Fatal(err)
		}
		assets[asset.Name] = data
		if err := codexConsumeWriteNew(filepath.Join(evidence, "assets", asset.Name), data); err != nil {
			t.Fatal(err)
		}
	}
	if err := json.Unmarshal(assets[publish.MetadataAssetName], &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.MetadataVersion != 1 || metadata.Name != p.Repository || metadata.Repository != "https://github.com/"+p.Repository || metadata.Version != p.Version || metadata.Commit != p.ProducerSHA || metadata.Tag != release.Tag || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(metadata.ContentHash) {
		t.Fatal("release metadata does not identify the expected producer")
	}
	archive := assets[metadata.Archive.Name]
	if len(archive) == 0 || codexConsumeHash(archive) != metadata.Archive.SHA256 {
		t.Fatal("published archive digest mismatch")
	}
	checksums := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(assets[publish.ChecksumsAssetName])), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || checksums[fields[1]] != "" {
			t.Fatal("invalid published checksums")
		}
		checksums[fields[1]] = fields[0]
	}
	if len(checksums) != 2 || checksums[metadata.Archive.Name] != codexConsumeHash(archive) || checksums[publish.MetadataAssetName] != codexConsumeHash(assets[publish.MetadataAssetName]) {
		t.Fatal("release checksums disagree with downloaded bytes")
	}
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(reader, 128<<20))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(raw) >= 128<<20 || codexConsumeHash(raw) != metadata.Archive.TarSHA256 {
		t.Fatal("published tar digest mismatch")
	}
	root := t.TempDir()
	if err := dependency.ExtractPackageArchive(archive, root); err != nil {
		t.Fatal(err)
	}
	value, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := dependency.HashPackageFiles(root, value)
	if err != nil {
		t.Fatal(err)
	}
	if hash != metadata.ContentHash || value.Name != p.Repository || value.Version != p.Version {
		t.Fatal("downloaded archive content differs from release metadata")
	}
	return metadata, root
}

func codexConsumeDeclared(emptyRoot string, pkg adapter.Package, native adapter.Adapter) ([]codexConsumeFile, error) {
	snapshot, err := adapter.NewRootSnapshot(emptyRoot)
	if err != nil {
		return nil, err
	}
	defer snapshot.Close()
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), native)
	if err != nil {
		return nil, err
	}
	intents, err := coordinator.Realize(context.Background(), snapshot, []adapter.Package{pkg}, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion})
	if err != nil {
		return nil, err
	}
	var result []codexConsumeFile
	for _, intent := range intents {
		if intent.Mode != 0644 && intent.Mode != 0755 {
			return nil, fmt.Errorf("unexpected declared native mode")
		}
		result = append(result, codexConsumeFile{Path: intent.Path, SHA256: codexConsumeHash(intent.Content), Mode: fmt.Sprintf("100%03o", intent.Mode)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}
func codexConsumeNativeInventory(root string) ([]codexConsumeFile, error) {
	var result []codexConsumeFile
	for _, name := range []string{".claude", ".codex", ".cursor", "CLAUDE.md", "AGENTS.md"} {
		err := filepath.WalkDir(filepath.Join(root, name), func(path string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) && path == filepath.Join(root, name) {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || (info.Mode().Perm() != 0644 && info.Mode().Perm() != 0755) {
				return fmt.Errorf("nonregular native output or unexpected mode")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			result = append(result, codexConsumeFile{Path: filepath.ToSlash(relative), SHA256: codexConsumeHash(data), Mode: fmt.Sprintf("100%03o", info.Mode().Perm())})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}
func codexConsumeCompare(declared, actual []codexConsumeFile) error {
	if len(declared) == 0 || !reflect.DeepEqual(declared, actual) {
		return fmt.Errorf("native bytes, modes or destinations differ from independent adapter declarations")
	}
	return nil
}
