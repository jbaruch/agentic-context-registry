package producerconvert

import (
	"fmt"
	"path"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"
)

// This exact callee's declared contract was inspected; other revisions require
// a new review. No remote workflow is fetched or interpreted during conversion.
const fleetPublisherIdentity = "jbaruch/coding-policy/.github/workflows/publish-plugin.yml"
const fleetPublisher = fleetPublisherIdentity + "@af116ebf18a7c46a672bf176064908736bc8ac28"

// Renew these converter-owned action pins monthly and when the ACR publisher
// pin changes, with the gate execution/preservation tests.
const gateCheckout = "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1"
const gatePython = "actions/setup-python@5fda3b95a4ea91299a34e894583c3862153e4b97"

func translatePublisher(data []byte, selected string, source tree, semantic bool) ([]byte, []byte, error) {
	if !strings.Contains(string(data), fleetPublisherIdentity+"@") {
		retained, err := translateWorkflow(data, selected)
		return retained, []byte(publishWorkflow), err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	if err := closedYAML(&doc); err != nil {
		return nil, nil, err
	}
	if len(doc.Content) != 1 {
		return nil, nil, fmt.Errorf("expected one workflow document")
	}
	top := doc.Content[0]
	if selected != "." || !keysOnly(top, "name", "on", "permissions", "jobs") {
		return nil, nil, fmt.Errorf("fleet publisher requires root package and supported workflow policy")
	}
	var trigger any
	on := member(top, "on")
	if on == nil {
		return nil, nil, fmt.Errorf("missing workflow trigger")
	}
	if err := on.Decode(&trigger); err != nil {
		return nil, nil, err
	}
	if !reflect.DeepEqual(trigger, map[string]any{"push": map[string]any{"branches": []any{"main"}}}) {
		return nil, nil, fmt.Errorf("only push.branches: [main] has a supported tag-publication translation")
	}
	permissions := member(top, "permissions")
	if !keysOnly(permissions, "contents", "id-token", "pull-requests") || scalar(permissions, "contents") != "write" {
		return nil, nil, fmt.Errorf("fleet publisher requires explicit contents: write and known permissions")
	}
	for i := 1; i < len(permissions.Content); i += 2 {
		if !workflowScalar(permissions.Content[i], "!!str") || permissions.Content[i].Value != "write" {
			return nil, nil, fmt.Errorf("unsupported publication permission")
		}
	}
	jobs := member(top, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("missing jobs map")
	}
	found := -1
	for i := 0; i < len(jobs.Content); i += 2 {
		uses := scalar(jobs.Content[i+1], "uses")
		if actionIdentity(uses) == fleetPublisherIdentity {
			if found >= 0 || uses != fleetPublisher {
				return nil, nil, fmt.Errorf("multiple or unsupported fleet publisher revision")
			}
			found = i
		}
	}
	if found < 0 {
		return nil, nil, fmt.Errorf("missing recognized fleet publisher")
	}
	job := jobs.Content[found+1]
	if !keysOnly(job, "name", "uses", "secrets", "with", "if") {
		return nil, nil, fmt.Errorf("unsupported fleet publisher job policy")
	}
	condition := member(job, "if")
	if condition != nil && (!semantic || !workflowScalar(condition, "!!str", "!!bool")) {
		return nil, nil, fmt.Errorf("fleet publisher condition requires supported semantic conversion")
	}
	secrets := member(job, "secrets")
	if !keysOnly(secrets, "TESSL_TOKEN") || scalar(secrets, "TESSL_TOKEN") != "${{ secrets.TESSL_TOKEN }}" {
		return nil, nil, fmt.Errorf("fleet publisher requires only explicit TESSL_TOKEN binding")
	}
	inputs := member(job, "with")
	if inputs != nil && !keysOnly(inputs, "python-version", "pre-publish-script", "skills-dir", "skill-review-credit-outage", "stamp-changelog", "publish-mode") {
		return nil, nil, fmt.Errorf("unknown fleet publisher inputs")
	}
	if inputs != nil {
		for i := 0; i < len(inputs.Content); i += 2 {
			key, value := inputs.Content[i].Value, inputs.Content[i+1]
			if !workflowScalar(value, "!!str") && !(key == "stamp-changelog" && workflowScalar(value, "!!bool")) || strings.Contains(value.Value, "${{") {
				return nil, nil, fmt.Errorf("fleet publisher input %s must be literal with its declared type", key)
			}
			if key == "stamp-changelog" && !workflowScalar(value, "!!bool") {
				return nil, nil, fmt.Errorf("stamp-changelog must be Boolean")
			}
			if key == "publish-mode" && value.Value != "auto-bump" && value.Value != "as-is" {
				return nil, nil, fmt.Errorf("unsupported publish-mode")
			}
			if key == "skill-review-credit-outage" && value.Value != "fail" && value.Value != "skip" {
				return nil, nil, fmt.Errorf("unsupported skill-review-credit-outage")
			}
		}
	}
	script, python := scalar(inputs, "pre-publish-script"), scalar(inputs, "python-version")
	if script != "" {
		state, exists := source[script]
		if path.IsAbs(script) || path.Clean(script) != script || script == "." || strings.HasPrefix(script, "../") || strings.ContainsAny(script, "\x00\r\n\\") || !exists || state.Directory || state.Link != "" || state.Digest == "" {
			return nil, nil, fmt.Errorf("pre-publish-script must name an existing regular repository-relative file")
		}
	}
	if python != "" && script == "" {
		return nil, nil, fmt.Errorf("python-version without a gate has no supported execution")
	}
	for i := 0; i < len(jobs.Content); i += 2 {
		if i == found {
			continue
		}
		sibling := jobs.Content[i+1]
		if member(sibling, "needs") != nil || workflowReferences(sibling, "needs", jobs.Content[found].Value) || workflowReferences(sibling, "jobs", jobs.Content[found].Value) {
			return nil, nil, fmt.Errorf("independent job depends on retired publisher")
		}
	}
	// Build only converter-owned output. The model cannot invent a reusable target.
	var output yaml.Node
	if err := yaml.Unmarshal([]byte(publishWorkflow), &output); err != nil {
		return nil, nil, err
	}
	outputJobs := member(output.Content[0], "jobs")
	publish := member(outputJobs, "publish")
	if condition != nil {
		publish.Content = append(publish.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "if"}, condition)
	}
	if script != "" {
		gate := map[string]any{"runs-on": "ubuntu-latest", "steps": []any{map[string]any{"uses": gateCheckout}}}
		steps := gate["steps"].([]any)
		if python != "" {
			steps = append(steps, map[string]any{"uses": gatePython, "with": map[string]any{"python-version": python}})
		}
		steps = append(steps, map[string]any{"run": "bash -- './" + strings.ReplaceAll(script, "'", "'\"'\"'") + "'"})
		gate["steps"] = steps
		var gateNode yaml.Node
		if err := gateNode.Encode(gate); err != nil {
			return nil, nil, err
		}
		if condition != nil {
			gateNode.Content = append(gateNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "if"}, condition)
		}
		publish.Content = append(publish.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "needs"}, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "pre-publish"}}})
		outputJobs.Content = append([]*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "pre-publish"}, &gateNode}, outputJobs.Content...)
	}
	encoded, err := yaml.Marshal(&output)
	if err != nil {
		return nil, nil, err
	}
	retained := removePublisherJob(data, top, jobs, found)
	if len(retained) > 0 {
		retained = append([]byte("# Paid Tessl skill review was retired; ACR has no equivalent score.\n"), retained...)
	}
	return retained, append([]byte("# Paid Tessl skill review was retired; ACR has no equivalent score.\n"), encoded...), nil
}
