package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/buildinfo"
)

// Build is the CLI-facing alias for the resolved executable identity.
type Build = buildinfo.Build

// Runner parses commands, invokes the application boundary, and renders output.
type Runner struct {
	stdout io.Writer
	stderr io.Writer
	app    Application
	build  buildinfo.Build
}

// New returns a command runner using the supplied application boundary.
func New(stdout, stderr io.Writer, app Application, build buildinfo.Build) *Runner {
	return &Runner{stdout: stdout, stderr: stderr, app: app, build: build}
}

// Run executes one acr command and returns its stable process exit code.
func (r *Runner) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return r.renderText(rootHelp())
	}

	switch metaCommandFor(args[0]) {
	case metaHelp:
		return r.runHelp(args[1:])
	case metaVersion:
		return r.runVersion(args[1:])
	}

	command, ok := commandFor(args[0])
	if !ok {
		return r.renderError("", wantsJSON(args[1:]), usageError("unknown command %q; run 'acr help' to list available commands", args[0]))
	}
	invocation, help, err := parseInvocation(command, args[1:])
	if err != nil {
		return r.renderError(string(command), wantsJSON(args[1:]), commandError(err))
	}
	if help {
		return r.renderText(helpFor(command))
	}

	result, err := r.app.Execute(ctx, invocation)
	if err != nil {
		if invocation.Output != OutputJSON {
			if r.renderNotices(result.Notices) != ExitSuccess {
				return ExitOperational
			}
		}
		return r.renderApplicationError(string(command), invocation.Output == OutputJSON, result, commandError(err))
	}
	if r.renderNotices(result.Notices) != ExitSuccess {
		return ExitOperational
	}
	exitCode := result.ExitCode
	if exitCode != ExitSuccess && !isFailureExitCode(exitCode) {
		exitCode = ExitOperational
	}
	if invocation.Output == OutputJSON {
		if rendered := r.renderJSONResult(string(command), result, exitCode == ExitSuccess); rendered != ExitSuccess {
			return rendered
		}
		return exitCode
	}
	if result.Message != "" {
		if rendered := r.renderText(result.Message + "\n"); rendered != ExitSuccess {
			return rendered
		}
	}
	return exitCode
}

func (r *Runner) runHelp(args []string) int {
	if len(args) == 0 {
		return r.renderText(rootHelp())
	}
	if len(args) != 1 {
		return r.renderError("", wantsJSON(args), usageError("usage: acr help [COMMAND]"))
	}
	switch metaCommandFor(args[0]) {
	case metaVersion:
		return r.renderText(versionHelp())
	case metaHelp:
		return r.renderText(helpCommandHelp())
	}
	command, ok := commandFor(args[0])
	if !ok {
		return r.renderError("", wantsJSON(args), usageError("unknown command %q; run 'acr help' to list available commands", args[0]))
	}
	return r.renderText(helpFor(command))
}

func (r *Runner) runVersion(args []string) int {
	jsonOutput := wantsJSON(args)
	for index, argument := range args {
		switch argument {
		case "--json":
		case "--":
			if index+1 < len(args) {
				return r.renderError("version", jsonOutput, usageError("usage: acr version [--json]"))
			}
		case "--help", "-h":
			return r.renderText(versionHelp())
		default:
			return r.renderError("version", jsonOutput, usageError("unknown flag or argument %q for version; run 'acr version --help' for supported options", argument))
		}
	}
	if jsonOutput {
		return r.renderJSONResult("version", Result{Value: r.build}, true)
	}
	return r.renderText(r.build.String() + "\n")
}

func (r *Runner) renderText(output string) int {
	if _, err := writeAll(r.stdout, []byte(output)); err != nil {
		diagnostic := fmt.Sprintf("acr: write output: %v; verify stdout is writable and retry the command\n", err)
		_, _ = writeAll(r.stderr, []byte(diagnostic))
		return ExitOperational
	}
	return ExitSuccess
}

func (r *Runner) renderJSONResult(command string, result Result, ok bool) int {
	value := valueWithNotices(result.Value, result.Notices)
	envelope := struct {
		OK      bool   `json:"ok"`
		Command string `json:"command"`
		Result  any    `json:"result,omitempty"`
	}{
		OK:      ok,
		Command: command,
		Result:  value,
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return r.renderError(command, true, &Error{
			ExitCode:   ExitOperational,
			Code:       "json_encoding_failed",
			Message:    fmt.Sprintf("encode JSON output: %v; retry without --json, then report the failure at https://github.com/jbaruch/agentic-context-registry/issues if it persists", err),
			Cause:      err,
			actionable: true,
		})
	}
	encoded = append(encoded, '\n')
	if _, err := writeAll(r.stdout, encoded); err != nil {
		return r.renderError(command, true, &Error{
			ExitCode:   ExitOperational,
			Code:       "output_failed",
			Message:    fmt.Sprintf("write JSON output: %v; verify stdout is writable and retry the command", err),
			Cause:      err,
			actionable: true,
		})
	}
	return ExitSuccess
}

type noticesEnvelope struct {
	Value   any
	Notices []Notice
}

