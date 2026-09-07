package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	sbomDoubleEnv      = "ACR_SBOM_DOUBLE"
	installDirEnv      = "ACR_SBOM_INSTALL_DIR"
	invocationLogEnv   = "ACR_SBOM_INVOCATION_LOG"
	realGoEnv          = "ACR_SBOM_REAL_GO"
	generatorModuleEnv = "ACR_SBOM_GENERATOR_MODULE"
	generatorFailEnv   = "ACR_SBOM_GENERATOR_FAIL"

	// checksumDoubleKind names the controlled sha256 checker the verify-step
	// tests put on PATH; see workflow_verify_test.go.
	checksumDoubleKind = "checksum"

	generatedDependencyName = "example.com/sbom-dependency"

	// Fixed stand-ins for the fields cyclonedx-gomod emits when -noserial or
	// -notimestamp is missing. They are constants so the generated document
	// never depends on a clock.
	generatedSerialNumber = "urn:uuid:6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	generatedTimestamp    = "2026-01-02T03:04:05Z"
)

func TestMain(m *testing.M) {
	switch os.Getenv(sbomDoubleEnv) {
	case "":
		os.Exit(m.Run())
	case "cyclonedx-gomod":
		os.Exit(runCycloneDXDouble())
	case "go":
		os.Exit(runGoDouble())
	case checksumDoubleKind:
		os.Exit(runChecksumDouble())
	default:
		fmt.Fprintf(os.Stderr, "unknown %s %q\n", sbomDoubleEnv, os.Getenv(sbomDoubleEnv))
		os.Exit(1)
	}
}

// The three tests below run the release workflow's own generation step. They
// assert what running it produced — never how the step is written.

func TestReleaseWorkflowGeneratesFourTargetSBOMs(t *testing.T) {
	t.Parallel()

	result := executeGenerationScript(t, releaseWorkflowGenerationStep(t), generationRunOptions{})
	if result.err != nil {
		t.Fatalf("run SBOM generation step: %v\n%s", result.err, result.output)
	}
	if err := checkGeneratedReleaseSBOMs(result); err != nil {
		t.Fatalf("release workflow generation step: %v\n%s", err, result.output)
	}
}

func TestReleaseWorkflowGenerationRejectsForeignModuleDocument(t *testing.T) {
	t.Parallel()

	result := executeGenerationScript(t, releaseWorkflowGenerationStep(t), generationRunOptions{generatorModule: foreignGeneratedModule})
	if result.err == nil {
		t.Fatalf("generation accepted a document for another module\n%s", result.output)
	}
	if !strings.Contains(string(result.output), "does not identify the agentic-context-registry module") {
		t.Fatalf("module guard output %q does not name the rejected module identity", result.output)
	}
	if len(result.documents) != 0 {
		t.Fatalf("module guard left %d SBOMs in release-assets", len(result.documents))
	}
}

func TestReleaseWorkflowGenerationFailureIsVisible(t *testing.T) {
	t.Parallel()

	result := executeGenerationScript(t, releaseWorkflowGenerationStep(t), generationRunOptions{failGenerator: true})
	if result.err == nil {
		t.Fatal("generator failure was swallowed")
	}
	if !strings.Contains(string(result.output), "cyclonedx-gomod failed") {
		t.Fatalf("generator failure output %q does not name the generator", result.output)
	}
}

const foreignGeneratedModule = "github.com/example/unrelated-module"

// generationFixture is an authored generation script plus the outcome the
// runner and the asset check owe it. Fixtures are test input, so their text is
// explicit; the production step above is never rewritten to make a point about
// it.
type generationFixture struct {
	name    string
	script  string
	options generationRunOptions
	// wantRejection is the text a rejection must name — from the failed run's
	// output, or from the asset check when the run itself succeeds. Empty means
	// the fixture must be accepted end to end.
	wantRejection string
	// why records what the fixture establishes.
	why string
}

