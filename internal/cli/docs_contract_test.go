package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestCLIReferenceMatchesCommandSurface(t *testing.T) {
	t.Parallel()

	root := docsRepositoryRoot(t)
	rows := markdownTable(t, filepath.Join(root, "docs", "cli.md"), "## Commands")
	actual := make(map[string]bool, len(rows))
	for _, row := range rows {
		actual[plainCode(row[0])] = true
	}

	expected := map[string]bool{
		"acr version [--json]": true,
		"acr help [COMMAND]":   true,
	}
	for _, command := range commandOrder {
		for _, usage := range strings.Split(commandSpecs[command].usage, "\n") {
			expected[strings.TrimSpace(usage)] = true
		}
	}
	assertStringSet(t, "documented command rows", actual, expected)

	document := readDocsFile(t, filepath.Join(root, "docs", "cli.md"))
	for _, universal := range []string{"--help", "--json", "--project"} {
		if !strings.Contains(document, universal) {
			t.Errorf("CLI reference does not document universal flag %s", universal)
		}
	}
	for flag := range parsedLongFlags(t, filepath.Join(root, "internal", "cli", "parse.go")) {
		if !strings.Contains(document, flag) {
			t.Errorf("CLI reference does not document parsed flag %s", flag)
		}
	}
}

func TestSafetyMatrixMatchesMutatingCommandSurface(t *testing.T) {
	t.Parallel()

	root := docsRepositoryRoot(t)
	rows := markdownTable(t, filepath.Join(root, "docs", "safety.md"), "## Command matrix")
	actual := make(map[string]bool, len(rows))
	for _, row := range rows {
		if len(row) != 5 {
			t.Fatalf("safety row has %d cells, want 5: %q", len(row), row)
		}
		for column, cell := range row {
			value := strings.TrimSpace(cell)
			if value == "" || value == "—" || value == "-" || value == "TBD" {
				t.Errorf("safety row %q column %d has no contract", row[0], column+1)
			}
		}
		actual[plainCode(row[0])] = true
	}

	expected := map[string]bool{}
	bases := map[Command][]string{
		CommandInit:      {"acr init"},
		CommandInstall:   {"acr install [SOURCE[@VERSION]]"},
		CommandRealize:   {"acr realize"},
		CommandList:      {"acr list"},
		CommandOutdated:  {"acr outdated"},
		CommandFreshness: {"acr freshness run"},
		CommandUpdate:    {"acr update [SOURCE]"},
		CommandResume:    {"acr resume SOURCE"},
		CommandUninstall: {"acr uninstall SOURCE"},
		CommandCheck:     {"acr check"},
		CommandPublish:   {"acr publish [PATH]"},
		CommandMigrate:   {"acr migrate tessl", "acr migrate tessl-plugin [PATH]"},
	}
	for _, command := range commandOrder {
		for _, base := range bases[command] {
			expected[base] = true
		}
		for _, flag := range mutatingHelpFlags(helpFor(command)) {
			base := bases[command][0]
			if command == CommandMigrate && (flag == "--repository" || flag == "--accept-agent-widening") {
				base = bases[command][1]
			}
			expected[safetyFlagKey(base, flag)] = true
		}
	}
	assertStringSet(t, "safety command rows", actual, expected)

	document := readDocsFile(t, filepath.Join(root, "docs", "safety.md"))
	headings := regexp.MustCompile(`(?m)^### (.+)$`).FindAllStringSubmatch(document, -1)
	gotHeadings := make([]string, 0, len(headings))
	for _, heading := range headings {
		gotHeadings = append(gotHeadings, heading[1])
	}
	wantHeadings := []string{"Dependency hold barrier", "Journal recovery", "Migration undo"}
	if !reflect.DeepEqual(gotHeadings, wantHeadings) {
		t.Errorf("rollback headings = %q, want %q", gotHeadings, wantHeadings)
	}
}

