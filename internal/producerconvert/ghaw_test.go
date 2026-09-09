package producerconvert

import (
	"bytes"
	"strings"
	"testing"
)

const ghOriginalHash = "25f559ca4eb71fc8fc48959f329c0325090b22bbc7905c2b7a0fdc85b2c7023c"
const ghChangedHash = "8c7369d7b10b7d99ecf16b4a1a2a29a031932b7c5b61c7e3c507c38f969622e9"
const ghOriginalSource = `---
name: Independent review
on: pull_request
description: Original policy
steps:
  - name: Load policy
    run: echo old-policy
---
Review the change.
`
const ghOriginalLock = `# gh-aw-metadata: {"schema_version":"v3","compiler_version":"v0.71.5","frontmatter_hash":"` + ghOriginalHash + `","agent_id":"codex"}
on: pull_request
jobs:
  review:
    env:
      WORKFLOW_DESCRIPTION: Original policy
    steps:
      - name: Load policy
        run: echo old-policy
      - name: Independent review
        run: reviewer --required
`

func ghFixture() (tree, tree) {
	before := tree{}
	for name, body := range map[string]string{".github/workflows/audit.md": ghOriginalSource, ".github/workflows/audit.lock.yml": ghOriginalLock} {
		before[name] = fileState{Content: []byte(body), Digest: digest([]byte(body)), Mode: 0o644}
	}
	after := tree{}
	for name, state := range before {
		state.Content = bytes.ReplaceAll(bytes.ReplaceAll(state.Content, []byte("Original policy"), []byte("Configured policy")), []byte("echo old-policy"), []byte("echo configured-policy"))
		state.Digest = digest(state.Content)
		after[name] = state
	}
	return before, after
}

func TestGHWorkflowMetadataBindsCoherentChangedSource(t *testing.T) {
	before, after := ghFixture()
	if err := reconcileGHWorkflowMetadata(before, after); err != nil {
		t.Fatal(err)
	}
	lock := after[".github/workflows/audit.lock.yml"]
	if !bytes.Contains(lock.Content, []byte(ghChangedHash)) || lock.Digest != digest(lock.Content) {
		t.Fatal("changed source was not bound to expected independent hash")
	}
	if !bytes.Contains(before[".github/workflows/audit.lock.yml"].Content, []byte(ghOriginalHash)) {
		t.Fatal("original hash binding changed")
	}
	if !bytes.Contains(lock.Content, []byte("reviewer --required")) || lock.Mode != 0o644 {
		t.Fatal("unrelated logic or mode changed")
	}
	settled := append([]byte(nil), lock.Content...)
	if err := reconcileGHWorkflowMetadata(before, after); err != nil || !bytes.Equal(settled, after[".github/workflows/audit.lock.yml"].Content) {
		t.Fatalf("refresh is not inert: %v", err)
	}
}

func TestGHWorkflowMetadataRefusesUnprovenCompilation(t *testing.T) {
	for _, kind := range []string{"stale-original", "unsupported-version", "missing-step", "compiled-condition", "description", "engine", "imports", "metadata-identity"} {
		t.Run(kind, func(t *testing.T) {
			before, after := ghFixture()
			replace := func(tree tree, name, old, new string) {
				state := tree[name]
				state.Content = []byte(strings.ReplaceAll(string(state.Content), old, new))
				state.Digest = digest(state.Content)
				tree[name] = state
			}
			source, lock := ".github/workflows/audit.md", ".github/workflows/audit.lock.yml"
			switch kind {
			case "stale-original":
				replace(before, lock, ghOriginalHash, "incorrect")
			case "unsupported-version":
				replace(before, lock, "v0.71.5", "v0.81.6")
			case "missing-step":
				replace(after, lock, "echo configured-policy", "echo omitted")
			case "compiled-condition":
				replace(after, lock, "      - name: Load policy", "      - if: false\n        name: Load policy")
			case "description":
				replace(after, lock, "Configured policy", "stale description")
			case "engine":
				replace(after, source, "name: Independent review", "engine: different\nname: Independent review")
			case "imports":
				replace(after, source, "name: Independent review", "imports: [../../private.md]\nname: Independent review")
			case "metadata-identity":
				replace(after, lock, `"agent_id":"codex"`, `"agent_id":"claude"`)
			}
			if err := reconcileGHWorkflowMetadata(before, after); err == nil {
				t.Fatal("accepted unproven compiled workflow")
			}
		})
	}
}

func TestGHWorkflowHashNormalizesLineEndingsAndKeepsExpressionBindings(t *testing.T) {
	hash, _, err := ghWorkflowHash([]byte(strings.ReplaceAll(ghOriginalSource, "\n", "\r\n")))
	if err != nil || hash != ghOriginalHash {
		t.Fatalf("CRLF hash: %s %v", hash, err)
	}
	one := ghOriginalSource + "${{ vars.POLICY }} ${{ env.LOCATION }}\n"
	two := ghOriginalSource + "${{ env.LOCATION }} ${{ vars.POLICY }} ${{ vars.POLICY }}\n"
	first, _, err := ghWorkflowHash([]byte(one))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := ghWorkflowHash([]byte(two))
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first == ghOriginalHash {
		t.Fatal("template bindings were lost or nondeterministic")
	}
}