func TestGenerationScriptFixtures(t *testing.T) {
	t.Parallel()

	for _, fixture := range generationFixtures(t) {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			step := workflowStep{Run: fixture.script, Env: map[string]string{"CGO_ENABLED": "0"}}
			result := executeGenerationScript(t, step, fixture.options)
			checkErr := checkGeneratedReleaseSBOMs(result)
			if fixture.wantRejection == "" {
				if result.err != nil {
					t.Fatalf("%s: run failed: %v\n%s", fixture.why, result.err, result.output)
				}
				if checkErr != nil {
					t.Fatalf("%s: %v\n%s", fixture.why, checkErr, result.output)
				}
				return
			}
			rejection := ""
			switch {
			case result.err != nil:
				rejection = string(result.output)
			case checkErr != nil:
				rejection = checkErr.Error()
			default:
				t.Fatalf("%s: fixture was accepted; want a rejection naming %q", fixture.why, fixture.wantRejection)
			}
			if !strings.Contains(rejection, fixture.wantRejection) {
				t.Fatalf("%s: rejection %q does not name %q", fixture.why, rejection, fixture.wantRejection)
			}
		})
	}
}

// The fragments below are slices of the authored baseline, so a fixture can
// remove or wrap exactly one of them.
const (
	fixtureValidation = `          go run ./internal/releasetool verify-sbom \
            --version "${VERSION}" \
            --goos "${goos}" \
            --goarch "${goarch}" \
            --path "${output}"
`
	fixtureModuleGuard = `          if ! jq -e '.metadata.component.name | test("agentic-context-registry")' "${raw}" > /dev/null; then
            echo "Generated ${target} SBOM does not identify the agentic-context-registry module" >&2
            exit 1
          fi
`
	fixtureIdentityRewrite = `          jq --arg version "${VERSION}" \
            '.metadata.component.name = "acr" | .metadata.component.version = $version' \
            "${raw}" > "${output}"
`
)

func baselineGenerationFixture() string {
	return strings.ReplaceAll(baselineGenerationScript, "GENERATOR_PIN", cyclonedxGomodPin)
}

// TestGenerationWithoutTheModuleGuardAcceptsAForeignModule is the control for
// the production foreign-module test: with the guard removed the same run
// succeeds, so the rejection there is the guard's doing and not validation's.
func TestGenerationWithoutTheModuleGuardAcceptsAForeignModule(t *testing.T) {
	t.Parallel()

	baseline := baselineGenerationFixture()
	script := strings.Replace(baseline, fixtureModuleGuard, "", 1)
	if script == baseline {
		t.Fatal("the baseline fixture no longer contains the module guard")
	}
	step := workflowStep{Run: script, Env: map[string]string{"CGO_ENABLED": "0"}}
	result := executeGenerationScript(t, step, generationRunOptions{generatorModule: foreignGeneratedModule})
	if result.err != nil {
		t.Fatalf("without the module guard the run must succeed: %v\n%s", result.err, result.output)
	}
	if len(result.documents) != len(Targets()) {
		t.Fatalf("without the module guard the run produced %d documents, want %d", len(result.documents), len(Targets()))
	}
}

