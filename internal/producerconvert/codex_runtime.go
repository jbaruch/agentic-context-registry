package producerconvert

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// CodexMinimumVersion is the oldest Codex CLI release whose proposal-only
// startup contract ACR verified. Older releases are refused with an upgrade
// instruction; newer releases are accepted when the runtime probes below pass.
// The version string alone never refuses a release at or above this floor.
const CodexMinimumVersion = "0.153.2"

// CodexRelease records a real release tested with auth-free startup probes.
// The table is documentation, not a version ceiling or proof of semantic
// conversion on every release. Actual conversion evidence is recorded separately.
type CodexRelease struct {
	Version  string
	Released string
}

// CodexVerifiedReleases lists the releases the runtime contract was
// demonstrated on, oldest first. Extend it after verifying a newer release
// per docs/cli.md#codex-runtime-support-policy.
var CodexVerifiedReleases = []CodexRelease{
	{Version: "0.153.2", Released: "2026-09-03"},
	{Version: "0.154.0", Released: "2026-09-09"},
	{Version: "0.155.0", Released: "2026-09-17"},
	{Version: "0.166.0", Released: "2026-10-29"},
	{Version: "0.167.2", Released: "2026-11-11"},
}

// codexPlatforms names every platform with a verified OS read boundary.
var codexPlatforms = []string{"darwin", "linux"}

// codexRequiredFlags are the exec flags every accepted release must parse.
// A release that rejects one changed its protocol; ACR refuses before any
// source reaches the provider and names the flag.
var codexRequiredFlags = []string{"--ignore-user-config", "--ignore-rules", "--strict-config", "--ephemeral", "--sandbox", "--skip-git-repo-check", "--color", "--json", "--output-schema", "--output-last-message", "--config", "--disable", "--enable"}

// codexRequiredControls are the feature switches ACR relies on to keep the
// proposal turn tool-free. Each must be advertised by the runtime and must
// report disabled once ACR's argv is applied.
var codexRequiredControls = []string{"shell_tool", "apps", "plugins", "multi_agent", "multi_agent_v2", "hooks", "browser_use", "browser_use_external", "computer_use", "image_generation", "in_app_browser", "in_app_local_automation", "view_image", "goals", "sleep_tool", "code_mode", "code_mode_host", "skill_search", "skill_mcp_dependency_install", "workspace_dependencies", "memories", "remote_plugin"}

// codexSkipHostSkills is the only feature ACR enables: it stops host skill
// discovery. It must be advertised and must report enabled.
const codexSkipHostSkills = "skip_host_skill_discovery"

// codexRuntimeForcedFeatures are switches the runtime keeps enabled whatever
// the caller passes. They are still disabled in argv for defence in depth, but
// the honored-controls check does not require them to read false. Every
// entry names the control that actually governs the surface.
var codexRuntimeForcedFeatures = map[string]string{
	"unified_exec": "exec engine selector; tool exposure is governed by shell_tool and the event contract",
}

// codexFeatureStagesLeftAlone are advertised stages ACR never toggles: removed
// switches carry no behavior, and touching a deprecated one adds a startup
// warning the strict prelude would refuse.
var codexFeatureStagesLeftAlone = map[string]bool{"removed": true, "deprecated": true}

