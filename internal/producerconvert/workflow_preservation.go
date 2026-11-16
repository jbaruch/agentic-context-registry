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

type deliveryJobType uint8

const (
	independentDeliveryJob deliveryJobType = iota
	publisherDeliveryJob
	scoreOnlyDeliveryJob
)

// Classify execution separately from authority to discard the job's policy.
// Checkout/setup support a service; neither alone authorizes job retirement.
func deliveryJobKind(job *yaml.Node) deliveryJobType {
	steps := member(job, "steps")
	if job == nil || job.Kind != yaml.MappingNode || member(job, "uses") != nil || member(job, "with") != nil || steps == nil || steps.Kind != yaml.SequenceNode {
		return independentDeliveryJob
	}
	kind := independentDeliveryJob
	for _, step := range steps.Content {
		if step.Kind != yaml.MappingNode || member(step, "run") != nil || !workflowScalar(member(step, "uses"), "!!str") {
			return independentDeliveryJob
		}
		switch actionIdentity(scalar(step, "uses")) {
		case "actions/checkout", "tesslio/setup-tessl":
		case "tesslio/patch-version-publish":
			kind = publisherDeliveryJob
		case "jbaruch/coding-policy/.github/actions/skill-review":
			if kind != publisherDeliveryJob {
				kind = scoreOnlyDeliveryJob
			}
		default:
			return independentDeliveryJob
		}
	}
	return kind
}

func ordinaryPublisherRunner(job *yaml.Node) bool {
	runner := member(job, "runs-on")
	return runner == nil || workflowScalar(runner, "!!str") && runner.Value == "ubuntu-latest"
}

// A retired paid score has no replacement execution to govern. Publication
// policy survives unless the original job proves the closed infrastructure form.
// The caller separately refuses all surviving references to affected outputs.
func removableDeliveryJob(job *yaml.Node) bool {
	if job == nil || closedYAML(job) != nil {
		return false
	}
	switch deliveryJobKind(job) {
	case scoreOnlyDeliveryJob:
		return true
	case publisherDeliveryJob:
		if !keysOnly(job, "name", "runs-on", "steps", "env", "outputs") || !ordinaryPublisherRunner(job) || !preservedWorkflowEnv(member(job, "env"), nil, false) {
			return false
		}
		if name := member(job, "name"); name != nil && !workflowScalar(name, "!!str") {
			return false
		}
		if outputs := member(job, "outputs"); outputs != nil && !workflowStringMap(outputs) {
			return false
		}
		return true
	}
	return false
}

// Publication conditions are obligations of the original execution occurrences,
// before service classification can remove their steps or their whole job.
func preservePublisherStepConditions(before, after *yaml.Node) error {
	var originals, candidates []*yaml.Node
	guarded := false
	if steps := member(before, "steps"); steps != nil && steps.Kind == yaml.SequenceNode {
		for _, step := range steps.Content {
			if workflowScalar(member(step, "uses"), "!!str") && actionIdentity(scalar(step, "uses")) == "tesslio/patch-version-publish" {
				condition := member(step, "if")
				if condition != nil && !workflowScalar(condition, "!!str", "!!bool") {
					return fmt.Errorf("unsupported publisher step if representation; retain a Boolean or string condition")
				}
				originals = append(originals, step)
				guarded = guarded || condition != nil
			}
		}
	}
	if len(originals) == 0 {
		return nil
	}
	if steps := member(after, "steps"); steps != nil && steps.Kind == yaml.SequenceNode && member(after, "uses") == nil {
		for _, step := range steps.Content {
			uses, run := member(step, "uses"), member(step, "run")
			if run == nil && workflowScalar(uses, "!!str") && actionIdentity(uses.Value) == "tesslio/patch-version-publish" || uses == nil && workflowScalar(run, "!!str") && strings.TrimSpace(run.Value) == "acr publish ." {
				candidates = append(candidates, step)
			}
		}
	}
	// Existing unguarded service retirement and reusable conversion remain valid.
	// A guarded publisher requires retained steps; no cross-scope guard transfer.
	if len(candidates) == 0 && !guarded {
		return nil
	}
	position := 0
	for _, original := range originals {
		retained := false
		for position < len(candidates) {
			candidate := candidates[position]
			position++
			if sameYAML(member(original, "if"), member(candidate, "if")) {
				retained = true
				break
			}
		}
		if !retained {
			return fmt.Errorf("publisher step if condition must remain on each corresponding publisher in order; retain guarded standalone acr publish . steps")
		}
	}
	return nil
}

// Explicit job permissions replace workflow permissions. Unknown repository
// defaults remain outside this finite compatibility check; never grant access.
func reusablePublisherContentsWrite(job, workflow *yaml.Node) bool {
	permissions := member(job, "permissions")
	if permissions == nil {
		permissions = member(workflow, "permissions")
	}
	if permissions == nil {
		return true
	}
	if workflowScalar(permissions, "!!str") {
		return permissions.Value == "write-all"
	}
	if !workflowStringMap(permissions) {
		return false
	}
	for i := 1; i < len(permissions.Content); i += 2 {
		if !slices.Contains([]string{"read", "write", "none"}, permissions.Content[i].Value) {
			return false
		}
	}
	return scalar(permissions, "contents") == "write"
}

