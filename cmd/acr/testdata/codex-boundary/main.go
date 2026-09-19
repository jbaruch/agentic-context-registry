// Deterministic native Codex protocol fixture. Never contacts a service.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var proposed string

const rotated = "synthetic-cli-rotated-credential-0123456789"

func main() {
	args := os.Args[1:]
	var rest []string
	config := map[string]string{}
	switches := map[string]bool{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c":
			i++
			key, value, _ := strings.Cut(args[i], "=")
			config[key] = value
		case "--disable", "--enable":
			enabled := args[i] == "--enable"
			i++
			switches[args[i]] = enabled
		default:
			rest = append(rest, args[i])
		}
	}
	joined := strings.Join(rest, " ")
	switch {
	case joined == "--version" || len(rest) > 0 && rest[len(rest)-1] == "-V":
		fmt.Println("codex-cli 0.154.0")
	case joined == "exec --help":
		fmt.Println("--ignore-user-config --ignore-rules --strict-config --ephemeral --sandbox --skip-git-repo-check --color --json --output-schema --output-last-message --config --disable --enable")
	case joined == "features list":
		for _, name := range strings.Fields("shell_tool apps plugins multi_agent multi_agent_v2 hooks browser_use browser_use_external computer_use image_generation in_app_browser in_app_local_automation view_image goals sleep_tool code_mode code_mode_host skill_search skill_mcp_dependency_install workspace_dependencies memories remote_plugin skip_host_skill_discovery") {
			fmt.Printf("%s stable %t\n", name, switches[name])
		}
	case joined == "debug prompt-input":
		fmt.Println(config["developer_instructions"])
	case len(rest) > 0 && rest[0] == "exec" && rest[len(rest)-1] == "-":
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			panic(err)
		}
		decoded, err := base64.StdEncoding.DecodeString(proposed)
		if err != nil {
			panic(err)
		}
		proposed = string(decoded)
		auth := filepath.Join(os.Getenv("CODEX_HOME"), "auth.json")
		if err := os.WriteFile(auth, []byte(`{"tokens":{"refresh_token":"`+rotated+`"}}`), 0600); err != nil {
			panic(err)
		}
		fmt.Fprintln(os.Stderr, rotated)
		output := ""
		for i, arg := range rest {
			if arg == "--output-last-message" {
				output = rest[i+1]
			}
		}
		if err := os.WriteFile(output, []byte(proposed), 0600); err != nil {
			panic(err)
		}
		emit := func(value any) {
			if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
				panic(err)
			}
		}
		emit(map[string]any{"type": "thread.started", "thread_id": "native-fixture"})
		emit(map[string]any{"type": "item.completed", "item": map[string]any{"type": "error", "message": "Code Mode is unavailable because code-mode host is disabled. Code mode will fail closed; enable `features.code_mode_host` and install `codex-code-mode-host`."}})
		emit(map[string]any{"type": "turn.started"})
		emit(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": proposed}})
		emit(map[string]any{"type": "turn.completed"})
	default:
		fmt.Fprintln(os.Stderr, "unsupported fixture command")
		os.Exit(2)
	}
}
