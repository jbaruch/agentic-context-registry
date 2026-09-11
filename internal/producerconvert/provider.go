package producerconvert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const maxProposalBytes = 4 << 20
const maxProviderBytes = 12 << 20
const maxRequestBytes = 2 << 20

// AgentRun records a request digest and bounded native response, including failures.
// It is returned to the caller, never stored in the portable source receipt.
type AgentRun struct {
	Provider       string   `json:"provider"`
	RuntimeVersion string   `json:"runtimeVersion,omitempty"`
	Isolation      string   `json:"isolation,omitempty"`
	Scope          string   `json:"scope,omitempty"`
	Arguments      []string `json:"arguments"`
	RequestDigest  string   `json:"requestDigest"`
	Stdout         string   `json:"stdout"`
	Stderr         string   `json:"stderr"`
	Warnings       []string `json:"warnings,omitempty"`
	Failure        string   `json:"failure,omitempty"`
}

type PolicyChange struct {
	Path string `json:"path"`
	From string `json:"from"`
	To   string `json:"to"`
}
type replacement struct {
	Old   string `json:"old"`
	New   string `json:"new"`
	Count int    `json:"count"`
}
type proposedEdit struct {
	Path         string        `json:"path"`
	BeforeDigest string        `json:"beforeDigest"`
	Action       string        `json:"action"`
	Content      string        `json:"content"`
	Replacements []replacement `json:"replacements"`
}
type proposal struct {
	Edits         []proposedEdit `json:"edits"`
	PolicyChanges []PolicyChange `json:"policyChanges"`
}

const proposalSchema = `{"type":"object","additionalProperties":false,"required":["edits","policyChanges"],"properties":{"edits":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["path","beforeDigest","action","content","replacements"],"properties":{"path":{"type":"string"},"beforeDigest":{"type":"string"},"action":{"type":"string","enum":["replace","patch","remove","create"]},"content":{"type":"string"},"replacements":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["old","new","count"],"properties":{"old":{"type":"string"},"new":{"type":"string"},"count":{"type":"integer"}}}}}}},"policyChanges":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["path","from","to"],"properties":{"path":{"type":"string"},"from":{"type":"string"},"to":{"type":"string"}}}}}}`

// boundedCapture stops the subprocess instead of accumulating unbounded output.
type boundedCapture struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	overflow bool
}

func (b *boundedCapture) Len() int       { return b.buffer.Len() }
func (b *boundedCapture) String() string { return b.buffer.String() }
func (b *boundedCapture) Bytes() []byte  { return b.buffer.Bytes() }

func (b *boundedCapture) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) > b.limit-b.Len() {
		remaining := b.limit - b.Len()
		if remaining > 0 {
			_, _ = b.buffer.Write(p[:remaining])
		}
		b.overflow = true
		b.cancel()
		return remaining, fmt.Errorf("provider output exceeds %d bytes", b.limit)
	}
	return b.buffer.Write(p)
}

func runProvider(ctx context.Context, provider, request string) (result proposal, evidence AgentRun, err error) {
	if provider == "codex" {
		return runCodex(ctx, request)
	}
	evidence.Provider, evidence.RequestDigest = provider, digest([]byte(request))
	defer func() {
		if err != nil {
			evidence.Failure = err.Error()
		}
	}()
	if provider != "claude" {
		return result, evidence, refuse("agent_unavailable", "--agent", "select --agent codex or --agent claude explicitly, or use deterministic mode")
	}
	if len(request) > maxRequestBytes {
		return result, evidence, fmt.Errorf("provider input exceeds %d bytes", maxRequestBytes)
	}
	executable, err := exec.LookPath("claude")
	if err != nil {
		return result, evidence, fmt.Errorf("configured Claude CLI is unavailable: %w", err)
	}
	directory, err := os.MkdirTemp("", "acr-proposal-")
	if err != nil {
		return result, evidence, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(directory)) }()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	args := []string{"--print", "--effort", "medium", "--safe-mode", "--tools", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--disable-slash-commands", "--no-session-persistence", "--verbose", "--output-format", "stream-json", "--json-schema", proposalSchema, "--permission-mode", "dontAsk", "--permission-prompts", "none"}
	evidence.Arguments = append([]string{executable}, args...)
	command := exec.CommandContext(ctx, executable, args...)
	command.Dir = directory
	command.Stdin = strings.NewReader(request)
	// Claude keeps the configured account. It receives no source path and has
	// neither filesystem tools nor MCP tools; safe mode ignores project rules.
	command.WaitDelay = 2 * time.Second
	stdout := &boundedCapture{limit: maxProviderBytes, cancel: cancel}
	stderr := &boundedCapture{limit: maxProposalBytes, cancel: cancel}
	command.Stdout, command.Stderr = stdout, stderr
	processErr := command.Run()
	evidence.Stdout, evidence.Stderr = stdout.String(), stderr.String()
	if stdout.overflow || stderr.overflow {
		return result, evidence, fmt.Errorf("provider output exceeded its byte limit; no source changes were made")
	}
	if ctx.Err() != nil {
		return result, evidence, fmt.Errorf("provider canceled or timed out: %w", ctx.Err())
	}
	raw, envelopeErr := claudeProposal(stdout.Bytes())
	if processErr != nil {
		if envelopeErr != nil {
			return result, evidence, fmt.Errorf("Claude failed: %w; %v; inspect agentRuns stdout/stderr", processErr, envelopeErr)
		}
		return result, evidence, fmt.Errorf("Claude failed: %w; inspect agentRuns stdout/stderr", processErr)
	}
	err = envelopeErr
	if err != nil {
		return result, evidence, err
	}
	if len(raw) > maxProposalBytes {
		return result, evidence, fmt.Errorf("structured proposal exceeds %d bytes", maxProposalBytes)
	}
	err = strictJSON(raw, &result)
	return result, evidence, err
}

