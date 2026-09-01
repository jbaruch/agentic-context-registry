package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Runner parses commands, invokes the application boundary, and renders output.
type Runner struct {
	stdout  io.Writer
	stderr  io.Writer
	app     Application
	version string
}

// New returns a command runner using the supplied application boundary.
func New(stdout, stderr io.Writer, app Application, version string) *Runner {
	return &Runner{stdout: stdout, stderr: stderr, app: app, version: version}
}

// Run executes one acr command and returns its stable process exit code.
func (r *Runner) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return r.renderText(rootHelp())
	}

	switch args[0] {
	case "help", "--help", "-h":
		return r.runHelp(args[1:])
	case "version", "--version", "-v":
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
		return r.renderError(string(command), invocation.Output == OutputJSON, commandError(err))
	}
	if invocation.Output == OutputJSON {
		return r.renderJSONSuccess(string(command), result.Value)
	}
	if result.Message != "" {
		return r.renderText(result.Message + "\n")
	}
	return ExitSuccess
}

func (r *Runner) runHelp(args []string) int {
	if len(args) == 0 {
		return r.renderText(rootHelp())
	}
	if len(args) != 1 {
		return r.renderError("", wantsJSON(args), usageError("usage: acr help [COMMAND]"))
	}
	command, ok := commandFor(args[0])
	if !ok {
		return r.renderError("", false, usageError("unknown command %q; run 'acr help' to list available commands", args[0]))
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
			return r.renderText("Usage:\n  acr version [--json]\n")
		default:
			return r.renderError("version", jsonOutput, usageError("unknown flag or argument %q for version; run 'acr version --help' for supported options", argument))
		}
	}
	if jsonOutput {
		return r.renderJSONSuccess("version", map[string]string{"version": r.version})
	}
	return r.renderText(r.version + "\n")
}

func (r *Runner) renderText(output string) int {
	if _, err := fmt.Fprint(r.stdout, output); err != nil {
		fmt.Fprintf(r.stderr, "acr: write output: %v; verify stdout is writable and retry the command\n", err)
		return ExitOperational
	}
	return ExitSuccess
}

func (r *Runner) renderJSONSuccess(command string, value any) int {
	envelope := struct {
		OK      bool   `json:"ok"`
		Command string `json:"command"`
		Result  any    `json:"result,omitempty"`
	}{
		OK:      true,
		Command: command,
		Result:  value,
	}
	if err := json.NewEncoder(r.stdout).Encode(envelope); err != nil {
		fmt.Fprintf(r.stderr, "acr: encode JSON output: %v; retry without --json, then report the failure at https://github.com/jbaruch/agentic-context-registry/issues if it persists\n", err)
		return ExitOperational
	}
	return ExitSuccess
}

func (r *Runner) renderError(command string, jsonOutput bool, err *Error) int {
	if jsonOutput {
		envelope := struct {
			OK      bool   `json:"ok"`
			Command string `json:"command,omitempty"`
			Error   struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}{
			OK:      false,
			Command: command,
		}
		envelope.Error.Code = err.Code
		envelope.Error.Message = err.Message
		if encodeErr := json.NewEncoder(r.stderr).Encode(envelope); encodeErr != nil {
			fmt.Fprintf(r.stderr, "acr: encode JSON diagnostic: %v; retry without --json, then report the failure at https://github.com/jbaruch/agentic-context-registry/issues if it persists\n", encodeErr)
			return ExitOperational
		}
		return err.ExitCode
	}
	prefix := "acr"
	if command != "" {
		prefix += " " + command
	}
	if _, writeErr := fmt.Fprintf(r.stderr, "%s: %s\n", prefix, err.Message); writeErr != nil {
		return ExitOperational
	}
	return err.ExitCode
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
	for _, command := range commandOrder {
		spec := commandSpecs[command]
		fmt.Fprintf(&builder, "  %-10s %s\n", command, spec.summary)
	}
	builder.WriteString("  version    Print the acr version\n")
	builder.WriteString("  help       Show help for a command\n")
	builder.WriteString("\nRun 'acr help COMMAND' for command-specific options.\n")
	return builder.String()
}
