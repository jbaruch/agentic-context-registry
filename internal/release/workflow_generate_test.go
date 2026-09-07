package release

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestMain(m *testing.M) {
	switch os.Getenv("ACR_SBOM_DOUBLE") {
	case "":
		os.Exit(m.Run())
	case "cyclonedx-gomod":
		os.Exit(runCycloneDXDouble())
	case "jq":
		os.Exit(runJQDouble())
	case "go":
		os.Exit(runGoDouble())
	default:
		fmt.Fprintf(os.Stderr, "unknown ACR_SBOM_DOUBLE %q\n", os.Getenv("ACR_SBOM_DOUBLE"))
		os.Exit(1)
	}
}

func TestReleaseWorkflowGeneratesFourTargetSBOMs(t *testing.T) {
	t.Parallel()

	result := executeGenerationRun(t, releaseWorkflowGenerationStep(t), generationRunOptions{})
	if result.err != nil {
		t.Fatalf("run SBOM generation step: %v\n%s", result.err, result.output)
	}
	assertGeneratedReleaseSBOMs(t, result)
}

func TestReleaseWorkflowGenerationAcceptsReorderedTargets(t *testing.T) {
	t.Parallel()

	step := releaseWorkflowGenerationStep(t)
	step.Run = reorderGenerationTargets(t, step.Run)
	result := executeGenerationRun(t, step, generationRunOptions{})
	if result.err != nil {
		t.Fatalf("run reordered SBOM generation step: %v\n%s", result.err, result.output)
	}
	assertGeneratedReleaseSBOMs(t, result)
}

func TestReleaseWorkflowCommentedGenerationBodyDoesNotProduceSBOMs(t *testing.T) {
	t.Parallel()

	step := releaseWorkflowGenerationStep(t)
	step.Run = commentRunScript(step.Run)
	result := executeGenerationRun(t, step, generationRunOptions{})
	if result.err != nil {
		t.Fatalf("commented generation body should be a no-op script, got %v\n%s", result.err, result.output)
	}
	if len(result.documents) != 0 {
		t.Fatalf("commented generation body produced %d SBOMs", len(result.documents))
	}
}

func TestReleaseWorkflowGenerationFailureIsVisible(t *testing.T) {
	t.Parallel()

	result := executeGenerationRun(t, releaseWorkflowGenerationStep(t), generationRunOptions{failGenerator: true})
	if result.err == nil {
		t.Fatal("generator failure was swallowed")
	}
	if !strings.Contains(string(result.output), "cyclonedx-gomod failed") {
		t.Fatalf("generator failure output %q does not name the generator", result.output)
	}
}

type generationStep struct {
	Run string
	Env map[string]string
}

type generationRunOptions struct {
	failGenerator bool
}

type generationRunResult struct {
	documents map[Target][]byte
	assetsDir string
	output    []byte
	err       error
}

func releaseWorkflowGenerationStep(t *testing.T) generationStep {
	t.Helper()
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string            `yaml:"name"`
				Env  map[string]string `yaml:"env"`
				Run  string            `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(releaseWorkflow(t), &workflow); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}
	for _, step := range workflow.Jobs["build"].Steps {
		if step.Name != "Generate deterministic CycloneDX SBOMs" {
			continue
		}
		if strings.TrimSpace(step.Run) == "" {
			t.Fatal("SBOM generation step has no run script")
		}
		return generationStep{Run: step.Run, Env: step.Env}
	}
	t.Fatal("SBOM generation step is missing")
	return generationStep{}
}

func executeGenerationRun(t *testing.T, step generationStep, options generationRunOptions) generationRunResult {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	runnerTemp := filepath.Join(root, "runner")
	assetsDir := filepath.Join(runnerTemp, "release-assets")
	for _, path := range []string{binDir, assetsDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeCommandDouble(t, binDir, "go", "go")
	writeCommandDouble(t, binDir, "jq", "jq")
	writeCommandDouble(t, binDir, "cyclonedx-gomod", "cyclonedx-gomod")

	cmd := exec.Command("bash", "-c", step.Run)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RUNNER_TEMP="+runnerTemp,
		"TEST_GENERATOR_FAIL=",
	)
	for key, value := range step.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Env = append(cmd.Env, "VERSION="+generationTestVersion)
	if options.failGenerator {
		cmd.Env = append(cmd.Env, "TEST_GENERATOR_FAIL=1")
	}
	output, err := cmd.CombinedOutput()
	return generationRunResult{
		documents: loadGeneratedSBOMAssets(t, assetsDir),
		assetsDir: assetsDir,
		output:    output,
		err:       err,
	}
}

func writeCommandDouble(t *testing.T, dir, name, kind string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeWorkflowTestCommand(t, dir, name, fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
export ACR_SBOM_DOUBLE=%s
exec %q "$@"
`, kind, executable))
}

