package producerconvert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// A filename alone cannot authorize description changes. Bind the original
// supported metadata to source bytes before preservation grants that exception.
func verifiedGHWorkflowPair(original tree, name string) bool {
	if !strings.HasPrefix(name, ".github/workflows/") || !strings.HasSuffix(name, ".lock.yml") {
		return false
	}
	source, exists := original[strings.TrimSuffix(name, ".lock.yml")+".md"]
	if !exists {
		return false
	}
	meta, _, err := ghWorkflowMetadata(original[name].Content)
	if err != nil || meta["schema_version"] != "v3" || meta["compiler_version"] != "v0.71.5" {
		return false
	}
	hash, _, err := ghWorkflowHash(source.Content)
	return err == nil && meta["frontmatter_hash"] == hash
}

// Refresh only the verified gh-aw v3 contract. Original metadata must bind the
// original source; changed custom steps and descriptions must agree with the
// compiled workflow before ACR computes a new hash. No compiler is executed.
func reconcileGHWorkflowMetadata(before, after tree) error {
	for _, name := range sortedPaths(before) {
		oldLock := before[name]
		if !strings.HasPrefix(name, ".github/workflows/") || !strings.HasSuffix(name, ".lock.yml") || !bytes.HasPrefix(oldLock.Content, []byte("# gh-aw-metadata: ")) {
			continue
		}
		source := strings.TrimSuffix(name, ".lock.yml") + ".md"
		oldSource, exists := before[source]
		nextSource, retained := after[source]
		nextLock, locked := after[name]
		if !exists || !locked {
			continue
		} // Independent job retention is checked separately.
		if !retained {
			return fmt.Errorf("%s: retain paired workflow source %s", name, source)
		}
		if bytes.Equal(oldSource.Content, nextSource.Content) && bytes.Equal(oldLock.Content, nextLock.Content) {
			continue
		}
		oldMeta, _, err := ghWorkflowMetadata(oldLock.Content)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		nextMeta, tail, err := ghWorkflowMetadata(nextLock.Content)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if oldMeta["schema_version"] != "v3" || oldMeta["compiler_version"] != "v0.71.5" {
			return fmt.Errorf("%s: unsupported compiled workflow metadata contract", name)
		}
		originalHash, oldFront, err := ghWorkflowHash(oldSource.Content)
		if err != nil {
			return fmt.Errorf("%s: %w", source, err)
		}
		if oldMeta["frontmatter_hash"] != originalHash {
			return fmt.Errorf("%s: original compiled workflow hash does not match source", source)
		}
		hash, front, err := ghWorkflowHash(nextSource.Content)
		if err != nil {
			return fmt.Errorf("%s: %w", source, err)
		}
		delete(oldMeta, "frontmatter_hash")
		delete(nextMeta, "frontmatter_hash")
		oldJSON, err := json.Marshal(oldMeta)
		if err != nil {
			return err
		}
		nextJSON, err := json.Marshal(nextMeta)
		if err != nil {
			return err
		}
		if !bytes.Equal(oldJSON, nextJSON) {
			return fmt.Errorf("%s: compiled workflow identity/version metadata changed", name)
		}
		// Changes beyond custom setup and description require a real compiler, not
		// a guessed update to generated controls, tools, engine or permissions.
		for _, top := range []*yaml.Node{oldFront, front} {
			for i := 0; i < len(top.Content); i += 2 {
				key := top.Content[i].Value
				if key != "description" && key != "steps" && !sameYAML(member(oldFront, key), member(front, key)) {
					return fmt.Errorf("%s: compiled configuration %s changed outside supported setup/description conversion", source, key)
				}
			}
		}
		var document yaml.Node
		if err := yaml.Unmarshal(nextLock.Content, &document); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := closedYAML(&document); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if len(document.Content) != 1 {
			return fmt.Errorf("%s: expected one compiled workflow", name)
		}
		jobs := member(document.Content[0], "jobs")
		steps := member(front, "steps")
		if steps != nil {
			if steps.Kind != yaml.SequenceNode {
				return fmt.Errorf("%s: custom steps must be a sequence", source)
			}
			for _, step := range steps.Content {
				found := false
				if jobs != nil {
					for i := 1; i < len(jobs.Content); i += 2 {
						if sequence := member(jobs.Content[i], "steps"); sequence != nil {
							for _, candidate := range sequence.Content {
								if ghStepMatches(step, candidate) {
									found = true
								}
							}
						}
					}
				}
				if !found {
					return fmt.Errorf("%s: custom step %q is missing or differs in %s", source, scalar(step, "name"), name)
				}
			}
		}
		description := scalar(front, "description")
		var checkDescription func(*yaml.Node) error
		checkDescription = func(node *yaml.Node) error {
			if node.Kind == yaml.MappingNode {
				for i := 0; i < len(node.Content); i += 2 {
					if node.Content[i].Value == "WORKFLOW_DESCRIPTION" && strings.TrimSpace(node.Content[i+1].Value) != strings.TrimSpace(description) {
						return fmt.Errorf("%s: WORKFLOW_DESCRIPTION differs from %s", name, source)
					}
				}
			}
			for _, child := range node.Content {
				if err := checkDescription(child); err != nil {
					return err
				}
			}
			return nil
		}
		if err := checkDescription(&document); err != nil {
			return err
		}
		nextMeta["frontmatter_hash"] = hash
		encoded, err := json.Marshal(nextMeta)
		if err != nil {
			return err
		}
		nextLock.Content = append(append([]byte("# gh-aw-metadata: "), encoded...), append([]byte("\n"), tail...)...)
		nextLock.Digest = digest(nextLock.Content)
		after[name] = nextLock
	}
	return nil
}