func generationFixtures(t *testing.T) []generationFixture {
	t.Helper()
	baseline := baselineGenerationFixture()
	installLine := "          go install " + cyclonedxGomodPin + "\n"
	validation := fixtureValidation
	rewrite := fixtureIdentityRewrite
	edit := func(script string, replacements ...[2]string) string {
		t.Helper()
		for _, replacement := range replacements {
			if !strings.Contains(script, replacement[0]) {
				t.Fatalf("fixture edit %q does not apply to the baseline script", replacement[0])
			}
			script = strings.ReplaceAll(script, replacement[0], replacement[1])
		}
		return script
	}

	return []generationFixture{
		{
			name:   "baseline",
			script: baseline,
			why:    "the authored baseline is what every other fixture varies from",
		},
		{
			name: "longest-match-expansion",
			script: edit(baseline,
				[2]string{"${target%-*}", "${target%%-*}"},
				[2]string{"${target#*-}", "${target##*-}"}),
			why: "a longest-match expansion selects the same targets, so it must be accepted",
		},
		{
			name: "renamed-target-variables",
			script: edit(baseline,
				[2]string{`goos="${target`, `target_os="${target`},
				[2]string{`goarch="${target`, `target_arch="${target`},
				[2]string{"${goos}", "${target_os}"},
				[2]string{"${goarch}", "${target_arch}"}),
			why: "renaming the derived variables changes nothing the step produces",
		},
		{
			name: "reordered-targets",
			script: edit(baseline,
				[2]string{"darwin-amd64 darwin-arm64 linux-amd64 linux-arm64", "linux-arm64 linux-amd64 darwin-arm64 darwin-amd64"}),
			why: "generation order is not part of the contract",
		},
		{
			name: "equivalent-jq-programs",
			script: edit(baseline,
				[2]string{`'.metadata.component.name | test("agentic-context-registry")'`, `'.metadata.component.name|test("agentic-context-registry")'`},
				[2]string{`'.metadata.component.name = "acr" | .metadata.component.version = $version'`, `'.metadata.component += {"name":"acr","version":$version}'`}),
			why: "jq programs that produce the same document must be accepted",
		},
		{
			name:   "explicit-targets-without-a-loop",
			script: strings.ReplaceAll(unrolledGenerationScript, "GENERATOR_PIN", cyclonedxGomodPin),
			why:    "the proof needs no loop and no shell expansion at all",
		},
		{
			name:          "no-validation",
			script:        edit(baseline, [2]string{validation, ""}),
			wantRejection: "never validated acr-darwin-amd64.cdx.json",
			why:           "a step that validates nothing must be rejected",
		},
		{
			name: "validates-only-the-first-target",
			script: edit(baseline, [2]string{validation,
				"          if [ \"${target}\" = \"darwin-amd64\" ]; then\n" + validation + "          fi\n"}),
			wantRejection: "never validated acr-darwin-arm64.cdx.json",
			why:           "skipping a target's validation must be rejected",
		},
		{
			name: "swapped-validation-targets",
			script: edit(baseline,
				[2]string{`--goos "${goos}"`, `--goos "${goarch}"`},
				[2]string{`--goarch "${goarch}"`, `--goarch "${goos}"`}),
			wantRejection: "outside the macOS/Linux amd64/arm64 release set",
			why:           "validating a document against the wrong target must fail",
		},
		{
			name:          "no-generator-install",
			script:        edit(baseline, [2]string{installLine, ""}),
			wantRejection: "cyclonedx-gomod: command not found",
			why:           "the install is load-bearing, not decorative",
		},
		{
			name:          "unresolvable-go-package",
			script:        edit(baseline, [2]string{"./internal/releasetool", "./internal/no-such-package"}),
			wantRejection: "internal/no-such-package: directory not found",
			why:           "the package the step names is the package that runs",
		},
		{
			name:          "single-target",
			script:        edit(baseline, [2]string{"darwin-amd64 darwin-arm64 linux-amd64 linux-arm64", "linux-amd64"}),
			wantRejection: "release-assets holds 1 entries, want 4 target documents",
			why:           "the issue #41 regression — one SBOM for four platforms",
		},
		{
			name:          "no-identity-rewrite",
			script:        edit(baseline, [2]string{rewrite, "          cp \"${raw}\" \"${output}\"\n"}),
			wantRejection: "expected acr; set the generated application identity",
			why:           "the release identity has to be written into every document",
		},
		{
			name:          "wrong-version",
			script:        edit(baseline, [2]string{`--arg version "${VERSION}"`, `--arg version "9.9.9"`}),
			wantRejection: `expected "1.2.3"`,
			why:           "a document stamped with another version must be refused",
		},
		{
			name:          "legacy-output-name",
			script:        edit(baseline, [2]string{"release-assets/acr-${target}.cdx.json", "release-assets/acr.cdx.json"}),
			wantRejection: "is not acr-darwin-amd64.cdx.json",
			why:           "the per-target asset name is the contract",
		},
		{
			name:          "raw-document-leaks-into-the-assets",
			script:        edit(baseline, [2]string{`raw="${RUNNER_TEMP}/acr`, `raw="${RUNNER_TEMP}/release-assets/acr`}),
			wantRejection: "release-assets holds 8 entries, want 4 target documents",
			why:           "only the four rewritten documents may reach release-assets",
		},
		{
			name:          "cgo-enabled",
			script:        edit(baseline, [2]string{"CGO_ENABLED=0 GOOS=", "CGO_ENABLED=1 GOOS="}),
			wantRejection: "expected 0; regenerate",
			why:           "the recorded build constraints have to match the release build",
		},
		{
			name:          "generator-without-noserial",
			script:        edit(baseline, [2]string{" -noserial", ""}),
			wantRejection: "carries serialNumber",
			why:           "a serial number makes the documents irreproducible",
		},
		{
			name:          "generator-without-notimestamp",
			script:        edit(baseline, [2]string{" -notimestamp", ""}),
			wantRejection: "carries metadata.timestamp",
			why:           "a timestamp makes the documents irreproducible",
		},
		{
			name:          "generator-without-main",
			script:        edit(baseline, [2]string{" -main cmd/acr", ""}),
			wantRejection: "no Go files in",
			why:           "the main package selects what the document describes",
		},
		{
			name:          "generator-without-json",
			script:        edit(baseline, [2]string{" -json", ""}),
			wantRejection: "app requires -json",
			why:           "an argument the double cannot emulate is refused, never ignored",
		},
		{
			name:          "generator-with-an-unknown-flag",
			script:        edit(baseline, [2]string{"cyclonedx-gomod app", "cyclonedx-gomod app -assert-license"}),
			wantRejection: `unsupported cyclonedx-gomod flag "-assert-license"`,
			why:           "an argument the double does not honour is refused, never ignored",
		},
		{
			name:          "empty-script",
			script:        "set -euo pipefail\n:\n",
			wantRejection: "release-assets holds 0 entries, want 4 target documents",
			why:           "a step that does nothing must not pass",
		},
	}
}

