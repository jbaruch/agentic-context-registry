package tesslplugin

import (
	"bytes"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"go.yaml.in/yaml/v3"
)

const emDash = "\u2014"

type ruleFrontmatter struct {
	AlwaysApply *bool  `yaml:"alwaysApply"`
	ApplyTo     string `yaml:"applyTo"`
	Globs       string `yaml:"globs"`
	Paths       string `yaml:"paths"`
	Description string `yaml:"description"`
}

// LossyItem is a Tessl concept copied into the report and omitted from YAML.
type LossyItem struct {
	ID     string `json:"id,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Field  string `json:"field"`
	Value  string `json:"value,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type ruleActivation struct {
	Activation manifest.RuleActivation
	Lossy      []LossyItem
}

func activationFromRuleFile(relative string, content []byte) (ruleActivation, error) {
	metadata, _, hasFrontmatter := splitFirstFrontmatter(content)
	if !hasFrontmatter {
		return ruleActivation{}, conversionError(string(manifest.CodeInvalidRuleActivation), relative,
			"rule %s has no YAML frontmatter; set alwaysApply and convert again", relative)
	}
	var frontmatter ruleFrontmatter
	if err := yaml.Unmarshal(metadata, &frontmatter); err != nil {
		return ruleActivation{}, conversionError(string(manifest.CodeInvalidRuleActivation), relative,
			"decode frontmatter in %s: %v; repair the YAML and convert again", relative, err)
	}
	return activationFromFrontmatter(relative, frontmatter)
}

func activationFromFrontmatter(relative string, frontmatter ruleFrontmatter) (ruleActivation, error) {
	var lossy []LossyItem
	if frontmatter.Description != "" {
		lossy = append(lossy, LossyItem{ID: "", Kind: "rule", Field: relative + "#description", Value: frontmatter.Description, Reason: "description"})
	}
	scoped := firstNonEmpty(frontmatter.ApplyTo, frontmatter.Globs, frontmatter.Paths)
	if frontmatter.AlwaysApply != nil && *frontmatter.AlwaysApply {
		if prose := applyToProse(scoped); prose != "" {
			lossy = append(lossy, LossyItem{Kind: "rule", Field: relative + "#applyTo", Value: prose, Reason: "applyTo-prose"})
		}
		return ruleActivation{Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}, Lossy: lossy}, nil
	}
	if frontmatter.AlwaysApply == nil || *frontmatter.AlwaysApply {
		return ruleActivation{}, conversionError(string(manifest.CodeInvalidRuleActivation), relative,
			"rule %s must set alwaysApply; activation is not a Tessl manifest field", relative)
	}
	if scoped == "" {
		return ruleActivation{}, conversionError(string(manifest.CodeInvalidRuleActivation), relative,
			"rule %s sets alwaysApply false with no applyTo/globs/paths; add an em-dash-separated glob list", relative)
	}
	if !strings.Contains(scoped, emDash) {
		return ruleActivation{}, conversionError(string(manifest.CodeInvalidRuleActivation), relative,
			"rule %s applyTo/globs/paths must split glob and prose on an em dash; activation cannot be guessed", relative)
	}
	paths, prose, ok := parseApplyTo(scoped)
	if !ok {
		return ruleActivation{}, conversionError(string(manifest.CodeInvalidRuleActivation), relative,
			"rule %s applyTo glob half is empty; declare at least one package-relative glob", relative)
	}
	if prose != "" {
		lossy = append(lossy, LossyItem{Kind: "rule", Field: relative + "#applyTo", Value: prose, Reason: "applyTo-prose"})
	}
	return ruleActivation{
		Activation: manifest.RuleActivation{Mode: manifest.ActivationPaths, Paths: uniquePreserve(paths)},
		Lossy:      lossy,
	}, nil
}

func parseApplyTo(value string) (paths []string, prose string, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, "", false
	}
	globHalf, rest, found := strings.Cut(value, emDash)
	if found {
		prose = strings.TrimSpace(rest)
	} else {
		globHalf = value
	}
	globHalf = strings.TrimSpace(globHalf)
	if globHalf == "" {
		return nil, prose, false
	}
	for _, part := range strings.Split(globHalf, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			paths = append(paths, part)
		}
	}
	return paths, prose, len(paths) != 0
}

func applyToProse(value string) string {
	_, prose, _ := parseApplyTo(value)
	return prose
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func uniquePreserve(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

type physicalLine struct {
	start      int
	contentEnd int
	end        int
}

func splitFirstFrontmatter(content []byte) (metadata, body []byte, ok bool) {
	lines := physicalLines(content)
	if len(lines) == 0 || !bytes.Equal(content[lines[0].start:lines[0].contentEnd], []byte("---")) {
		return nil, content, false
	}
	closing := -1
	for index := 1; index < len(lines); index++ {
		if bytes.Equal(content[lines[index].start:lines[index].contentEnd], []byte("---")) {
			closing = index
			break
		}
	}
	if closing < 0 {
		return nil, content, false
	}
	return content[lines[0].end:lines[closing].start], content[lines[closing].end:], true
}

func physicalLines(content []byte) []physicalLine {
	if len(content) == 0 {
		return nil
	}
	var lines []physicalLine
	for start := 0; start < len(content); {
		newline := bytes.IndexByte(content[start:], '\n')
		if newline < 0 {
			lines = append(lines, physicalLine{start: start, contentEnd: len(content), end: len(content)})
			break
		}
		end := start + newline + 1
		contentEnd := end - 1
		if contentEnd > start && content[contentEnd-1] == '\r' {
			contentEnd--
		}
		lines = append(lines, physicalLine{start: start, contentEnd: contentEnd, end: end})
		start = end
	}
	return lines
}