func TestMachineReadableCodeRegistriesMatchSourceAndDocs(t *testing.T) {
	t.Parallel()

	refusals := uniqueStrings(t, "RefusalCodes", RefusalCodes)
	notices := uniqueStrings(t, "NoticeCodes", NoticeCodes)
	for code := range refusals {
		if notices[code] {
			t.Errorf("code %q is in both RefusalCodes and NoticeCodes", code)
		}
	}

	registered := make(map[string]bool, len(refusals)+len(notices))
	for code := range refusals {
		registered[code] = true
	}
	for code := range notices {
		registered[code] = true
	}
	root := docsRepositoryRoot(t)
	assertStringSet(t, "machine-readable code registry", registered, sourceCodes(t, filepath.Join(root, "internal")))

	rows := markdownTable(t, filepath.Join(root, "docs", "troubleshooting.md"), "")
	documentedRefusals := map[string]bool{}
	documentedNotices := map[string]bool{}
	inNoticeSection := false
	for _, line := range strings.Split(readDocsFile(t, filepath.Join(root, "docs", "troubleshooting.md")), "\n") {
		if line == "## Notices" {
			inNoticeSection = true
		}
		if strings.HasPrefix(line, "## ") && line != "## Notices" {
			inNoticeSection = false
		}
		if !strings.HasPrefix(line, "|") {
			continue
		}
		row := splitMarkdownRow(line)
		if len(row) < 2 || isMarkdownSeparator(row) {
			continue
		}
		if inNoticeSection {
			code := plainCode(row[0])
			if notices[code] && strings.TrimSpace(row[1]) != "" {
				documentedNotices[code] = true
			}
			continue
		}
		if len(row) == 5 {
			code := plainCode(row[2])
			if refusals[code] {
				documentedRefusals[code] = true
				if !regexp.MustCompile("`[^`]+`").MatchString(row[3]) {
					t.Errorf("refusal %q remedy has no concrete command: %q", code, row[3])
				}
			}
		}
	}
	_ = rows // The parser also rejects malformed or unterminated target tables.
	assertStringSet(t, "documented refusal codes", documentedRefusals, refusals)
	assertStringSet(t, "documented notice codes", documentedNotices, notices)
}

func TestCLIReferenceDocumentsEveryExitCode(t *testing.T) {
	t.Parallel()

	root := docsRepositoryRoot(t)
	rows := markdownTable(t, filepath.Join(root, "docs", "cli.md"), "## Exit codes")
	actual := map[string]bool{}
	for _, row := range rows {
		actual[plainCode(row[0])] = true
	}
	expected := map[string]bool{"0": true}
	for code := 1; code <= 255; code++ {
		if isFailureExitCode(code) {
			expected[strconv.Itoa(code)] = true
		}
	}
	assertStringSet(t, "documented exit codes", actual, expected)
}

func docsRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve docs contract test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func readDocsFile(t *testing.T, filename string) string {
	t.Helper()
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
	return strings.ReplaceAll(strings.TrimPrefix(string(content), "\ufeff"), "\r\n", "\n")
}

func markdownTable(t *testing.T, filename, heading string) [][]string {
	t.Helper()
	lines := strings.Split(readDocsFile(t, filename), "\n")
	inside := heading == ""
	started := false
	var rows [][]string
	for _, line := range lines {
		if line == heading {
			inside = true
			continue
		}
		if !inside {
			continue
		}
		if strings.HasPrefix(line, "## ") && heading != "" {
			break
		}
		if !strings.HasPrefix(line, "|") {
			if started && heading != "" {
				break
			}
			continue
		}
		row := splitMarkdownRow(line)
		if !started {
			started = true
			continue
		}
		if isMarkdownSeparator(row) {
			continue
		}
		rows = append(rows, row)
	}
	if heading != "" && len(rows) == 0 {
		t.Fatalf("no table rows found below %q in %s", heading, filename)
	}
	return rows
}

func splitMarkdownRow(line string) []string {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(line), "|"), "|"))
	var rows []string
	var cell strings.Builder
	escaped := false
	for _, character := range line {
		switch {
		case escaped:
			cell.WriteRune(character)
			escaped = false
		case character == '\\':
			escaped = true
		case character == '|':
			rows = append(rows, strings.TrimSpace(cell.String()))
			cell.Reset()
		default:
			cell.WriteRune(character)
		}
	}
	rows = append(rows, strings.TrimSpace(cell.String()))
	return rows
}

