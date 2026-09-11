package producerconvert

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

func sameYAML(a, b *yaml.Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	var left, right any
	if a.Decode(&left) != nil || b.Decode(&right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

// Only service-only jobs can disappear. Merely containing a Tessl setup step
// does not give a proposal authority to delete that job's tests or review.
func removableDeliveryJob(job *yaml.Node) bool {
	steps := member(job, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		return false
	}
	service := false
	for _, step := range steps.Content {
		uses := scalar(step, "uses")
		switch {
		case strings.HasPrefix(uses, "actions/checkout@"):
		case strings.Contains(uses, "setup-tessl@"):
		case strings.Contains(uses, "patch-version-publish@"), strings.Contains(uses, "/skill-review@"):
			service = true
		default:
			return false
		}
	}
	return service
}

// Only the observed three-command installation may disappear as a run step.
// Literal paths and operands exclude shell syntax; arbitrary scripts stay
// subject to preservation even when they contain a service reference.
func serviceOnlyStep(step *yaml.Node) bool {
	if run := member(step, "run"); run != nil {
		if run.Kind != yaml.ScalarNode || run.Tag != "!!str" {
			return false
		}
		for i := 0; i < len(step.Content); i += 2 {
			if key := step.Content[i].Value; key != "name" && key != "run" {
				return false
			}
		}
		var lines []string
		for _, line := range strings.Split(run.Value, "\n") {
			line = strings.Trim(line, " \t\r")
			if line != "" {
				lines = append(lines, line)
			}
		}
		if len(lines) != 3 {
			return false
		}
		mkdir, cd, install := serviceMkdir.FindStringSubmatch(lines[0]), serviceCD.FindStringSubmatch(lines[1]), serviceInstall.FindStringSubmatch(lines[2])
		return mkdir != nil && cd != nil && install != nil && mkdir[1] == cd[1] && mkdir[1] == "/tmp/gh-aw/"+install[1]
	}
	uses := scalar(step, "uses")
	return strings.Contains(uses, "setup-tessl@") || strings.Contains(uses, "patch-version-publish@") || strings.Contains(uses, "/skill-review@")
}

var serviceMkdir = regexp.MustCompile(`^mkdir[ \t]+-p[ \t]+(/tmp/gh-aw/[A-Za-z0-9][A-Za-z0-9._-]*)$`)
var serviceCD = regexp.MustCompile(`^cd[ \t]+(/tmp/gh-aw/[A-Za-z0-9][A-Za-z0-9._-]*)$`)
var serviceInstall = regexp.MustCompile(`^tessl[ \t]+install[ \t]+[A-Za-z0-9][A-Za-z0-9._-]*/([A-Za-z0-9][A-Za-z0-9._-]*)[ \t]+--yes$`)

func serviceCredential(key string) bool {
	return strings.HasPrefix(key, "TESSL_") || strings.HasPrefix(key, "SECRET_TESSL_")
}

// Allow only removal of existing service entries. Keep surviving environment
// entries and comma-list items in their original order, with original values.
func preservedWorkflowEnv(before, after *yaml.Node, description bool) bool {
	if before == nil || before.Kind != yaml.MappingNode {
		return sameYAML(before, after)
	}
	if after != nil && after.Kind != yaml.MappingNode {
		return false
	}
	var next []*yaml.Node
	if after != nil {
		next = after.Content
	}
	j := 0
	removed := false
	for i := 0; i < len(before.Content); i += 2 {
		key, value := before.Content[i].Value, before.Content[i+1]
		if serviceCredential(key) && member(after, key) == nil {
			removed = true
			continue
		}
		if j >= len(next) || !sameYAML(before.Content[i], next[j]) {
			return false
		}
		candidate := next[j+1]
		j += 2
		if key == "GH_AW_SECRET_NAMES" {
			if !preservedSecretNames(value, candidate) {
				return false
			}
		} else if description && key == "WORKFLOW_DESCRIPTION" {
			// Final source/lock agreement is mandatory in metadata reconciliation.
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" || candidate.Kind != yaml.ScalarNode || candidate.Tag != "!!str" {
				return false
			}
		} else if !sameYAML(value, candidate) {
			return false
		}
	}
	return j == len(next) && (after != nil || removed)
}

func preservedSecretNames(before, after *yaml.Node) bool {
	if sameYAML(before, after) {
		return true
	}
	if before == nil || after == nil || before.Kind != yaml.ScalarNode || after.Kind != yaml.ScalarNode || before.Tag != "!!str" || after.Tag != "!!str" {
		return false
	}
	next := strings.Split(after.Value, ",")
	if after.Value == "" {
		next = nil
	}
	j := 0
	for _, item := range strings.Split(before.Value, ",") {
		if j < len(next) && item == next[j] {
			j++
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(item), "TESSL_") {
			return false
		}
	}
	return j == len(next)
}

// Compare both key sets so unknown fields and newly introduced policy remain
// protected. Only the named structural fields are handled by the caller.
func preserveWorkflowFields(before, after *yaml.Node, description bool, except ...string) error {
	for _, node := range []*yaml.Node{before, after} {
		if node == nil || node.Kind != yaml.MappingNode {
			return fmt.Errorf("independent workflow/job mapping must remain")
		}
		for i := 0; i < len(node.Content); i += 2 {
			field := node.Content[i].Value
			if slices.Contains(except, field) {
				continue
			}
			old, next := member(before, field), member(after, field)
			if field == "env" {
				if !preservedWorkflowEnv(old, next, description) {
					return fmt.Errorf("independent environment/credential policy must remain")
				}
			} else if !sameYAML(old, next) {
				return fmt.Errorf("independent %s condition/policy must remain", field)
			}
		}
	}
	return nil
}

func preserveWorkflowJob(before, after *yaml.Node, description bool) error {
	if err := preserveWorkflowFields(before, after, false, "steps"); err != nil {
		return err
	}
	oldSteps, newSteps := member(before, "steps"), member(after, "steps")
	if oldSteps == nil {
		return nil
	}
	for _, oldStep := range oldSteps.Content {
		if serviceOnlyStep(oldStep) {
			continue
		}
		retained := false
		if newSteps != nil {
			for _, newStep := range newSteps.Content {
				if preserveWorkflowFields(oldStep, newStep, description) == nil {
					retained = true
					break
				}
			}
		}
		if !retained {
			return fmt.Errorf("independent step %q (%s) must retain its logic", scalar(oldStep, "name"), scalar(oldStep, "id"))
		}
	}
	return nil
}
