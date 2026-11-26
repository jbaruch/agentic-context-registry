package antigravity

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// These types model what makes agy 1.5.14 discard all of hooks.json.
// Fields and events that it only warns about or ignores remain valid.
// https://antigravity.google/docs/hooks
type nativeHandler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Prompt  string `json:"prompt"`
	Model   string `json:"model"`
	Timeout int32  `json:"timeout"`
}

type nativeToolGroup struct {
	Matcher string          `json:"matcher"`
	Hooks   []nativeHandler `json:"hooks"`
}

// nativeMembers maps each member agy reads to the members it reads inside it.
type nativeMembers map[string]nativeMembers

var (
	handlerMembers = nativeMembers{"type": nil, "command": nil, "prompt": nil, "model": nil, "timeout": nil}
	groupMembers   = nativeMembers{"matcher": nil, "hooks": handlerMembers}
	hookMembers    = nativeMembers{
		"enabled": nil, "PreToolUse": groupMembers, "PostToolUse": groupMembers,
		"SessionStart": handlerMembers, "PreInvocation": handlerMembers, "PostInvocation": handlerMembers, "Stop": handlerMembers,
	}
)

// repeatedMember finds two spellings of one member in the same object. agy
// ignores letter case, and what a repeat means depends on its decoder: it can
// discard a file that decodes cleanly here.
func repeatedMember(value any, read nativeMembers) (first, second string) {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			if first, second = repeatedMember(item, read); second != "" {
				return first, second
			}
		}
	case map[string]any:
		seen := map[string]string{}
		for _, name := range slices.Sorted(maps.Keys(value)) {
			for member, inner := range read {
				if !strings.EqualFold(name, member) {
					continue
				}
				if earlier, ok := seen[member]; ok {
					return earlier, name
				}
				seen[member] = name
				if first, second = repeatedMember(value[name], inner); second != "" {
					return first, second
				}
			}
		}
	}
	return "", ""
}

func validateNativeHook(content json.RawMessage) error {
	var members map[string]json.RawMessage
	if err := decodeNative(content, &members, "hook"); err != nil {
		return err
	}
	// agy ignores letter case in member names. Validate refuses repeated
	// spellings first, so each member read here has one value.
	for _, name := range slices.Sorted(maps.Keys(members)) {
		is := func(event string) bool { return strings.EqualFold(name, event) }
		var handlers []nativeHandler
		var err error
		switch {
		case is("enabled"):
			var enabled bool
			err = decodeNative(members[name], &enabled, name)
		case is("PreToolUse"), is("PostToolUse"):
			var groups []nativeToolGroup
			err = decodeNative(members[name], &groups, name)
			for _, group := range groups {
				handlers = append(handlers, group.Hooks...)
			}
		case is("SessionStart"), is("PreInvocation"), is("PostInvocation"), is("Stop"):
			err = decodeNative(members[name], &handlers, name)
		}
		if err != nil {
			return err
		}
		for _, handler := range handlers {
			switch handler.Type {
			case "", "command":
				if handler.Command == "" {
					return fmt.Errorf("%s handler needs a nonempty command", name)
				}
				if handler.Prompt != "" || handler.Model != "" {
					return fmt.Errorf("%s handler of type command cannot set prompt or model", name)
				}
			case "prompt":
				if is("PostToolUse") {
					return fmt.Errorf("%s handler cannot have type prompt", name)
				}
				if handler.Prompt == "" || handler.Command != "" {
					return fmt.Errorf("%s handler of type prompt needs a nonempty prompt and no command", name)
				}
				// EqualFold would admit Unicode spellings that agy refuses.
				model := strings.ToLower(strings.TrimSpace(handler.Model))
				if handler.Model != "" && !slices.Contains([]string{"inherit", "flash", "pro", "flash_lite"}, model) {
					return fmt.Errorf("%s handler model %q must be inherit, flash, pro, or flash_lite", name, handler.Model)
				}
			default:
				return fmt.Errorf("%s handler type %q must be command or prompt", name, handler.Type)
			}
		}
	}
	return nil
}

func decodeNative(content []byte, target any, at string) error {
	err := json.Unmarshal(content, target)
	var mismatch *json.UnmarshalTypeError
	if !errors.As(err, &mismatch) {
		return err
	}
	if mismatch.Field != "" {
		at += "." + mismatch.Field
	}
	return fmt.Errorf("%s cannot be a JSON %s", at, mismatch.Value)
}
