package publishapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/publish"
	"go.yaml.in/yaml/v3"
)

func TestHostileExistingReleaseFailsSafe(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	tests := []struct {
		name     string
		existing dependency.Release
		want     string
	}{
		{name: "published", existing: dependency.Release{ID: 1, Tag: prepared.Identity.Tag}, want: publish.CodeReleaseExists},
		{name: "prerelease", existing: dependency.Release{ID: 1, Tag: prepared.Identity.Tag, Prerelease: true}, want: publish.CodeReleaseExists},
		{name: "foreignDraft", existing: dependency.Release{ID: 1, Tag: prepared.Identity.Tag, Draft: true, Assets: []dependency.ReleaseAsset{{Name: "notes.txt"}}}, want: publish.CodeForeignDraft},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			remote := &fakeReleases{existing: test.existing, exists: true, tagCommit: prepared.Identity.Commit, tagExists: true}
			_, err := NewService(fakePreparer{prepared: prepared}, remote).Publish(context.Background(), ".", false)
			assertPublishCode(t, err, test.want)
			if remote.createCalls != 0 || remote.uploadCalls != 0 || remote.deleteCalls != 0 || remote.publishCalls != 0 {
				t.Fatalf("%s wrote to GitHub: create %d upload %d delete %d publish %d", test.name, remote.createCalls, remote.uploadCalls, remote.deleteCalls, remote.publishCalls)
			}
		})
	}
}

func TestHostileLostCreateRaceUploadsNothing(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{
		tagCommit: prepared.Identity.Commit, tagExists: true,
		createErr: &dependency.GitHubAPIError{StatusCode: http.StatusUnprocessableEntity, Message: "already exists"},
	}
	_, err := NewService(fakePreparer{prepared: prepared}, remote).Publish(context.Background(), ".", false)
	assertPublishCode(t, err, publish.CodeReleaseExists)
	if remote.createCalls != 1 || remote.uploadCalls != 0 || remote.publishCalls != 0 || remote.deleteCalls != 0 {
		t.Fatalf("422 race writes = create %d upload %d publish %d delete %d", remote.createCalls, remote.uploadCalls, remote.publishCalls, remote.deleteCalls)
	}
}

func TestHostilePartialUploadStaysDraft(t *testing.T) {
	t.Parallel()

	prepared := fixturePrepared(t)
	remote := &fakeReleases{tagCommit: prepared.Identity.Commit, tagExists: true, uploadErrorAt: 2}
	_, err := NewService(fakePreparer{prepared: prepared}, remote).Publish(context.Background(), ".", false)
	assertPublishCode(t, err, publish.CodeReleaseUpload)
	if remote.publishCalls != 0 || !remote.draft {
		t.Fatalf("partial upload became visible: publish %d draft %t", remote.publishCalls, remote.draft)
	}
	if remote.createCalls != 1 || remote.uploadCalls != 2 {
		t.Fatalf("partial upload writes = create %d upload %d", remote.createCalls, remote.uploadCalls)
	}
}

func TestHostileMissingOrEscapingFileUploadsNothing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rulePath string
		files    map[string]string
		want     manifest.ErrorCode
	}{
		{name: "missing", rulePath: "rules/missing.md", want: manifest.CodePathNotFound},
		{name: "escaping", rulePath: "../outside.md", files: map[string]string{"rules/guidance.md": "# Guidance\n"}, want: manifest.CodeInvalidPath},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeHostilePackage(t, root, test.rulePath, test.files)
			remote := &fakeReleases{}
			_, err := NewService(publish.NewBuilder("test"), remote).Publish(context.Background(), root, false)
			var validation *manifest.ValidationErrors
			if !errors.As(err, &validation) || !validation.Has(test.want) {
				t.Fatalf("Publish() error = %#v, want %s", err, test.want)
			}
			if remote.lookupCalls != 0 || remote.writeCalls() != 0 {
				t.Fatalf("GitHub accessed after %s refusal: lookup %d writes %d", test.name, remote.lookupCalls, remote.writeCalls())
			}
		})
	}
}

func TestHostileDeclaredSymlinkUploadsNothing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeHostilePackage(t, root, "rules/link.md", map[string]string{"rules/target.md": "# Target\n"})
	if err := os.Symlink("target.md", filepath.Join(root, "rules", "link.md")); err != nil {
		t.Fatal(err)
	}
	remote := &fakeReleases{}
	_, err := NewService(publish.NewBuilder("test"), remote).Publish(context.Background(), root, false)
	var validation *manifest.ValidationErrors
	if !errors.As(err, &validation) || !validation.Has(manifest.CodeInvalidArtifactType) {
		t.Fatalf("Publish() error = %#v, want %s", err, manifest.CodeInvalidArtifactType)
	}
	if remote.lookupCalls != 0 || remote.writeCalls() != 0 {
		t.Fatalf("GitHub accessed after symlink refusal: lookup %d writes %d", remote.lookupCalls, remote.writeCalls())
	}
}

