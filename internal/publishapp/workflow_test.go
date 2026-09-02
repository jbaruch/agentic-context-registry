package publishapp

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestPublishWorkflowContract(t *testing.T) {
	t.Parallel()

	filename := filepath.Join("..", "..", ".github", "workflows", "publish-package.yml")
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On struct {
			WorkflowCall struct {
				Inputs map[string]struct {
					Description string `yaml:"description"`
					Type        string `yaml:"type"`
					Required    bool   `yaml:"required"`
					Default     any    `yaml:"default"`
				} `yaml:"inputs"`
			} `yaml:"workflow_call"`
		} `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Steps []struct {
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		t.Fatal(err)
	}
	inputs := workflow.On.WorkflowCall.Inputs
	if len(inputs) != 3 || inputs["path"].Type != "string" || inputs["path"].Default != "." || inputs["dry-run"].Type != "boolean" || inputs["dry-run"].Default != false || inputs["acr-version"].Type != "string" {
		t.Fatalf("workflow inputs = %#v", inputs)
	}
	if !strings.Contains(inputs["dry-run"].Description, "tagged release") {
		t.Fatalf("dry-run description = %q, want tag-triggered scope", inputs["dry-run"].Description)
	}
	version, ok := inputs["acr-version"].Default.(string)
	if !ok || version == "" || version == "main" || version == "latest" {
		t.Fatalf("acr-version default = %#v, want immutable literal tag", inputs["acr-version"].Default)
	}
	if len(workflow.Permissions) != 1 || workflow.Permissions["contents"] != "write" {
		t.Fatalf("permissions = %#v", workflow.Permissions)
	}
	job, ok := workflow.Jobs["publish"]
	if !ok || len(job.Steps) < 4 {
		t.Fatalf("publish job = %#v", job)
	}
	if !strings.Contains(job.Steps[0].Run, `GITHUB_REF_TYPE`) || !strings.Contains(job.Steps[0].Run, `!= "tag"`) {
		t.Fatalf("first step does not reject untagged refs: %q", job.Steps[0].Run)
	}
	pinnedAction := regexp.MustCompile(`^[^@]+@[0-9a-f]{40}$`)
	foundPublish := false
	for _, step := range job.Steps {
		if step.Uses != "" && !pinnedAction.MatchString(step.Uses) {
			t.Errorf("workflow action is not SHA-pinned: %q", step.Uses)
		}
		if strings.Contains(step.Run, "acr publish") {
			foundPublish = true
			if step.Env["GH_TOKEN"] == "" || step.Env["PACKAGE_PATH"] == "" {
				t.Errorf("publish environment = %#v", step.Env)
			}
		}
	}
	if !foundPublish {
		t.Fatal("workflow does not invoke acr publish")
	}
	raw := string(contents)
	if strings.Contains(raw, "pull_request:") || strings.Contains(raw, "pull_request_target:") || strings.Contains(raw, "secrets:") {
		t.Fatal("workflow exposes a forbidden trigger or secrets input")
	}
}
