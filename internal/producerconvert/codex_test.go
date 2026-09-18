package producerconvert

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// codexPlatformsUnderTest are the boundary shapes every deterministic test
// exercises on every host: the darwin profile wrapper and the linux view
// wrapper are fakes that reproduce the real wrappers' argv contracts.
var codexPlatformsUnderTest = []string{"darwin", "linux"}

// codexDefaultFeatures is the fake runtime's advertised table: every required
// control, the skip, a runtime-forced switch, unrelated infrastructure, and
// stages ACR leaves alone.
var codexDefaultFeatures = [][]any{
	{"apps", "stable", true}, {"browser_use", "stable", true}, {"browser_use_external", "stable", true}, {"code_mode", "under development", false}, {"code_mode_host", "stable", true}, {"computer_use", "stable", true}, {"goals", "stable", true}, {"hooks", "stable", true}, {"image_generation", "stable", true}, {"in_app_browser", "stable", true}, {"in_app_local_automation", "stable", true}, {"memories", "stable", false}, {"multi_agent", "stable", true}, {"multi_agent_v2", "stable", false}, {"plugins", "stable", true}, {"remote_plugin", "stable", true}, {"shell_tool", "stable", true}, {"skill_mcp_dependency_install", "stable", true}, {"skill_search", "stable", true}, {"sleep_tool", "stable", true}, {"view_image", "stable", true}, {"workspace_dependencies", "stable", true},
	{"skip_host_skill_discovery", "under development", false},
	{"unified_exec", "stable", true},
	{"enable_request_compression", "stable", true}, {"shell_snapshot", "stable", true}, {"network_proxy", "experimental", false},
	{"sqlite", "removed", true}, {"apply_patch_freeform", "removed", false}, {"web_search_cached", "deprecated", false},
}

// codexFixture builds a fake runtime for the host's own platform shape.
func codexFixture(t *testing.T, proposed proposal) codexRuntime {
	t.Helper()
	platform := runtime.GOOS
	if platform != "darwin" && platform != "linux" {
		platform = "linux"
	}
	return codexFixtureFor(t, proposed, platform)
}