// baselineGenerationScript is authored test input, not a copy of the workflow
// held to be byte-identical with it. It exists so the fixtures below can vary
// one thing at a time.
const baselineGenerationScript = `          set -euo pipefail
          go install GENERATOR_PIN
          for target in darwin-amd64 darwin-arm64 linux-amd64 linux-arm64; do
            goos="${target%-*}"
            goarch="${target#*-}"
            raw="${RUNNER_TEMP}/acr-${target}.cdx.raw.json"
            output="${RUNNER_TEMP}/release-assets/acr-${target}.cdx.json"
            CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
              cyclonedx-gomod app -json -licenses -noserial -notimestamp \
                -main cmd/acr -output "${raw}" .
          if ! jq -e '.metadata.component.name | test("agentic-context-registry")' "${raw}" > /dev/null; then
            echo "Generated ${target} SBOM does not identify the agentic-context-registry module" >&2
            exit 1
          fi
          jq --arg version "${VERSION}" \
            '.metadata.component.name = "acr" | .metadata.component.version = $version' \
            "${raw}" > "${output}"
          go run ./internal/releasetool verify-sbom \
            --version "${VERSION}" \
            --goos "${goos}" \
            --goarch "${goarch}" \
            --path "${output}"
          done
`

// unrolledGenerationScript names every target explicitly, so it shares no loop
// and no parameter expansion with the workflow.
const unrolledGenerationScript = `          set -euo pipefail
          go install GENERATOR_PIN
          generate() {
            name="acr-$1-$2.cdx.json"
            raw="${RUNNER_TEMP}/$1-$2.raw.json"
            output="${RUNNER_TEMP}/release-assets/${name}"
            CGO_ENABLED=0 GOOS="$1" GOARCH="$2" \
              cyclonedx-gomod app -json -licenses -noserial -notimestamp \
                -main cmd/acr -output "${raw}" .
            jq -e '.metadata.component.name | test("agentic-context-registry")' "${raw}" > /dev/null
            jq --arg version "${VERSION}" \
              '.metadata.component.name = "acr" | .metadata.component.version = $version' \
              "${raw}" > "${output}"
            go run ./internal/releasetool verify-sbom \
              --version "${VERSION}" --goos "$1" --goarch "$2" --path "${output}"
          }
          generate darwin amd64
          generate darwin arm64
          generate linux amd64
          generate linux arm64
`

type generationRunOptions struct {
	failGenerator   bool
	generatorModule string
}

type generationRunResult struct {
	documents   map[Target][]byte
	assetsDir   string
	invocations []toolInvocation
	output      []byte
	err         error
}

// toolInvocation records one call the running script made into the go command
// double, so the checks below read what the script did rather than how it is
// written.
type toolInvocation struct {
	Tool    string   `json:"tool"`
	Command string   `json:"command"`
	Package string   `json:"package,omitempty"`
	Args    []string `json:"args"`
	Stdout  string   `json:"stdout,omitempty"`
	Exit    int      `json:"exit"`
}

