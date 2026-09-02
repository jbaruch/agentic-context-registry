package migrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

const (
	rulesIndexPath           = ".tessl/RULES.md"
	lossyApplyToProse        = "applyTo prose clause"
	lossyDescription         = "description"
	reasonMissingRule        = "missing-rule"
	reasonMdcDrift           = "mdc-drift"
	reasonUnparsedApplyTo    = "unparsed-applyTo"
	reasonUnparsedActivation = "unparsed-activation"
)

// NormalizedRule is one Tessl rule on the #4 artifact model.
type NormalizedRule struct {
	ID          string
	Path        string
	Activation  manifest.RuleActivation
	Digest      string
	Lossy       []string
	Natives     []string
	Ambiguous   bool
	Reason      string
	SourceBytes []byte
}

type ruleFrontmatter struct {
	AlwaysApply *bool  `yaml:"alwaysApply"`
	ApplyTo     string `yaml:"applyTo"`
	Globs       string `yaml:"globs"`
	Paths       string `yaml:"paths"`
	Description string `yaml:"description"`
}

type physicalLine struct {
	start      int
	contentEnd int
	end        int
}

// NormalizeRules reads source frontmatter, Cursor .mdc identity, and RULES.md
// includes for one installed package.
func NormalizeRules(snapshot adapter.Snapshot, install PackageInstall) ([]NormalizedRule, error) {
	index, err := rulesIndexByPackage(snapshot)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]NormalizedRule)
	for _, declared := range install.Rules {
		rule, err := normalizeDeclaredRule(snapshot, install, declared)
		if err != nil {
			return nil, err
		}
		byID[rule.ID] = rule
	}
	for _, relative := range index[install.TesslIdentity] {
		id := ruleID(relative)
		rule, ok := byID[id]
		if !ok {
			rule = NormalizedRule{ID: id, Path: relative}
		}
		full := posixJoin(install.Root, relative)
		content, present, err := readOptional(snapshot, full)
		if err != nil {
			return nil, err
		}
		if !present {
			rule.Ambiguous = true
			rule.Reason = reasonMissingRule
		} else if rule.Digest == "" {
			normalized, err := ruleFromSource(relative, content)
			if err != nil {
				return nil, err
			}
			normalized.ID = id
			rule = normalized
		}
		byID[id] = rule
	}
	rules := make([]NormalizedRule, 0, len(byID))
	for _, rule := range byID {
		natives, err := ruleNatives(snapshot, install.TesslIdentity, rule)
		if err != nil {
			return nil, err
		}
		rule.Natives = natives
		drifted, err := mdcDrifted(snapshot, rule)
		if err != nil {
			return nil, err
		}
		if drifted {
			rule.Ambiguous = true
			if rule.Reason == "" {
				rule.Reason = reasonMdcDrift
			}
		}
		rules = append(rules, rule)
	}
	sort.Slice(rules, func(left, right int) bool { return rules[left].ID < rules[right].ID })
	return rules, nil
}

func normalizeDeclaredRule(snapshot adapter.Snapshot, install PackageInstall, declared DeclaredPath) (NormalizedRule, error) {
	if !validPluginRelPath(declared.Path) {
		return NormalizedRule{}, fmt.Errorf("declared rule path %q is not a package-relative POSIX path", declared.Path)
	}
	rule := NormalizedRule{ID: declared.ID, Path: declared.Path, Ambiguous: declared.Ambiguous}
	content, present, err := readOptional(snapshot, posixJoin(install.Root, declared.Path))
	if err != nil {
		return NormalizedRule{}, err
	}
	if !present {
		rule.Ambiguous = true
		rule.Reason = reasonMissingRule
		return rule, nil
	}
	normalized, err := ruleFromSource(declared.Path, content)
	if err != nil {
		return NormalizedRule{}, err
	}
	normalized.ID = declared.ID
	normalized.Ambiguous = declared.Ambiguous || normalized.Ambiguous
	if declared.Ambiguous && normalized.Reason == "" {
		normalized.Reason = "manifest-disagreement"
	}
	return normalized, nil
}

func ruleFromSource(relative string, content []byte) (NormalizedRule, error) {
	rule := NormalizedRule{Path: relative, SourceBytes: append([]byte(nil), content...)}
	metadata, body, hasFrontmatter := splitFirstFrontmatter(content)
	if !hasFrontmatter {
		rule.Activation = manifest.RuleActivation{Mode: manifest.ActivationAlways}
		rule.Digest = contentDigest(content)
		return rule, nil
	}
	var frontmatter ruleFrontmatter
	if err := yaml.Unmarshal(metadata, &frontmatter); err != nil {
		rule.Ambiguous = true
		rule.Reason = reasonUnparsedActivation
		rule.Digest = contentDigest(body)
		return rule, nil
	}
	activation, lossy, reason, ok := activationFromFrontmatter(frontmatter)
	rule.Activation = activation
	rule.Lossy = lossy
	rule.Digest = contentDigest(body)
	if !ok {
		rule.Ambiguous = true
		rule.Reason = reason
	}
	return rule, nil
}

