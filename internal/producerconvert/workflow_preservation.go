package producerconvert

import (
	"fmt"
	"reflect"
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

// A step may disappear only when it is a proven service-only action. Service
// detection on a run block proves that Tessl is present, not that every command
// in the block belongs to the service, so a run step is never exempt: it must
// be retained unchanged, and the residual Tessl operation then refuses the
// candidate conversion instead of a proposal deleting independent tests.
func serviceOnlyStep(step *yaml.Node) bool {
	if member(step, "run") != nil {
		return false
	}
	uses := scalar(step, "uses")
	return strings.Contains(uses, "setup-tessl@") || strings.Contains(uses, "patch-version-publish@") || strings.Contains(uses, "/skill-review@")
}

func preserveWorkflowJob(before, after *yaml.Node) error {
	for _, field := range []string{"if", "needs", "permissions", "environment", "strategy", "concurrency", "timeout-minutes"} {
		if !sameYAML(member(before, field), member(after, field)) {
			return fmt.Errorf("independent %s condition/policy must remain", field)
		}
	}
	// Preserve unrelated credentials/config even when the job also had Tessl.
	oldEnv, newEnv := member(before, "env"), member(after, "env")
	if oldEnv != nil && oldEnv.Kind == yaml.MappingNode {
		for i := 0; i < len(oldEnv.Content); i += 2 {
			key := oldEnv.Content[i].Value
			data, err := yaml.Marshal(oldEnv.Content[i+1])
			if err != nil {
				return err
			}
			if strings.HasPrefix(key, "TESSL_") || workflowSemantic(data) {
				continue
			}
			if !sameYAML(oldEnv.Content[i+1], member(newEnv, key)) {
				return fmt.Errorf("independent environment/credential %s must remain", key)
			}
		}
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
				if sameYAML(oldStep, newStep) {
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