func loadGeneratedSBOMAssets(t *testing.T, dir string) map[Target][]byte {
	t.Helper()
	documents := make(map[Target][]byte)
	for _, target := range Targets() {
		contents, err := os.ReadFile(filepath.Join(dir, target.SBOMName()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		documents[target] = contents
	}
	return documents
}

func assertGeneratedReleaseSBOMs(t *testing.T, result generationRunResult) {
	t.Helper()
	entries, err := os.ReadDir(result.assetsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(Targets()) {
		t.Fatalf("release-assets has %d files, want %d SBOMs", len(entries), len(Targets()))
	}
	if len(result.documents) != len(Targets()) {
		t.Fatalf("generated %d SBOMs, want %d", len(result.documents), len(Targets()))
	}
	for _, target := range Targets() {
		contents, ok := result.documents[target]
		if !ok {
			t.Fatalf("missing %s", target.SBOMName())
		}
		if err := ValidateSBOM(contents, generationTestVersion, target); err != nil {
			t.Fatalf("ValidateSBOM(%s): %v", target.SBOMName(), err)
		}
		var document cyclonedxDocument
		if err := json.Unmarshal(contents, &document); err != nil {
			t.Fatalf("decode %s: %v", target.SBOMName(), err)
		}
		if document.Metadata.Component.Name != "acr" || document.Metadata.Component.Version != generationTestVersion {
			t.Fatalf("%s identity = %s %s, want acr %s", target.SBOMName(), document.Metadata.Component.Name, document.Metadata.Component.Version, generationTestVersion)
		}
	}
	for _, target := range Targets() {
		for _, other := range Targets() {
			if other == target {
				continue
			}
			if err := ValidateSBOM(result.documents[other], generationTestVersion, target); err == nil {
				t.Fatalf("generated %s was accepted as %s", other.SBOMName(), target.SBOMName())
			}
		}
	}
}

func reorderGenerationTargets(t *testing.T, run string) string {
	t.Helper()
	re := regexp.MustCompile(`for target in ([^;]+); do`)
	loc := re.FindStringSubmatchIndex(run)
	if loc == nil {
		t.Fatal("generation step has no target loop to reorder")
	}
	fields := strings.Fields(run[loc[2]:loc[3]])
	if len(fields) != len(Targets()) {
		t.Fatalf("generation loop targets %v, want %d entries", fields, len(Targets()))
	}
	for i, j := 0, len(fields)-1; i < j; i, j = i+1, j-1 {
		fields[i], fields[j] = fields[j], fields[i]
	}
	return run[:loc[2]] + strings.Join(fields, " ") + run[loc[3]:]
}

func commentRunScript(run string) string {
	lines := strings.Split(run, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		lines[i] = "# " + line
	}
	return strings.Join(lines, "\n")
}

func runCycloneDXDouble() int {
	if os.Getenv("TEST_GENERATOR_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "cyclonedx-gomod failed: generator error")
		return 1
	}
	goos := os.Getenv("GOOS")
	goarch := os.Getenv("GOARCH")
	cgo := os.Getenv("CGO_ENABLED")
	if goos == "" || goarch == "" {
		fmt.Fprintln(os.Stderr, "cyclonedx-gomod requires GOOS and GOARCH")
		return 1
	}
	output := ""
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		if args[i] == "-output" {
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "cyclonedx-gomod -output requires a path")
				return 1
			}
			output = args[i+1]
			break
		}
	}
	if output == "" {
		fmt.Fprintln(os.Stderr, "cyclonedx-gomod requires -output")
		return 1
	}
	document := map[string]any{
		"bomFormat":   "CycloneDX",
		"specVersion": "1.6",
		"metadata": map[string]any{
			"component": map[string]any{
				"name":    generatedModuleName,
				"version": "dev",
				"purl":    fmt.Sprintf("pkg:golang/%s@dev?goos=%s&goarch=%s&type=module#cmd/acr", generatedModuleName, goos, goarch),
				"properties": []map[string]string{
					{"name": cyclonedxPropertyCGO, "value": cgo},
					{"name": cyclonedxPropertyGOOS, "value": goos},
					{"name": cyclonedxPropertyGOARCH, "value": goarch},
				},
			},
		},
		"components": []any{},
		"dependencies": []map[string]string{
			{"ref": fmt.Sprintf("pkg:golang/%s@dev?goos=%s&goarch=%s&type=module#cmd/acr", generatedModuleName, goos, goarch)},
		},
	}
	contents, err := json.Marshal(document)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode generated SBOM: %v\n", err)
		return 1
	}
	if err := os.WriteFile(output, contents, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write generated SBOM %s: %v\n", output, err)
		return 1
	}
	return 0
}

