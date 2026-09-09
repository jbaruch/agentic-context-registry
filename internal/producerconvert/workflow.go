package producerconvert

import (
	"fmt"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Renew this pin after each stable ACR release, with the workflow contract tests.
const publishWorkflow = `name: Publish ACR package
on:
  push:
    tags: ['v*']
permissions:
  contents: write
jobs:
  publish:
    uses: jbaruch/agentic-context-registry/.github/workflows/publish-package.yml@d3bc96b33b42293aecd1702c04aa94513a3dab1b
    with:
      path: .
      acr-version: v0.1.6
`
const publishWorkflowPath = ".github/workflows/acr-publish.yml"

// translateWorkflow accepts the observed two-step patch-version publisher and
// no implicit shell semantics. Independent jobs keep their original source
// bytes and original trigger in the old workflow; publishing gets its own tag
// workflow. Neither review gates nor dependent jobs have a translation.
func translateWorkflow(data []byte, selected string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if err := closedYAML(&doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 {
		return nil, fmt.Errorf("expected one workflow document")
	}
	top := doc.Content[0]
	if !keysOnly(top, "name", "on", "permissions", "jobs") {
		return nil, fmt.Errorf("unknown workflow-level logic")
	}
	on := member(top, "on")
	var trigger any
	if on == nil {
		return nil, fmt.Errorf("missing workflow trigger")
	}
	if err := on.Decode(&trigger); err != nil {
		return nil, err
	}
	want := map[string]any{"push": map[string]any{"branches": []any{"main"}}}
	if !reflect.DeepEqual(trigger, want) {
		return nil, fmt.Errorf("only push.branches: [main] has a supported tag-publication translation")
	}
	permissions := member(top, "permissions")
	if permissions != nil {
		if !keysOnly(permissions, "contents", "pull-requests") {
			return nil, fmt.Errorf("unknown publication permissions")
		}
		for i := 1; i < len(permissions.Content); i += 2 {
			if permissions.Content[i].Value != "write" {
				return nil, fmt.Errorf("unsupported publication permissions")
			}
		}
	}
	jobs := member(top, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("missing jobs map")
	}
	found := -1
	for i := 0; i < len(jobs.Content); i += 2 {
		encoded, err := yaml.Marshal(jobs.Content[i+1])
		if err != nil {
			return nil, err
		}
		if strings.Contains(strings.ToLower(string(encoded)), "tessl") {
			if found >= 0 {
				return nil, fmt.Errorf("multiple Tessl jobs require semantic conversion")
			}
			found = i
		}
	}
	if found < 0 {
		return nil, fmt.Errorf("Tessl configuration outside a standalone publish job")
	}
	job := jobs.Content[found+1]
	if !keysOnly(job, "name", "runs-on", "steps") || scalar(job, "runs-on") != "ubuntu-latest" {
		return nil, fmt.Errorf("mixed or unknown Tessl job; review and custom logic require semantic conversion")
	}
	steps := member(job, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode || len(steps.Content) != 2 {
		return nil, fmt.Errorf("publish job must contain only checkout and patch-version-publish")
	}
	checkout, publish := steps.Content[0], steps.Content[1]
	if !keysOnly(checkout, "name", "uses") || scalar(checkout, "uses") != "actions/checkout@v4" {
		return nil, fmt.Errorf("unrecognized checkout step; supported form is actions/checkout@v4 without options")
	}
	if !keysOnly(publish, "name", "uses", "with") || scalar(publish, "uses") != "tesslio/patch-version-publish@v1" {
		return nil, fmt.Errorf("unrecognized publisher; supported form is tesslio/patch-version-publish@v1")
	}
	inputs := member(publish, "with")
	if !keysOnly(inputs, "path", "token") || scalar(inputs, "token") != "${{ secrets.TESSL_TOKEN }}" {
		return nil, fmt.Errorf("unknown publisher inputs or token binding")
	}
	packagePath := scalar(inputs, "path")
	if packagePath == "" {
		packagePath = "."
	}
	if packagePath != selected {
		return nil, fmt.Errorf("publisher path %q does not select %q", packagePath, selected)
	}
	for i := 0; i < len(jobs.Content); i += 2 {
		if i == found {
			continue
		}
		if member(jobs.Content[i+1], "needs") != nil {
			return nil, fmt.Errorf("job %s has dependencies; preserve its policy through a reviewed semantic conversion", jobs.Content[i].Value)
		}
	}
	if len(jobs.Content) == 2 {
		return nil, nil
	}
	// yaml.Node line numbers let us remove just this job without reformatting
	// any independent test job. Comments are retained with the surrounding file.
	lines := strings.SplitAfter(string(data), "\n")
	from := jobs.Content[found].Line - 1
	to := len(lines)
	if found+2 < len(jobs.Content) {
		to = jobs.Content[found+2].Line - 1
	} else {
		for i := 0; i < len(top.Content); i += 2 {
			if top.Content[i].Line > jobs.Line && top.Content[i].Line-1 < to {
				to = top.Content[i].Line - 1
			}
		}
	}
	// Comments and spacing before the next job belong to the retained
	// workflow. Keep them byte-for-byte, including its head comments.
	for to > from {
		trimmed := strings.TrimSpace(lines[to-1])
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			break
		}
		to--
	}
	return []byte(strings.Join(lines[:from], "") + strings.Join(lines[to:], "")), nil
}

func member(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
func scalar(node *yaml.Node, key string) string {
	value := member(node, key)
	if value == nil || value.Kind != yaml.ScalarNode {
		return ""
	}
	return value.Value
}
func keysOnly(node *yaml.Node, allowed ...string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i < len(node.Content); i += 2 {
		ok := false
		for _, key := range allowed {
			if node.Content[i].Value == key {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
func closedYAML(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return fmt.Errorf("YAML aliases and anchors require semantic conversion")
	}
	if node.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] || key.Value == "<<" {
				return fmt.Errorf("duplicate, merged or non-string YAML key at line %d", key.Line)
			}
			seen[key.Value] = true
		}
	}
	for _, child := range node.Content {
		if err := closedYAML(child); err != nil {
			return err
		}
	}
	return nil
}
