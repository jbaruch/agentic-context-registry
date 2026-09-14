package producerconvert

import (
	"path"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

var foreignTestStateNames = regexp.MustCompile(`tessl(?:-lock|-package)?\.json`)

// Refusals recognize literal operation syntax, never infer arbitrary behavior.
var semanticPatterns = []struct {
	pattern *regexp.Regexp
	reason  string
}{
	{regexp.MustCompile("(?m)(?:^|[;&|()`]|\\b(?:exec|command|env|sudo|then|do|if)\\s+)\\s*tessl(?:\\s|$)|[\"']tessl[\"']"), "unknown or custom Tessl command requires semantic conversion"},
	{regexp.MustCompile(`(?i)\btessl\s+(?:--?[^\s]+\s+)*(install|uninstall|update|publish|review|login|plugin|lint|init|tile|build)\b`), "Tessl command/dependency operation has no deterministic semantic translation"},
	{regexp.MustCompile(`(?i)(?:tessl(?:-lock|-package)?\.json|\.tessl-plugin/plugin\.json)`), "Tessl configuration/manifest reference requires semantic conversion; state, pins and rollback cannot be inferred"},
	{regexp.MustCompile(`\bTESSL_[A-Z0-9_]+\b`), "custom Tessl environment or dynamic path operation requires semantic conversion"},
	{regexp.MustCompile(`\.tessl/(?:RULES\.md|tiles/|config|cache|plugins/\$)`), "custom Tessl state or dynamic installed path requires semantic conversion"},
}

// Runtime/support files can construct a metadata path from separate components,
// using either platform's separators. Recognize the explicit directory component
// without interpreting the program. Markdown instructions receive the same check
// on the code a reader copies and runs; their prose and legal notices keep the
// literal-operation checks above.
var retiredMetadataDirectory = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_.-])\.tessl-plugin(?:$|[^a-z0-9_.-])`)

func runtimeSemanticOperation(data []byte) string {
	data = publicRepositoryURLs.ReplaceAll(data, nil)
	return retiredMetadataReason(semanticOperation(data), data, "explicit retired .tessl-plugin directory reference requires semantic conversion")
}

func instructionSemanticOperation(data []byte) string {
	data = publicRepositoryURLs.ReplaceAll(data, nil)
	return retiredMetadataReason(semanticOperation(data), markdownCode(data), "executable Markdown example references the retired .tessl-plugin directory and requires semantic conversion")
}

func retiredMetadataReason(reason string, code []byte, subject string) string {
	if !retiredMetadataDirectory.Match(code) {
		return reason
	}
	if reason != "" {
		reason += "; "
	}
	return reason + subject + "; preserve metadata-dependent behavior before removing its manifest"
}

func semanticOperation(data []byte) string {
	data = publicRepositoryURLs.ReplaceAll(data, nil)
	var reasons []string
	for _, rule := range semanticPatterns {
		if rule.pattern.Match(data) {
			reasons = append(reasons, rule.reason)
		}
	}
	return strings.Join(reasons, "; ")
}

func workflowFile(name string) bool {
	return strings.HasPrefix(name, ".github/workflows/") && (strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml"))
}

// Recognize decoded operations and complete action identities, including JSON
// lock keys. Historical URLs/comments alone confer no editing authority.
func workflowSemantic(body []byte) bool {
	var document yaml.Node
	if err := yaml.Unmarshal(body, &document); err == nil {
		var active func(*yaml.Node) bool
		active = func(node *yaml.Node) bool {
			if node.Kind == yaml.ScalarNode && (serviceAction(node.Value) || runtimeSemanticOperation(publicRepositoryURLs.ReplaceAll([]byte(node.Value), nil)) != "") {
				return true
			}
			for _, child := range node.Content {
				if active(child) {
					return true
				}
			}
			return false
		}
		return active(&document)
	}
	return runtimeSemanticOperation(publicRepositoryURLs.ReplaceAll(body, nil)) != ""
}

var publicRepositoryURLs = regexp.MustCompile("https?://(?:github\\.com|gitlab\\.com|bitbucket\\.org|raw\\.githubusercontent\\.com)/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+[^\\s\"'`<>()\\[\\]{}]*")

func repositoryURLCounts(body []byte) map[string]int {
	counts := map[string]int{}
	for _, token := range publicRepositoryURLs.FindAllString(string(body), -1) {
		counts[token]++
	}
	return counts
}

func semanticScope(name string) string {
	if strings.HasPrefix(name, ".github/") {
		return "delivery"
	}
	if strings.HasPrefix(name, "tests/") || strings.Contains(name, "/templates/") || strings.HasSuffix(name, "_TEMPLATE.md") {
		return "runtime"
	}
	if strings.EqualFold(path.Ext(name), ".md") {
		return "instructions"
	}
	return "runtime"
}