// codexFixtureFor builds a fake Codex runtime: a scripted `codex`, a scripted
// boundary wrapper for the requested platform, an isolated home base and a
// cleared environment. The fake reads its behavior from fixture.json beside
// itself because the production boundary clears the environment.
func codexFixtureFor(t *testing.T, proposed proposal, platform string) codexRuntime {
	t.Helper()
	directory := t.TempDir()
	encoded, err := json.Marshal(proposed)
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("fake Codex requires python3: %v", err)
	}
	features, err := json.Marshal(codexDefaultFeatures)
	if err != nil {
		t.Fatal(err)
	}
	fixture := map[string]any{
		"proposal":     string(encoded),
		"init":         disabledCodeHost,
		"behavior":     "success",
		"version":      "codex-cli 0.154.0",
		"spawn_log":    filepath.Join(directory, "spawns.log"),
		"stdin_marker": filepath.Join(directory, "stdin.marker"),
		"features":     json.RawMessage(features),
	}
	writeCodexFixture(t, directory, fixture)
	put(t, directory, "sandbox", "#!/bin/sh\nif [ \"$3\" = /bin/cat ]; then printf 'Operation not permitted\\n' >&2; exit 1; fi\nshift 2\nexec \"$@\"\n", 0o755)
	put(t, directory, "bwrap", `#!/bin/sh
# Fake bubblewrap: reproduces the allow-list view contract. A chdir target
# outside the bound paths is absent, exactly as in the real namespace.
chdir=""; exe=""; binds=""
while [ $# -gt 0 ]; do
  case "$1" in
    --ro-bind|--bind) if [ "$3" = /opt/acr/codex ]; then exe="$2"; fi; binds="$binds $3"; shift 3;;
    --ro-bind-try) binds="$binds $3"; shift 3;;
    --chdir) chdir="$2"; shift 2;;
    --dev|--proc|--tmpfs|--cap-drop) shift 2;;
    --) shift; break;;
    *) shift;;
  esac
done
ok=0
for b in $binds; do case "$chdir" in "$b"|"$b"/*) ok=1;; esac; done
if [ "$ok" = 0 ]; then printf "bwrap: Can't chdir to %s: No such file or directory\n" "$chdir" >&2; exit 1; fi
cmd="$1"; shift
if [ "$cmd" = /opt/acr/codex ]; then cmd="$exe"; fi
cd "$chdir" && exec "$cmd" "$@"
`, 0o755)
	put(t, directory, "codex", "#!"+python+`
import json, os, sys
here = os.path.dirname(os.path.realpath(sys.argv[0]))
fixture = json.load(open(os.path.join(here, 'fixture.json')))
behavior = fixture['behavior']
with open(fixture['spawn_log'], 'a') as log: log.write(json.dumps(sys.argv[1:]) + '\n')
argv = sys.argv[1:]
disabled, enabled, config = [], [], {}
rest = []
i = 0
while i < len(argv):
    if argv[i] == '--disable': disabled.append(argv[i+1]); i += 2; continue
    if argv[i] == '--enable': enabled.append(argv[i+1]); i += 2; continue
    if argv[i] == '-c': key, _, value = argv[i+1].partition('='); config[key] = value; i += 2; continue
    rest.append(argv[i]); i += 1
known = [row[0] for row in fixture['features']]
for name in disabled + enabled:
    if name not in known:
        print('Error: Unknown feature flag: ' + name, file=sys.stderr); sys.exit(1)
if rest == ['--version']:
    if behavior == 'version-exit': print(fixture['version']); sys.exit(3)
    print(fixture['version']); sys.exit(0)
if rest == ['exec', '--help']:
    flags = ['--ignore-user-config', '--ignore-rules', '--strict-config', '--ephemeral', '--sandbox', '--skip-git-repo-check', '--color', '--json', '--output-schema', '--output-last-message', '-c, --config', '--disable', '--enable']
    if behavior == 'flag-missing': flags.remove('--output-schema')
    print('Run Codex non-interactively\n\nOptions:\n' + '\n'.join('      ' + f for f in flags)); sys.exit(0)
if rest == ['features', 'list']:
    for name, stage, effective in fixture['features']:
        if behavior == 'feature-missing' and name == 'goals': continue
        if name in disabled and name != 'unified_exec' and not (behavior == 'feature-unhonored' and name == 'shell_tool'): effective = False
        if name in enabled: effective = True
        print('%-40s %-18s %s' % (name, stage, 'true' if effective else 'false'))
    sys.exit(0)
if rest == ['debug', 'prompt-input']:
    parts = []
    if behavior != 'prompt-no-dev': parts.append({'type': 'input_text', 'text': json.loads(config.get('developer_instructions', '""'))})
    if behavior == 'prompt-leak' and os.path.exists('AGENTS.md'): parts.append({'type': 'input_text', 'text': open('AGENTS.md').read()})
    print(json.dumps([{'type': 'message', 'role': 'developer', 'content': parts}])); sys.exit(0)
if rest and rest[0] == 'exec' and rest[-1] == '-V':
    if behavior == 'flag-rejected':
        print("error: unexpected argument '--output-schema' found", file=sys.stderr); sys.exit(2)
    print('codex-cli-exec ' + fixture['version'].split()[-1]); sys.exit(0)
if not (rest and rest[0] == 'exec' and rest[-1] == '-'):
    print('unexpected fake invocation: ' + json.dumps(argv), file=sys.stderr); sys.exit(64)
if behavior == 'config-rejected':
    print('Error loading config.toml: unknown configuration field `+"`agents.enabled`"+` in -c/--config override', file=sys.stderr); sys.exit(1)
request = sys.stdin.read()
if request: open(fixture['stdin_marker'], 'w').write('read')
if behavior == 'block':
    import socket
    host, port = fixture['listener'].split(':')
    with socket.create_connection((host, int(port))) as stream:
        stream.sendall(b'ready')
        stream.recv(1)
    sys.exit(3)
if behavior == 'process':
    print('provider unavailable', file=sys.stderr)
    sys.exit(7)
if behavior == 'env-echo':
    for key, value in sorted(os.environ.items()): print(key + '=' + value, file=sys.stderr)
    print('Incorrect API key provided: sk-abc12***xyz-key', file=sys.stderr)
if behavior == 'overflow':
    sys.stdout.write('x' * 12582913)
    sys.exit(0)
if behavior == 'rotate':
    path = os.path.join(os.environ['CODEX_HOME'], 'auth.json')
    if os.path.exists(path): open(path, 'a').write('\n')
output = rest[rest.index('--output-last-message') + 1]
proposal = fixture['proposal']
if behavior == 'invalid': proposal = '{invalid'
with open(output, 'w') as handle: handle.write(proposal if behavior != 'mismatch' else '{}')
def event(value): print(json.dumps(value))
event({'type':'thread.started', 'thread_id':'fixture-thread'})
if behavior != 'initialization':
    event({'type':'item.completed','item':{'type':'error','message':fixture['init']}})
event({'type':'turn.started'})
if behavior == 'auth':
    event({'type':'error','message':'Reconnecting... 2/5 (unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: wss://api.openai.com/v1/responses)'})
    event({'type':'item.completed','item':{'type':'error','message':'Falling back from WebSockets to HTTPS transport. unexpected status 401 Unauthorized: Missing bearer or basic authentication in header'}})
    event({'type':'error','message':'unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: https://api.openai.com/v1/responses'})
    event({'type':'turn.failed','error':{'message':'unexpected status 401 Unauthorized'}})
    print('ERROR codex_api::endpoint::responses_websocket: failed to connect to websocket: HTTP error: 401 Unauthorized', file=sys.stderr)
    sys.exit(1)
if behavior == 'tool': event({'type':'item.completed','item':{'type':'command_execution','command':'cat secret'}})
if behavior == 'router': print('ERROR codex_core::tools::router: error=code-mode host is disabled',file=sys.stderr)
if behavior in ('empty-first', 'identical', 'distinct'):
    first = '' if behavior == 'empty-first' else proposal if behavior == 'identical' else '{"edits":[],"policyChanges":[]}'
    event({'type':'item.completed','item':{'type':'agent_message','text':first}})
if behavior != 'missing-final':
    event({'type':'item.completed','item':{'type':'agent_message','text':'' if behavior == 'empty-final' else proposal}})
if behavior != 'truncated': event({'type':'turn.completed','usage':{'input_tokens':1,'output_tokens':1}})
`, 0o755)
	home := filepath.Join(directory, "source-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	put(t, directory, "ca.pem", "fixture certificate bundle\n", 0o644)
	wrapper := filepath.Join(directory, "sandbox")
	if platform == "linux" {
		wrapper = filepath.Join(directory, "bwrap")
	}
	return codexRuntime{
		platform:   platform,
		arch:       runtime.GOARCH,
		wrapper:    wrapper,
		executable: filepath.Join(directory, "codex"),
		home:       home,
		homeBase:   filepath.Join(directory, "homes"),
		environ:    []string{"PATH=" + os.Getenv("PATH"), "SSL_CERT_FILE=" + filepath.Join(directory, "ca.pem")},
	}
}

