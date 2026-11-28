package producerconvert

import (
	"strings"

	"go.yaml.in/yaml/v4"
)

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
