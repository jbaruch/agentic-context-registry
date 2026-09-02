// releasetool executes deterministic CLI-release operations for GitHub Actions.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/release"
)

func main() {
	os.Exit(run(context.Background(), os.Stdout, os.Stderr, os.Args[1:], productionRemote()))
}

func run(ctx context.Context, stdout, stderr io.Writer, args []string, remote release.Remote) int {
	if len(args) == 0 {
		return reportError(stderr, errors.New("release tool requires guard, pack, publish, formula, or verify-sbom; select one operation and retry"))
	}
	var result any
	var err error
	switch args[0] {
	case "guard":
		result, err = runGuard(ctx, args[1:], remote)
	case "pack":
		result, err = runPack(args[1:])
	case "publish":
		result, err = runPublish(ctx, args[1:], remote)
	case "formula":
		result, err = runFormula(args[1:])
	case "verify-sbom":
		result, err = runVerifySBOM(args[1:])
	default:
		err = fmt.Errorf("unknown release operation %q; use guard, pack, publish, formula, or verify-sbom", args[0])
	}
	if err != nil {
		return reportError(stderr, err)
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return reportError(stderr, fmt.Errorf("encode release tool result: %w; verify stdout is writable and retry", err))
	}
	return 0
}

func runGuard(ctx context.Context, args []string, remote release.Remote) (release.GuardResult, error) {
	flags := newFlagSet("guard")
	repositoryName := flags.String("repository", "github:jbaruch/agentic-context-registry", "canonical github:owner/repository")
	tag := flags.String("tag", "", "release tag")
	commit := flags.String("commit", "", "workflow commit")
	if err := flags.Parse(args); err != nil {
		return release.GuardResult{}, err
	}
	if flags.NArg() != 0 || *tag == "" || *commit == "" {
		return release.GuardResult{}, errors.New("guard requires --tag and --commit with no positional arguments")
	}
	repository, err := dependency.ParseSource(*repositoryName)
	if err != nil {
		return release.GuardResult{}, err
	}
	return release.Guard(ctx, remote, repository, *tag, *commit)
}

func runPack(args []string) (struct {
	Assets []string `json:"assets"`
}, error) {
	flags := newFlagSet("pack")
	input := flags.String("input", "", "directory containing cross-compiled binaries")
	output := flags.String("output", "", "directory for release assets")
	licensePath := flags.String("license", "LICENSE", "repository license path")
	if err := flags.Parse(args); err != nil {
		return struct {
			Assets []string `json:"assets"`
		}{}, err
	}
	if flags.NArg() != 0 || *input == "" || *output == "" {
		return struct {
			Assets []string `json:"assets"`
		}{}, errors.New("pack requires --input and --output with no positional arguments")
	}
	license, err := os.ReadFile(*licensePath)
	if err != nil {
		return struct {
			Assets []string `json:"assets"`
		}{}, fmt.Errorf("read release LICENSE %q: %w; check out the repository before packing", *licensePath, err)
	}
	binaries := make([]release.Binary, 0, len(release.Targets()))
	for _, target := range release.Targets() {
		path := filepath.Join(*input, "acr-"+target.GOOS+"-"+target.GOARCH)
		contents, err := os.ReadFile(path)
		if err != nil {
			return struct {
				Assets []string `json:"assets"`
			}{}, fmt.Errorf("read compiled binary %q: %w; build all four targets before packing", path, err)
		}
		binaries = append(binaries, release.Binary{Target: target, Bytes: contents})
	}
	bundle, err := release.Pack(binaries, license)
	if err != nil {
		return struct {
			Assets []string `json:"assets"`
		}{}, err
	}
	if err := os.MkdirAll(*output, 0o755); err != nil {
		return struct {
			Assets []string `json:"assets"`
		}{}, fmt.Errorf("create release asset directory %q: %w; verify the workspace is writable", *output, err)
	}
	names := make([]string, 0, len(bundle.Archives)+1)
	for _, asset := range append(append([]release.Asset(nil), bundle.Archives...), bundle.Checksums) {
		if err := os.WriteFile(filepath.Join(*output, asset.Name), asset.Bytes, 0o644); err != nil {
			return struct {
				Assets []string `json:"assets"`
			}{}, fmt.Errorf("write release asset %q: %w; verify the workspace is writable", asset.Name, err)
		}
		names = append(names, asset.Name)
	}
	return struct {
		Assets []string `json:"assets"`
	}{Assets: names}, nil
}

