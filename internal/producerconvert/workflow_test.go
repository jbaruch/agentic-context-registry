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

func TestPublisherJobNameRequiresString(t *testing.T) {
	for _, name := range []string{"123", "true", "null", "[Publish]", "{title: Publish}", `"Publish"`, `!!str 123`} {
		for _, independent := range []string{"", independentTestJob} {
			source := strings.Replace(fixturePublisher, "  publish:\n", "  publish:\n    name: "+name+"\n", 1) + independent
			out, err := translateWorkflow([]byte(source), "plugins/orbit")
			allowed := independent == "" || name == `"Publish"` || name == `!!str 123`
			if allowed {
				if err != nil || independent != "" && !strings.Contains(string(out), independent) {
					t.Fatalf("name=%s independent=%t: %s %v", name, independent != "", out, err)
				}
			} else if err == nil || err.Error() != "publisher job name must be a string; the file requires manual migration" {
				t.Fatalf("name=%s independent=%t: expected manual refusal, got %v", name, independent != "", err)
			}
		}
	}
}

func TestWorkflowManualMigrationReasons(t *testing.T) {
	for _, tc := range []struct{ name, source, want string }{
		{"multiple", fixturePublisher + strings.Split(strings.Replace(fixturePublisher, "  publish:", "  second:", 1), "jobs:\n")[1], "multiple Tessl publishers require manual migration"},
		{"custom-policy", strings.Replace(fixturePublisher, "  publish:\n", "  publish:\n    if: github.actor == 'robot'\n", 1), "mixed or unknown Tessl job; review and custom logic require manual migration"},
		{"dependencies", fixturePublisher + strings.Replace(independentTestJob, "  test:\n", "  test:\n    needs: other\n", 1), "job test has dependencies; the file requires manual migration to preserve its policy"},
		{"alias", strings.Replace(fixturePublisher, "  publish:", "  publish: &publication", 1), "YAML aliases and anchors require manual migration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := translateWorkflow([]byte(tc.source), "plugins/orbit")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error=%v; want %q", err, tc.want)
			}
		})
	}
}

func TestWorkflowFirstDependencyReason(t *testing.T) {
	for _, publisher := range []string{fixturePublisher, strings.Replace(fixturePublisher, "  publish:\n", "  publish:\n    name: 123\n", 1)} {
		data := publisher + "  one:\n    needs: publish\n    runs-on: ubuntu-latest\n    steps: []\n  two:\n    needs: publish\n    runs-on: ubuntu-latest\n    steps: []\n"
		_, err := translateWorkflow([]byte(data), "plugins/orbit")
		const want = "job one has dependencies; the file requires manual migration to preserve its policy"
		if err == nil || err.Error() != want {
			t.Fatalf("error=%v; want %s", err, want)
		}
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

func TestDeterministicPublisherKeepsReferencedJobs(t *testing.T) {
	for _, expression := range []string{
		"needs.publish.outputs.version",
		"needs['publish'].outputs.version",
		"needs[inputs.job].outputs.version",
		"toJSON(needs)",
		"jobs.publish.outputs.version",
	} {
		t.Run(expression, func(t *testing.T) {
			consumer := strings.Replace(independentTestJob, "./tests/run.sh", "echo \"${{ "+expression+" }}\"", 1)
			if _, err := translateWorkflow([]byte(fixturePublisher+consumer), "plugins/orbit"); err == nil {
				t.Fatal("referenced publisher was retired")
			}
		})
	}
	consumer := strings.Replace(independentTestJob, "./tests/run.sh", "echo \"${{ 'needs.publish is historical' }}\"", 1)
	output, err := translateWorkflow([]byte(fixturePublisher+consumer), "plugins/orbit")
	want := strings.Split(fixturePublisher, "  publish:\n")[0] + consumer
	if err != nil || string(output) != want {
		t.Fatalf("historical expression must remain byte-identical: %q, %v", output, err)
	}
}
