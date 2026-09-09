package producerconvert

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func codexFixture(t *testing.T, proposed proposal) codexRuntime {
	t.Helper()
	directory := t.TempDir()
	encoded, err := json.Marshal(proposed)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACR_CODEX_PROPOSAL", string(encoded))
	t.Setenv("ACR_CODEX_INIT", disabledCodeHost)
	t.Setenv("ACR_CODEX_BEHAVIOR", "success")
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	put(t, directory, "sandbox", "#!/bin/sh\nif [ \"$3\" = /bin/cat ]; then printf 'Operation not permitted\\n' >&2; exit 1; fi\nshift 2\nexec \"$@\"\n", 0o755)
	put(t, directory, "codex", `#!/usr/bin/env python3
import json, os, sys
behavior = os.environ['ACR_CODEX_BEHAVIOR']
if sys.argv[1:] == ['--version']:
    print('codex-cli 0.153.2' if behavior != 'version' else 'codex-cli 0.153.3')
    sys.exit(0)
request = sys.stdin.read()
if behavior == 'block':
    import socket
    host, port = os.environ['ACR_CODEX_LISTENER'].split(':')
    with socket.create_connection((host, int(port))) as stream:
        stream.sendall(b'ready')
        stream.recv(1)
    sys.exit(3)
if behavior == 'process':
    print('provider unavailable', file=sys.stderr)
    sys.exit(7)
if behavior == 'overflow':
    sys.stdout.write('x' * 12582913)
    sys.exit(0)
output = sys.argv[sys.argv.index('--output-last-message') + 1]
proposal = os.environ['ACR_CODEX_PROPOSAL']
if behavior == 'invalid': proposal = '{invalid'
with open(output, 'w') as handle: handle.write(proposal if behavior != 'mismatch' else '{}')
def event(value): print(json.dumps(value))
event({'type':'thread.started', 'thread_id':'fixture-thread'})
if behavior != 'initialization':
    event({'type':'item.completed','item':{'type':'error','message':os.environ['ACR_CODEX_INIT']}})
event({'type':'turn.started'})
if behavior == 'tool': event({'type':'item.completed','item':{'type':'command_execution','command':'cat secret'}})
if behavior == 'router': print('ERROR codex_core::tools::router: error=code-mode host is disabled',file=sys.stderr)
event({'type':'item.completed','item':{'type':'agent_message','text':proposal}})
if behavior != 'truncated': event({'type':'turn.completed','usage':{'input_tokens':1,'output_tokens':1}})
`, 0o755)
	return codexRuntime{"darwin", filepath.Join(directory, "sandbox"), directory}
}

func TestCodexProposalAppliesThroughOriginalTransaction(t *testing.T) {
	root, options, proposed := semanticFixture(t)
	options.Agent = "codex"
	native := codexFixture(t, proposed)
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
	if len(plan.Report.AgentRuns) != 1 || plan.Report.AgentRuns[0].RuntimeVersion != codexVersion || plan.Report.AgentRuns[0].Isolation == "" {
		t.Fatal("missing runtime evidence")
	}
	if _, err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if read(t, root, proposed.Edits[0].Path) != proposed.Edits[0].Content {
		t.Fatal("proposal not applied")
	}
	current, err := PrepareContext(context.Background(), options)
	if err != nil || !current.Report.Current {
		t.Fatalf("inert rerun: %v %+v", err, current.Report)
	}
}

func TestCodexProviderFailuresPreserveInput(t *testing.T) {
	for _, behavior := range []string{"version", "process", "overflow", "invalid", "mismatch", "initialization", "tool", "router", "truncated"} {
		t.Run(behavior, func(t *testing.T) {
			root, _, proposed := semanticFixture(t)
			native := codexFixture(t, proposed)
			t.Setenv("ACR_CODEX_BEHAVIOR", behavior)
			before := treeAt(t, root)
			_, run, err := runCodexWithRuntime(context.Background(), "bounded selected text", native)
			if err == nil || run.Failure == "" {
				t.Fatalf("accepted %s: %+v", behavior, run)
			}
			if !matches(before, treeAt(t, root)) {
				t.Fatal("provider changed source")
			}
		})
	}
}

func TestCodexRejectsUnsupportedIsolationBeforeProposal(t *testing.T) {
	_, _, proposed := semanticFixture(t)
	native := codexFixture(t, proposed)
	native.platform = "linux"
	if _, _, err := runCodexWithRuntime(context.Background(), "input", native); err == nil || !strings.Contains(err.Error(), "platform") {
		t.Fatal(err)
	}
	native.platform = "darwin"
	put(t, filepath.Dir(native.sandbox), "sandbox", "#!/bin/sh\nexit 0\n", 0o755)
	if _, _, err := runCodexWithRuntime(context.Background(), "input", native); err == nil || !strings.Contains(err.Error(), "boundary") {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(native.home, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCodexWithRuntime(context.Background(), "input", native); err == nil || !strings.Contains(err.Error(), "instruction path") {
		t.Fatal(err)
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
	t.Setenv("ACR_CODEX_LISTENER", listener.Addr().String())
	t.Setenv("ACR_CODEX_BEHAVIOR", "block")
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