func runPublish(ctx context.Context, args []string, remote release.Remote) (release.PublishResult, error) {
	flags := newFlagSet("publish")
	repositoryName := flags.String("repository", "github:jbaruch/agentic-context-registry", "canonical github:owner/repository")
	tag := flags.String("tag", "", "release tag")
	commit := flags.String("commit", "", "workflow commit")
	assetDirectory := flags.String("assets", "", "directory containing exactly seven release assets")
	if err := flags.Parse(args); err != nil {
		return release.PublishResult{}, err
	}
	if flags.NArg() != 0 || *tag == "" || *commit == "" || *assetDirectory == "" {
		return release.PublishResult{}, errors.New("publish requires --tag, --commit, and --assets with no positional arguments")
	}
	repository, err := dependency.ParseSource(*repositoryName)
	if err != nil {
		return release.PublishResult{}, err
	}
	assets, err := loadAssets(*assetDirectory)
	if err != nil {
		return release.PublishResult{}, err
	}
	return release.Publish(ctx, remote, repository, *tag, *commit, assets)
}

func runFormula(args []string) (struct {
	Path string `json:"path"`
}, error) {
	flags := newFlagSet("formula")
	version := flags.String("version", "", "release version without v")
	checksumsPath := flags.String("checksums", "", "published checksum manifest")
	output := flags.String("output", "", "formula output path")
	if err := flags.Parse(args); err != nil {
		return struct {
			Path string `json:"path"`
		}{}, err
	}
	if flags.NArg() != 0 || *version == "" || *checksumsPath == "" || *output == "" {
		return struct {
			Path string `json:"path"`
		}{}, errors.New("formula requires --version, --checksums, and --output with no positional arguments")
	}
	manifest, err := os.ReadFile(*checksumsPath)
	if err != nil {
		return struct {
			Path string `json:"path"`
		}{}, fmt.Errorf("read release checksums %q: %w; download checksums.txt from the published release", *checksumsPath, err)
	}
	digests, err := release.ParseArchiveDigests(manifest)
	if err != nil {
		return struct {
			Path string `json:"path"`
		}{}, err
	}
	formula, err := release.RenderFormula(*version, digests)
	if err != nil {
		return struct {
			Path string `json:"path"`
		}{}, err
	}
	if err := os.MkdirAll(filepath.Dir(*output), 0o755); err != nil {
		return struct {
			Path string `json:"path"`
		}{}, fmt.Errorf("create formula directory for %q: %w; verify the workspace is writable", *output, err)
	}
	if err := os.WriteFile(*output, formula, 0o644); err != nil {
		return struct {
			Path string `json:"path"`
		}{}, fmt.Errorf("write Homebrew formula %q: %w; verify the workspace is writable", *output, err)
	}
	return struct {
		Path string `json:"path"`
	}{Path: *output}, nil
}

func runVerifySBOM(args []string) (struct {
	Path string `json:"path"`
}, error) {
	flags := newFlagSet("verify-sbom")
	version := flags.String("version", "", "release version without v")
	path := flags.String("path", "", "CycloneDX JSON path")
	if err := flags.Parse(args); err != nil {
		return struct {
			Path string `json:"path"`
		}{}, err
	}
	if flags.NArg() != 0 || *version == "" || *path == "" {
		return struct {
			Path string `json:"path"`
		}{}, errors.New("verify-sbom requires --version and --path with no positional arguments")
	}
	contents, err := os.ReadFile(*path)
	if err != nil {
		return struct {
			Path string `json:"path"`
		}{}, fmt.Errorf("read release SBOM %q: %w; regenerate acr.cdx.json and retry", *path, err)
	}
	if err := release.ValidateSBOM(contents, *version); err != nil {
		return struct {
			Path string `json:"path"`
		}{}, err
	}
	return struct {
		Path string `json:"path"`
	}{Path: *path}, nil
}

func loadAssets(directory string) ([]release.Asset, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read release asset directory %q: %w; assemble all seven assets and retry", directory, err)
	}
	expected := make(map[string]struct{}, len(release.ExpectedAssetNames()))
	for _, name := range release.ExpectedAssetNames() {
		expected[name] = struct{}{}
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, fmt.Errorf("release asset directory contains subdirectory %q; provide exactly the seven regular asset files", entry.Name())
		}
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf("release asset %q is not a regular file; provide exactly the seven generated files", entry.Name())
		}
		if _, ok := expected[entry.Name()]; !ok {
			return nil, fmt.Errorf("release asset directory contains unexpected file %q; provide exactly the seven release assets", entry.Name())
		}
	}
	assets := make([]release.Asset, 0, len(expected))
	for _, name := range release.ExpectedAssetNames() {
		path := filepath.Join(directory, name)
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read release asset %q: %w; assemble all seven assets and retry", name, err)
		}
		assets = append(assets, release.Asset{Name: name, Bytes: contents})
	}
	return assets, nil
}

func newFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func reportError(stderr io.Writer, err error) int {
	var releaseErr *release.Error
	if errors.As(err, &releaseErr) {
		fmt.Fprintf(stderr, "%s: %s\n", releaseErr.Code, releaseErr.Message)
		return 1
	}
	fmt.Fprintf(stderr, "release_tool_failed: %s\n", err)
	return 1
}