func writeCodexFixture(t *testing.T, directory string, fixture map[string]any) {
	t.Helper()
	encoded, err := json.MarshalIndent(fixture, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "fixture.json"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
}

// codexSet changes one fake-runtime setting for the rest of the test.
func codexSet(t *testing.T, native codexRuntime, key string, value any) {
	t.Helper()
	directory := filepath.Dir(native.executable)
	data, err := os.ReadFile(filepath.Join(directory, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]any
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	fixture[key] = value
	writeCodexFixture(t, directory, fixture)
}

// codexSpawns returns every argv the fake Codex received, in order.
func codexSpawns(t *testing.T, native codexRuntime) [][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(native.executable), "spawns.log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var spawns [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatal(err)
		}
		spawns = append(spawns, argv)
	}
	return spawns
}

// codexStdinRead reports whether the fake exec stage received the request.
func codexStdinRead(native codexRuntime) bool {
	_, err := os.Stat(filepath.Join(filepath.Dir(native.executable), "stdin.marker"))
	return err == nil
}

func codexExecSpawned(spawns [][]string) bool {
	for _, argv := range spawns {
		if len(argv) > 1 && argv[0] == "exec" && argv[len(argv)-1] == "-" {
			return true
		}
	}
	return false
}

func TestCodexProposalAppliesThroughOriginalTransaction(t *testing.T) {
	for _, platform := range codexPlatformsUnderTest {
		t.Run(platform, func(t *testing.T) {
			root, options, proposed := semanticFixture(t)
			options.Agent = "codex"
			native := codexFixtureFor(t, proposed, platform)
			codexSet(t, native, "version", "codex-cli 0.154.3")
			before := treeAt(t, root)
			plan, err := prepareWithProvider(context.Background(), options, func(ctx context.Context, provider, request string) (proposal, AgentRun, error) {
				if provider != "codex" || strings.Contains(request, root) {
					t.Fatal("unexpected source path or provider")
				}
				return runCodexWithRuntime(ctx, request, native)
			})
			if err != nil {
				t.Fatal(err)
			}
			if !matches(before, treeAt(t, root)) {
				t.Fatal("planning changed input")
			}
			run := plan.Report.AgentRuns
			if len(run) != 1 || run[0].RuntimeVersion != "codex-cli 0.154.3" || run[0].Isolation == "" || len(run[0].Arguments) == 0 || run[0].Arguments[0] != native.wrapper {
				t.Fatalf("missing runtime evidence: %+v", run)
			}
			if platform == "linux" && (strings.Contains(run[0].Isolation, "--ro-bind / /") || !strings.Contains(run[0].Isolation, "--disable-userns")) {
				t.Fatalf("linux isolation is not the allow-list view: %s", run[0].Isolation)
			}
			for _, argument := range run[0].Arguments {
				if strings.Contains(argument, "SSL_CERT_FILE") || strings.Contains(argument, "CODEX_API_KEY") {
					t.Fatal("environment leaked into argv")
				}
			}
			if _, err := plan.Apply(); err != nil {
				t.Fatal(err)
			}
			if read(t, root, proposed.Edits[0].Path) != proposed.Edits[0].Content {
				t.Fatal("proposal not applied")
			}
			spawned := len(codexSpawns(t, native))
			current, err := PrepareContext(context.Background(), options)
			if err != nil || !current.Report.Current {
				t.Fatalf("inert rerun: %v %+v", err, current.Report)
			}
			if len(codexSpawns(t, native)) != spawned {
				t.Fatal("inert rerun spawned Codex")
			}
			if entries, err := os.ReadDir(native.homeBase); err != nil || len(entries) != 0 {
				t.Fatalf("isolated home retained: %v %v", entries, err)
			}
		})
	}
}

func TestCodexProviderFailuresPreserveInput(t *testing.T) {
	for _, platform := range codexPlatformsUnderTest {
		for _, behavior := range []string{"version-exit", "process", "overflow", "invalid", "mismatch", "initialization", "tool", "router", "truncated", "flag-missing", "flag-rejected", "feature-missing", "feature-unhonored", "prompt-leak", "prompt-no-dev", "auth", "config-rejected"} {
			t.Run(platform+"/"+behavior, func(t *testing.T) {
				root, _, proposed := semanticFixture(t)
				native := codexFixtureFor(t, proposed, platform)
				codexSet(t, native, "behavior", behavior)
				before := treeAt(t, root)
				_, run, err := runCodexWithRuntime(context.Background(), "bounded selected text", native)
				if err == nil || run.Failure == "" {
					t.Fatalf("accepted %s: %+v", behavior, run)
				}
				if !matches(before, treeAt(t, root)) {
					t.Fatal("provider changed source")
				}
				if entries, readErr := os.ReadDir(native.homeBase); readErr != nil || len(entries) != 0 {
					t.Fatalf("isolated home retained after refusal: %v %v", entries, readErr)
				}
			})
		}
	}
}