func releaseWorkflowGenerationStep(t *testing.T) workflowStep {
	t.Helper()
	return releaseWorkflowStep(t, "build", "Generate deterministic CycloneDX SBOMs")
}

// executeGenerationScript runs a generation script the way the release runner
// does: from the module root, with jq and the Go toolchain real, and with the
// generator and the go entry point standing in for tools the suite must not
// download or install for real.
func executeGenerationScript(t *testing.T, step workflowStep, options generationRunOptions) generationRunResult {
	t.Helper()
	requireWorkflowTool(t, "jq")
	realGo := requireWorkflowTool(t, "go")
	moduleDir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	installDir := filepath.Join(root, "gobin")
	runnerTemp := filepath.Join(root, "runner")
	assetsDir := filepath.Join(runnerTemp, "release-assets")
	for _, path := range []string{binDir, installDir, assetsDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeCommandDouble(t, binDir, "go")
	invocationLog := filepath.Join(root, "invocations.jsonl")

	cmd := exec.Command("bash", "-c", step.Run)
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(),
		"PATH="+strings.Join([]string{binDir, installDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
		"RUNNER_TEMP="+runnerTemp,
		installDirEnv+"="+installDir,
		invocationLogEnv+"="+invocationLog,
		realGoEnv+"="+realGo,
		generatorModuleEnv+"="+options.generatorModule,
		generatorFailEnv+"=",
	)
	for key, value := range step.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Env = append(cmd.Env, "VERSION="+generationTestVersion)
	if options.failGenerator {
		cmd.Env = append(cmd.Env, generatorFailEnv+"=1")
	}
	output, err := cmd.CombinedOutput()
	return generationRunResult{
		documents:   loadGeneratedSBOMAssets(t, assetsDir),
		assetsDir:   assetsDir,
		invocations: loadToolInvocations(t, invocationLog),
		output:      output,
		err:         err,
	}
}

// requireWorkflowTool fails the release workflow tests with an installation
// instruction rather than letting the executed script report a bare
// "command not found".
func requireWorkflowTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("release workflow tests require %s: %v; install %s and put it on PATH (CONTRIBUTING.md lists the prerequisites)", name, err, name)
	}
	return path
}

func writeCommandDouble(t *testing.T, dir, kind string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeWorkflowTestCommand(t, dir, kind, commandDoubleScript(kind, executable))
}

func commandDoubleScript(kind, executable string) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
export %s=%s
exec %q "$@"
`, sbomDoubleEnv, kind, executable)
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

func loadToolInvocations(t *testing.T, path string) []toolInvocation {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var invocations []toolInvocation
	for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
		if line == "" {
			continue
		}
		var invocation toolInvocation
		if err := json.Unmarshal([]byte(line), &invocation); err != nil {
			t.Fatalf("decode recorded invocation %q: %v", line, err)
		}
		invocations = append(invocations, invocation)
	}
	return invocations
}

// generatedSBOM is the shape the checks read back out of a produced document.
type generatedSBOM struct {
	Metadata struct {
		Component struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"component"`
	} `json:"metadata"`
	Components []struct {
		Name string `json:"name"`
	} `json:"components"`
	Dependencies []struct {
		Ref string `json:"ref"`
	} `json:"dependencies"`
}