// Only this execution-shape exception may exchange a runner/step job for a
// reusable call. Other policy is still compared against the unsanitized source.
func preservePublisherRewrite(before, after, workflow *yaml.Node) error {
	for _, job := range []*yaml.Node{before, after} {
		if err := closedYAML(job); err != nil {
			return err
		}
	}
	if deliveryJobKind(before) != publisherDeliveryJob || !ordinaryPublisherRunner(before) || !keysOnly(before, "name", "if", "needs", "permissions", "concurrency", "strategy", "env", "runs-on", "steps") || !keysOnly(after, "name", "if", "needs", "permissions", "concurrency", "strategy", "uses", "with") {
		return fmt.Errorf("unsupported publisher execution/policy shape; preserve the job's policy in a supported representation")
	}
	var expected yaml.Node
	if err := yaml.Unmarshal([]byte(publishWorkflow), &expected); err != nil {
		return fmt.Errorf("decode supported publisher contract: %w", err)
	}
	publisher := member(member(expected.Content[0], "jobs"), "publish")
	if !sameYAML(member(after, "uses"), member(publisher, "uses")) || !sameYAML(member(after, "with"), member(publisher, "with")) {
		return fmt.Errorf("unsupported reusable publisher; retain the pinned ACR workflow and its exact root inputs")
	}
	if err := preserveWorkflowFields(before, after, false, "runs-on", "steps", "uses", "with"); err != nil {
		return err
	}
	for i := 0; i < len(after.Content); i += 2 {
		field, value := after.Content[i].Value, after.Content[i+1]
		valid := true
		switch field {
		case "name":
			valid = workflowScalar(value, "!!str")
		case "if":
			valid = workflowScalar(value, "!!str", "!!bool")
		case "needs":
			valid = workflowScalar(value, "!!str")
			if value.Kind == yaml.SequenceNode {
				valid = true
				for _, item := range value.Content {
					valid = valid && workflowScalar(item, "!!str")
				}
			}
		case "permissions":
			valid = workflowScalar(value, "!!str") && (value.Value == "read-all" || value.Value == "write-all")
			if workflowStringMap(value) {
				valid = true
				for j := 1; j < len(value.Content); j += 2 {
					valid = valid && slices.Contains([]string{"read", "write", "none"}, value.Content[j].Value)
				}
			}
		case "concurrency":
			valid = workflowScalar(value, "!!str")
			if keysOnly(value, "group", "cancel-in-progress") {
				cancel := member(value, "cancel-in-progress")
				valid = workflowScalar(member(value, "group"), "!!str") && (cancel == nil || workflowScalar(cancel, "!!str", "!!bool"))
			}
		case "strategy":
			valid = keysOnly(value, "matrix", "fail-fast", "max-parallel")
			if matrix := member(value, "matrix"); matrix != nil {
				valid = valid && (matrix.Kind == yaml.MappingNode || workflowScalar(matrix, "!!str"))
			}
			if failFast := member(value, "fail-fast"); failFast != nil {
				valid = valid && workflowScalar(failFast, "!!str", "!!bool")
			}
			if parallel := member(value, "max-parallel"); parallel != nil {
				valid = valid && workflowScalar(parallel, "!!str", "!!int")
			}
		}
		if !valid {
			return fmt.Errorf("unsupported reusable publisher %s representation; preserve it in a supported job shape", field)
		}
	}
	if !reusablePublisherContentsWrite(after, workflow) {
		return fmt.Errorf("unsupported reusable publisher permissions: explicit effective caller permissions must grant contents: write; preserve original permissions in a supported publisher shape")
	}
	return nil
}

func workflowScalar(node *yaml.Node, tags ...string) bool {
	return node != nil && node.Kind == yaml.ScalarNode && slices.Contains(tags, node.Tag)
}