// TestCodexRefusalClassesAreDistinct measures the stage each refusal happens
// at from the fake's own evidence: the stdin marker exists only once the
// request reached the exec stage, and the spawn log shows what ran.
func TestCodexRefusalClassesAreDistinct(t *testing.T) {
	cases := []struct {
		behavior  string
		version   string
		message   string
		preSend   bool
		execSpawn bool
	}{
		{"success", "codex-cli 0.153.1", "ACR requires codex-cli 0.153.2 or newer; upgrade Codex", true, false},
		{"success", "codex 0.154.0", "unrecognized Codex version output", true, false},
		{"success", "", "unrecognized Codex version output", true, false},
		{"success", "codex-cli 0.154.0\nwarning: update available", "", false, true},
		{"flag-missing", "codex-cli 0.154.0", "unsupported Codex capability: `codex exec --help` lacks --output-schema", true, false},
		{"flag-rejected", "codex-cli 0.154.0", "unsupported Codex capability: this release rejects the --output-schema flag", true, false},
		{"feature-missing", "codex-cli 0.154.0", `required control "goals" is not advertised`, true, false},
		{"feature-unhonored", "codex-cli 0.154.0", "--disable shell_tool was not honored", true, false},
		{"prompt-leak", "codex-cli 0.154.0", "instruction isolation was not honored", true, false},
		{"prompt-no-dev", "codex-cli 0.154.0", "developer instructions did not reach", true, false},
		{"config-rejected", "codex-cli 0.154.0", "unsupported Codex capability: Error loading config.toml", true, true},
		{"auth", "codex-cli 0.154.0", "Codex authentication failed (401 Unauthorized): run `codex login`", false, true},
		{"initialization", "codex-cli 0.154.0", "did not initialize the verified disabled execution host", false, true},
		{"tool", "codex-cli 0.154.0", "unexpected tool/item", false, true},
	}
	for _, platform := range codexPlatformsUnderTest {
		for _, item := range cases {
			t.Run(platform+"/"+item.behavior+"/"+strings.ReplaceAll(item.version, "\n", "_"), func(t *testing.T) {
				_, _, proposed := semanticFixture(t)
				native := codexFixtureFor(t, proposed, platform)
				codexSet(t, native, "behavior", item.behavior)
				codexSet(t, native, "version", item.version)
				_, run, err := runCodexWithRuntime(context.Background(), "input", native)
				if item.message == "" {
					if err != nil {
						t.Fatalf("multi-line version output refused: %v", err)
					}
					if run.RuntimeVersion != "codex-cli 0.154.0" {
						t.Fatalf("runtime version = %q", run.RuntimeVersion)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), item.message) {
					t.Fatalf("refusal = %v, want %q", err, item.message)
				}
				if codexStdinRead(native) == item.preSend {
					t.Fatalf("request reached exec stage = %t, want %t", codexStdinRead(native), !item.preSend)
				}
				if codexExecSpawned(codexSpawns(t, native)) != item.execSpawn {
					t.Fatalf("exec spawned = %t, want %t", !item.execSpawn, item.execSpawn)
				}
				if item.version != "" && item.version != "codex 0.154.0" && run.RuntimeVersion == "" {
					t.Fatal("runtime version evidence lost on a later refusal")
				}
			})
		}
	}
}

func TestCodexVersionPolicy(t *testing.T) {
	for _, item := range []struct {
		output string
		accept bool
	}{
		{"codex-cli 0.153.2", true}, {"codex-cli 0.153.3", true}, {"codex-cli 0.154.0", true}, {"codex-cli 0.154.9", true}, {"codex-cli 0.155.0", true}, {"codex-cli 0.156.0-alpha.2", true}, {"codex-cli 1.0.0", true}, {"codex-cli 0.154.0\r\n", true}, {"codex-cli 0.154.0\nwarning: update available", true},
		{"codex-cli 0.153.1", false}, {"codex-cli 0.152.9", false}, {"codex 0.154.0", false}, {"", false}, {"codex-cli\n0.154.0", false}, {"codex-cli 0.154.0 (abc1234)", false}, {"codex-cli 0.154.0\x00", false},
	} {
		version, err := parseCodexVersion(item.output)
		if err == nil {
			err = codexVersionSupported(version)
		}
		if (err == nil) != item.accept {
			t.Errorf("%q: accepted=%t (%v), want %t", item.output, err == nil, err, item.accept)
		}
	}
	for _, release := range CodexVerifiedReleases {
		version, err := parseCodexVersion("codex-cli " + release.Version)
		if err != nil || codexVersionSupported(version) != nil {
			t.Errorf("verified release %s is not accepted by the version gate", release.Version)
		}
	}
}

// codexFeatureTable renders rows the way `codex features list` prints them.
func codexFeatureTable(rows [][3]string) string {
	var table strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&table, "%-40s %-18s %s\n", row[0], row[1], row[2])
	}
	return table.String()
}

