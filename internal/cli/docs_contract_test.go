package cli

import (
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
		if !documentsLongFlag(document, universal) {
			t.Errorf("CLI reference does not document universal flag %s", universal)
		}
	}
	for flag := range parsedLongFlags(t, filepath.Join(root, "internal", "cli", "parse.go")) {
		if !documentsLongFlag(document, flag) {
			t.Errorf("CLI reference does not document parsed flag %s", flag)
		}
	}
}

func TestDocumentsLongFlagRequiresTokenBoundaries(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		document string
		flag     string
		want     bool
	}{
		{name: "exact flag", document: "Use `--map` to map a package.", flag: "--map", want: true},
		{name: "pi under pin", document: "Use `--pin` to pin a release.", flag: "--pi", want: false},
		{name: "map under mapping file", document: "Use `--mapping-file` for mappings.", flag: "--map", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := documentsLongFlag(test.document, test.flag); got != test.want {
				t.Errorf("documentsLongFlag(%q, %q) = %t, want %t", test.document, test.flag, got, test.want)
			}
		})
	}
}

func documentsLongFlag(document, flag string) bool {
	boundary := `[^[:alnum:]_-]`
	return regexp.MustCompile(`(^|` + boundary + `)` + regexp.QuoteMeta(flag) + `($|` + boundary + `)`).MatchString(document)
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
	for _, command := range commandOrder {
		usages := safetyUsages(t, command)
		for _, usage := range usages {
			expected[usage.base] = true
		}
		for _, flag := range mutatingHelpFlags(helpFor(command)) {
			found := false
			for _, usage := range usages {
				if strings.Contains(usage.line, flag) {
					expected[safetyFlagKey(usage.base, flag)] = true
					found = true
				}
			}
			if !found {
				t.Fatalf("command %q mutating flag %q has no matching usage", command, flag)
			}
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

type safetyUsage struct {
	line string
	base string
}

func safetyUsages(t *testing.T, command Command) []safetyUsage {
	t.Helper()
	spec, ok := commandSpecs[command]
	if !ok {
		t.Fatalf("command %q has no command specification", command)
	}
	var result []safetyUsage
	for _, line := range strings.Split(spec.usage, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		base := line
		if flag := strings.Index(base, " [--"); flag >= 0 {
			base = base[:flag]
		}
		if !strings.HasPrefix(base, "acr "+string(command)) {
			t.Fatalf("command %q usage %q has no safety-matrix base", command, line)
		}
		result = append(result, safetyUsage{line: line, base: base})
	}
	if len(result) == 0 {
		t.Fatalf("command %q has no safety-matrix base", command)
	}
	return result
}

func TestMachineReadableCodeRegistriesMatchDocs(t *testing.T) {
	t.Parallel()

	refusals := uniqueStrings(t, "RefusalCodes", RefusalCodes)
	notices := uniqueStrings(t, "NoticeCodes", NoticeCodes)
	for code := range refusals {
		if notices[code] {
			t.Errorf("code %q is in both RefusalCodes and NoticeCodes", code)
		}
	}

	root := docsRepositoryRoot(t)

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