func workflowStringMap(node *yaml.Node) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i < len(node.Content); i += 2 {
		if !workflowScalar(node.Content[i], "!!str") || !workflowScalar(node.Content[i+1], "!!str") {
			return false
		}
	}
	return true
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
	return serviceAction(uses)
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
	for _, steps := range []*yaml.Node{oldSteps, newSteps} {
		if steps == nil || steps.Kind != yaml.SequenceNode || len(steps.Content) == 0 {
			return fmt.Errorf("unsupported retained steps representation; retain a sequence of executable steps")
		}
		for _, step := range steps.Content {
			if step.Kind != yaml.MappingNode {
				return fmt.Errorf("unsupported retained step representation; retain step mappings")
			}
		}
	}
	supportingCheckout := deliveryJobKind(before) != independentDeliveryJob
	position := 0
	for _, oldStep := range oldSteps.Content {
		// Checkout is supporting content only inside a proven service-only
		// job; its display name may carry an established paid-retirement notice.
		if serviceOnlyStep(oldStep) || supportingCheckout && actionIdentity(scalar(oldStep, "uses")) == "actions/checkout" {
			if id := scalar(oldStep, "id"); id != "" && !unchangedStep(oldStep, newSteps) && workflowReferences(after, "steps", id) {
				return fmt.Errorf("referenced service step %q must remain unchanged", id)
			}
			continue
		}
		retained := false
		if newSteps != nil {
			for position < len(newSteps.Content) {
				newStep := newSteps.Content[position]
				position++
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

// Match complete remote identities. Tags and compiled SHAs share this contract.
func actionIdentity(uses string) string {
	identity, ref, found := strings.Cut(uses, "@")
	if !found || ref == "" || strings.ContainsAny(ref, "@ \t\r\n") {
		return ""
	}
	return identity
}

func serviceAction(uses string) bool {
	switch actionIdentity(uses) {
	case "tesslio/setup-tessl", "tesslio/patch-version-publish", "jbaruch/coding-policy/.github/actions/skill-review":
		return true
	}
	return false
}

func unchangedStep(before, sequence *yaml.Node) bool {
	if sequence != nil {
		for _, step := range sequence.Content {
			if sameYAML(before, step) {
				return true
			}
		}
	}
	return false
}

// Look only for references affected by retirement. This is a lexical context
// check, not an expression evaluator or a validator of pre-existing references.
// Dynamic indexes, wildcards and whole-context access cannot prove independence.
func expressionReferences(expression, context, id string) bool {
	for i := 0; i < len(expression); {
		if expression[i] == '\'' || expression[i] == '"' {
			quote := expression[i]
			i++
			for i < len(expression) {
				if expression[i] == quote {
					i++
					if i < len(expression) && expression[i] == quote {
						i++
						continue
					}
					break
				}
				i++
			}
			continue
		}
		start := i
		if !expressionIdentifier(expression[i]) {
			i++
			continue
		}
		for i < len(expression) && expressionIdentifier(expression[i]) {
			i++
		}
		if !strings.EqualFold(expression[start:i], context) || start > 0 && expression[start-1] == '.' {
			continue
		}
		tail := strings.TrimSpace(expression[i:])
		var producer string
		switch {
		case strings.HasPrefix(tail, "."):
			tail = strings.TrimSpace(tail[1:])
			end := 0
			for end < len(tail) && expressionIdentifier(tail[end]) {
				end++
			}
			if end == 0 {
				return true
			}
			producer = tail[:end]
		case strings.HasPrefix(tail, "["):
			tail = strings.TrimSpace(tail[1:])
			if len(tail) == 0 || tail[0] != '\'' && tail[0] != '"' {
				return true
			}
			quote := tail[0]
			end := strings.IndexByte(tail[1:], quote)
			if end < 0 {
				return true
			}
			producer = tail[1 : 1+end]
			if !strings.HasPrefix(strings.TrimSpace(tail[end+2:]), "]") {
				return true
			}
		default:
			return true
		}
		if strings.EqualFold(producer, id) {
			return true
		}
	}
	return false
}

func expressionIdentifier(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func workflowReferences(node *yaml.Node, context, id string) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.ScalarNode {
		expressions, complete := workflowExpressions(node.Value)
		if !complete {
			return true
		}
		for _, expression := range expressions {
			if expressionReferences(expression, context, id) {
				return true
			}
		}
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i].Value, node.Content[i+1]
			if key == "if" && value.Kind == yaml.ScalarNode && !strings.Contains(value.Value, "${{") && expressionReferences(value.Value, context, id) {
				return true
			}
		}
	}
	for _, child := range node.Content {
		if workflowReferences(child, context, id) {
			return true
		}
	}
	return false
}

// Actions string literals use single quotes, with doubled quotes for escaping.
// An incomplete or nested opening cannot prove independence from a producer
// being retired. Callers only ask this question when retirement is affected.
func workflowExpressions(value string) ([]string, bool) {
	var expressions []string
	for {
		start := strings.Index(value, "${{")
		if start < 0 {
			return expressions, true
		}
		value = value[start+3:]
		quoted, end := false, -1
		for i := 0; i < len(value); i++ {
			if value[i] == '\'' {
				if quoted && i+1 < len(value) && value[i+1] == '\'' {
					i++
					continue
				}
				quoted = !quoted
			} else if !quoted {
				if strings.HasPrefix(value[i:], "${{") {
					return nil, false
				}
				if strings.HasPrefix(value[i:], "}}") {
					end = i
					break
				}
			}
		}
		if end < 0 {
			return nil, false
		}
		expressions = append(expressions, value[:end])
		value = value[end+2:]
	}
}

func jobReferences(workflow, jobs *yaml.Node, id string) bool {
	if jobs != nil {
		for i := 1; i < len(jobs.Content); i += 2 {
			needs := member(jobs.Content[i], "needs")
			if needs == nil {
				continue
			}
			if needs.Kind == yaml.ScalarNode && strings.EqualFold(needs.Value, id) {
				return true
			}
			for _, item := range needs.Content {
				if strings.EqualFold(item.Value, id) {
					return true
				}
			}
		}
	}
	return workflowReferences(workflow, "needs", id) || workflowReferences(workflow, "jobs", id)
}
