package tesslplugin

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

const tesslPluginDirPrefix = "${TESSL_PLUGIN_DIR}/"

var tesslEvents = map[string]manifest.HookEvent{
	"SessionStart":     manifest.HookSessionStart,
	"SessionEnd":       manifest.HookSessionEnd,
	"UserPromptSubmit": manifest.HookUserPromptSubmit,
	"PreToolUse":       manifest.HookPreToolUse,
	"PostToolUse":      manifest.HookPostToolUse,
	"Stop":             manifest.HookStop,
}

var acrAdapterIDs = []string{"claude-code", "codex", "cursor"}

type mappedHook struct {
	Path      string
	Event     manifest.HookEvent
	Args      []string
	Consensus bool
	Agents    []string
}

// ParsedHookCommand is the one normalized representation of Tessl's closed
// hook-command grammar.
type ParsedHookCommand struct {
	Path string
	Args []string
	Argv []string
}

type hookKey struct {
	path  string
	event manifest.HookEvent
}

func mapPluginHooks(plugin *PluginManifest, acceptWidening bool) ([]mappedHook, []LossyItem, error) {
	if plugin == nil {
		return nil, nil, nil
	}
	var notes []LossyItem
	grouped := make(map[hookKey][]mappedHook)

	add := func(eventName, agent string, group HookGroup) error {
		if group.Matcher != "" {
			return conversionError(CodeUnmappedField, "matcher",
				"hook matcher %q has no agent-plugin.yaml field; move the restriction into the hook script or drop the matcher", group.Matcher)
		}
		event, ok := tesslEvents[eventName]
		if !ok {
			return unsupportedHookEventError(eventName)
		}
		for _, command := range group.Hooks {
			if command.Type != "command" {
				return unsupportedHookTypeError(command.Type)
			}
			parsed, err := ParseHookCommand(command.Command, command.Args)
			if err != nil {
				return conversionError(CodeUnmappedField, "command",
					"%v", err)
			}
			if err := validateEmittedPath(parsed.Path); err != nil {
				return err
			}
			hook := mappedHook{Path: parsed.Path, Event: event, Args: parsed.Args, Agents: nil}
			if agent == "" {
				hook.Consensus = true
			} else {
				hook.Agents = []string{agent}
			}
			key := hookKey{path: parsed.Path, event: event}
			grouped[key] = append(grouped[key], hook)
		}
		return nil
	}

	events := sortedKeys(plugin.Hooks)
	for _, eventName := range events {
		for _, group := range plugin.Hooks[eventName] {
			if err := add(eventName, "", group); err != nil {
				return nil, nil, err
			}
		}
	}
	agents := sortedKeys(plugin.NativeHooks)
	for _, agent := range agents {
		eventNames := sortedKeys(plugin.NativeHooks[agent])
		for _, eventName := range eventNames {
			for _, group := range plugin.NativeHooks[agent][eventName] {
				if err := add(eventName, agent, group); err != nil {
					return nil, nil, err
				}
			}
		}
	}

	keys := make([]hookKey, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		if keys[left].event != keys[right].event {
			return keys[left].event < keys[right].event
		}
		return keys[left].path < keys[right].path
	})

	result := make([]mappedHook, 0, len(keys))
	for _, key := range keys {
		collapsed, widening, err := collapseHookGroup(grouped[key])
		if err != nil {
			return nil, nil, err
		}
		if widening {
			if !acceptWidening {
				return nil, nil, conversionError(CodeAgentWidening, collapsed.Path,
					"nativeHooks for %s on %s run on %s; converting would also fire on %s. Move the entry into hooks or re-run with --accept-agent-widening",
					collapsed.Path, strings.Join(collapsed.Agents, ", "), strings.Join(collapsed.Agents, ", "), strings.Join(acrAdapterIDs, ", "))
			}
			notes = append(notes, LossyItem{
				Kind:   "hook",
				Field:  collapsed.Path,
				Value:  strings.Join(collapsed.Agents, ","),
				Reason: "accepted-agent-widening",
			})
		}
		result = append(result, collapsed)
	}
	return result, notes, nil
}

func unsupportedHookEventError(eventName string) *Error {
	return conversionError(CodeUnmappedField, eventName,
		"hook event %q is outside the v1 vocabulary; use a v1 Tessl event or drop the hook", eventName)
}