// checkGeneratedReleaseSBOMs is what a release-ready generation run has to
// satisfy: four reproducible per-target documents in release-assets, each one
// valid for its own target and invalid for the others, produced by the pinned
// generator and validated by the production release tool.
func checkGeneratedReleaseSBOMs(result generationRunResult) error {
	entries, err := os.ReadDir(result.assetsDir)
	if err != nil {
		return fmt.Errorf("read release-assets: %w", err)
	}
	if len(entries) != len(Targets()) {
		return fmt.Errorf("release-assets holds %d entries, want %d target documents", len(entries), len(Targets()))
	}
	for _, target := range Targets() {
		contents, ok := result.documents[target]
		if !ok {
			return fmt.Errorf("%s is missing from release-assets", target.SBOMName())
		}
		if err := ValidateSBOM(contents, generationTestVersion, target); err != nil {
			return fmt.Errorf("validate %s: %w", target.SBOMName(), err)
		}
		var document generatedSBOM
		if err := json.Unmarshal(contents, &document); err != nil {
			return fmt.Errorf("decode %s: %w", target.SBOMName(), err)
		}
		if document.Metadata.Component.Name != "acr" || document.Metadata.Component.Version != generationTestVersion {
			return fmt.Errorf("%s identity is %q %q, want acr %s", target.SBOMName(), document.Metadata.Component.Name, document.Metadata.Component.Version, generationTestVersion)
		}
		if err := checkReproducibleShape(target, contents); err != nil {
			return err
		}
		if err := checkGraphPreserved(target, document); err != nil {
			return err
		}
	}
	for _, target := range Targets() {
		for _, other := range Targets() {
			if other == target {
				continue
			}
			if err := ValidateSBOM(result.documents[other], generationTestVersion, target); err == nil {
				return fmt.Errorf("%s was accepted as %s", other.SBOMName(), target.SBOMName())
			}
		}
	}
	if err := checkPinnedGeneratorInstalled(result.invocations); err != nil {
		return err
	}
	validated, err := validatedGenerationTargets(result.invocations)
	if err != nil {
		return err
	}
	for _, target := range Targets() {
		if _, ok := validated[target]; !ok {
			return fmt.Errorf("the executed step never validated %s", target.SBOMName())
		}
	}
	return nil
}

// checkReproducibleShape holds the step to the "deterministic" in its own name:
// a document carrying a serial number or a generation timestamp differs on
// every run.
func checkReproducibleShape(target Target, contents []byte) error {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(contents, &document); err != nil {
		return fmt.Errorf("decode %s: %w", target.SBOMName(), err)
	}
	if _, ok := document["serialNumber"]; ok {
		return fmt.Errorf("%s carries serialNumber; generate it with -noserial", target.SBOMName())
	}
	metadata, ok := document["metadata"]
	if !ok {
		return fmt.Errorf("%s has no metadata object", target.SBOMName())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &fields); err != nil {
		return fmt.Errorf("decode %s metadata: %w", target.SBOMName(), err)
	}
	if _, ok := fields["timestamp"]; ok {
		return fmt.Errorf("%s carries metadata.timestamp; generate it with -notimestamp", target.SBOMName())
	}
	return nil
}

// checkGraphPreserved holds the identity rewrite to changing the release
// identity and nothing else.
func checkGraphPreserved(target Target, document generatedSBOM) error {
	if len(document.Components) != 1 || document.Components[0].Name != generatedDependencyName {
		return fmt.Errorf("%s components are %#v, want the generated %s entry", target.SBOMName(), document.Components, generatedDependencyName)
	}
	if len(document.Dependencies) != 1 || !strings.Contains(document.Dependencies[0].Ref, generatedModuleName) {
		return fmt.Errorf("%s dependencies are %#v, want the generated module reference", target.SBOMName(), document.Dependencies)
	}
	return nil
}

func checkPinnedGeneratorInstalled(invocations []toolInvocation) error {
	installs := 0
	for _, invocation := range invocations {
		if invocation.Tool != "go" || invocation.Command != "install" {
			continue
		}
		if invocation.Exit != 0 {
			return fmt.Errorf("go install %v exited %d", invocation.Args, invocation.Exit)
		}
		installs++
	}
	if installs != 1 {
		return fmt.Errorf("the executed step installed the pinned generator %d times, want 1", installs)
	}
	return nil
}

// validatedGenerationTargets reports which targets the executed script actually
// validated. The production release tool only succeeds when the document it was
// handed is the one named for the target it was told to check, so the path it
// reports accepting identifies the pair.
func validatedGenerationTargets(invocations []toolInvocation) (map[Target]string, error) {
	validated := make(map[Target]string)
	for _, invocation := range invocations {
		if invocation.Tool != "go" || invocation.Command != "run" || invocation.Exit != 0 {
			continue
		}
		var accepted struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(invocation.Stdout), &accepted); err != nil || accepted.Path == "" {
			continue
		}
		target, ok := targetBySBOMName(filepath.Base(accepted.Path))
		if !ok {
			continue
		}
		if previous, seen := validated[target]; seen {
			return nil, fmt.Errorf("the executed step validated %s twice (%s and %s)", target.SBOMName(), previous, accepted.Path)
		}
		validated[target] = accepted.Path
	}
	return validated, nil
}