func TestCodexRuntimeDerivedFeatureSwitches(t *testing.T) {
	rows := [][3]string{{"shell_tool", "stable", "true"}, {"new_default_tool", "stable", "true"}, {"under_dev_thing", "under development", "false"}, {"experimental_thing", "experimental", "true"}, {"sqlite", "removed", "true"}, {"web_search_cached", "deprecated", "false"}, {"unified_exec", "stable", "true"}, {"skip_host_skill_discovery", "under development", "false"}}
	for _, name := range codexRequiredControls {
		if name != "shell_tool" {
			rows = append(rows, [3]string{name, "stable", "true"})
		}
	}
	features, err := parseCodexFeatures(codexFeatureTable(rows))
	if err != nil {
		t.Fatal(err)
	}
	disable, err := codexFeaturePlan(features)
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, name := range disable {
		set[name] = true
	}
	for _, want := range []string{"new_default_tool", "under_dev_thing", "experimental_thing", "unified_exec", "shell_tool"} {
		if !set[want] {
			t.Errorf("%s is not disabled", want)
		}
	}
	for _, untouched := range []string{"sqlite", "web_search_cached", "skip_host_skill_discovery"} {
		if set[untouched] {
			t.Errorf("%s must not be toggled", untouched)
		}
	}
	// What the runtime reports with the switches applied: everything false
	// except the enabled skip and the runtime-forced exec engine selector.
	honored := make([][3]string, len(rows))
	for index, row := range rows {
		honored[index] = [3]string{row[0], row[1], "false"}
		if row[0] == codexSkipHostSkills || row[0] == "unified_exec" || row[1] == "removed" {
			honored[index][2] = row[2]
		}
	}
	honored[7][2] = "true"
	applied, err := parseCodexFeatures(codexFeatureTable(honored))
	if err != nil {
		t.Fatal(err)
	}
	if err := codexControlsHonored(applied, disable); err != nil {
		t.Fatalf("runtime-forced unified_exec must be tolerated: %v", err)
	}
	unhonored := append([][3]string{}, honored...)
	unhonored[1] = [3]string{"new_default_tool", "stable", "true"}
	applied, err = parseCodexFeatures(codexFeatureTable(unhonored))
	if err != nil {
		t.Fatal(err)
	}
	if err := codexControlsHonored(applied, disable); err == nil || !strings.Contains(err.Error(), "--disable new_default_tool was not honored") {
		t.Fatalf("unhonored switch accepted: %v", err)
	}
	skipOff := append([][3]string{}, honored...)
	skipOff[7] = [3]string{codexSkipHostSkills, "under development", "false"}
	applied, err = parseCodexFeatures(codexFeatureTable(skipOff))
	if err != nil {
		t.Fatal(err)
	}
	if err := codexControlsHonored(applied, disable); err == nil || !strings.Contains(err.Error(), "--enable skip_host_skill_discovery was not honored") {
		t.Fatalf("unhonored enable accepted: %v", err)
	}
	if _, err := codexFeaturePlan(features[1:]); err == nil || !strings.Contains(err.Error(), `required control "shell_tool"`) {
		t.Fatalf("missing control accepted: %v", err)
	}
	for _, malformed := range []string{"", "shell_tool stable maybe\n", "shell_tool\n", "shell_tool stable true\nshell_tool stable true\n"} {
		if _, err := parseCodexFeatures(malformed); err == nil {
			t.Errorf("accepted malformed table %q", malformed)
		}
	}
	switches := codexFeatureSwitches(disable)
	if switches[len(switches)-2] != "--enable" || switches[len(switches)-1] != codexSkipHostSkills {
		t.Fatalf("switches = %v", switches)
	}
	args := codexArguments("schema", "output", disable)
	if args[0] != "exec" || args[len(args)-1] != "-" || !strings.Contains(strings.Join(args, " "), "--disable new_default_tool") || !strings.Contains(strings.Join(args, " "), `-c project_doc_max_bytes=0`) {
		t.Fatalf("exec argv = %v", args)
	}
}

func TestCodexLinuxViewIsAllowList(t *testing.T) {
	view := codexLinuxView("/usr/local/bin/codex", "/etc/ssl/certs/ca-certificates.crt", "/tmp/work", "/home/u/.cache/acr/codex/home-1", "/tmp/work")
	joined := " " + strings.Join(view, " ") + " "
	for _, forbidden := range []string{" --ro-bind / / ", " /usr/lib ", " /home/u/.codex ", " /root "} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("view binds %q", forbidden)
		}
	}
	for _, required := range []string{" --unshare-user ", " --unshare-pid ", " --disable-userns ", " --cap-drop ALL ", " --die-with-parent ", " --new-session ", " --ro-bind /usr/local/bin/codex /opt/acr/codex ", " --ro-bind /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt ", " --ro-bind /etc/resolv.conf /etc/resolv.conf ", " --dev /dev ", " --proc /proc ", " --tmpfs /tmp ", " --bind /tmp/work /tmp/work ", " --bind /home/u/.cache/acr/codex/home-1 /home/u/.cache/acr/codex/home-1 ", " --chdir /tmp/work "} {
		if !strings.Contains(joined, required) {
			t.Fatalf("view lacks %q: %s", required, joined)
		}
	}
	binds := 0
	for index, argument := range view {
		if (argument == "--bind" || argument == "--ro-bind" || argument == "--ro-bind-try") && index+2 < len(view) {
			binds++
		}
	}
	if binds != 6 {
		t.Fatalf("view has %d binds, want exactly the six allow-listed entries", binds)
	}
	if hint := codexLinuxNamespaceHint("bwrap: No permissions to create a new namespace"); !strings.Contains(hint, "bubblewrap") {
		t.Fatal("namespace refusal carries no remedy")
	}
	if codexLinuxNamespaceHint("bwrap: Can't chdir to /x: No such file or directory") != "" {
		t.Fatal("chdir failure is not a namespace problem")
	}
}

func TestCodexBoundaryRefusesBeforeProposal(t *testing.T) {
	for _, platform := range codexPlatformsUnderTest {
		t.Run(platform, func(t *testing.T) {
			_, _, proposed := semanticFixture(t)
			native := codexFixtureFor(t, proposed, platform)
			unsupported := native
			unsupported.platform = "windows"
			if _, _, err := runCodexWithRuntime(context.Background(), "input", unsupported); err == nil || !strings.Contains(err.Error(), "platform windows is unsupported") || len(codexSpawns(t, native)) != 0 {
				t.Fatalf("unsupported platform: %v (spawns %d)", err, len(codexSpawns(t, native)))
			}
			// A wrapper that runs the command without any boundary makes the
			// negative canary succeed; the run refuses before the exec stage.
			put(t, filepath.Dir(native.wrapper), filepath.Base(native.wrapper), "#!/bin/sh\nif [ \"$1\" = -f ]; then shift 2; exec \"$@\"; fi\nwhile [ $# -gt 0 ]; do case \"$1\" in --) shift; break;; *) shift;; esac; done\nif [ \"$1\" = /opt/acr/codex ]; then shift; set -- \""+native.executable+"\" \"$@\"; fi\nexec \"$@\"\n", 0o755)
			if _, _, err := runCodexWithRuntime(context.Background(), "input", native); err == nil || !strings.Contains(err.Error(), "boundary was not established") || codexStdinRead(native) {
				t.Fatalf("no-op wrapper accepted: %v", err)
			}
		})
	}
}