func activationFromFrontmatter(frontmatter ruleFrontmatter) (manifest.RuleActivation, []string, string, bool) {
	var lossy []string
	scoped := firstNonEmpty(frontmatter.ApplyTo, frontmatter.Globs, frontmatter.Paths)
	if frontmatter.Description != "" {
		lossy = append(lossy, lossyDescription)
	}
	if frontmatter.AlwaysApply != nil && *frontmatter.AlwaysApply {
		if prose := applyToProse(scoped); prose != "" {
			lossy = append(lossy, lossyApplyToProse)
		}
		return manifest.RuleActivation{Mode: manifest.ActivationAlways}, lossy, "", true
	}
	if scoped == "" {
		if frontmatter.AlwaysApply != nil && !*frontmatter.AlwaysApply {
			return manifest.RuleActivation{}, lossy, reasonUnparsedApplyTo, false
		}
		return manifest.RuleActivation{Mode: manifest.ActivationAlways}, lossy, "", true
	}
	paths, prose, ok := parseApplyTo(scoped)
	if prose != "" {
		lossy = append(lossy, lossyApplyToProse)
	}
	if !ok {
		return manifest.RuleActivation{}, lossy, reasonUnparsedApplyTo, false
	}
	return manifest.RuleActivation{Mode: manifest.ActivationPaths, Paths: paths}, lossy, "", true
}

func parseApplyTo(value string) (paths []string, prose string, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, "", false
	}
	globHalf, rest, found := strings.Cut(value, "\u2014")
	if !found {
		globHalf, rest, found = strings.Cut(value, " -- ")
	}
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

func rulesIndexByPackage(snapshot adapter.Snapshot) (map[string][]string, error) {
	content, present, err := readOptional(snapshot, rulesIndexPath)
	if err != nil || !present {
		return map[string][]string{}, err
	}
	result := make(map[string][]string)
	for _, line := range bytes.Split(content, []byte("\n")) {
		token := firstAtToken(line)
		identity, relative, ok := parsePluginRuleInclude(token)
		if !ok {
			continue
		}
		if !validPluginRelPath(relative) || !strings.HasPrefix(relative, "rules/") {
			return nil, fmt.Errorf("RULES.md include %q is not a package-relative rules path; repair %s", token, rulesIndexPath)
		}
		result[identity] = append(result[identity], relative)
	}
	return result, nil
}

func firstAtToken(line []byte) string {
	trimmed := strings.TrimLeftFunc(string(line), unicode.IsSpace)
	if trimmed == "" || trimmed[0] != '@' {
		return ""
	}
	token := trimmed
	if index := strings.IndexAny(token, " \t"); index >= 0 {
		token = token[:index]
	}
	return token
}

func parsePluginRuleInclude(token string) (identity, relative string, ok bool) {
	const prefix = "@plugins/"
	if !strings.HasPrefix(token, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(token, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) < 4 || parts[2] != "rules" {
		return "", "", false
	}
	identity = parts[0] + "/" + parts[1]
	relative = strings.Join(parts[2:], "/")
	return identity, relative, true
}

func ruleNatives(snapshot adapter.Snapshot, identity string, rule NormalizedRule) ([]string, error) {
	filename := cursorRuleNative(identity, rule.ID)
	_, present, err := readOptional(snapshot, filename)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	return []string{filename}, nil
}

func cursorRuleNative(identity, id string) string {
	workspace, pkg, found := strings.Cut(identity, "/")
	if !found {
		return path.Join(".cursor/rules", "tessl__rule__"+identity+"__"+id+".mdc")
	}
	return path.Join(".cursor/rules", "tessl__rule__"+workspace+"__"+pkg+"__"+id+".mdc")
}

func mdcDrifted(snapshot adapter.Snapshot, rule NormalizedRule) (bool, error) {
	if len(rule.Natives) == 0 || len(rule.SourceBytes) == 0 {
		return false, nil
	}
	content, present, err := readOptional(snapshot, rule.Natives[0])
	if err != nil || !present {
		return false, err
	}
	remainder, ok := stripOneFrontmatterAndSeparator(content)
	if !ok {
		return true, nil
	}
	return !bytes.Equal(remainder, rule.SourceBytes), nil
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

func stripOneFrontmatterAndSeparator(content []byte) ([]byte, bool) {
	_, body, ok := splitFirstFrontmatter(content)
	if !ok {
		return nil, false
	}
	switch {
	case bytes.HasPrefix(body, []byte("\r\n")):
		return body[2:], true
	case bytes.HasPrefix(body, []byte("\n")):
		return body[1:], true
	default:
		return body, true
	}
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

func contentDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}
