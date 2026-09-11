package producerconvert

import (
	"bytes"
	"context"
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

const serviceInstallStep = "      - name: Install policy\n        run: |\n          mkdir -p /tmp/gh-aw/policy\n          cd /tmp/gh-aw/policy\n          tessl install owner/policy --yes\n"

func ghProposalFixture(t *testing.T) (string, Options, proposal) {
	t.Helper()
	root, opts, p := semanticFixture(t)
	const oldDescription = "Load policy using tessl install owner/policy"
	const newDescription = "Load configured policy"
	custom := strings.ReplaceAll(serviceInstallStep, "      -", "  -")
	custom = strings.ReplaceAll(custom, "        ", "    ")
	source := "---\nname: Audit\non: pull_request\ndescription: " + oldDescription + "\nsteps:\n" + custom + "---\nReview independently.\n"
	hash, _, err := ghWorkflowHash([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	lock := `# gh-aw-metadata: {"schema_version":"v3","compiler_version":"v0.71.5","frontmatter_hash":"` + hash + `","agent_id":"audit"}` + "\non: pull_request\njobs:\n  audit:\n    runs-on: ubuntu-latest\n    steps:\n" + serviceInstallStep + `      - name: Redact
        if: always()
        uses: actions/github-script@v7
        with:
          script: redact();
        env:
          GH_AW_SECRET_NAMES: 'TOKEN,TESSL_TOKEN,OTHER'
          SECRET_TOKEN: '${{ secrets.TOKEN }}'
          SECRET_TESSL_TOKEN: '${{ secrets.TESSL_TOKEN }}'
          SECRET_OTHER: '${{ secrets.OTHER }}'
      - name: Detect
        if: always()
        run: detect --required
        env:
          WORKFLOW_DESCRIPTION: ` + oldDescription + `
          HAS_PATCH: '${{ needs.agent.outputs.has_patch }}'
      - run: review --required
`
	nextSource := strings.Replace(source, custom, "  - name: Load configured policy\n    run: echo configured-policy\n", 1)
	nextSource = strings.Replace(nextSource, oldDescription, newDescription, 1)
	nextLock := strings.Replace(lock, serviceInstallStep, "      - name: Load configured policy\n        run: echo configured-policy\n", 1)
	nextLock = strings.Replace(nextLock, "TOKEN,TESSL_TOKEN,OTHER", "TOKEN,OTHER", 1)
	nextLock = strings.Replace(nextLock, "          SECRET_TESSL_TOKEN: '${{ secrets.TESSL_TOKEN }}'\n", "", 1)
	nextLock = strings.Replace(nextLock, oldDescription, newDescription, 1)
	for name, body := range map[string]string{".github/workflows/audit.md": source, ".github/workflows/audit.lock.yml": lock} {
		put(t, root, name, body, 0o640)
		next := nextSource
		if strings.HasSuffix(name, ".yml") {
			next = nextLock
		}
		p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(body)), Action: "replace", Content: next})
	}
	return root, opts, p
}

func TestGHWorkflowProposalPreservesLockPolicy(t *testing.T) {
	for _, kind := range []string{"valid", "missing-source", "missing-metadata", "forged-hash", "compiler", "source-imports", "metadata-identity", "missing-custom-step", "description-disagrees", "description-removed", "unrelated-redaction-removed", "redaction-reordered", "redaction-added", "service-credential-changed", "service-credential-added", "step-condition"} {
		t.Run(kind, func(t *testing.T) {
			root, opts, p := ghProposalFixture(t)
			const source = ".github/workflows/audit.md"
			const lock = ".github/workflows/audit.lock.yml"
			for i := range p.Edits {
				edit := &p.Edits[i]
				if edit.Path == source && kind == "missing-source" {
					edit.Action = "remove"
					edit.Content = ""
				}
				if edit.Path == source && kind == "source-imports" {
					edit.Content = strings.Replace(edit.Content, "name: Audit", "imports: [other.md]\nname: Audit", 1)
				}
				if edit.Path != lock {
					continue
				}
				old := read(t, root, lock)
				switch kind {
				case "missing-metadata":
					old = strings.SplitN(old, "\n", 2)[1]
				case "forged-hash":
					old = strings.Replace(old, `"frontmatter_hash":"`, `"frontmatter_hash":"forged`, 1)
				case "compiler":
					old = strings.Replace(old, "v0.71.5", "v9.0.0", 1)
				case "metadata-identity":
					edit.Content = strings.Replace(edit.Content, `"agent_id":"audit"`, `"agent_id":"other"`, 1)
				case "missing-custom-step":
					edit.Content = strings.Replace(edit.Content, "echo configured-policy", "echo different-policy", 1)
				case "description-disagrees":
					edit.Content = strings.Replace(edit.Content, "WORKFLOW_DESCRIPTION: Load configured policy", "WORKFLOW_DESCRIPTION: Different policy", 1)
				case "description-removed":
					edit.Content = strings.Replace(edit.Content, "          WORKFLOW_DESCRIPTION: Load configured policy\n", "", 1)
				case "unrelated-redaction-removed":
					edit.Content = strings.Replace(edit.Content, "TOKEN,OTHER", "TOKEN", 1)
				case "redaction-reordered":
					edit.Content = strings.Replace(edit.Content, "TOKEN,OTHER", "OTHER,TOKEN", 1)
				case "redaction-added":
					edit.Content = strings.Replace(edit.Content, "TOKEN,OTHER", "TOKEN,OTHER,TESSL_NEW", 1)
				case "service-credential-changed":
					edit.Content = strings.Replace(edit.Content, "          SECRET_TOKEN:", "          SECRET_TESSL_TOKEN: different\n          SECRET_TOKEN:", 1)
				case "service-credential-added":
					edit.Content = strings.Replace(edit.Content, "          SECRET_TOKEN:", "          TESSL_NEW: different\n          SECRET_TOKEN:", 1)
				case "step-condition":
					edit.Content = strings.Replace(edit.Content, "if: always()", "if: false", 1)
				}
				put(t, root, lock, old, 0o640)
				edit.BeforeDigest = digest([]byte(old))
			}
			original := treeAt(t, root)
			calls := 0
			provider := func(context.Context, string, string) (proposal, AgentRun, error) { calls++; return p, AgentRun{}, nil }
			plan, err := prepareWithProvider(context.Background(), opts, provider)
			if !matches(original, treeAt(t, root)) {
				t.Fatal("planning changed source")
			}
			absent(t, root, ReceiptPath)
			absent(t, root, transactionPath)
			if kind != "valid" {
				if err == nil {
					t.Fatal("accepted invalid lock proposal")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			meta, _, err := ghWorkflowMetadata(plan.after[lock].Content)
			if err != nil {
				t.Fatal(err)
			}
			hash, _, err := ghWorkflowHash(plan.after[source].Content)
			if err != nil || meta["frontmatter_hash"] != hash {
				t.Fatalf("incoherent metadata: %v", err)
			}
			if _, err := plan.Apply(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(read(t, root, lock), "review --required") {
				t.Fatal("review lost")
			}
			current, err := prepareWithProvider(context.Background(), opts, provider)
			if err != nil || !current.Report.Current || calls != 1 {
				t.Fatalf("rerun: %v calls=%d", err, calls)
			}
		})
	}
}
