package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The tests below run the release workflow's own target-verification step and
// assert what running it did. They never read how the step is written, so any
// expression enforcing the same identity and target constraints passes and any
// expression that stops enforcing them fails.

const (
	verifyStepJob    = "verify"
	verifyStepName   = "Verify, extract, and execute the target asset"
	verifyStepTag    = "v" + generationTestVersion
	verifyStepCommit = "0123456789abcdef0123456789abcdef01234567"
)

func TestReleaseWorkflowVerifyStepAcceptsOnlyItsOwnTargetSBOM(t *testing.T) {
	t.Parallel()

	assertVerifyStepPairsTargetsWithDocuments(t, releaseWorkflowStep(t, verifyStepJob, verifyStepName).Run)
}

// TestReleaseWorkflowVerifyStepAcceptsAnEquivalentPropertyExpression runs the
// same matrix against an authored script that builds the two target property
// names by concatenation instead of spelling them out. Identical verdicts show
// the coverage above reads the step's outcomes, not its wording.
func TestReleaseWorkflowVerifyStepAcceptsAnEquivalentPropertyExpression(t *testing.T) {
	t.Parallel()

	assertVerifyStepPairsTargetsWithDocuments(t, equivalentVerifyScript)
}

// assertVerifyStepPairsTargetsWithDocuments requires the verification script to
// accept each target's own document and refuse all three others — the pairing
// #41 exists for.
func assertVerifyStepPairsTargetsWithDocuments(t *testing.T, script string) {
	t.Helper()

	for _, target := range Targets() {
		for _, other := range Targets() {
			target, other := target, other
			t.Run(fmt.Sprintf("%s-%s/verifies-%s-%s", other.GOOS, other.GOARCH, target.GOOS, target.GOARCH), func(t *testing.T) {
				t.Parallel()

				assets := buildVerifyStepAssets(t, target, verifyStepFixture{document: verifyStepSBOM(verifyStepDocument{target: other})})
				result := runVerifyStep(t, script, assets, target)
				if other == target {
					if result.err != nil {
						t.Fatalf("the step refused %s: %v\n%s", target.SBOMName(), result.err, result.output)
					}
					if err := ValidateSBOM(assets.document, generationTestVersion, target); err != nil {
						t.Fatalf("the accepted document fails the production validator: %v", err)
					}
					assertVerifyStepExecutedTheArchive(t, assets)
					return
				}
				if result.err == nil {
					t.Fatalf("the step accepted the %s document as %s\n%s", other.SBOMName(), target.SBOMName(), result.output)
				}
				assertVerifyStepNamed(t, result, fmt.Sprintf("does not describe %s/%s acr %s", target.GOOS, target.GOARCH, generationTestVersion))
			})
		}
	}
}