func valueWithNotices(value any, notices []Notice) any {
	if len(notices) == 0 {
		return value
	}
	return noticesEnvelope{Value: value, Notices: notices}
}

func (envelope noticesEnvelope) MarshalJSON() ([]byte, error) {
	encoded, err := json.Marshal(envelope.Value)
	if err != nil {
		return nil, err
	}
	if len(encoded) != 0 && encoded[0] == '{' {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &object); err != nil {
			return nil, fmt.Errorf("decode JSON result object: %w", err)
		}
		notices, err := json.Marshal(envelope.Notices)
		if err != nil {
			return nil, err
		}
		object["notices"] = notices
		return json.Marshal(object)
	}
	return json.Marshal(struct {
		Value   any      `json:"value,omitempty"`
		Notices []Notice `json:"notices"`
	}{Value: envelope.Value, Notices: envelope.Notices})
}

func (r *Runner) renderNotices(notices []Notice) int {
	for _, notice := range notices {
		if _, err := writeAll(r.stderr, []byte(notice.Code+": "+notice.Message+"\n")); err != nil {
			return ExitOperational
		}
	}
	return ExitSuccess
}

func (r *Runner) renderError(command string, jsonOutput bool, err *Error) int {
	return r.renderApplicationError(command, jsonOutput, Result{}, err)
}

func (r *Runner) renderApplicationError(command string, jsonOutput bool, result Result, err *Error) int {
	if jsonOutput {
		envelope := struct {
			OK      bool   `json:"ok"`
			Command string `json:"command,omitempty"`
			Result  any    `json:"result,omitempty"`
			Error   struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Field   string `json:"field,omitempty"`
				Remedy  string `json:"remedy,omitempty"`
			} `json:"error"`
		}{
			OK:      false,
			Command: command,
			Result:  result.Value,
		}
		envelope.Error.Code = err.Code
		envelope.Error.Message = err.Message
		envelope.Error.Field = err.Field
		envelope.Error.Remedy = err.Remedy
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if encodeErr := encoder.Encode(envelope); encodeErr != nil {
			return ExitOperational
		}
		if _, writeErr := writeAll(r.stderr, encoded.Bytes()); writeErr != nil {
			return ExitOperational
		}
		return err.ExitCode
	}
	prefix := "acr"
	if command != "" {
		prefix += " " + command
	}
	diagnostic := fmt.Sprintf("%s: %s\n", prefix, err.Message)
	if result.Message != "" {
		diagnostic += result.Message
		if !strings.HasSuffix(diagnostic, "\n") {
			diagnostic += "\n"
		}
	}
	if _, writeErr := writeAll(r.stderr, []byte(diagnostic)); writeErr != nil {
		return ExitOperational
	}
	return err.ExitCode
}

func writeAll(writer io.Writer, data []byte) (int, error) {
	total := 0
	for total < len(data) {
		written, err := writer.Write(data[total:])
		total += written
		if err != nil {
			return total, err
		}
		if written == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func wantsJSON(args []string) bool {
	for _, argument := range args {
		if argument == "--" {
			break
		}
		if argument == "--json" {
			return true
		}
	}
	return false
}

func rootHelp() string {
	var builder strings.Builder
	builder.WriteString("acr is the Agentic Context Registry CLI.\n\n")
	builder.WriteString("Usage:\n  acr COMMAND [OPTIONS]\n\nCommands:\n")
	for _, command := range dispatchableCommands() {
		spec := commandSpecs[command]
		fmt.Fprintf(&builder, "  %-10s %s\n", command, spec.summary)
	}
	for _, meta := range metaCommandOrder {
		fmt.Fprintf(&builder, "  %-10s %s\n", meta.name, meta.summary)
	}
	builder.WriteString("\nRun 'acr help COMMAND' for command-specific options.\n")
	return builder.String()
}

// metaCommandSpec describes a command that reads no project state. The Runner
// dispatches these before the registry, and their names, aliases and usage
// come from here rather than from a literal in each caller.
type metaCommandSpec struct {
	name    string
	aliases []string
	summary string
	usage   string
}

const (
	metaNone    = ""
	metaVersion = "version"
	metaHelp    = "help"
)

var metaCommandOrder = []metaCommandSpec{
	{name: metaVersion, aliases: []string{"--version", "-v"}, summary: "Print the acr version", usage: "acr version [--json]"},
	{name: metaHelp, aliases: []string{"--help", "-h"}, summary: "Show help for a command", usage: "acr help [COMMAND]"},
}

// metaCommandFor returns the meta command one argument selects, by name or by
// alias, and metaNone when the argument selects none.
func metaCommandFor(value string) string {
	for _, meta := range metaCommandOrder {
		if value == meta.name {
			return meta.name
		}
		for _, alias := range meta.aliases {
			if value == alias {
				return meta.name
			}
		}
	}
	return metaNone
}

func metaUsage(name string) string {
	for _, meta := range metaCommandOrder {
		if meta.name == name {
			return meta.usage
		}
	}
	return ""
}

func versionHelp() string {
	return "Usage:\n  " + metaUsage(metaVersion) + "\n"
}

func helpCommandHelp() string {
	return "Usage:\n  " + metaUsage(metaHelp) + "\n\nShow root or command-specific help.\n"
}