func runJQDouble() int {
	args := os.Args[1:]
	switch {
	case len(args) == 3 && args[0] == "-e":
		if args[1] != `.metadata.component.name | test("agentic-context-registry")` {
			fmt.Fprintf(os.Stderr, "unexpected jq expression: %s\n", args[1])
			return 1
		}
		if err := requireGeneratedModuleNameFile(args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		return 0
	case len(args) == 5 && args[0] == "--arg" && args[1] == "version":
		if args[3] != `.metadata.component.name = "acr" | .metadata.component.version = $version` {
			fmt.Fprintf(os.Stderr, "unexpected jq rewrite: %s\n", args[3])
			return 1
		}
		contents, err := os.ReadFile(args[4])
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		var document map[string]any
		if err := json.Unmarshal(contents, &document); err != nil {
			fmt.Fprintf(os.Stderr, "decode SBOM for rewrite: %v\n", err)
			return 1
		}
		metadata, ok := document["metadata"].(map[string]any)
		if !ok {
			fmt.Fprintln(os.Stderr, "SBOM has no metadata object")
			return 1
		}
		component, ok := metadata["component"].(map[string]any)
		if !ok {
			fmt.Fprintln(os.Stderr, "SBOM has no metadata.component object")
			return 1
		}
		component["name"] = "acr"
		component["version"] = args[2]
		rewritten, err := json.Marshal(document)
		if err != nil {
			fmt.Fprintf(os.Stderr, "encode rewritten SBOM: %v\n", err)
			return 1
		}
		if _, err := os.Stdout.Write(rewritten); err != nil {
			fmt.Fprintf(os.Stderr, "write rewritten SBOM: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unexpected jq command: %v\n", args)
		return 1
	}
}

func requireGeneratedModuleNameFile(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return requireGeneratedModuleName(contents)
}

func runGoDouble() int {
	args := os.Args[1:]
	switch {
	case len(args) == 2 && args[0] == "install":
		if args[1] != cyclonedxGomodPin {
			fmt.Fprintf(os.Stderr, "go install %q, want pinned %s\n", args[1], cyclonedxGomodPin)
			return 1
		}
		return 0
	case len(args) >= 3 && args[0] == "run" && args[1] == "./internal/releasetool" && args[2] == "verify-sbom":
		return runVerifySBOMDouble(args[3:])
	default:
		fmt.Fprintf(os.Stderr, "unexpected go command: %v\n", args)
		return 1
	}
}

func runVerifySBOMDouble(args []string) int {
	flags := flag.NewFlagSet("verify-sbom", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	version := flags.String("version", "", "")
	path := flags.String("path", "", "")
	goos := flags.String("goos", "", "")
	goarch := flags.String("goarch", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 0 || *version == "" || *path == "" || *goos == "" || *goarch == "" {
		fmt.Fprintln(os.Stderr, "verify-sbom requires --version, --path, --goos, and --goarch")
		return 1
	}
	var target Target
	for _, candidate := range Targets() {
		if candidate.GOOS == *goos && candidate.GOARCH == *goarch {
			target = candidate
			break
		}
	}
	if target == (Target{}) {
		fmt.Fprintf(os.Stderr, "verify-sbom target %s/%s is outside the release set\n", *goos, *goarch)
		return 1
	}
	if filepath.Base(*path) != target.SBOMName() {
		fmt.Fprintf(os.Stderr, "verify-sbom path %q is not %s\n", *path, target.SBOMName())
		return 1
	}
	contents, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if err := ValidateSBOM(contents, *version, target); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		Path string `json:"path"`
	}{Path: *path}); err != nil {
		fmt.Fprintf(os.Stderr, "encode verify-sbom result: %v\n", err)
		return 1
	}
	return 0
}