func isMarkdownSeparator(row []string) bool {
	if len(row) == 0 {
		return false
	}
	for _, cell := range row {
		if strings.Trim(strings.TrimSpace(cell), ":-") != "" {
			return false
		}
	}
	return true
}

func plainCode(value string) string {
	return strings.ReplaceAll(strings.TrimSpace(value), "`", "")
}

func parsedLongFlags(t *testing.T, filename string) map[string]bool {
	t.Helper()
	content := readDocsFile(t, filename)
	matches := regexp.MustCompile(`"(--[a-z][a-z-]*)"`).FindAllStringSubmatch(content, -1)
	flags := map[string]bool{}
	for _, match := range matches {
		flags[match[1]] = true
	}
	return flags
}

func mutatingHelpFlags(help string) []string {
	mutable := map[string]bool{
		"--accept-agent-widening": true,
		"--agent":                 true,
		"--finalize":              true,
		"--freshness":             true,
		"--hold":                  true,
		"--map":                   true,
		"--mapping-file":          true,
		"--non-interactive":       true,
		"--pin":                   true,
		"--policy":                true,
		"--repository":            true,
		"--vendor-unmapped":       true,
	}
	var result []string
	for _, line := range strings.Split(help, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 0 && mutable[fields[0]] {
			result = append(result, fields[0])
		}
	}
	return result
}

func safetyFlagKey(base, flag string) string {
	base = strings.ReplaceAll(base, " [SOURCE[@VERSION]]", "")
	base = strings.ReplaceAll(base, " [PATH]", "")
	suffixes := map[string]string{
		"--accept-agent-widening": "--accept-agent-widening",
		"--agent":                 "--agent NAME",
		"--finalize":              "--finalize",
		"--freshness":             "--freshness POLICY",
		"--hold":                  "--hold",
		"--map":                   "--map FROM=SOURCE",
		"--mapping-file":          "--mapping-file PATH",
		"--non-interactive":       "--non-interactive",
		"--pin":                   "--pin",
		"--policy":                "--policy POLICY",
		"--repository":            "--repository URL",
		"--vendor-unmapped":       "--vendor-unmapped",
	}
	return base + " " + suffixes[flag]
}

func uniqueStrings(t *testing.T, name string, values []string) map[string]bool {
	t.Helper()
	result := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" {
			t.Errorf("%s contains an empty code", name)
		}
		if result[value] {
			t.Errorf("%s contains duplicate %q", name, value)
		}
		result[value] = true
	}
	return result
}

func sourceCodes(t *testing.T, root string) map[string]bool {
	t.Helper()
	result := map[string]bool{}
	files := token.NewFileSet()
	err := filepath.WalkDir(root, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(filename, ".go") || strings.HasSuffix(filename, "_test.go") {
			return nil
		}
		parsed, parseErr := parser.ParseFile(files, filename, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.ValueSpec:
				for index, name := range value.Names {
					if !regexp.MustCompile(`^Code[A-Z]`).MatchString(name.Name) || index >= len(value.Values) {
						continue
					}
					if code, ok := stringLiteral(value.Values[index]); ok {
						result[code] = true
					}
				}
			case *ast.KeyValueExpr:
				key, ok := value.Key.(*ast.Ident)
				if !ok || key.Name != "Code" {
					return true
				}
				if code, ok := stringLiteral(value.Value); ok {
					result[code] = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan machine-readable codes: %v", err)
	}
	return result
}

func stringLiteral(expression ast.Expr) (string, bool) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}

func assertStringSet(t *testing.T, name string, actual, expected map[string]bool) {
	t.Helper()
	var missing, extra []string
	for value := range expected {
		if !actual[value] {
			missing = append(missing, value)
		}
	}
	for value := range actual {
		if !expected[value] {
			extra = append(extra, value)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) != 0 || len(extra) != 0 {
		t.Errorf("%s mismatch: missing=%q extra=%q", name, missing, extra)
	}
}