func unsupportedHookTypeError(hookType string) *Error {
	return conversionError(CodeUnmappedField, "type",
		"hook type %q is not supported; only type \"command\" maps onto agent-plugin.yaml", hookType)
}

func collapseHookGroup(hooks []mappedHook) (mappedHook, bool, error) {
	if len(hooks) == 0 {
		return mappedHook{}, false, fmt.Errorf("internal error: empty hook group")
	}
	canonical := hooks[0]
	agents := map[string]struct{}{}
	consensus := canonical.Consensus
	for _, hook := range hooks {
		if hook.Path != canonical.Path || hook.Event != canonical.Event || !stringSlicesEqual(hook.Args, canonical.Args) {
			return mappedHook{}, false, conversionError(CodeUnmappedField, hook.Path,
				"hook command bodies for %s on %s diverge; keep one command form or move the consensus into hooks", hook.Path, hook.Event)
		}
		if hook.Consensus {
			consensus = true
		}
		for _, agent := range hook.Agents {
			agents[agent] = struct{}{}
		}
	}
	canonical.Consensus = consensus
	canonical.Agents = sortedSet(agents)
	widening := !consensus && widensAgents(canonical.Agents, acrAdapterIDs)
	return canonical, widening, nil
}

// ParseHookCommand accepts the two forms Tessl emits and refuses every other
// spelling with the shared operator remedy.
func ParseHookCommand(command string, args []string) (ParsedHookCommand, error) {
	if len(args) != 0 {
		if command != "bash" {
			return ParsedHookCommand{}, invalidHookCommand(command)
		}
		if !strings.HasPrefix(args[0], tesslPluginDirPrefix) {
			return ParsedHookCommand{}, invalidHookCommand(command)
		}
		relpath := strings.TrimPrefix(args[0], tesslPluginDirPrefix)
		if !ValidPluginRelPath(relpath) {
			return ParsedHookCommand{}, invalidHookCommand(command)
		}
		extra := append([]string(nil), args[1:]...)
		return ParsedHookCommand{Path: relpath, Args: extra, Argv: append([]string{command}, args...)}, nil
	}
	const prefix = `bash "${TESSL_PLUGIN_DIR}/`
	if strings.HasPrefix(command, prefix) && strings.HasSuffix(command, `"`) {
		relpath := strings.TrimSuffix(strings.TrimPrefix(command, prefix), `"`)
		if !ValidPluginRelPath(relpath) {
			return ParsedHookCommand{}, invalidHookCommand(command)
		}
		return ParsedHookCommand{Path: relpath, Argv: []string{"bash", tesslPluginDirPrefix + relpath}}, nil
	}
	return ParsedHookCommand{}, invalidHookCommand(command)
}

func invalidHookCommand(command string) error {
	return fmt.Errorf("hook command %q is outside the closed Tessl grammar; use bash with ${TESSL_PLUGIN_DIR}/ or drop the hook", command)
}

// ValidPluginRelPath reports whether a path is a non-empty relative POSIX path.
func ValidPluginRelPath(relpath string) bool {
	if relpath == "" || relpath == "." || strings.ContainsRune(relpath, '\x00') || strings.HasPrefix(relpath, "/") || strings.Contains(relpath, "\\") {
		return false
	}
	for _, segment := range strings.Split(relpath, "/") {
		if segment == "" || segment == ".." {
			return false
		}
	}
	return path.Clean(relpath) == relpath
}

func validateEmittedPath(relative string) error {
	if relative == "" || strings.HasPrefix(relative, "/") || strings.Contains(relative, "\\") {
		return conversionError(string(manifest.CodeInvalidPath), relative,
			"path %q is absolute or uses a backslash; use a package-relative POSIX path", relative)
	}
	for _, segment := range strings.Split(relative, "/") {
		if segment == ".." {
			return conversionError(string(manifest.CodeInvalidPath), relative,
				"path %q contains a parent directory segment; use a package-relative POSIX path", relative)
		}
	}
	return nil
}

func hookBasename(hookPath string) string {
	base := path.Base(hookPath)
	return strings.TrimSuffix(base, path.Ext(base))
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func widensAgents(got, universe []string) bool {
	have := make(map[string]struct{}, len(got))
	for _, item := range got {
		have[item] = struct{}{}
	}
	for _, adapter := range universe {
		if _, ok := have[adapter]; !ok {
			return true
		}
	}
	return false
}