var codexVersionPattern = regexp.MustCompile(`^codex-cli (\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)

type codexVersion struct {
	text       string
	major      int
	minor      int
	patch      int
	prerelease string
}

// parseCodexVersion reads the first line of `codex --version`. Anything other
// than `codex-cli MAJOR.MINOR.PATCH[-prerelease]` is unrecognized output.
func parseCodexVersion(output string) (codexVersion, error) {
	first, _, _ := strings.Cut(strings.TrimSpace(output), "\n")
	first = strings.TrimSpace(first)
	match := codexVersionPattern.FindStringSubmatch(first)
	if match == nil {
		return codexVersion{}, fmt.Errorf("unrecognized Codex version output %q; expected \"codex-cli MAJOR.MINOR.PATCH\" from `codex --version`", bounded(first, 200))
	}
	version := codexVersion{text: first, prerelease: match[4]}
	var err error
	for index, target := range []*int{&version.major, &version.minor, &version.patch} {
		if *target, err = strconv.Atoi(match[index+1]); err != nil {
			return codexVersion{}, fmt.Errorf("unrecognized Codex version output %q: %w", first, err)
		}
	}
	return version, nil
}

// codexVersionSupported applies the documented floor. Patch, minor and major
// differences above the floor never refuse on their own; the capability probes
// decide. Prerelease builds are accepted by the version gate above the floor
// (a prerelease of the floor itself precedes it) and are not part of the
// verified-release table.
func codexVersionSupported(version codexVersion) error {
	floor, err := parseCodexVersion("codex-cli " + CodexMinimumVersion)
	if err != nil {
		return err
	}
	for _, pair := range [][2]int{{version.major, floor.major}, {version.minor, floor.minor}, {version.patch, floor.patch}} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return fmt.Errorf("unsupported Codex version %q: ACR requires codex-cli %s or newer; upgrade Codex", version.text, CodexMinimumVersion)
			}
			return nil
		}
	}
	// A prerelease of the minimum precedes it (semantic version order).
	if version.prerelease != "" && floor.prerelease == "" {
		return fmt.Errorf("unsupported Codex version %q: ACR requires codex-cli %s or newer; upgrade Codex", version.text, CodexMinimumVersion)
	}
	return nil
}

func bounded(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

type codexFeature struct {
	name      string
	stage     string
	effective bool
}

// parseCodexFeatures reads the `codex features list` table: one feature per
// line as `name  stage words  true|false`.
func parseCodexFeatures(output string) ([]codexFeature, error) {
	var features []codexFeature
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 3 || (fields[len(fields)-1] != "true" && fields[len(fields)-1] != "false") {
			return nil, fmt.Errorf("unrecognized Codex feature row %q; expected \"name stage true|false\" from `codex features list`", bounded(line, 200))
		}
		name := fields[0]
		if seen[name] {
			return nil, fmt.Errorf("duplicate Codex feature %q in `codex features list`", name)
		}
		seen[name] = true
		features = append(features, codexFeature{name: name, stage: strings.Join(fields[1:len(fields)-1], " "), effective: fields[len(fields)-1] == "true"})
	}
	if len(features) == 0 {
		return nil, errors.New("`codex features list` advertised no features")
	}
	return features, nil
}

// codexFeaturePlan derives the switches for this runtime from what it
// advertises: every advertised feature outside the stages left alone is
// disabled, the host skill discovery skip is enabled, and every required
// control must be advertised so the honored-controls check can measure it.
func codexFeaturePlan(features []codexFeature) (disable []string, err error) {
	index := map[string]codexFeature{}
	for _, feature := range features {
		index[feature.name] = feature
	}
	for _, name := range append(append([]string{}, codexRequiredControls...), codexSkipHostSkills) {
		feature, advertised := index[name]
		if !advertised || feature.stage == "removed" {
			return nil, fmt.Errorf("unsupported Codex capability: required control %q is not advertised by this runtime; ACR needs an update for this Codex protocol, report at https://github.com/jbaruch/agentic-context-registry/issues", name)
		}
	}
	for _, feature := range features {
		if feature.name == codexSkipHostSkills || codexFeatureStagesLeftAlone[feature.stage] {
			continue
		}
		disable = append(disable, feature.name)
	}
	sort.Strings(disable)
	return disable, nil
}

// codexControlsHonored checks the feature table the runtime reports with
// ACR's switches applied: every disabled feature reads false unless the
// runtime forces it, and the skip reads true.
func codexControlsHonored(features []codexFeature, disabled []string) error {
	index := map[string]codexFeature{}
	for _, feature := range features {
		index[feature.name] = feature
	}
	skip, advertised := index[codexSkipHostSkills]
	if !advertised || skip.stage == "removed" || !skip.effective {
		return fmt.Errorf("unsupported Codex capability: --enable %s was not honored; ACR needs an update for this Codex protocol, report at https://github.com/jbaruch/agentic-context-registry/issues", codexSkipHostSkills)
	}
	for _, name := range disabled {
		feature, advertised := index[name]
		if !advertised {
			return fmt.Errorf("unsupported Codex capability: feature %q disappeared between probes", name)
		}
		if feature.effective {
			if _, forced := codexRuntimeForcedFeatures[name]; forced {
				continue
			}
			return fmt.Errorf("unsupported Codex capability: --disable %s was not honored (still effective); ACR needs an update for this Codex protocol, report at https://github.com/jbaruch/agentic-context-registry/issues", name)
		}
	}
	for _, name := range codexRequiredControls {
		feature, advertised := index[name]
		if !advertised || feature.stage == "removed" || feature.effective {
			return fmt.Errorf("unsupported Codex capability: required control %q must remain advertised and effective=false", name)
		}
	}
	return nil
}

// codexMissingFlags names required exec flags absent from `codex exec --help`.
func codexMissingFlags(help string) []string {
	var missing []string
	for _, flag := range codexRequiredFlags {
		if !strings.Contains(help, flag) {
			missing = append(missing, flag)
		}
	}
	return missing
}

var codexRejectedArgument = regexp.MustCompile(`unexpected argument '([^']+)'`)