func targetBySBOMName(name string) (Target, bool) {
	for _, target := range Targets() {
		if target.SBOMName() == name {
			return target, true
		}
	}
	return Target{}, false
}

// generatorInvocation is the cyclonedx-gomod command line the double honours.
type generatorInvocation struct {
	moduleDir   string
	mainPackage string
	output      string
	noSerial    bool
	noTimestamp bool
}

func runCycloneDXDouble() int {
	if os.Getenv(generatorFailEnv) == "1" {
		fmt.Fprintln(os.Stderr, "cyclonedx-gomod failed: generator error")
		return 1
	}
	invocation, err := parseGeneratorInvocation(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "cyclonedx-gomod: %v\n", err)
		return 1
	}
	goos := os.Getenv("GOOS")
	goarch := os.Getenv("GOARCH")
	if goos == "" || goarch == "" {
		fmt.Fprintln(os.Stderr, "cyclonedx-gomod requires GOOS and GOARCH")
		return 1
	}
	module := os.Getenv(generatorModuleEnv)
	if module == "" {
		module = generatedModuleName
	}
	reference := fmt.Sprintf("pkg:golang/%s@dev?goos=%s&goarch=%s&type=module#%s", module, goos, goarch, invocation.mainPackage)
	metadata := map[string]any{
		"component": map[string]any{
			"name":    module,
			"version": "dev",
			"purl":    reference,
			"properties": []map[string]string{
				{"name": cyclonedxPropertyCGO, "value": os.Getenv("CGO_ENABLED")},
				{"name": cyclonedxPropertyGOOS, "value": goos},
				{"name": cyclonedxPropertyGOARCH, "value": goarch},
			},
		},
	}
	if !invocation.noTimestamp {
		metadata["timestamp"] = generatedTimestamp
	}
	document := map[string]any{
		"bomFormat":   "CycloneDX",
		"specVersion": "1.6",
		"metadata":    metadata,
		"components": []map[string]string{
			{"type": "library", "name": generatedDependencyName, "version": "v1.0.0"},
		},
		"dependencies": []map[string]string{
			{"ref": reference},
		},
	}
	if !invocation.noSerial {
		document["serialNumber"] = generatedSerialNumber
	}
	contents, err := json.Marshal(document)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode generated SBOM: %v\n", err)
		return 1
	}
	if err := os.WriteFile(invocation.output, contents, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write generated SBOM %s: %v\n", invocation.output, err)
		return 1
	}
	return 0
}

// parseGeneratorInvocation reads the generator's command line the way
// cyclonedx-gomod does. Every argument is either honoured or refused by name;
// none is ignored, so dropping one from the workflow cannot pass unnoticed.
func parseGeneratorInvocation(args []string) (generatorInvocation, error) {
	if len(args) == 0 || args[0] != "app" {
		return generatorInvocation{}, fmt.Errorf("this double emulates the app command, got %v", args)
	}
	invocation := generatorInvocation{}
	emitsJSON := false
	positional := make([]string, 0, 1)
	for i := 1; i < len(args); i++ {
		switch argument := args[i]; argument {
		case "-json":
			emitsJSON = true
		case "-licenses":
			// Licenses change the emitted component data, not the target
			// constraints or the identity this release validates.
		case "-noserial":
			invocation.noSerial = true
		case "-notimestamp":
			invocation.noTimestamp = true
		case "-main", "-output":
			if i+1 >= len(args) {
				return generatorInvocation{}, fmt.Errorf("%s requires a value", argument)
			}
			i++
			if argument == "-main" {
				invocation.mainPackage = args[i]
			} else {
				invocation.output = args[i]
			}
		default:
			if strings.HasPrefix(argument, "-") {
				return generatorInvocation{}, fmt.Errorf("unsupported cyclonedx-gomod flag %q", argument)
			}
			positional = append(positional, argument)
		}
	}
	if !emitsJSON {
		return generatorInvocation{}, errors.New("app requires -json; this double emits JSON only")
	}
	if invocation.output == "" {
		return generatorInvocation{}, errors.New("app requires -output")
	}
	if len(positional) != 1 {
		return generatorInvocation{}, fmt.Errorf("app takes exactly one module directory, got %v", positional)
	}
	invocation.moduleDir = positional[0]
	if err := requireMainPackage(invocation.moduleDir, invocation.mainPackage); err != nil {
		return generatorInvocation{}, err
	}
	return invocation, nil
}