// TestReleaseWorkflowVerifyStepRejectsInvalidTargetAssets varies one property of
// one release-assets directory at a time and requires the executed step to
// refuse it, naming the cause.
func TestReleaseWorkflowVerifyStepRejectsInvalidTargetAssets(t *testing.T) {
	t.Parallel()

	step := releaseWorkflowStep(t, verifyStepJob, verifyStepName).Run
	target := Target{GOOS: "linux", GOARCH: "amd64"}
	for _, testCase := range []struct {
		name          string
		fixture       verifyStepFixture
		wantRejection string
		why           string
	}{
		{
			name:          "no-sbom-for-this-target",
			fixture:       verifyStepFixture{},
			wantRejection: "Release candidate is missing acr-linux-amd64.cdx.json",
			why:           "a target without its own document must not be published",
		},
		{
			name:          "goos-property-names-another-platform",
			fixture:       verifyStepFixture{document: verifyStepSBOM(verifyStepDocument{target: target, goos: "windows"})},
			wantRejection: "does not describe linux/amd64",
			why:           "the recorded build platform is the constraint #41 adds",
		},
		{
			name:          "goarch-property-names-another-architecture",
			fixture:       verifyStepFixture{document: verifyStepSBOM(verifyStepDocument{target: target, goarch: "arm64"})},
			wantRejection: "does not describe linux/amd64",
			why:           "the recorded architecture is the other half of that constraint",
		},
		{
			name:          "goos-property-is-absent",
			fixture:       verifyStepFixture{document: verifyStepSBOM(verifyStepDocument{target: target, dropGOOS: true})},
			wantRejection: "does not describe linux/amd64",
			why:           "a document that records no platform proves nothing about one",
		},
		{
			name:          "goarch-property-is-absent",
			fixture:       verifyStepFixture{document: verifyStepSBOM(verifyStepDocument{target: target, dropGOARCH: true})},
			wantRejection: "does not describe linux/amd64",
			why:           "a document that records no architecture proves nothing about one",
		},
		{
			name:          "application-identity-was-never-rewritten",
			fixture:       verifyStepFixture{document: verifyStepSBOM(verifyStepDocument{target: target, name: generatedModuleName})},
			wantRejection: "does not describe linux/amd64",
			why:           "the published document describes acr, not the module it was generated from",
		},
		{
			name:          "version-belongs-to-another-release",
			fixture:       verifyStepFixture{document: verifyStepSBOM(verifyStepDocument{target: target, version: "9.9.9"})},
			wantRejection: "does not describe linux/amd64",
			why:           "a document stamped with another version must be refused",
		},
		{
			name: "archive-does-not-match-the-checksum-manifest",
			fixture: verifyStepFixture{
				document:       verifyStepSBOM(verifyStepDocument{target: target}),
				corruptArchive: true,
			},
			wantRejection: "acr-linux-amd64.tar.gz: FAILED",
			why:           "the checksum gate runs before anything is extracted",
		},
		{
			name: "extracted-binary-reports-another-version",
			fixture: verifyStepFixture{
				document:      verifyStepSBOM(verifyStepDocument{target: target}),
				binaryVersion: "9.9.9",
			},
			wantRejection: "Published candidate reports 9.9.9",
			why:           "the step executes the archived binary and reads what it reports",
		},
		{
			name: "extracted-binary-reports-another-commit",
			fixture: verifyStepFixture{
				document:     verifyStepSBOM(verifyStepDocument{target: target}),
				binaryCommit: "89abcdef0123456789abcdef0123456789abcdef",
			},
			wantRejection: "89abcdef0123456789abcdef0123456789abcdef",
			why:           "the archived binary has to carry the tagged commit",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			assets := buildVerifyStepAssets(t, target, testCase.fixture)
			result := runVerifyStep(t, step, assets, target)
			if result.err == nil {
				t.Fatalf("%s: the step accepted these assets\n%s", testCase.why, result.output)
			}
			assertVerifyStepNamed(t, result, testCase.wantRejection)
		})
	}
}

func assertVerifyStepNamed(t *testing.T, result verifyStepResult, want string) {
	t.Helper()
	if !strings.Contains(string(result.output), want) {
		t.Fatalf("the step's rejection does not name %q\n%s", want, result.output)
	}
}

// assertVerifyStepExecutedTheArchive proves an accepted run got all the way
// through: it unpacked the archive and ran the binary it contains.
func assertVerifyStepExecutedTheArchive(t *testing.T, assets verifyStepAssets) {
	t.Helper()
	reported, err := os.ReadFile(filepath.Join(assets.assetsDir, "version.json"))
	if err != nil {
		t.Fatalf("the accepted run never recorded what the extracted binary reported: %v", err)
	}
	var version struct {
		Result struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
		} `json:"result"`
	}
	if err := json.Unmarshal(reported, &version); err != nil {
		t.Fatalf("decode the reported version: %v", err)
	}
	if version.Result.Version != generationTestVersion || version.Result.Commit != verifyStepCommit {
		t.Fatalf("the extracted binary reported %q %q, want %q %q", version.Result.Version, version.Result.Commit, generationTestVersion, verifyStepCommit)
	}
}

// verifyStepFixture describes one release-assets directory: the four archives
// the pack step produces, their checksum manifest, and the document under test.
type verifyStepFixture struct {
	// document is filed under the verified target's SBOM name, which is the only
	// name that job looks for. Nil leaves the target without one.
	document []byte
	// binaryVersion and binaryCommit are what the archived executable reports.
	// Empty means the values this release expects.
	binaryVersion  string
	binaryCommit   string
	corruptArchive bool
}

type verifyStepAssets struct {
	root      string
	assetsDir string
	document  []byte
}