// codexArgvRejection turns clap's rejection of a flag into the capability
// diagnostic, naming the flag from the runtime's own stderr.
func codexArgvRejection(stderr string, missing []string) error {
	if match := codexRejectedArgument.FindStringSubmatch(stderr); match != nil {
		return fmt.Errorf("unsupported Codex capability: this release rejects the %s flag; ACR needs an update for this Codex protocol, report at https://github.com/jbaruch/agentic-context-registry/issues", match[1])
	}
	if len(missing) != 0 {
		return fmt.Errorf("unsupported Codex capability: `codex exec --help` lacks %s; ACR needs an update for this Codex protocol, report at https://github.com/jbaruch/agentic-context-registry/issues", strings.Join(missing, ", "))
	}
	return fmt.Errorf("unsupported Codex capability: this release rejected ACR's exec arguments: %s", bounded(strings.TrimSpace(stderr), 500))
}

// codexConfigRejection recognizes a strict-config refusal at exec startup, so
// a renamed configuration key is reported as a protocol change.
func codexConfigRejection(stderr string) error {
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "Error loading config.toml:") || strings.HasPrefix(line, "Error: Unknown feature flag:") {
			return fmt.Errorf("unsupported Codex capability: %s; ACR needs an update for this Codex protocol, report at https://github.com/jbaruch/agentic-context-registry/issues", strings.TrimSpace(line))
		}
	}
	return nil
}

// codexUnauthorized recognizes the model service refusing the configured
// credential, which is the operator's to fix rather than a protocol change.
func codexUnauthorized(stdout, stderr string) bool {
	if strings.Contains(stderr, "401 Unauthorized") {
		return true
	}
	for _, line := range strings.Split(stdout, "\n") {
		var event struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
			Item struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		message := ""
		switch event.Type {
		case "error":
			message = event.Message
		case "turn.failed":
			message = event.Error.Message
		case "item.completed":
			if event.Item.Type == "error" {
				message = event.Item.Message
			}
		}
		if strings.Contains(message, "401 Unauthorized") {
			return true
		}
	}
	return false
}

// codexUsageLimit returns the model service's usage-limit message when the
// turn failed on account quota, so the refusal carries the service's own
// retry time and link rather than a generic process failure.
func codexUsageLimit(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		var event struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(line), &event) != nil || event.Type != "error" || !strings.Contains(event.Message, "usage limit") {
			continue
		}
		return event.Message
	}
	return ""
}

var codexSecretFragment = regexp.MustCompile(`(^|[^A-Za-z0-9_])sk-[A-Za-z0-9_*.…-]{6,}`)