func TestHostileWorkflowContractAgainstDesignNote(t *testing.T) {
	t.Parallel()

	filename := filepath.Join("..", "..", ".github", "workflows", "publish-package.yml")
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(contents)
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
			PullRequest       any `yaml:"pull_request"`
			PullRequestTarget any `yaml:"pull_request_target"`
		} `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Steps []struct {
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				Env  map[string]string `yaml:"env"`
				With map[string]any    `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		t.Fatal(err)
	}
	inputs := workflow.On.WorkflowCall.Inputs
	if len(inputs) != 3 {
		t.Fatalf("workflow inputs = %#v, want path, dry-run, acr-version", inputs)
	}
	if inputs["path"].Type != "string" || inputs["path"].Default != "." {
		t.Fatalf("path input = %#v", inputs["path"])
	}
	if inputs["dry-run"].Type != "boolean" || inputs["dry-run"].Default != false {
		t.Fatalf("dry-run input = %#v", inputs["dry-run"])
	}
	if !strings.Contains(inputs["dry-run"].Description, "tagged") {
		t.Fatalf("dry-run description = %q, want tag-triggered rehearsal", inputs["dry-run"].Description)
	}
	version, ok := inputs["acr-version"].Default.(string)
	if !ok || version == "" || version == "main" || version == "latest" || !strings.HasPrefix(version, "v") {
		t.Fatalf("acr-version default = %#v, want immutable tag", inputs["acr-version"].Default)
	}
	if workflow.On.PullRequest != nil || workflow.On.PullRequestTarget != nil || strings.Contains(raw, "secrets:") {
		t.Fatal("workflow exposes a pull-request trigger or secrets input")
	}
	if len(workflow.Permissions) != 1 || workflow.Permissions["contents"] != "write" {
		t.Fatalf("permissions = %#v, want only contents: write", workflow.Permissions)
	}
	if strings.Contains(raw, "\ndraft:") || strings.Contains(raw, "prerelease:") {
		t.Fatal("workflow YAML sets draft or prerelease; publication state belongs to acr publish")
	}
	job, ok := workflow.Jobs["publish"]
	if !ok || len(job.Steps) == 0 {
		t.Fatalf("publish job = %#v", job)
	}
	if !strings.Contains(job.Steps[0].Run, "GITHUB_REF_TYPE") || !strings.Contains(job.Steps[0].Run, `!= "tag"`) {
		t.Fatalf("first step does not refuse untagged refs: %q", job.Steps[0].Run)
	}
	pinnedAction := regexp.MustCompile(`^[^@]+@[0-9a-f]{40}$`)
	foundPublish := false
	foundTagCheckout := false
	for _, step := range job.Steps {
		if step.Uses != "" && !pinnedAction.MatchString(step.Uses) {
			t.Errorf("workflow action is not SHA-pinned: %q", step.Uses)
		}
		if strings.Contains(step.Uses, "actions/checkout@") {
			if fmt.Sprint(step.With["fetch-depth"]) != "0" || fmt.Sprint(step.With["fetch-tags"]) != "true" {
				t.Errorf("checkout is not a full tag checkout: %#v", step.With)
			}
			foundTagCheckout = true
		}
		if step.Run != "" && !strings.Contains(step.Run, "set -euo pipefail") {
			t.Errorf("run step missing set -euo pipefail: %q", step.Run)
		}
		if strings.Contains(step.Run, "acr publish") {
			foundPublish = true
			if step.Env["GH_TOKEN"] != "${{ github.token }}" || step.Env["PACKAGE_PATH"] == "" {
				t.Errorf("publish environment = %#v", step.Env)
			}
		}
	}
	if !foundPublish {
		t.Fatal("workflow does not invoke acr publish")
	}
	if !foundTagCheckout {
		t.Fatal("workflow does not check out full tag history")
	}
}

func writeHostilePackage(t *testing.T, root, rulePath string, files map[string]string) {
	t.Helper()
	writeHostileAppFile(t, root, manifest.Filename, "schemaVersion: 1\nname: owner/plugin\nversion: 1.2.3\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  rules:\n    - id: guidance\n      path: "+rulePath+"\n      activation:\n        mode: always\n")
	for name, content := range files {
		writeHostileAppFile(t, root, name, content)
	}
}

func writeHostileAppFile(t *testing.T, root, relative, content string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
