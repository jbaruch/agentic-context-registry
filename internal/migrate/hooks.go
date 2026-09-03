package migrate

import (
	"bytes"
	"path"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/tesslplugin"
)

const (
	reasonBadGrammar     = "hook-command-grammar"
	reasonBadEvent       = "hook-event"
	reasonHookDivergence = "hook-command-divergence"
	reasonMissingHook    = "missing-hook-script"
)

var nativeHookEvents = map[string]manifest.HookEvent{
	"SessionStart":       manifest.HookSessionStart,
	"sessionStart":       manifest.HookSessionStart,
	"SessionEnd":         manifest.HookSessionEnd,
	"sessionEnd":         manifest.HookSessionEnd,
	"UserPromptSubmit":   manifest.HookUserPromptSubmit,
	"beforeSubmitPrompt": manifest.HookUserPromptSubmit,
	"PreToolUse":         manifest.HookPreToolUse,
	"preToolUse":         manifest.HookPreToolUse,
	"PostToolUse":        manifest.HookPostToolUse,
	"postToolUse":        manifest.HookPostToolUse,
	"Stop":               manifest.HookStop,
	"stop":               manifest.HookStop,
}

// NormalizedHook is one Tessl hook on the #4 artifact model.
type NormalizedHook struct {
	ID          string
	Event       manifest.HookEvent
	Digest      string
	RelPath     string
	Argv        []string
	Natives     []string
	Ambiguous   bool
	Unsupported bool
	Reason      string
}

// NormalizeHooks maps plugin.json hook commands onto canonical events and
// collapses per-agent spelling-only duplicates.
func NormalizeHooks(snapshot adapter.Snapshot, install PackageInstall) ([]NormalizedHook, error) {
	type key struct {
		id    string
		event manifest.HookEvent
	}
	grouped := make(map[key][]NormalizedHook)
	for _, declared := range install.Hooks {
		hook, err := normalizeDeclaredHook(snapshot, install, declared)
		if err != nil {
			return nil, err
		}
		grouped[key{id: hook.ID, event: hook.Event}] = append(grouped[key{id: hook.ID, event: hook.Event}], hook)
	}
	keys := make([]key, 0, len(grouped))
	for k := range grouped {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(left, right int) bool {
		if keys[left].event != keys[right].event {
			return keys[left].event < keys[right].event
		}
		return keys[left].id < keys[right].id
	})
	result := make([]NormalizedHook, 0, len(keys))
	for _, k := range keys {
		result = append(result, collapseHooks(grouped[k]))
	}
	return result, nil
}

func normalizeDeclaredHook(snapshot adapter.Snapshot, install PackageInstall, declared DeclaredHook) (NormalizedHook, error) {
	hook := NormalizedHook{ID: declared.ID}
	event, ok := nativeHookEvents[declared.NativeEvent]
	if !ok {
		hook.Unsupported = true
		hook.Reason = reasonBadEvent
		hook.ID = declared.ID
		return hook, nil
	}
	hook.Event = event
	parsed, err := tesslplugin.ParseHookCommand(declared.Command, declared.Args)
	if err != nil {
		hook.Unsupported = true
		hook.Reason = reasonBadGrammar
		return hook, nil
	}
	hook.RelPath = parsed.Path
	hook.Argv = parsed.Argv
	if hook.ID == "" || hook.ID == "hook" {
		hook.ID = sanitizeID(strings.TrimSuffix(path.Base(parsed.Path), path.Ext(parsed.Path)))
	}
	script, present, err := readOptional(snapshot, posixJoin(install.Root, parsed.Path))
	if err != nil {
		return NormalizedHook{}, err
	}
	if !present {
		hook.Ambiguous = true
		hook.Reason = reasonMissingHook
		return hook, nil
	}
	hook.Digest = hookDigest(script, parsed.Args)
	return hook, nil
}

func validPluginRelPath(relpath string) bool {
	if relpath == "" || relpath == "." || strings.ContainsRune(relpath, '\x00') || strings.Contains(relpath, "\\") || strings.HasPrefix(relpath, "/") {
		return false
	}
	for _, segment := range strings.Split(relpath, "/") {
		if segment == "" || segment == ".." {
			return false
		}
	}
	return path.Clean(relpath) == relpath
}

func collapseHooks(hooks []NormalizedHook) NormalizedHook {
	if len(hooks) == 0 {
		return NormalizedHook{}
	}
	canonical := hooks[0]
	for _, hook := range hooks[1:] {
		if hook.Unsupported {
			canonical.Unsupported = true
			if canonical.Reason == "" {
				canonical.Reason = hook.Reason
			}
			continue
		}
		if hook.Ambiguous {
			canonical.Ambiguous = true
			if canonical.Reason == "" {
				canonical.Reason = hook.Reason
			}
		}
		if !canonical.Unsupported && hook.Digest != canonical.Digest {
			canonical.Ambiguous = true
			canonical.Reason = reasonHookDivergence
		}
	}
	return canonical
}

func hookDigest(script []byte, argv []string) string {
	var buffer bytes.Buffer
	buffer.Write(script)
	buffer.WriteByte(0)
	buffer.WriteString(strings.Join(argv, "\x00"))
	return contentDigest(buffer.Bytes())
}