// RedactCodexSecrets removes literal and JSON-escaped credential values and
// key-shaped fragments, masked or not, from text bound for a report.
func RedactCodexSecrets(text string, secrets []string) string {
	const mask = "[redacted]"
	unique := make(map[string]bool)
	longest := 0
	for _, secret := range secrets {
		if secret != "" {
			unique[secret] = true
			encoded, _ := json.Marshal(secret) // A string always marshals successfully.
			unique[string(encoded[1:len(encoded)-1])] = true
		}
	}
	ordered := make([]string, 0, len(unique))
	for secret := range unique {
		ordered = append(ordered, secret)
		longest = max(longest, len(secret))
	}
	sort.Strings(ordered)

	// Difference counts union original-byte coverage without retaining an
	// interval for every overlapping occurrence, or revisiting covered bytes.
	var coverage []int
	cover := func(start, end int) {
		if coverage == nil {
			coverage = make([]int, len(text)+1)
		}
		coverage[start]++
		coverage[end]--
	}
	for start := 0; start < len(text); {
		i := strings.Index(text[start:], mask)
		if i < 0 {
			break
		}
		start += i
		cover(start, start+len(mask))
		start += len(mask)
	}
	for _, secret := range ordered {
		for start := 0; start < len(text); {
			i := strings.Index(text[start:], secret)
			if i < 0 {
				break
			}
			start += i
			cover(start, start+len(secret))
			start++ // Include self-overlapping occurrences.
		}
	}
	for _, match := range codexSecretFragment.FindAllStringSubmatchIndex(text, -1) {
		cover(match[3], match[1]) // Keep the captured leading delimiter.
	}

	// Only merged masked regions need links. The gaps remain slices of the
	// original input, and a region is always rendered as one atomic mask.
	type region struct {
		start, end int
		prev, next *region
		removed    bool
	}
	var first, last *region
	var queue []*region
	depth, start := 0, 0
	for i, delta := range coverage {
		before := depth
		depth += delta
		if before == 0 && depth > 0 {
			start = i
		}
		if before > 0 && depth == 0 {
			r := &region{start: start, end: i, prev: last}
			if last == nil {
				first = r
			} else {
				last.next = r
			}
			last = r
			queue = append(queue, r)
		}
	}

	if first == nil {
		return text
	}

	// A changed mask can only enable literal matches within longest-1 logical
	// bytes of its boundaries. Map that small window back to original bytes;
	// characters inside a mask all map to its complete protected region.
	var window []byte
	var starts, ends []int
	appendByte := func(b byte, start, end int) {
		window = append(window, b)
		starts = append(starts, start)
		ends = append(ends, end)
	}
	for head := 0; head < len(queue); {
		r := queue[head]
		head++
		if head*2 >= len(queue) {
			copy(queue, queue[head:])
			queue = queue[:len(queue)-head]
			head = 0
		}
		if r.removed {
			continue
		}
		start, end := r.start, r.end
		window, starts, ends = window[:0], starts[:0], ends[:0]
		p, previous := r.start, r.prev
		for p > 0 && len(window) < longest-1 {
			if previous != nil && p == previous.end {
				for i := len(mask) - 1; i >= 0 && len(window) < longest-1; i-- {
					appendByte(mask[i], previous.start, previous.end)
				}
				p, previous = previous.start, previous.prev
			} else {
				p--
				appendByte(text[p], p, p+1)
			}
		}
		for i, j := 0, len(window)-1; i < j; i, j = i+1, j-1 {
			window[i], window[j] = window[j], window[i]
			starts[i], starts[j] = starts[j], starts[i]
			ends[i], ends[j] = ends[j], ends[i]
		}
		maskStart := len(window)
		for i := range mask {
			appendByte(mask[i], r.start, r.end)
		}
		maskEnd := len(window)
		p, next := r.end, r.next
		for p < len(text) && len(window)-maskEnd < longest-1 {
			if next != nil && p == next.start {
				for i := 0; i < len(mask) && len(window)-maskEnd < longest-1; i++ {
					appendByte(mask[i], next.start, next.end)
				}
				p, next = next.end, next.next
			} else {
				appendByte(text[p], p, p+1)
				p++
			}
		}
		logical := string(window)
		for _, secret := range ordered {
			for offset := 0; offset < len(logical); {
				i := strings.Index(logical[offset:], secret)
				if i < 0 {
					break
				}
				i += offset
				j := i + len(secret)
				if i < maskEnd && j > maskStart && (i < maskStart || j > maskEnd) {
					start = min(start, starts[i])
					end = max(end, ends[j-1])
				}
				offset = i + 1
			}
		}

		// The regex cannot cross a mask ('[' ends its token alphabet). Only
		// a token immediately after the mask can gain a new leading delimiter.
		limit := len(text)
		if r.next != nil {
			limit = r.next.start
		}
		gap := text[r.end:limit]
		if strings.HasPrefix(gap, "sk-") {
			if match := codexSecretFragment.FindStringSubmatchIndex(gap); match != nil && match[0] == 0 {
				end = max(end, r.end+match[1])
			}
		}
		if start == r.start && end == r.end {
			continue
		}
		// Every successful step covers another original byte. Absorb complete
		// touched masks and adjacent coverage, then revisit only this boundary.
		r.start, r.end = start, end
		for r.prev != nil && r.prev.end >= r.start {
			previous := r.prev
			r.start = min(r.start, previous.start)
			r.prev = previous.prev
			previous.removed = true
		}
		if r.prev == nil {
			first = r
		} else {
			r.prev.next = r
		}
		for r.next != nil && r.next.start <= r.end {
			next := r.next
			r.end = max(r.end, next.end)
			r.next = next.next
			next.removed = true
		}
		if r.next != nil {
			r.next.prev = r
		}
		queue = append(queue, r)
	}

	var result strings.Builder
	start = 0
	for r := first; r != nil; r = r.next {
		result.WriteString(text[start:r.start])
		result.WriteString(mask)
		start = r.end
	}
	result.WriteString(text[start:])
	return result.String()
}

