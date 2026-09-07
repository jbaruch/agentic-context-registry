package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"go.yaml.in/yaml/v3"
)

const (
	sbomDoubleEnv      = "ACR_SBOM_DOUBLE"
	installDirEnv      = "ACR_SBOM_INSTALL_DIR"
	invocationLogEnv   = "ACR_SBOM_INVOCATION_LOG"
	releaseToolEnv     = "ACR_SBOM_RELEASE_TOOL"
	generatorModuleEnv = "ACR_SBOM_GENERATOR_MODULE"
	generatorCGOEnv    = "ACR_SBOM_GENERATOR_CGO"
	generatorFailEnv   = "ACR_SBOM_GENERATOR_FAIL"

	generatedDependencyName = "example.com/sbom-dependency"
)

func TestMain(m *testing.M) {
	switch os.Getenv(sbomDoubleEnv) {
	case "":
	case "cyclonedx-gomod":
		os.Exit(runCycloneDXDouble())
	case "go":
		os.Exit(runGoDouble())
	default:
		fmt.Fprintf(os.Stderr, "unknown %s %q\n", sbomDoubleEnv, os.Getenv(sbomDoubleEnv))
		os.Exit(1)
	}
	code := m.Run()
	if err := removeReleaseToolBuild(); err != nil {
		fmt.Fprintf(os.Stderr, "remove release tool build directory: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
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

func TestReleaseWorkflowGenerationAcceptsEquivalentJQPrograms(t *testing.T) {
	t.Parallel()

	step := releaseWorkflowGenerationStep(t)
	step.Run = wrapGenerationJQPrograms(t, step.Run)
	result := executeGenerationRun(t, step, generationRunOptions{})
	if result.err != nil {
		t.Fatalf("run SBOM generation step with equivalent jq programs: %v\n%s", result.err, result.output)
	}
	assertGeneratedReleaseSBOMs(t, result)
}

func TestReleaseWorkflowGenerationAcceptsRenamedTargetVariables(t *testing.T) {
	t.Parallel()

	step := releaseWorkflowGenerationStep(t)
	step.Run = renameGenerationTargetVariables(t, step.Run)
	result := executeGenerationRun(t, step, generationRunOptions{})
	if result.err != nil {
		t.Fatalf("run SBOM generation step with renamed target variables: %v\n%s", result.err, result.output)
	}
	assertGeneratedReleaseSBOMs(t, result)
}

func TestReleaseWorkflowGenerationRejectsForeignModuleDocument(t *testing.T) {
	t.Parallel()

	result := executeGenerationRun(t, releaseWorkflowGenerationStep(t), generationRunOptions{generatorModule: "github.com/example/unrelated-module"})
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
	if verified := verifiedGenerationTargets(t, result); len(verified) != 0 {
		t.Fatalf("commented generation body validated %d targets", len(verified))
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

func TestReleaseWorkflowGenerationWithoutGeneratorInstallFails(t *testing.T) {
	t.Parallel()

	step := releaseWorkflowGenerationStep(t)
	step.Run = withoutGeneratorInstall(t, step.Run)
	result := executeGenerationRun(t, step, generationRunOptions{})
	if result.err == nil {
		t.Fatalf("generation succeeded without installing the pinned generator\n%s", result.output)
	}
	if len(result.documents) != 0 {
		t.Fatalf("uninstalled generator still produced %d SBOMs", len(result.documents))
	}
}

func TestReleaseWorkflowGenerationPropagatesValidationFailure(t *testing.T) {
	t.Parallel()

	result := executeGenerationRun(t, releaseWorkflowGenerationStep(t), generationRunOptions{generatorCGO: "1"})
	if result.err == nil {
		t.Fatalf("generation accepted a document validation rejects\n%s", result.output)
	}
	if !strings.Contains(string(result.output), cyclonedxPropertyCGO) {
		t.Fatalf("validation failure output %q does not name the violated constraint", result.output)
	}
}

func TestReleaseWorkflowGenerationWithoutValidationValidatesNoTarget(t *testing.T) {
	t.Parallel()

	step := releaseWorkflowGenerationStep(t)
	step.Run = withoutTargetValidation(t, step.Run)
	result := executeGenerationRun(t, step, generationRunOptions{})
	if result.err != nil {
		t.Fatalf("run generation step without validation: %v\n%s", result.err, result.output)
	}
	if len(result.documents) != len(Targets()) {
		t.Fatalf("generated %d SBOMs, want %d", len(result.documents), len(Targets()))
	}
	if verified := verifiedGenerationTargets(t, result); len(verified) != 0 {
		t.Fatalf("removed validation still reported %d validated targets", len(verified))
	}
}

func TestReleaseWorkflowGenerationSkippingValidationLeavesTargetsUnvalidated(t *testing.T) {
	t.Parallel()

	step := releaseWorkflowGenerationStep(t)
	run, validated := validatingOnlyFirstTarget(t, step.Run)
	step.Run = run
	result := executeGenerationRun(t, step, generationRunOptions{})
	if result.err != nil {
		t.Fatalf("run generation step validating one target: %v\n%s", result.err, result.output)
	}
	verified := verifiedGenerationTargets(t, result)
	if len(verified) != 1 {
		t.Fatalf("skipped validation reported %d validated targets, want 1", len(verified))
	}
	for target := range verified {
		if got := target.GOOS + "-" + target.GOARCH; got != validated {
			t.Fatalf("skipped validation reported %s, want only %s", got, validated)
		}
	}
}

func TestReleaseWorkflowGenerationSwappedValidationFails(t *testing.T) {
	t.Parallel()

	step := releaseWorkflowGenerationStep(t)
	step.Run = swapValidationTargetVariables(t, step.Run)
	result := executeGenerationRun(t, step, generationRunOptions{})
	if result.err == nil {
		t.Fatalf("generation accepted validation against the swapped target\n%s", result.output)
	}
	if verified := verifiedGenerationTargets(t, result); len(verified) != 0 {
		t.Fatalf("swapped validation reported %d validated targets", len(verified))
	}
}

type generationStep struct {
	Run string
	Env map[string]string
}

type generationRunOptions struct {
	failGenerator   bool
	generatorModule string
	generatorCGO    string
}

type generationRunResult struct {
	documents   map[Target][]byte
	assetsDir   string
	invocations []toolInvocation
	output      []byte
	err         error
}

// toolInvocation records one call the release workflow made into the go
// command double, so the tests assert what the executed script did instead of
// how the script spells it.
type toolInvocation struct {
	Tool    string   `json:"tool"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Stdout  string   `json:"stdout,omitempty"`
	Exit    int      `json:"exit"`
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
	requireWorkflowTool(t, "jq")
	releaseTool := productionReleaseTool(t)
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
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+strings.Join([]string{binDir, installDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
		"RUNNER_TEMP="+runnerTemp,
		installDirEnv+"="+installDir,
		invocationLogEnv+"="+invocationLog,
		releaseToolEnv+"="+releaseTool,
		generatorModuleEnv+"="+options.generatorModule,
		generatorCGOEnv+"="+options.generatorCGO,
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
func requireWorkflowTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Fatalf("release workflow tests require %s: %v; install %s and put it on PATH (CONTRIBUTING.md lists the prerequisites)", name, err, name)
	}
}

var (
	releaseToolOnce  sync.Once
	releaseToolDir   string
	releaseToolPath  string
	releaseToolError error
)

// productionReleaseTool builds the shipped release tool once per test binary so
// the workflow's verify-sbom call runs the production command instead of a
// second implementation of its flags and validation.
func productionReleaseTool(t *testing.T) string {
	t.Helper()
	releaseToolOnce.Do(func() {
		dir, err := os.MkdirTemp("", "acr-release-tool")
		if err != nil {
			releaseToolError = fmt.Errorf("create release tool build directory: %w", err)
			return
		}
		releaseToolDir = dir
		path := filepath.Join(dir, "releasetool")
		command := exec.Command("go", "build", "-o", path, "./internal/releasetool")
		command.Dir = filepath.Join("..", "..")
		if output, err := command.CombinedOutput(); err != nil {
			releaseToolError = fmt.Errorf("build ./internal/releasetool: %w\n%s", err, output)
			return
		}
		releaseToolPath = path
	})
	if releaseToolError != nil {
		t.Fatalf("prepare the production release tool: %v", releaseToolError)
	}
	return releaseToolPath
}

func removeReleaseToolBuild() error {
	if releaseToolDir == "" {
		return nil
	}
	return os.RemoveAll(releaseToolDir)
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
		assertGeneratedGraphPreserved(t, target, contents)
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
	assertPinnedGeneratorInstalled(t, result)
	verified := verifiedGenerationTargets(t, result)
	for _, target := range Targets() {
		if _, ok := verified[target]; !ok {
			t.Fatalf("the executed step never validated %s", target.SBOMName())
		}
	}
	if len(verified) != len(Targets()) {
		t.Fatalf("the executed step validated %d targets, want %d", len(verified), len(Targets()))
	}
}

// assertGeneratedGraphPreserved holds the identity rewrite to changing the
// release identity and nothing else: the component graph the generator emitted
// has to survive it.
func assertGeneratedGraphPreserved(t *testing.T, target Target, contents []byte) {
	t.Helper()
	var document struct {
		Components []struct {
			Name string `json:"name"`
		} `json:"components"`
		Dependencies []struct {
			Ref string `json:"ref"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatalf("decode %s graph: %v", target.SBOMName(), err)
	}
	if len(document.Components) != 1 || document.Components[0].Name != generatedDependencyName {
		t.Fatalf("%s components = %#v, want the generated %s entry", target.SBOMName(), document.Components, generatedDependencyName)
	}
	if len(document.Dependencies) != 1 || !strings.Contains(document.Dependencies[0].Ref, generatedModuleName) {
		t.Fatalf("%s dependencies = %#v, want the generated module reference", target.SBOMName(), document.Dependencies)
	}
}

func assertPinnedGeneratorInstalled(t *testing.T, result generationRunResult) {
	t.Helper()
	installs := 0
	for _, invocation := range result.invocations {
		if invocation.Tool != "go" || invocation.Command != "install" {
			continue
		}
		installs++
		if invocation.Exit != 0 {
			t.Fatalf("go install %v exited %d", invocation.Args, invocation.Exit)
		}
	}
	if installs != 1 {
		t.Fatalf("the executed step installed the pinned generator %d times, want 1", installs)
	}
}

// verifiedGenerationTargets reports which targets the executed step actually
// validated. The production release tool only succeeds when the document it was
// handed is the one named for the target it was told to check, so the accepted
// path it reports identifies the pair.
func verifiedGenerationTargets(t *testing.T, result generationRunResult) map[Target]string {
	t.Helper()
	verified := make(map[Target]string)
	for _, invocation := range result.invocations {
		if invocation.Tool != "go" || invocation.Command != "verify-sbom" {
			continue
		}
		if invocation.Exit != 0 {
			continue
		}
		var accepted struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(invocation.Stdout), &accepted); err != nil {
			t.Fatalf("decode verify-sbom result %q: %v", invocation.Stdout, err)
		}
		target, ok := targetBySBOMName(filepath.Base(accepted.Path))
		if !ok {
			t.Fatalf("verify-sbom accepted %q, which is no release target's document", accepted.Path)
		}
		if previous, seen := verified[target]; seen {
			t.Fatalf("verify-sbom accepted %s twice (%s and %s)", target.SBOMName(), previous, accepted.Path)
		}
		verified[target] = accepted.Path
	}
	return verified
}

func targetBySBOMName(name string) (Target, bool) {
	for _, target := range Targets() {
		if target.SBOMName() == name {
			return target, true
		}
	}
	return Target{}, false
}

var generationLoopPattern = regexp.MustCompile(`for (\w+) in ([^;]+); do`)

func generationTargetLoop(t *testing.T, run string) (variable string, targets []string, list [2]int) {
	t.Helper()
	loc := generationLoopPattern.FindStringSubmatchIndex(run)
	if loc == nil {
		t.Fatal("generation step has no target loop")
	}
	variable = run[loc[2]:loc[3]]
	targets = strings.Fields(run[loc[4]:loc[5]])
	if len(targets) != len(Targets()) {
		t.Fatalf("generation loop targets %v, want %d entries", targets, len(Targets()))
	}
	return variable, targets, [2]int{loc[4], loc[5]}
}

func reorderGenerationTargets(t *testing.T, run string) string {
	t.Helper()
	_, targets, list := generationTargetLoop(t, run)
	for i, j := 0, len(targets)-1; i < j; i, j = i+1, j-1 {
		targets[i], targets[j] = targets[j], targets[i]
	}
	return run[:list[0]] + strings.Join(targets, " ") + run[list[1]:]
}

// wrapGenerationJQPrograms parenthesizes every jq program the step runs. A
// parenthesized filter is the same filter, so a jq program the step spells
// differently has to leave the tests green.
func wrapGenerationJQPrograms(t *testing.T, run string) string {
	t.Helper()
	lines := strings.Split(run, "\n")
	wrapped := 0
	for i, line := range lines {
		program, ok := singleQuotedArgument(line)
		if !ok || !runsJQ(lines, i) {
			continue
		}
		lines[i] = strings.Replace(line, "'"+program+"'", "'( "+program+" )'", 1)
		wrapped++
	}
	if wrapped == 0 {
		t.Fatal("generation step runs no quoted jq program; update this equivalence mutation")
	}
	return strings.Join(lines, "\n")
}

func singleQuotedArgument(line string) (string, bool) {
	start := strings.Index(line, "'")
	end := strings.LastIndex(line, "'")
	if start < 0 || end <= start {
		return "", false
	}
	return line[start+1 : end], true
}

func runsJQ(lines []string, index int) bool {
	start := index
	for start > 0 && strings.HasSuffix(strings.TrimSpace(lines[start-1]), `\`) {
		start--
	}
	for _, field := range strings.Fields(lines[start]) {
		if field == "jq" {
			return true
		}
	}
	return false
}

// generationTargetVariables reports the shell variables the loop derives its
// GOOS and GOARCH from, so the mutations below never depend on what those
// variables are called.
func generationTargetVariables(t *testing.T, run string) (string, string) {
	t.Helper()
	loopVariable, _, _ := generationTargetLoop(t, run)
	quoted := regexp.QuoteMeta(loopVariable)
	osVariable := derivedTargetVariable(t, run, `(?m)^\s*(\w+)="\$\{`+quoted+`%-\*\}"`)
	archVariable := derivedTargetVariable(t, run, `(?m)^\s*(\w+)="\$\{`+quoted+`#\*-\}"`)
	return osVariable, archVariable
}

func derivedTargetVariable(t *testing.T, run, pattern string) string {
	t.Helper()
	match := regexp.MustCompile(pattern).FindStringSubmatch(run)
	if match == nil {
		t.Fatalf("generation step derives no variable matching %s; update these mutations", pattern)
	}
	return match[1]
}

// renameGenerationTargetVariables renames the loop's derived shell variables
// consistently, which changes no behaviour and therefore must not fail.
func renameGenerationTargetVariables(t *testing.T, run string) string {
	t.Helper()
	osVariable, archVariable := generationTargetVariables(t, run)
	renamed := run
	for _, variable := range []string{osVariable, archVariable} {
		assignment := regexp.MustCompile(`(?m)^(\s*)` + regexp.QuoteMeta(variable) + `=`)
		renamed = assignment.ReplaceAllString(renamed, "${1}"+variable+"_renamed=")
		renamed = strings.ReplaceAll(renamed, "${"+variable+"}", "${"+variable+"_renamed}")
		if strings.Contains(renamed, "${"+variable+"}") {
			t.Fatalf("rename left ${%s} in place:\n%s", variable, renamed)
		}
	}
	if renamed == run {
		t.Fatal("generation step references no derived target variables; update this rename mutation")
	}
	return renamed
}

func withoutGeneratorInstall(t *testing.T, run string) string {
	t.Helper()
	lines := strings.Split(run, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "go install ") {
			continue
		}
		return strings.Join(append(append([]string(nil), lines[:i]...), lines[i+1:]...), "\n")
	}
	t.Fatal("generation step does not install the generator")
	return ""
}

func locateTargetValidation(t *testing.T, lines []string) (int, int) {
	t.Helper()
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "go run ") || !strings.Contains(trimmed, "verify-sbom") {
			continue
		}
		end := i
		for end+1 < len(lines) && strings.HasSuffix(strings.TrimSpace(lines[end]), `\`) {
			end++
		}
		return i, end
	}
	t.Fatal("generation step does not validate the generated documents")
	return 0, 0
}

func withoutTargetValidation(t *testing.T, run string) string {
	t.Helper()
	lines := strings.Split(run, "\n")
	start, end := locateTargetValidation(t, lines)
	return strings.Join(append(append([]string(nil), lines[:start]...), lines[end+1:]...), "\n")
}

func validatingOnlyFirstTarget(t *testing.T, run string) (string, string) {
	t.Helper()
	variable, targets, _ := generationTargetLoop(t, run)
	lines := strings.Split(run, "\n")
	start, end := locateTargetValidation(t, lines)
	guarded := make([]string, 0, len(lines)+2)
	guarded = append(guarded, lines[:start]...)
	guarded = append(guarded, fmt.Sprintf(`if [ "${%s}" = "%s" ]; then`, variable, targets[0]))
	guarded = append(guarded, lines[start:end+1]...)
	guarded = append(guarded, "fi")
	guarded = append(guarded, lines[end+1:]...)
	return strings.Join(guarded, "\n"), targets[0]
}

func swapValidationTargetVariables(t *testing.T, run string) string {
	t.Helper()
	lines := strings.Split(run, "\n")
	start, end := locateTargetValidation(t, lines)
	block := strings.Join(lines[start:end+1], "\n")
	osVariable, archVariable := generationTargetVariables(t, run)
	swapped := strings.NewReplacer("${"+osVariable+"}", "${"+archVariable+"}", "${"+archVariable+"}", "${"+osVariable+"}").Replace(block)
	if swapped == block {
		t.Fatalf("validation call passes neither ${%s} nor ${%s}; update this swap mutation", osVariable, archVariable)
	}
	return strings.Join(lines[:start], "\n") + "\n" + swapped + "\n" + strings.Join(lines[end+1:], "\n")
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
	if os.Getenv(generatorFailEnv) == "1" {
		fmt.Fprintln(os.Stderr, "cyclonedx-gomod failed: generator error")
		return 1
	}
	goos := os.Getenv("GOOS")
	goarch := os.Getenv("GOARCH")
	if goos == "" || goarch == "" {
		fmt.Fprintln(os.Stderr, "cyclonedx-gomod requires GOOS and GOARCH")
		return 1
	}
	cgo := os.Getenv("CGO_ENABLED")
	if override := os.Getenv(generatorCGOEnv); override != "" {
		cgo = override
	}
	module := os.Getenv(generatorModuleEnv)
	if module == "" {
		module = generatedModuleName
	}
	output := ""
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		if args[i] != "-output" {
			continue
		}
		if i+1 >= len(args) {
			fmt.Fprintln(os.Stderr, "cyclonedx-gomod -output requires a path")
			return 1
		}
		output = args[i+1]
		break
	}
	if output == "" {
		fmt.Fprintln(os.Stderr, "cyclonedx-gomod requires -output")
		return 1
	}
	reference := fmt.Sprintf("pkg:golang/%s@dev?goos=%s&goarch=%s&type=module#cmd/acr", module, goos, goarch)
	document := map[string]any{
		"bomFormat":   "CycloneDX",
		"specVersion": "1.6",
		"metadata": map[string]any{
			"component": map[string]any{
				"name":    module,
				"version": "dev",
				"purl":    reference,
				"properties": []map[string]string{
					{"name": cyclonedxPropertyCGO, "value": cgo},
					{"name": cyclonedxPropertyGOOS, "value": goos},
					{"name": cyclonedxPropertyGOARCH, "value": goarch},
				},
			},
		},
		"components": []map[string]string{
			{"type": "library", "name": generatedDependencyName, "version": "v1.0.0"},
		},
		"dependencies": []map[string]string{
			{"ref": reference},
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

func runGoDouble() int {
	args := os.Args[1:]
	command, stdout, code := goDoubleResult(args)
	if err := recordToolInvocation(toolInvocation{Tool: "go", Command: command, Args: args, Stdout: stdout, Exit: code}); err != nil {
		fmt.Fprintf(os.Stderr, "record go invocation: %v\n", err)
		return 1
	}
	if _, err := os.Stdout.WriteString(stdout); err != nil {
		fmt.Fprintf(os.Stderr, "write go output: %v\n", err)
		return 1
	}
	return code
}

func goDoubleResult(args []string) (string, string, int) {
	switch {
	case len(args) == 2 && args[0] == "install":
		return "install", "", installGeneratorDouble(args[1])
	case len(args) >= 3 && args[0] == "run":
		stdout, code := runProductionReleaseTool(args[2:])
		return args[2], stdout, code
	default:
		fmt.Fprintf(os.Stderr, "unexpected go command: %v\n", args)
		return "", "", 1
	}
}

// installGeneratorDouble stands in for `go install`: it places the generator
// double where the workflow expects the installed binary, so a step that never
// installs the generator cannot run it either.
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

// runProductionReleaseTool executes the shipped release tool so the workflow
// test exercises production flag parsing and validation rather than a copy.
func runProductionReleaseTool(args []string) (string, int) {
	binary := os.Getenv(releaseToolEnv)
	if binary == "" {
		fmt.Fprintf(os.Stderr, "%s is unset; the workflow test must name the built release tool\n", releaseToolEnv)
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
		fmt.Fprintf(os.Stderr, "run the release tool: %v\n", err)
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