func buildVerifyStepAssets(t *testing.T, verified Target, fixture verifyStepFixture) verifyStepAssets {
	t.Helper()
	root := t.TempDir()
	assetsDir := filepath.Join(root, "release-assets")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	version := fixture.binaryVersion
	if version == "" {
		version = generationTestVersion
	}
	commit := fixture.binaryCommit
	if commit == "" {
		commit = verifyStepCommit
	}
	binaries := make([]Binary, 0, len(Targets()))
	for _, target := range Targets() {
		binaries = append(binaries, Binary{Target: target, Bytes: []byte(verifyStepBinaryScript(version, commit))})
	}
	bundle, err := Pack(binaries, []byte("fixture license\n"))
	if err != nil {
		t.Fatalf("pack the release fixture: %v", err)
	}
	for _, asset := range append(append([]Asset(nil), bundle.Archives...), bundle.Checksums) {
		contents := asset.Bytes
		if fixture.corruptArchive && asset.Name == verified.Name() {
			contents = append(append([]byte(nil), contents...), 'x')
		}
		if err := os.WriteFile(filepath.Join(assetsDir, asset.Name), contents, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.document != nil {
		if err := os.WriteFile(filepath.Join(assetsDir, verified.SBOMName()), fixture.document, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return verifyStepAssets{root: root, assetsDir: assetsDir, document: fixture.document}
}

// verifyStepBinaryScript stands in for the packaged acr executable. The hosted
// verify matrix runs the real cross-compiled binary on a runner of that
// platform; this fixture reports fixed values so the step's extract-and-execute
// behaviour can be observed for every target from one host.
func verifyStepBinaryScript(version, commit string) string {
	return "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		`[[ "$1" == "version" && "$2" == "--json" ]]` + "\n" +
		fmt.Sprintf("printf '{\"result\":{\"version\":\"%s\",\"commit\":\"%s\"}}\\n'\n", version, commit)
}

// verifyStepDocument describes one released SBOM. The zero value of each field
// means "what this release expects", so a case names only what it varies.
type verifyStepDocument struct {
	target     Target
	name       string
	version    string
	goos       string
	goarch     string
	dropGOOS   bool
	dropGOARCH bool
}

func verifyStepSBOM(specification verifyStepDocument) []byte {
	name := specification.name
	if name == "" {
		name = "acr"
	}
	version := specification.version
	if version == "" {
		version = generationTestVersion
	}
	goos := specification.goos
	if goos == "" {
		goos = specification.target.GOOS
	}
	goarch := specification.goarch
	if goarch == "" {
		goarch = specification.target.GOARCH
	}
	properties := []map[string]string{{"name": cyclonedxPropertyCGO, "value": "0"}}
	if !specification.dropGOOS {
		properties = append(properties, map[string]string{"name": cyclonedxPropertyGOOS, "value": goos})
	}
	if !specification.dropGOARCH {
		properties = append(properties, map[string]string{"name": cyclonedxPropertyGOARCH, "value": goarch})
	}
	reference := fmt.Sprintf("pkg:golang/%s@dev?goos=%s&goarch=%s&type=module#cmd/acr", generatedModuleName, goos, goarch)
	document := map[string]any{
		"bomFormat":   "CycloneDX",
		"specVersion": "1.6",
		"metadata": map[string]any{
			"component": map[string]any{
				"name":       name,
				"version":    version,
				"purl":       reference,
				"properties": properties,
			},
		},
		"components":   []map[string]string{{"type": "library", "name": generatedDependencyName, "version": "v1.0.0"}},
		"dependencies": []map[string]string{{"ref": reference}},
	}
	contents, err := json.Marshal(document)
	if err != nil {
		panic(fmt.Sprintf("encode the SBOM fixture: %v", err))
	}
	return contents
}

type verifyStepResult struct {
	output []byte
	err    error
}

// runVerifyStep executes one verification script the way the verify job does:
// from the directory holding release-assets, with the target's environment, real
// jq and real tar, and a controlled sha256 checker so the checksum gate behaves
// the same on every host.
func runVerifyStep(t *testing.T, script string, assets verifyStepAssets, target Target) verifyStepResult {
	t.Helper()
	requireWorkflowTool(t, "jq")
	requireWorkflowTool(t, "tar")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(assets.root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shasum", "sha256sum"} {
		writeWorkflowTestCommand(t, binDir, name, commandDoubleScript(checksumDoubleKind, executable))
	}
	command := exec.Command("bash", "-c", script)
	command.Dir = assets.root
	command.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GOOS="+target.GOOS,
		"GOARCH="+target.GOARCH,
		"VERSION="+generationTestVersion,
		"TAG="+verifyStepTag,
		"COMMIT="+verifyStepCommit,
	)
	output, err := command.CombinedOutput()
	return verifyStepResult{output: output, err: err}
}

// runChecksumDouble implements the sha256 manifest check both branches of the
// verify step call. It compares real SHA-256 digests; standing in for shasum and
// sha256sum keeps the gate's outcome identical on every host instead of
// depending on which checksum tools happen to be installed.
func runChecksumDouble() int {
	manifest := ""
	arguments := os.Args[1:]
	for index := 0; index < len(arguments); index++ {
		switch argument := arguments[index]; argument {
		case "-a":
			if index+1 >= len(arguments) || arguments[index+1] != "256" {
				fmt.Fprintf(os.Stderr, "this checksum double implements SHA-256 only, got %v\n", arguments)
				return 1
			}
			index++
		case "-c":
			if index+1 >= len(arguments) {
				fmt.Fprintln(os.Stderr, "-c requires a checksum manifest")
				return 1
			}
			index++
			manifest = arguments[index]
		default:
			fmt.Fprintf(os.Stderr, "unsupported checksum argument %q\n", argument)
			return 1
		}
	}
	if manifest == "" {
		fmt.Fprintln(os.Stderr, "no checksum manifest to check")
		return 1
	}
	contents, err := os.ReadFile(manifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read checksum manifest %s: %v\n", manifest, err)
		return 1
	}
	status := 0
	for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
		if line == "" {
			continue
		}
		digest, name, separated := strings.Cut(line, "  ")
		if !separated {
			fmt.Fprintf(os.Stderr, "malformed checksum line %q\n", line)
			return 1
		}
		payload, readErr := os.ReadFile(name)
		if readErr != nil {
			fmt.Fprintf(os.Stderr, "%s: FAILED open or read\n", name)
			status = 1
			continue
		}
		sum := sha256.Sum256(payload)
		if hex.EncodeToString(sum[:]) != digest {
			fmt.Printf("%s: FAILED\n", name)
			status = 1
			continue
		}
		fmt.Printf("%s: OK\n", name)
	}
	return status
}

// equivalentVerifyScript is authored test input: the same verification with the
// two target property names built by concatenation. It exists so the coverage
// above can be shown to judge outcomes rather than wording.
const equivalentVerifyScript = `set -euo pipefail
cd release-assets
if [[ "${GOOS}" == "darwin" ]]; then
  shasum -a 256 -c checksums.txt
else
  sha256sum -c checksums.txt
fi
sbom="acr-${GOOS}-${GOARCH}.cdx.json"
if [[ ! -f "${sbom}" ]]; then
  echo "Release candidate is missing ${sbom}; generate one SBOM per target and retry." >&2
  exit 1
fi
if ! jq -e --arg goos "${GOOS}" --arg goarch "${GOARCH}" --arg version "${VERSION}" \
  --arg prefix "cdx:gomod:build:env:" \
  '.metadata.component.name == "acr"
   and .metadata.component.version == $version
   and any(.metadata.component.properties[]; .name == ($prefix + "GOOS") and .value == $goos)
   and any(.metadata.component.properties[]; .name == ($prefix + "GOARCH") and .value == $goarch)' \
  "${sbom}" > /dev/null; then
  echo "${sbom} does not describe ${GOOS}/${GOARCH} acr ${VERSION}; regenerate the matching target SBOM." >&2
  exit 1
fi
mkdir extracted
tar -xzf "acr-${GOOS}-${GOARCH}.tar.gz" -C extracted
"${PWD}/extracted/acr" version --json > version.json
reported_version="$(jq -r '.result.version' version.json)"
reported_commit="$(jq -r '.result.commit' version.json)"
if [[ "v${reported_version}" != "${TAG}" || "${reported_commit}" != "${COMMIT}" ]]; then
  echo "Published candidate reports ${reported_version} (${reported_commit}); expected ${TAG} (${COMMIT})." >&2
  exit 1
fi
`