// codexForwardedVariables are the only inherited variables the provider
// receives. Credentials travel in the environment, never in argv.
var codexForwardedVariables = []string{"CODEX_API_KEY", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy"}

// codexEnvironmentValue reads one variable from a process environment slice.
func codexEnvironmentValue(environ []string, name string) (string, bool) {
	for _, entry := range environ {
		if key, value, found := strings.Cut(entry, "="); found && key == name {
			return value, true
		}
	}
	return "", false
}

// codexBoundaryEnvironment is the cleared environment inside the boundary: an
// isolated home, ACR's private temporary directory, and the forwarded
// variables. PATH is the platform's minimal search path.
func codexBoundaryEnvironment(path, home, work string, environ []string, extra ...string) []string {
	env := []string{"PATH=" + path, "HOME=" + home, "CODEX_HOME=" + filepath.Join(home, ".codex"), "TMPDIR=" + work, "LANG=C.UTF-8"}
	env = append(env, extra...)
	for _, name := range codexForwardedVariables {
		if value, found := codexEnvironmentValue(environ, name); found && value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// codexNativeMagic reports whether the file starts with the platform's native
// executable signature: ELF on Linux, Mach-O (thin or fat) on macOS.
func codexNativeMagic(platform string, head []byte) bool {
	if len(head) < 4 {
		return false
	}
	switch platform {
	case "linux":
		return bytes.Equal(head[:4], []byte{0x7f, 'E', 'L', 'F'})
	case "darwin":
		for _, magic := range [][]byte{{0xcf, 0xfa, 0xed, 0xfe}, {0xce, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}} {
			if bytes.Equal(head[:4], magic) {
				return true
			}
		}
	}
	return false
}

func codexFileHead(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	head := make([]byte, 4)
	n, err := file.Read(head)
	if err != nil && n == 0 {
		return nil, err
	}
	return head[:n], nil
}

// codexNPMTargets maps a Go platform to the npm platform package suffix and
// the vendor triple the official launcher resolves.
var codexNPMTargets = map[string]struct{ suffix, triple string }{
	"linux/amd64":  {"linux-x64", "x86_64-unknown-linux-musl"},
	"linux/arm64":  {"linux-arm64", "aarch64-unknown-linux-musl"},
	"darwin/amd64": {"darwin-x64", "x86_64-apple-darwin"},
	"darwin/arm64": {"darwin-arm64", "aarch64-apple-darwin"},
}

// resolveCodexExecutable turns the `codex` on PATH into the native executable
// the boundary runs. A native binary is used directly. The official npm
// launcher (bin/codex.js of @openai/codex) is never executed; its vendored
// native binary is located through the package's documented layout and must
// carry the native signature. Anything else is refused with the install
// options.
func resolveCodexExecutable(platform, arch string, lookPath func(string) (string, error)) (string, error) {
	found, err := lookPath("codex")
	if err != nil {
		return "", fmt.Errorf("configured Codex CLI is unavailable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(found)
	if err != nil {
		return "", fmt.Errorf("resolve Codex executable %s: %w", found, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("Codex executable %s is not a regular file", resolved)
	}
	head, err := codexFileHead(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect Codex executable %s: %w", resolved, err)
	}
	if codexNativeMagic(platform, head) {
		return resolved, nil
	}
	if bytes.HasPrefix(head, []byte("#!")) {
		native, launcherErr := codexLauncherNative(platform, arch, resolved)
		if launcherErr == nil {
			return native, nil
		}
		return "", fmt.Errorf("codex on PATH (%s) is a script launcher whose native binary could not be located: %w; install the native release from https://github.com/openai/codex/releases or reinstall the official npm package (npm install -g @openai/codex)", resolved, launcherErr)
	}
	return "", fmt.Errorf("codex on PATH (%s) is neither a native Codex binary nor the official npm launcher; install the native release from https://github.com/openai/codex/releases", resolved)
}

// codexLauncherNative locates the native binary behind the official npm
// launcher: the platform package installed beside or beneath @openai/codex,
// or the launcher package's own vendor directory.
func codexLauncherNative(platform, arch, launcher string) (string, error) {
	if filepath.Base(launcher) != "codex.js" {
		return "", fmt.Errorf("%s is not the official @openai/codex launcher (bin/codex.js)", launcher)
	}
	target, supported := codexNPMTargets[platform+"/"+arch]
	if !supported {
		return "", fmt.Errorf("no official Codex npm platform package exists for %s/%s", platform, arch)
	}
	packageRoot := filepath.Dir(filepath.Dir(launcher))
	name, version, err := codexNPMPackage(packageRoot)
	if err != nil {
		return "", err
	}
	if name != "@openai/codex" || version == "" {
		return "", fmt.Errorf("%s does not belong to the official @openai/codex package", launcher)
	}
	platformPackage := "codex-" + target.suffix
	var tried []string
	for _, candidate := range []struct {
		root         string
		checkVersion bool
	}{
		{filepath.Join(packageRoot, "node_modules", "@openai", platformPackage), true},
		{filepath.Join(filepath.Dir(packageRoot), platformPackage), true},
		{packageRoot, false},
	} {
		binary := filepath.Join(candidate.root, "vendor", target.triple, "bin", "codex")
		tried = append(tried, binary)
		if candidate.checkVersion {
			pkgName, pkgVersion, pkgErr := codexNPMPackage(candidate.root)
			if pkgErr != nil || pkgName != "@openai/codex" || !strings.HasPrefix(pkgVersion, version+"-") {
				continue
			}
		}
		info, statErr := os.Stat(binary)
		if statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		head, headErr := codexFileHead(binary)
		if headErr != nil || !codexNativeMagic(platform, head) {
			return "", fmt.Errorf("%s is not a native %s executable", binary, platform)
		}
		return binary, nil
	}
	return "", fmt.Errorf("no vendored native binary for %s at %s", target.triple, strings.Join(tried, ", "))
}

func codexNPMPackage(root string) (name, version string, err error) {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return "", "", err
	}
	var manifest struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", "", fmt.Errorf("parse %s: %w", filepath.Join(root, "package.json"), err)
	}
	return manifest.Name, manifest.Version, nil
}

// codexCABundle picks the certificate bundle the Linux view binds: an explicit
// SSL_CERT_FILE first, then the distribution locations.
func codexCABundle(environ []string) (string, error) {
	var candidates []string
	if value, found := codexEnvironmentValue(environ, "SSL_CERT_FILE"); found && value != "" {
		candidates = append(candidates, value)
	}
	candidates = append(candidates, "/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem", "/etc/ssl/cert.pem")
	for _, candidate := range candidates {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		if info, statErr := os.Stat(resolved); statErr == nil && info.Mode().IsRegular() {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("no CA certificate bundle found for the Codex view (tried %s); set SSL_CERT_FILE to the bundle path", strings.Join(candidates, ", "))
}

// codexLinuxViewTarget is where the Linux view exposes the native binary. The
// file alone is bound, so siblings such as bundled skills stay absent.
const codexLinuxViewTarget = "/opt/acr/codex"

// codexLinuxView is the allow-list mount namespace: an empty root plus exactly
// the native binary, the TLS roots, the resolver, private dev/proc/tmp, the
// private working directory and the isolated home. Every instruction root
// Codex can discover resolves to an ACR-created directory or to nothing.
func codexLinuxView(executable, caBundle, work, home, chdir string) []string {
	return []string{
		"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup-try",
		"--disable-userns", "--cap-drop", "ALL", "--die-with-parent", "--new-session",
		"--ro-bind", executable, codexLinuxViewTarget,
		"--ro-bind", "/etc/resolv.conf", "/etc/resolv.conf",
		"--ro-bind-try", "/etc/hosts", "/etc/hosts",
		"--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp",
		"--bind", work, work,
		"--bind", home, home,
		// Bind the CA after its private parents so a bundle under /tmp stays visible.
		"--ro-bind", caBundle, caBundle,
		"--chdir", chdir,
	}
}

// codexLinuxNamespaceHint explains a bubblewrap namespace refusal in terms the
// operator can act on without a host-wide setting change.
func codexLinuxNamespaceHint(stderr string) string {
	for _, marker := range []string{"No permissions to create a new namespace", "setting up uid map", "Can't mount proc", "Operation not permitted"} {
		if strings.Contains(stderr, marker) {
			return "; unprivileged user namespaces appear restricted on this host: ask the administrator to verify the package-owned bubblewrap executable and a compatible enforced AppArmor setup/child profile, namespace sysctls, and proc permissions; installing bubblewrap alone may not grant namespace setup (see docs/testing.md)"
		}
	}
	return ""
}

// codexDarwinProfile is the Seatbelt profile: allow everything, then deny
// reading instruction and skill files by name anywhere, the system-managed
// Codex roots, macOS managed preferences, and the configured home's own
// instruction files by resolved path.
func codexDarwinProfile(literals []string, subpaths []string) string {
	var profile strings.Builder
	profile.WriteString("(version 1)\n(allow default)\n")
	profile.WriteString("(deny file-read-data (regex #\"/(AGENTS([.]override)?[.]md|SKILL[.]md)$\"))\n")
	for _, subpath := range subpaths {
		fmt.Fprintf(&profile, "(deny file-read* (subpath %q))\n", subpath)
	}
	for _, literal := range literals {
		fmt.Fprintf(&profile, "(deny file-read-data (literal %q))\n", literal)
	}
	return profile.String()
}

// codexDarwinSystemRoots are the directories the macOS binary probes for
// managed configuration, requirements and skills outside --ignore-user-config.
var codexDarwinSystemRoots = []string{"/etc/codex", "/private/etc/codex", "/Library/Managed Preferences"}