func claudeProposal(data []byte) (json.RawMessage, error) {
	var events []struct {
		Type       string            `json:"type"`
		Subtype    string            `json:"subtype"`
		Tools      []string          `json:"tools"`
		MCPServers []json.RawMessage `json:"mcp_servers"`
		IsError    bool              `json:"is_error"`
		Result     string            `json:"result"`
		APIStatus  int               `json:"api_error_status"`
		Output     json.RawMessage   `json:"structured_output"`
		Message    struct {
			Content []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content"`
		} `json:"message"`
	}
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("[")) {
		if err := json.Unmarshal(data, &events); err != nil {
			return nil, fmt.Errorf("invalid Claude event envelope: %w", err)
		}
	} else {
		decoder := json.NewDecoder(bytes.NewReader(data))
		for {
			var raw json.RawMessage
			err := decoder.Decode(&raw)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("invalid Claude event stream: %w", err)
			}
			var one = events[:0:0]
			wrapped := append(append([]byte{'['}, raw...), ']')
			if err := json.Unmarshal(wrapped, &one); err != nil {
				return nil, fmt.Errorf("invalid Claude event: %w", err)
			}
			events = append(events, one...)
		}
	}
	initialized := false
	var output json.RawMessage
	for _, event := range events {
		if event.Type == "system" && event.Subtype == "init" {
			if initialized || len(event.MCPServers) != 0 {
				return nil, fmt.Errorf("Claude did not establish a unique tool-free session")
			}
			for _, tool := range event.Tools {
				if tool != "StructuredOutput" {
					return nil, fmt.Errorf("Claude exposed unexpected tool %q", tool)
				}
			}
			initialized = true
		}
		for _, part := range event.Message.Content {
			if part.Type == "tool_use" && part.Name != "StructuredOutput" {
				return nil, fmt.Errorf("Claude attempted unexpected tool %q", part.Name)
			}
		}
		if event.Type == "result" {
			if event.IsError {
				message := event.Result
				if len(message) > 1000 {
					message = message[:1000] + "…"
				}
				return nil, fmt.Errorf("Claude result failed (API status %d): %q", event.APIStatus, message)
			}
			if output != nil || event.IsError || event.Subtype != "success" || len(event.Output) == 0 {
				return nil, fmt.Errorf("Claude returned an error, duplicate, or incomplete result")
			}
			output = event.Output
		}
	}
	if !initialized || len(output) == 0 {
		return nil, fmt.Errorf("Claude omitted initialization or structured result")
	}
	return output, nil
}

// Go's JSON decoder accepts duplicate keys. Refuse them before strict decoding
// so a reviewed value cannot disagree with the value the transaction uses.
func strictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return fmt.Errorf("duplicate or invalid JSON key %v", key)
				}
				seen[name] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := walk(); err != nil {
		return fmt.Errorf("invalid proposal JSON: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("proposal contains trailing JSON")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid proposal schema: %w", err)
	}
	return nil
}