func ghWorkflowMetadata(data []byte) (map[string]any, []byte, error) {
	line, tail, found := bytes.Cut(data, []byte("\n"))
	raw, ok := bytes.CutPrefix(line, []byte("# gh-aw-metadata: "))
	if !found || !ok {
		return nil, nil, fmt.Errorf("missing compiled workflow metadata")
	}
	var meta map[string]any
	if err := strictJSON(raw, &meta); err != nil {
		return nil, nil, err
	}
	return meta, tail, nil
}

// Matches github/gh-aw v0.71.5 pkg/parser/frontmatter_hash.go's text-based
// algorithm. Imports and inlined bodies require additional compilation inputs
// and are deliberately refused instead of reading any path outside the plan.
func ghWorkflowHash(data []byte) (string, *yaml.Node, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return "", nil, fmt.Errorf("missing workflow frontmatter")
	}
	end := 0
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == 0 {
		return "", nil, fmt.Errorf("unclosed workflow frontmatter")
	}
	front := strings.Join(lines[1:end], "\n")
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(front), &document); err != nil {
		return "", nil, err
	}
	if err := closedYAML(&document); err != nil {
		return "", nil, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return "", nil, fmt.Errorf("workflow frontmatter requires a mapping")
	}
	node := document.Content[0]
	if member(node, "imports") != nil || member(node, "inlined-imports") != nil {
		return "", nil, fmt.Errorf("compiled workflow imports require an unsupported compiler input")
	}
	canonical := map[string]any{"frontmatter-text": strings.TrimSpace(front)}
	body := strings.Join(lines[end+1:], "\n")
	expressions := map[string]bool{}
	for _, match := range ghTemplateExpressions.FindAllStringSubmatch(body, -1) {
		if strings.Contains(match[1], "env.") || strings.Contains(match[1], "vars.") {
			expressions[match[0]] = true
		}
	}
	if len(expressions) > 0 {
		values := make([]string, 0, len(expressions))
		for value := range expressions {
			values = append(values, value)
		}
		sort.Strings(values)
		canonical["template-expressions"] = values
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(canonical); err != nil {
		return "", nil, err
	}
	return strings.TrimPrefix(digest(bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))), "sha256:"), node, nil
}

var ghTemplateExpressions = regexp.MustCompile(`\$\{\{(.*?)\}\}`)

func ghStepMatches(source, compiled *yaml.Node) bool {
	if source.Kind != yaml.MappingNode || compiled.Kind != yaml.MappingNode {
		return false
	}
	if len(source.Content) != len(compiled.Content) {
		return false
	}
	for i := 0; i < len(source.Content); i += 2 {
		key := source.Content[i].Value
		if key == "run" {
			if strings.TrimRight(source.Content[i+1].Value, "\r\n") != strings.TrimRight(scalar(compiled, key), "\r\n") {
				return false
			}
			continue
		}
		if key == "uses" {
			left, _, _ := strings.Cut(source.Content[i+1].Value, "@")
			right, _, _ := strings.Cut(scalar(compiled, key), "@")
			if left != right {
				return false
			}
			continue
		}
		if !sameYAML(source.Content[i+1], member(compiled, key)) {
			return false
		}
	}
	return true
}