func TestCodexHomeInstructionAndCredentialShapesRefused(t *testing.T) {
	_, _, proposed := semanticFixture(t)
	native := codexFixtureFor(t, proposed, "darwin")
	if err := os.Symlink("elsewhere", filepath.Join(native.home, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCodexWithRuntime(context.Background(), "input", native); err == nil || !strings.Contains(err.Error(), "instruction path") || len(codexSpawns(t, native)) != 0 {
		t.Fatalf("symlinked home instructions accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(native.home, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	for _, platform := range codexPlatformsUnderTest {
		t.Run(platform, func(t *testing.T) {
			_, _, proposed := semanticFixture(t)
			native := codexFixtureFor(t, proposed, platform)
			if err := os.Mkdir(filepath.Join(native.home, "auth.json"), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, _, err := runCodexWithRuntime(context.Background(), "input", native); err == nil || !strings.Contains(err.Error(), "credential path") || len(codexSpawns(t, native)) != 0 {
				t.Fatalf("directory credential accepted: %v", err)
			}
			if err := os.Remove(filepath.Join(native.home, "auth.json")); err != nil {
				t.Fatal(err)
			}
			// A regular credential is copied into the isolated home alone; the
			// configured home's other files never reach the provider.
			put(t, native.home, "auth.json", `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-fixture-credential-value-0123456789"}`, 0o600)
			put(t, native.home, "config.toml", "model_reasoning_effort = \"low\"\n", 0o600)
			put(t, native.home, "AGENTS.md", "inherited instructions\n", 0o644)
			codexSet(t, native, "behavior", "env-echo")
			_, run, err := runCodexWithRuntime(context.Background(), "input", native)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(run.Stderr, "sk-fixture-credential-value") || strings.Contains(run.Stderr, "sk-abc12***xyz") || !strings.Contains(run.Stderr, "sk-[redacted]") {
				t.Fatalf("credential fragments reached the report: %s", run.Stderr)
			}
			for _, line := range strings.Split(run.Stderr, "\n") {
				if strings.HasPrefix(line, "CODEX_HOME=") && strings.HasPrefix(strings.TrimPrefix(line, "CODEX_HOME="), native.home) {
					t.Fatal("provider received the configured home instead of the isolated copy")
				}
			}
			if !strings.Contains(run.Stderr, "HOME=") || strings.Contains(run.Stderr, "PATH="+os.Getenv("PATH")+"\n") {
				t.Fatalf("environment was not cleared: %s", run.Stderr)
			}
			if entries, err := os.ReadDir(native.homeBase); err != nil || len(entries) != 0 {
				t.Fatalf("isolated home retained: %v %v", entries, err)
			}
		})
	}
}

func TestCodexReportNeverCarriesCredential(t *testing.T) {
	const sentinel = "acr-test-credential-sentinel-6f2a-0123456789"
	for _, platform := range codexPlatformsUnderTest {
		for _, behavior := range []string{"env-echo", "process"} {
			t.Run(platform+"/"+behavior, func(t *testing.T) {
				native := codexFixtureFor(t, proposal{Edits: []proposedEdit{}, PolicyChanges: []PolicyChange{}}, platform)
				native.environ = append(native.environ, "CODEX_API_KEY="+sentinel)
				codexSet(t, native, "behavior", behavior)
				_, run, err := runCodexWithRuntime(context.Background(), "request", native)
				if (err != nil) != (behavior == "process") {
					t.Fatalf("provider result: %v", err)
				}
				data, marshalErr := json.Marshal(Report{AgentRuns: []AgentRun{run}})
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if strings.Contains(string(data), sentinel) {
					t.Fatalf("credential reached the report: %s", data)
				}
				if behavior == "env-echo" && !strings.Contains(run.Stderr, "CODEX_API_KEY=[redacted]") {
					t.Fatalf("credential was not forwarded through the environment: %s", run.Stderr)
				}
			})
		}
	}
}

func TestCodexCredentialRotationWarning(t *testing.T) {
	_, _, proposed := semanticFixture(t)
	native := codexFixture(t, proposed)
	put(t, native.home, "auth.json", `{"auth_mode":"chatgpt","tokens":{"refresh_token":"fixture-refresh-token-0123456789"}}`, 0o600)
	original := read(t, native.home, "auth.json")
	codexSet(t, native, "behavior", "rotate")
	_, run, err := runCodexWithRuntime(context.Background(), "input", native)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Warnings) != 1 || !strings.Contains(run.Warnings[0], "refreshed the copied credential") {
		t.Fatalf("warnings = %v", run.Warnings)
	}
	if read(t, native.home, "auth.json") != original {
		t.Fatal("configured credential was rewritten")
	}
	codexSet(t, native, "behavior", "success")
	if _, run, err := runCodexWithRuntime(context.Background(), "input", native); err != nil || len(run.Warnings) != 0 {
		t.Fatalf("unchanged credential warned: %v %v", err, run.Warnings)
	}
}

func TestCodexExecutableResolution(t *testing.T) {
	// Resolution returns canonical paths; macOS temp directories sit behind a
	// symlink, so the fixture root is canonicalized once up front.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lookPath := func(target string) func(string) (string, error) {
		return func(string) (string, error) { return target, nil }
	}
	native := filepath.Join(root, "native")
	put(t, root, "native", "\x7fELFfixture", 0o755)
	if got, err := resolveCodexExecutable("linux", "amd64", lookPath(native)); err != nil || got != native {
		t.Fatalf("native ELF: %q %v", got, err)
	}
	put(t, root, "mach", "\xcf\xfa\xed\xfefixture", 0o755)
	if got, err := resolveCodexExecutable("darwin", "arm64", lookPath(filepath.Join(root, "mach"))); err != nil || got != filepath.Join(root, "mach") {
		t.Fatalf("native Mach-O: %q %v", got, err)
	}
	if _, err := resolveCodexExecutable("darwin", "arm64", lookPath(native)); err == nil || !strings.Contains(err.Error(), "neither a native Codex binary nor the official npm launcher") {
		t.Fatalf("foreign binary accepted: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(native, link); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveCodexExecutable("linux", "amd64", lookPath(link)); err != nil || got != native {
		t.Fatalf("symlinked native: %q %v", got, err)
	}
	// The official npm layout: the launcher package plus a nested platform
	// package whose vendored binary is native.
	modules := filepath.Join(root, "lib", "node_modules", "@openai")
	put(t, modules, "codex/package.json", `{"name":"@openai/codex","version":"0.154.0"}`, 0o644)
	put(t, modules, "codex/bin/codex.js", "#!/usr/bin/env node\nconsole.log('launcher')\n", 0o755)
	launcher := filepath.Join(modules, "codex", "bin", "codex.js")
	if _, err := resolveCodexExecutable("linux", "arm64", lookPath(launcher)); err == nil || !strings.Contains(err.Error(), "native binary could not be located") || !strings.Contains(err.Error(), "npm install -g @openai/codex") {
		t.Fatalf("launcher without vendor accepted: %v", err)
	}
	put(t, modules, "codex/node_modules/@openai/codex-linux-arm64/package.json", `{"name":"@openai/codex","version":"0.154.0-linux-arm64"}`, 0o644)
	put(t, modules, "codex/node_modules/@openai/codex-linux-arm64/vendor/aarch64-unknown-linux-musl/bin/codex", "#!/bin/sh\necho not native\n", 0o755)
	if _, err := resolveCodexExecutable("linux", "arm64", lookPath(launcher)); err == nil || !strings.Contains(err.Error(), "is not a native linux executable") {
		t.Fatalf("script vendored binary accepted: %v", err)
	}
	put(t, modules, "codex/node_modules/@openai/codex-linux-arm64/vendor/aarch64-unknown-linux-musl/bin/codex", "\x7fELFvendored", 0o755)
	if got, err := resolveCodexExecutable("linux", "arm64", lookPath(launcher)); err != nil || got != filepath.Join(modules, "codex/node_modules/@openai/codex-linux-arm64/vendor/aarch64-unknown-linux-musl/bin/codex") {
		t.Fatalf("nested platform package: %q %v", got, err)
	}
	// A hoisted sibling platform package with a mismatched version is skipped;
	// a matching one resolves.
	put(t, modules, "codex-darwin-arm64/package.json", `{"name":"@openai/codex","version":"0.153.2-darwin-arm64"}`, 0o644)
	put(t, modules, "codex-darwin-arm64/vendor/aarch64-apple-darwin/bin/codex", "\xcf\xfa\xed\xfevendored", 0o755)
	if _, err := resolveCodexExecutable("darwin", "arm64", lookPath(launcher)); err == nil {
		t.Fatal("mismatched platform package version accepted")
	}
	put(t, modules, "codex-darwin-arm64/package.json", `{"name":"@openai/codex","version":"0.154.0-darwin-arm64"}`, 0o644)
	if got, err := resolveCodexExecutable("darwin", "arm64", lookPath(launcher)); err != nil || got != filepath.Join(modules, "codex-darwin-arm64/vendor/aarch64-apple-darwin/bin/codex") {
		t.Fatalf("hoisted platform package: %q %v", got, err)
	}
	put(t, root, "other/package.json", `{"name":"someone/else","version":"1.0.0"}`, 0o644)
	put(t, root, "other/bin/codex.js", "#!/usr/bin/env node\n", 0o755)
	if _, err := resolveCodexExecutable("linux", "amd64", lookPath(filepath.Join(root, "other", "bin", "codex.js"))); err == nil || !strings.Contains(err.Error(), "does not belong to the official @openai/codex package") {
		t.Fatalf("foreign launcher accepted: %v", err)
	}
	if _, err := resolveCodexExecutable("linux", "amd64", func(string) (string, error) { return "", exec.ErrNotFound }); err == nil || !strings.Contains(err.Error(), "configured Codex CLI is unavailable") {
		t.Fatalf("missing codex: %v", err)
	}
}

func TestCodexDarwinProfileDeniesSystemRoots(t *testing.T) {
	profile := codexDarwinProfile([]string{"/Users/u/.codex/AGENTS.md"}, codexDarwinSystemRoots)
	for _, required := range []string{"(allow default)", `(deny file-read-data (regex #"/(AGENTS([.]override)?[.]md|SKILL[.]md)$"))`, `(deny file-read* (subpath "/etc/codex"))`, `(deny file-read* (subpath "/private/etc/codex"))`, `(deny file-read* (subpath "/Library/Managed Preferences"))`, `(deny file-read-data (literal "/Users/u/.codex/AGENTS.md"))`} {
		if !strings.Contains(profile, required) {
			t.Fatalf("profile lacks %s:\n%s", required, profile)
		}
	}
}

func TestCodexTimeoutAndRunningCancellation(t *testing.T) {
	_, _, proposed := semanticFixture(t)
	native := codexFixture(t, proposed)
	expired, stop := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer stop()
	if _, _, err := runCodexWithRuntime(expired, "input", native); err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	codexSet(t, native, "listener", listener.Addr().String())
	codexSet(t, native, "behavior", "block")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := runCodexWithRuntime(ctx, "input", native); done <- err }()
	// The TCP handshake proves the provider is running; time is only a liveness cap.
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("provider did not stop on cancellation")
	}
}

func TestCodexEventOrderingAndStartupErrors(t *testing.T) {
	init := `{"type":"thread.started","thread_id":"test"}` + "\n" + `{"type":"item.completed","item":{"type":"error","message":` + quoted(disabledCodeHost) + `}}` + "\n"
	body := `{"type":"turn.started"}` + "\n" + `{"type":"item.completed","item":{"type":"agent_message","text":"{}"}}` + "\n" + `{"type":"turn.completed"}`
	if _, _, err := codexFinal(init+body, ""); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{init + `{"type":"turn.started"}` + "\n" + `{"type":"item.completed","item":{"type":"command_execution","type":"agent_message","text":"{}"}}` + "\n" + `{"type":"turn.completed"}`, body, init + body + "\n" + `{"type":"turn.completed"}`, init + `{"type":"turn.failed"}`, `{"type":"thread.started","thread_id":"test"}` + "\n" + `{"type":"item.completed","item":{"type":"error","message":"unexpected MCP server loaded"}}`, init + `{"type":"item.completed","item":{"type":"mcp_tool_call"}}`} {
		if _, _, err := codexFinal(value, ""); err == nil {
			t.Fatal("accepted invalid initialization/tool/completion")
		}
	}
}
func quoted(value string) string { return strconv.Quote(value) }

func TestCodexKnownReconnectRequiresCompleteValidatedStream(t *testing.T) {
	init := `{"type":"thread.started","thread_id":"test"}` + "\n" + `{"type":"item.completed","item":{"type":"error","message":` + quoted(disabledCodeHost) + `}}` + "\n"
	start := `{"type":"turn.started"}` + "\n"
	retry := `{"type":"error","message":"Reconnecting... 2/5 (stream disconnected before completion: idle timeout waiting for websocket)"}` + "\n"
	end := `{"type":"item.completed","item":{"type":"agent_message","text":"{}"}}` + "\n" + `{"type":"turn.completed"}`
	if final, warnings, err := codexFinal(init+start+retry+end, ""); err != nil || final != "{}" || len(warnings) != 1 {
		t.Fatalf("completed recovery failed: %v", err)
	}
	for _, stream := range []string{init + retry + start + end, init + start + retry, init + start + retry + retry + end, init + start + strings.Replace(retry, "idle timeout waiting for websocket", "authentication failed", 1) + end, init + start + retry + `{"type":"turn.failed"}`} {
		if _, _, err := codexFinal(stream, ""); err == nil {
			t.Fatal("invalid or incomplete recovery passed")
		}
	}
}

func TestCodexReportOmitsRawRequest(t *testing.T) {
	const request = "PRIVATE_SOURCE_REQUEST_SENTINEL"
	for _, behavior := range []string{"success", "process"} {
		t.Run(behavior, func(t *testing.T) {
			native := codexFixture(t, proposal{Edits: []proposedEdit{}, PolicyChanges: []PolicyChange{}})
			codexSet(t, native, "behavior", behavior)
			_, run, err := runCodexWithRuntime(context.Background(), request, native)
			if (err != nil) != (behavior == "process") {
				t.Fatalf("provider result: %v", err)
			}
			assertDigestOnlyRequest(t, run, request)
		})
	}
}

// Multiplicity inputs follow the full11 reviewer/tester discriminators and
// judge12 contract; these synthetic executables do not claim native emission.
func TestCorrection12CodexFinalMessages(t *testing.T) {
	for _, dry := range []bool{true, false} {
		for _, behavior := range []string{"success", "missing-final", "empty-final", "empty-first", "identical", "distinct"} {
			t.Run(behavior+"/dry="+strconv.FormatBool(dry), func(t *testing.T) {
				root, opts, p := semanticFixture(t)
				opts.Agent, opts.DryRun = "codex", dry
				native := codexFixture(t, p)
				codexSet(t, native, "behavior", behavior)
				clean := correctionStageCheck(t)
				defer clean()
				before := correction12Inventory(t, root)
				calls := 0
				provider := func(ctx context.Context, _ string, request string) (proposal, AgentRun, error) {
					calls++
					return runCodexWithRuntime(ctx, request, native)
				}
				plan, err := prepareWithProvider(context.Background(), opts, provider)
				correction12Unchanged(t, root, before)
				if behavior != "success" {
					if err == nil {
						t.Fatal("accepted missing, empty or multiple completed messages")
					}
					if calls != 1 || len(plan.Report.AgentRuns) != 1 || plan.Report.AgentRuns[0].Failure == "" || plan.Report.AgentRuns[0].Stdout == "" {
						t.Fatalf("lost process refusal evidence: %+v", plan.Report)
					}
					if _, applyErr := plan.Apply(); applyErr == nil {
						t.Fatal("refused plan applied")
					}
					correction12Unchanged(t, root, before)
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				correction12Apply(t, root, opts, plan, before, provider)
				if calls != 1 || read(t, root, p.Edits[0].Path) != p.Edits[0].Content {
					t.Fatal("single-message candidate or provider-free rerun changed")
				}
			})
		}
	}
}