// requireMainPackage mirrors what the real generator does when -main is missing
// or names something that is not a package: it refuses to load one.
func requireMainPackage(moduleDir, mainPackage string) error {
	directory := filepath.Join(moduleDir, mainPackage)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("failed to load package: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
	}
	return fmt.Errorf("failed to load package: no Go files in %s", directory)
}

func runGoDouble() int {
	args := os.Args[1:]
	invocation := toolInvocation{Tool: "go", Args: args}
	switch {
	case len(args) > 0 && args[0] == "install":
		invocation.Command = "install"
		if len(args) != 2 {
			fmt.Fprintf(os.Stderr, "go install expects one module specification, got %v\n", args[1:])
			invocation.Exit = 1
			break
		}
		invocation.Package = args[1]
		invocation.Exit = installGeneratorDouble(args[1])
	case len(args) > 1 && args[0] == "run":
		invocation.Command = "run"
		invocation.Package = args[1]
		invocation.Stdout, invocation.Exit = runRealGo(args)
	default:
		fmt.Fprintf(os.Stderr, "unsupported go command: %v\n", args)
		invocation.Exit = 1
	}
	if err := recordToolInvocation(invocation); err != nil {
		fmt.Fprintf(os.Stderr, "record go invocation: %v\n", err)
		return 1
	}
	if _, err := os.Stdout.WriteString(invocation.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "write go output: %v\n", err)
		return 1
	}
	return invocation.Exit
}

// installGeneratorDouble stands in for the one `go install` this step runs. The
// real install downloads and builds the pinned generator, which the suite gets
// from TestCycloneDXGomodRecordsPerTargetBuildConstraints instead; here the
// install places the generator double where the workflow expects the installed
// binary, so a step that never installs cannot generate either. Any other
// module specification is refused by name.
func installGeneratorDouble(specification string) int {
	if specification != cyclonedxGomodPin {
		fmt.Fprintf(os.Stderr, "go install %q, want pinned %s\n", specification, cyclonedxGomodPin)
		return 1
	}
	dir := os.Getenv(installDirEnv)
	if dir == "" {
		fmt.Fprintf(os.Stderr, "%s is unset; the workflow test must name a generator install directory\n", installDirEnv)
		return 1
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "locate the generator double: %v\n", err)
		return 1
	}
	path := filepath.Join(dir, "cyclonedx-gomod")
	if err := os.WriteFile(path, []byte(commandDoubleScript("cyclonedx-gomod", executable)), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "install the generator double at %s: %v\n", path, err)
		return 1
	}
	return 0
}

// runRealGo forwards the command to the real Go toolchain, so the package the
// script names is the package that runs and an unresolvable one fails exactly
// as it would on the runner.
func runRealGo(args []string) (string, int) {
	binary := os.Getenv(realGoEnv)
	if binary == "" {
		fmt.Fprintf(os.Stderr, "%s is unset; the workflow test must name the real go command\n", realGoEnv)
		return "", 1
	}
	command := exec.Command(binary, args...)
	command.Stderr = os.Stderr
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return string(output), exit.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "run %s %v: %v\n", binary, args, err)
		return string(output), 1
	}
	return string(output), 0
}

func recordToolInvocation(invocation toolInvocation) error {
	path := os.Getenv(invocationLogEnv)
	if path == "" {
		return fmt.Errorf("%s is unset; the workflow test must name an invocation log", invocationLogEnv)
	}
	line, err := json.Marshal(invocation)
	if err != nil {
		return fmt.Errorf("encode invocation record: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open invocation log %s: %w", path, err)
	}
	if _, writeErr := file.Write(append(line, '\n')); writeErr != nil {
		return errors.Join(fmt.Errorf("append to invocation log %s: %w", path, writeErr), file.Close())
	}
	return file.Close()
}
