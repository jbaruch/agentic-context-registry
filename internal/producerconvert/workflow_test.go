package producerconvert

import (
	"strings"
	"testing"
)

func TestStandalonePublishAndIndependentJobs(t *testing.T) {
	out, err := translateWorkflow([]byte(fixturePublisher), "plugins/orbit")
	if err != nil || out != nil {
		t.Fatalf("standalone publisher=%s %v", out, err)
	}
	for _, source := range []string{fixturePublisher + independentTestJob, strings.Replace(fixturePublisher, "  publish:", independentTestJob+"  publish:", 1)} {
		out, err := translateWorkflow([]byte(source), "plugins/orbit")
		if err != nil || !strings.Contains(string(out), independentTestJob) || strings.Contains(string(out), "patch-version-publish") {
			t.Fatalf("independent jobs=%s %v", out, err)
		}
	}
	comment := "\n  # Independent tests retain this explanation.\n"
	commented, err := translateWorkflow([]byte(fixturePublisher+comment+independentTestJob), "plugins/orbit")
	if err != nil || !strings.Contains(string(commented), comment+independentTestJob) {
		t.Fatalf("lost independent job comment: %s %v", commented, err)
	}
	if !strings.Contains(publishWorkflow, "push:\n    tags: ['v*']") || !strings.Contains(publishWorkflow, "path: .") || !strings.Contains(publishWorkflow, "acr-version: v0.1.6") || !strings.Contains(publishWorkflow, "@d3bc96b33b42293aecd1702c04aa94513a3dab1b") {
		t.Fatal("generated publication contract lost explicit pin, version or path")
	}
}

func TestWorkflowUnknownLogicRefuses(t *testing.T) {
	cases := map[string]string{
		"review":           strings.Replace(fixturePublisher, "tesslio/patch-version-publish@v1", "tesslio/skill-review@v1", 1),
		"trigger":          strings.Replace(fixturePublisher, "branches: [main]", "branches: [release]", 1),
		"path":             strings.Replace(fixturePublisher, "path: plugins/orbit", "path: ${{ inputs.path }}", 1),
		"checkout-options": strings.Replace(fixturePublisher, "      - uses: actions/checkout@v4", "      - uses: actions/checkout@v4\n        with:\n          ref: main", 1),
		"permission":       strings.Replace(fixturePublisher, "  contents: write", "  id-token: write", 1),
		"extra-field":      strings.Replace(fixturePublisher, "  publish:\n", "  publish:\n    if: github.actor == 'robot'\n", 1),
		"needs":            fixturePublisher + strings.Replace(independentTestJob, "  test:\n", "  test:\n    needs: publish\n", 1),
		"alias":            strings.Replace(fixturePublisher, "  publish:", "  publish: &publication", 1),
		"duplicate":        strings.Replace(fixturePublisher, "    runs-on: ubuntu-latest", "    runs-on: ubuntu-latest\n    runs-on: other", 1),
		"extra-step":       fixturePublisher + "      - run: ./policy.sh\n",
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := translateWorkflow([]byte(source), "plugins/orbit"); err == nil {
				t.Fatal("unknown logic accepted")
			}
		})
	}
}

func TestCorrectionPublisherIdentity(t *testing.T) {
	historical := strings.Replace(independentTestJob, "./tests/run.sh", "curl https://github.com/tessl-labs/research && ./tests/run.sh", 1)
	source := fixturePublisher + historical
	out, err := translateWorkflow([]byte(source), "plugins/orbit")
	expected := strings.Split(fixturePublisher, "  publish:\n")[0] + historical
	if err != nil || string(out) != expected {
		t.Fatalf("historical independent job: %s %v", out, err)
	}
	for _, other := range []string{strings.Replace(fixturePublisher, "  publish:", "  second:", 1), strings.Replace(fixturePublisher, "tesslio/patch-version-publish@v1", "custom/patch-version-publish@v1", 1)} {
		if _, err := translateWorkflow([]byte(other), "plugins/orbit"); err == nil && strings.Contains(other, "custom/") {
			t.Fatal("lookalike publisher accepted")
		}
	}
	both := fixturePublisher + strings.Split(strings.Replace(fixturePublisher, "  publish:", "  second:", 1), "jobs:\n")[1]
	if _, err := translateWorkflow([]byte(both), "plugins/orbit"); err == nil {
		t.Fatal("multiple publishers accepted")
	}
}

func TestCorrectionPublisherRetainedOutputReference(t *testing.T) {
	consumer := strings.Replace(independentTestJob, "./tests/run.sh", "echo '${{ needs.publish.outputs.version }}'", 1)
	if _, err := translateWorkflow([]byte(fixturePublisher+consumer), "plugins/orbit"); err == nil {
		t.Fatal("retired publisher output still referenced")
	}
}
