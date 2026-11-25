#!/usr/bin/env python3
"""Translate canonical command hooks to Codex and Cursor's native contracts."""

import sys
import posix

if not sys.flags.isolated:
    posix.execv(sys.executable, [sys.executable, "-I", sys.argv[0], *sys.argv[1:]])

import json
import math
import os
import signal
import subprocess
import time
from typing import Any, Union, cast


EVENTS = {
    "session-start": "SessionStart",
    "session-end": "SessionEnd",
    "user-prompt-submit": "UserPromptSubmit",
    "pre-tool-use": "PreToolUse",
    "post-tool-use": "PostToolUse",
    "stop": "Stop",
}


def hook_message(path: str, event: str, kind: str, detail: str, recovery: str) -> str:
    # Bound rendered detail before adding ACR identity, outcome and recovery.
    parts: list[str] = []
    remaining = 16 * 1024
    for char in detail:
        code = ord(char)
        part = (
            f"\\u{code:04X}"
            if (code < 0x20 and char not in "\n\t")
            or 0x7F <= code <= 0x9F
            or 0x202A <= code <= 0x202E
            else char
        )
        size = len(part.encode("utf-8"))
        if size > remaining:
            parts.append("\n[hook detail truncated]")
            break
        parts.append(part)
        remaining -= size
    detail = "".join(parts)
    prefix = f"ACR hook {path} ({event}): "
    if kind == "watchdog":
        return f"{prefix}timed out; stdout and stderr discarded; {recovery}"
    if kind == "runtime":
        return f"{prefix}runtime failure: {detail.strip()}; {recovery}"
    if kind == "missing-interpreter":
        return f"{prefix}could not run because Python >= 3.9 is unavailable; {recovery}"
    action = "stopped the response" if kind == "termination" else {
        "pre-tool-use": "denied the tool call",
        "user-prompt-submit": "blocked the prompt",
        "stop": "requested continuation",
        "post-tool-use": "provided feedback after the tool call",
        "session-start": "provided session feedback",
        "session-end": "provided session feedback",
    }[event]
# acr:antigravity
    if kind == "permission":
        action = "reported policy feedback"
# acr:end
    if detail.strip():
        return f"{prefix}{action}: {detail.strip()}\n{recovery}"
    return f"{prefix}{action} without providing a reason. {recovery}"


cancelled = 0
defer_cancellation = False


def cancel(signum: int, frame: object) -> None:
    global cancelled
    if not cancelled:
        cancelled = signum
    if not defer_cancellation:
        raise SystemExit(128 + cancelled)


def object_json(value: str) -> dict[str, Any]:
    parsed: Any = json.loads(value)
    if not isinstance(parsed, dict):
        raise ValueError("expected a JSON object")
    return cast(dict[str, Any], parsed)


def note(path: str, message: str) -> None:
    sys.stderr.buffer.write(f"ACR hook {path}: {message}\n".encode("utf-8"))
    sys.stderr.buffer.flush()


def cursor_input(raw: bytes, event: str) -> bytes:
    value = object_json(raw.decode("utf-8"))
    value["hook_event_name"] = EVENTS[event]
    value["session_id"] = value.get("session_id") or value.get("conversation_id")
    roots: list[str] = value.get("workspace_roots") or []
    value["cwd"] = value.get("cwd") or (roots[0] if roots else None)
    if event == "session-start":
        value["source"] = "startup"
    if event == "stop":
        value["stop_hook_active"] = value.get("loop_count", 0) > 0
    if event == "post-tool-use" and "tool_output" in value:
        response = value["tool_output"]
        if isinstance(response, str):
            try:
                response = json.loads(response)
            except json.JSONDecodeError:
                pass  # A non-JSON tool result remains the agent's original string.
        value["tool_response"] = response
    return json.dumps(value).encode("utf-8")


# acr:antigravity
class AntigravityHookProcess(subprocess.Popen[bytes]):
    def wait(self, timeout: Union[float, None] = None) -> int:
        global defer_cancellation
        if defer_cancellation or timeout is None:
            return super().wait(timeout)
        # Latch cancellation during reaping so the caller can distinguish a
        # still-owned group ID from a PID that may have been recycled.
        deadline = time.monotonic() + timeout
        while True:
            defer_cancellation = True
            try:
                status = super().wait(min(0.1, max(0, deadline - time.monotonic())))
            except subprocess.TimeoutExpired:
                if not cancelled:
                    if time.monotonic() >= deadline:
                        raise
                    continue
            else:
                if not cancelled:
                    return status
            finally:
                defer_cancellation = False
            raise SystemExit(128 + cancelled)


def antigravity_object(text: str) -> dict[str, Any]:
    def unique_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
        fields: dict[str, Any] = {}
        for key, value in pairs:
            if key in fields:
                raise ValueError("duplicate JSON member")
            fields[key] = value
        return fields

    def finite_number(text: str) -> float:
        value = float(text)
        if not math.isfinite(value):
            raise ValueError("non-finite JSON number")
        return value

    parsed: Any = json.loads(text, object_pairs_hook=unique_fields, parse_float=finite_number, parse_constant=finite_number)
    if not isinstance(parsed, dict):
        raise ValueError("expected a JSON object")
    return cast(dict[str, Any], parsed)


def antigravity_input(raw: bytes, event: str, root: str) -> dict[str, Any]:
    value = antigravity_object(raw.decode("utf-8"))
    value["hook_event_name"] = EVENTS[event]
    for native, canonical in (("conversationId", "session_id"), ("transcriptPath", "transcript_path")):
        if native in value:
            value[canonical] = None if value[native] is None else string_field(value, native)
    cwd: Any = value.get("cwd", root)
    if not isinstance(cwd, str) or not os.path.isabs(cwd) or "\x00" in cwd:
        raise ValueError("cwd must be an absolute path")
    value["cwd"] = cwd
    if event == "session-start":
        value["source"] = "startup"
    if event in ("pre-tool-use", "post-tool-use"):
        call: Any = value.get("toolCall")
        if not isinstance(call, dict):
            raise ValueError("toolCall must be an object")
        tool = cast(dict[str, Any], call)
        name = string_field(tool, "name")
        args: Any = tool.get("args")
        if not name or not isinstance(args, dict):
            raise ValueError("toolCall requires a name and an args object")
        value["tool_name"], value["tool_input"] = name, args
        tool_id = string_field(tool, "id")
        if not tool_id:
            conversation = string_field(value, "conversationId")
            step = value.get("stepIdx")
            if not conversation or type(step) is not int or step < 0:
                raise ValueError("toolCall.id or conversationId and stepIdx required for tool correlation")
            tool_id = f"{conversation}:{step}"
        value["tool_use_id"] = tool_id
    if event == "post-tool-use":
        for key in ("toolResult", "toolResponse", "result"):
            if key in value:
                value["tool_response"] = value[key]
                break
    if event == "stop":
        active = value.get("continuationActive", False)
        count = value.get("executionNum", 0)
        if not isinstance(active, bool) or type(count) is not int or count < 0:
            raise ValueError("continuationActive must be a boolean and executionNum a nonnegative integer")
        value["stop_hook_active"] = active or count > 0
    return value


def validate_antigravity(event: str, value: dict[str, Any]) -> None:
    if not value and event in ("session-start", "post-tool-use", "stop"):
        return
    if event == "session-start" and set(value) == {"injectSteps"}:
        steps = value["injectSteps"]
        if isinstance(steps, list) and len(cast(list[Any], steps)) == 1:
            step: Any = cast(list[Any], steps)[0]
            if isinstance(step, dict):
                message: Any = cast(dict[str, Any], step).get("systemMessage")
                if isinstance(message, dict):
                    text = cast(dict[str, Any], message).get("systemMessage")
                    if isinstance(text, str) and step == {"systemMessage": {"systemMessage": text}}:
                        return
    if event in ("pre-tool-use", "stop"):
        decision = value.get("decision")
        decisions = ("continue",) if event == "stop" else ("allow", "deny", "ask", "force_ask")
        allowed = {"decision", "reason"}
        if event == "pre-tool-use" and decision in ("allow", "ask", "force_ask") and isinstance(value.get("overwrite"), dict):
            allowed.add("overwrite")
        if decision in decisions and value.keys() <= allowed and isinstance(value.get("reason", ""), str):
            return
    raise ValueError("invalid native Antigravity reply")


def antigravity_reply(event: str, path: str, native: dict[str, Any], raw: bytes, stderr: bytes, code: int) -> int:
    agent = "antigravity"
    text = raw.decode("utf-8").strip() if code == 0 else ""
    obj = text.startswith("{")
    value = antigravity_object(text) if obj else {}
    supplied: Any = value.get("hookSpecificOutput", {})
    if not isinstance(supplied, dict):
        raise ValueError("hookSpecificOutput must be an object")
    specific = cast(dict[str, Any], supplied)
    context = string_field(specific, "additionalContext")
    reason = stderr.decode("utf-8", errors="replace").strip() if code == 2 else string_field(value, "reason")
    for key in ("continue", "suppressOutput"):
        if key in value and not isinstance(value[key], bool):
            raise ValueError(f"{key} must be a boolean")
    for key in ("stopReason", "systemMessage"):
        string_field(value, key)
    stopping = value.get("continue") is False
    blocked = code == 2 or value.get("decision") == "block"
    valid_decisions = ("block", "approve") if event == "pre-tool-use" else ("block",)
    if "decision" in value and value["decision"] not in valid_decisions:
        raise ValueError("invalid decision")

    allowed_top = {"hookSpecificOutput", "continue"}
    allowed_specific = {"hookEventName"}
    if event in ("pre-tool-use", "stop"):
        allowed_top.update(("decision", "reason"))
    if event in ("session-start", "stop"):
        allowed_specific.add("additionalContext")
    if event == "pre-tool-use":
        allowed_specific.update(("permissionDecision", "permissionDecisionReason", "updatedInput"))
        if stopping:
            allowed_top.add("stopReason")
    if (specific.get("hookEventName", EVENTS[event]) != EVENTS[event]
            or ((blocked or stopping) and event in ("session-start", "post-tool-use"))
            or (event == "post-tool-use" and ("additionalContext" in specific or "updatedMCPToolOutput" in specific))):
        if event == "post-tool-use":
            return failure(agent, event, path, "the tool result was kept. Return {} after side effects or use an agent that supports post-tool feedback")
        return failure(agent, event, path, "canonical reply has behavior this native event cannot express. Rewrite this hook for Antigravity or select a compatible agent")
    # Any other field has no native effect, so dropping it cannot change the outcome.
    drop_fields(value, tuple(value.keys() - allowed_top), path)
    drop_fields(specific, tuple(specific.keys() - allowed_specific), path)
    if text and not obj and event != "session-start":
        note(path, f"{EVENTS[event]} requires JSON; dropped plain text")
    reply: dict[str, Any] = {}
    if event == "session-start":
        context = context if obj else text
        if context:
            reply["injectSteps"] = [{"systemMessage": {"systemMessage": context}}]
    elif event == "pre-tool-use":
        permission = specific.get("permissionDecision")
        if "permissionDecision" in specific and permission not in ("allow", "deny", "ask", "defer"):
            raise ValueError("invalid permissionDecision")
        reason = reason or string_field(specific, "permissionDecisionReason") or string_field(value, "stopReason")
        if blocked or stopping or permission == "deny":
            reply = {"decision": "deny", "reason": hook_message(path, event, "intentional", reason, "Review this hook's policy before retrying.")}
        else:
            if permission == "allow":
                reply["decision"] = "allow"
            elif permission == "ask":
                reply["decision"] = "force_ask"
            else:
                reply["decision"] = "ask"
            if reason:
                reply["reason"] = hook_message(path, event, "permission", reason, "Review this hook's policy.")
            if permission != "defer" and "updatedInput" in specific:
                updated = specific["updatedInput"]
                original = native.get("toolCall", {}).get("args")
                if (not isinstance(original, dict) or not isinstance(updated, dict)
                        or not cast(dict[str, Any], original).keys() <= cast(dict[str, Any], updated).keys()):
                    return failure(agent, event, path, "the tool call was denied. Include every original top-level argument key in updatedInput")
                reply["overwrite"] = updated
    elif event == "stop" and not stopping and (blocked or context):
        detail = "\n\n".join(part for part in (reason, context) if part)
        reply = {"decision": "continue", "reason": hook_message(path, event, "intentional", detail, "Review this hook's policy before continuing.")}
    if stderr:
        if code == 2 or reply.get("decision") in ("deny", "force_ask", "continue"):
            prepared = string_field(reply, "reason") if code == 2 else hook_message(
                path, event, "permission", stderr.decode("utf-8", errors="replace"), "Review this hook's policy."
            )
            sys.stderr.buffer.write((prepared + "\n").encode("utf-8"))
        else:
            sys.stderr.buffer.write(stderr)
    return emit(agent, event, path, reply)


# acr:end
def emit(agent: str, event: str, path: str, value: Union[dict[str, Any], bytes], code: int = 0, stderr: bytes = b"") -> int:
    """Final native output check: every reply, including failure replies, passes here."""
    if agent == "codex" and code == 2:
        # Codex ignores stdout on exit 2, but blank stderr makes the gate fail open.
        if not stderr.decode("utf-8", errors="replace").strip():
            raise ValueError("exit-2 denial requires a prepared message")
        sys.stderr.buffer.write(stderr)
        sys.stderr.buffer.flush()
        if isinstance(value, bytes):
            sys.stdout.buffer.write(value)
        return 2
    if isinstance(value, bytes):
        if agent != "codex" or event not in ("session-start", "session-end", "user-prompt-submit"):
            raise ValueError("unexpected native text reply")
        sys.stdout.buffer.write(value)
        return 0

    # Codex: codex-rs/hooks/schema/generated/<event>.command.output.schema.json.
    # Cursor: https://cursor.com/docs/hooks, the output table for each event.
    if agent == "codex":
        allowed = ("continue", "stopReason", "suppressOutput", "systemMessage")
        if event in ("pre-tool-use", "post-tool-use", "user-prompt-submit", "stop"):
            allowed += ("decision", "reason")
        if event not in ("stop", "session-end"):
            allowed += ("hookSpecificOutput",)
        if event == "pre-tool-use":
            allowed = tuple(key for key in allowed if key not in ("continue", "stopReason", "suppressOutput"))
            # Codex rejects legacy approve, and a reason without a legacy decision.
            if value.get("decision") != "block":
                value.pop("decision", None)
        if value.get("decision") != "block" and (event == "pre-tool-use" or value.get("continue") is not False):
            value.pop("reason", None)
        if event == "post-tool-use":
            allowed = tuple(key for key in allowed if key != "suppressOutput")
        drop_fields(value, tuple(key for key in value if key not in allowed), path)
        for key in ("continue", "suppressOutput"):
            if key in value and not isinstance(value[key], bool):
                raise ValueError(f"{key} must be a boolean")
        for key in ("stopReason", "systemMessage", "reason"):
            if key in value and not isinstance(value[key], str):
                raise ValueError(f"{key} must be a string")
        if "decision" in value:
            decisions = ("approve", "block") if event == "pre-tool-use" else ("block",)
            if value["decision"] not in decisions:
                raise ValueError("invalid native decision")
            if value["decision"] == "block" and not string_field(value, "reason").strip():
                raise ValueError("block requires a prepared reason")
        if "hookSpecificOutput" in value:
            specific = value["hookSpecificOutput"]
            if not isinstance(specific, dict):
                raise ValueError("invalid native hookSpecificOutput")
            specific = cast(dict[str, Any], specific)
            fields = ("hookEventName", "additionalContext")
            if event == "pre-tool-use":
                fields += ("permissionDecision", "permissionDecisionReason", "updatedInput")
            drop_fields(specific, tuple(key for key in specific if key not in fields), path)
            specific["hookEventName"] = EVENTS[event]
            for key in ("additionalContext", "permissionDecisionReason"):
                if key in specific and not isinstance(specific[key], str):
                    raise ValueError(f"{key} must be a string")
            if "permissionDecision" in specific:
                decision = specific["permissionDecision"]
                if decision not in ("allow", "deny"):
                    raise ValueError("invalid native permissionDecision")
                if decision == "deny":
                    if not string_field(specific, "permissionDecisionReason").strip():
                        raise ValueError("deny requires a prepared reason")
                    specific.pop("updatedInput", None)
                elif specific.get("updatedInput") is None:
                    del specific["permissionDecision"]  # Codex rejects a bare allow.
            if "permissionDecision" not in specific:
                specific.pop("permissionDecisionReason", None)
                specific.pop("updatedInput", None)
# acr:antigravity
    elif agent == "antigravity":
        validate_antigravity(event, value)
# acr:end
    else:
        fields_by_event = {
            "session-start": ("additional_context",),
            "session-end": (),
            "pre-tool-use": ("permission", "user_message", "agent_message", "updated_input"),
            "post-tool-use": ("additional_context", "updated_mcp_tool_output"),
            "user-prompt-submit": ("continue", "user_message"),
            "stop": ("followup_message",),
        }
        drop_fields(value, tuple(key for key in value if key not in fields_by_event[event]), path)
        for key in ("additional_context", "user_message", "agent_message", "followup_message"):
            if key in value and not isinstance(value[key], str):
                raise ValueError(f"{key} must be a string")
        if "continue" in value and not isinstance(value["continue"], bool):
            raise ValueError("continue must be a boolean")
        if "permission" in value and value["permission"] not in ("allow", "deny"):
            raise ValueError("invalid native permission")
        if value.get("permission") == "deny":
            value.pop("updated_input", None)
        for key in ("updated_input", "updated_mcp_tool_output"):
            if key in value and not isinstance(value[key], dict):
                raise ValueError(f"{key} must be an object")
    if agent == "codex":
        # serde_json Number stores i64/u64 or finite f64 (no arbitrary_precision):
        # https://docs.rs/serde_json/1.0.154/serde_json/struct.Number.html
        # Its 128-entry recursion budget rejects the 128th nested container:
        # https://docs.rs/serde_json/1.0.154/src/serde_json/de.rs.html#63
        # https://docs.rs/serde_json/1.0.154/src/serde_json/de.rs.html#1372-1386
        pending: list[tuple[Any, int]] = [(value, 0)]
        while pending:
            item, depth = pending.pop()
            if isinstance(item, (dict, list)):
                depth += 1
                if depth >= 128:
                    raise ValueError("native JSON nesting limit exceeded")
                children = cast(dict[str, Any], item).values() if isinstance(item, dict) else cast(list[Any], item)
                pending.extend((child, depth) for child in children)
            elif isinstance(item, int) and not -(1 << 63) <= item < (1 << 64):
                raise ValueError("native JSON integer out of range")
            elif isinstance(item, float) and not math.isfinite(item):
                raise ValueError("native JSON float out of range")
    sys.stdout.buffer.write((json.dumps(value, ensure_ascii=False, allow_nan=False) + "\n").encode("utf-8"))
    return 0


def drop_fields(value: dict[str, Any], keys: tuple[str, ...], path: str) -> bool:
    removed = [key for key in keys if key in value]
    for key in removed:
        del value[key]
    if removed:
        note(path, "dropped unsupported fields")
    return bool(removed)


def failure(agent: str, event: str, path: str, message: str, kind: str = "runtime") -> int:
# acr:antigravity
    if agent == "antigravity":
        recovery = "For local packages, fix the source and run acr install before acr realize. For release packages, install a corrected release."
        message = hook_message(path, event, kind, message, recovery)
        sys.stderr.buffer.write((message + "\n").encode("utf-8"))
        sys.stderr.buffer.flush()
        return emit(agent, event, path, {"decision": "deny", "reason": message} if event == "pre-tool-use" else {})
# acr:end
    message = hook_message(path, event, kind, message, "Repair this hook and run acr realize before retrying.")
    diagnostic = (message + "\n").encode("utf-8")
    if agent == "codex" and event in ("pre-tool-use", "user-prompt-submit"):
        return emit(agent, event, path, b"", 2, diagnostic)
    sys.stderr.buffer.write(diagnostic)
    sys.stderr.buffer.flush()
    if event in ("pre-tool-use", "user-prompt-submit"):
        if event == "pre-tool-use":
            return emit(agent, event, path, {"permission": "deny", "user_message": message, "agent_message": message})
        return emit(agent, event, path, {"continue": False, "user_message": message})
    if event == "stop":
        if agent == "codex":
            return emit(agent, event, path, {"systemMessage": message})
        return emit(agent, event, path, {})  # Never request continuation on failure.
    if agent == "codex":
        return emit(agent, event, path, message.encode("utf-8") if event == "session-end" else {"systemMessage": message})
    return emit(agent, event, path, {"additional_context": message} if event != "session-end" else {})


def string_field(value: dict[str, Any], key: str) -> str:
    field: Any = value.get(key)
    if field is None:
        return ""
    if not isinstance(field, str):
        raise ValueError(f"{key} must be a string")
    return field


# acr:antigravity
# acr:legacy def translate(agent: str, event: str, path: str, raw: bytes, stderr: bytes, code: int) -> int:
def translate(agent: str, event: str, path: str, raw: bytes, stderr: bytes, code: int, native: dict[str, Any]) -> int:
    if agent == "antigravity":
        return antigravity_reply(event, path, native, raw, stderr, code)
# acr:end
    if agent == "codex" and code == 2 and event in ("pre-tool-use", "user-prompt-submit", "stop"):
        recovery = "Review this hook's policy before continuing." if event == "stop" else "Review this hook's policy before retrying."
        message = hook_message(path, event, "intentional", stderr.decode("utf-8", errors="replace"), recovery)
        diagnostic = message + "\n"
        return emit(agent, event, path, raw, 2, diagnostic.encode("utf-8"))
    text = raw.decode("utf-8", errors="replace") if code == 0 else ""
    is_object = text.lstrip().startswith("{")
    value = object_json(text) if is_object else {}
    raw_specific: Any = value.get("hookSpecificOutput")
    if raw_specific is not None and not isinstance(raw_specific, dict):
        raise ValueError("hookSpecificOutput must be an object")
    specific = cast(dict[str, Any], raw_specific) if raw_specific is not None else {}
    context = string_field(specific, "additionalContext")
    if not is_object and event in ("session-start", "user-prompt-submit"):
        context = text.strip()
    reason = stderr.decode("utf-8", errors="replace").strip() if code == 2 else string_field(value, "reason")
    blocked = code == 2 or value.get("decision") == "block" or (event in ("pre-tool-use", "user-prompt-submit") and value.get("continue") is False)
    decision = string_field(specific, "permissionDecision")
    permission_reason = string_field(specific, "permissionDecisionReason")
    if decision not in ("", "allow", "deny", "ask", "defer"):
        raise ValueError("invalid permissionDecision")
    allowed_decisions = (None, "block", "approve") if event == "pre-tool-use" else (None, "block")
    if value.get("decision") not in allowed_decisions:
        raise ValueError("invalid decision")
    for key in ("continue", "suppressOutput"):
        if key in value and not isinstance(value[key], bool):
            raise ValueError(f"{key} must be a boolean")
    for key in ("stopReason", "systemMessage"):
        string_field(value, key)

    intentional = blocked or (event == "pre-tool-use" and decision in ("deny", "ask")) or (event == "stop" and bool(context)) or value.get("continue") is False
    message = ""
    if intentional:
        detail = reason.strip() or (permission_reason if event == "pre-tool-use" else "")
        if event == "stop" and value.get("continue") is False:
            detail = string_field(value, "stopReason") or detail
        elif event != "session-end":
            detail = "\n\n".join(part for part in (detail, context) if part)
        recovery = "Review this hook's policy before retrying." if event in ("pre-tool-use", "user-prompt-submit") else "Review this hook's policy before continuing."
        if event == "pre-tool-use" and decision == "ask" and not blocked:
            recovery = f"{agent.capitalize()} cannot ask for confirmation here. Ask the user in the conversation and retry if they approve."
        message = hook_message(path, event, "termination" if event == "stop" and value.get("continue") is False else "intentional", detail, recovery)
        if "systemMessage" in value:
            value["systemMessage"] = message
        if value.get("decision") == "block" or (value.get("continue") is False and "reason" in value):
            value["reason"] = message
        if "stopReason" in value:
            value["stopReason"] = message
        for key in ("additionalContext", "permissionDecisionReason"):
            if key in specific:
                specific[key] = message

    if stderr or code == 2:
        sys.stderr.buffer.write((message + "\n").encode("utf-8") if intentional else stderr)
        sys.stderr.buffer.flush()

    if agent == "codex":
        if event == "session-end":
            return emit(agent, event, path, message.encode("utf-8") if intentional else raw if not is_object else b"")
        if event == "session-start" and intentional:
            value["systemMessage"] = message
        if not is_object and code == 0:
            if event in ("session-start", "user-prompt-submit"):
                return emit(agent, event, path, raw)
            elif text.strip():
                note(path, f"Codex {EVENTS[event]} requires JSON; dropped plain text")
            return emit(agent, event, path, {})
        if event == "pre-tool-use":
            if blocked or decision == "ask":
                specific["permissionDecision"] = "deny"
            elif decision == "defer":
                del specific["permissionDecision"]
            if specific.get("permissionDecision") == "deny":
                specific["permissionDecisionReason"] = message
                value.pop("decision", None)
                value.pop("reason", None)
            if specific.get("permissionDecision") != "allow":
                drop_fields(specific, ("updatedInput",), path)
        elif event == "post-tool-use":
            if intentional:
                specific["additionalContext"] = message
                value.pop("decision", None)
                if value.get("continue") is not False:
                    value.pop("reason", None)
        elif event == "user-prompt-submit" and blocked:
            value["decision"] = "block"
            value["reason"] = message
        elif event == "stop" and value.get("continue") is not False and (blocked or context):
            value["decision"] = "block"
            value["reason"] = message
        if event != "stop" and (specific or "hookSpecificOutput" in value):
            value["hookSpecificOutput"] = specific
        return emit(agent, event, path, value)

    # Cursor consumes only its native fields. Report canonical fields it cannot use.
    stopping = value.get("continue") is False
    drop_fields(value, tuple(key for key in value if key not in (
        "hookSpecificOutput", "decision", "reason"
    )), path)
    drop_fields(specific, tuple(key for key in specific if key not in (
        "hookEventName", "additionalContext", "permissionDecision", "permissionDecisionReason",
        "updatedInput", "updatedMCPToolOutput"
    )), path)
    if event == "session-start":
        return emit(agent, event, path, {"additional_context": message or context} if message or context else {})
    elif event == "user-prompt-submit":
        if context:
            note(path, f"Cursor beforeSubmitPrompt cannot add model context; dropped {len(context)} characters.")
        return emit(agent, event, path, {"continue": False, "user_message": message} if blocked else {"continue": True})
    elif event == "pre-tool-use":
        if blocked or decision in ("deny", "ask"):
            result: dict[str, Any] = {"permission": "deny", "user_message": message, "agent_message": message}
        else:
            result = {"permission": "allow"}
            if "updatedInput" in specific:
                result["updated_input"] = specific["updatedInput"]
        if context:
            note(path, f"Cursor preToolUse cannot add model context; dropped {len(context)} characters.")
        return emit(agent, event, path, result)
    elif event in ("post-tool-use", "stop"):
        if event == "stop" and stopping:
            return emit(agent, event, path, {})
        joined = message if intentional else context
        result = {}
        if joined:
            result["additional_context" if event == "post-tool-use" else "followup_message"] = joined
        if event == "post-tool-use" and "updatedMCPToolOutput" in specific:
            result["updated_mcp_tool_output"] = specific["updatedMCPToolOutput"]
        if result or event == "post-tool-use":
            return emit(agent, event, path, result)
    return emit(agent, event, path, {})


def main() -> int:
    global defer_cancellation
# acr:antigravity
# acr:legacy     if len(sys.argv) < 4 or sys.argv[1] not in ("codex", "cursor") or sys.argv[2] not in EVENTS:
# acr:legacy         print("ACR hook: expected <codex|cursor> <event> <project-relative-hook-path> [args...]", file=sys.stderr)
    if len(sys.argv) < 4 or sys.argv[1] not in ("codex", "cursor", "antigravity") or sys.argv[2] not in EVENTS:
        print("ACR hook: expected <codex|cursor|antigravity> <event> <project-relative-hook-path> [args...]", file=sys.stderr)
# acr:end
        return 1
    agent, event, path = sys.argv[1:4]
# acr:antigravity
    if agent == "antigravity":
        # A host can pass down SIG_IGN across exec. Keep child exit waitable
        # so an early exit cannot release the private group leader's PID.
        signal.signal(signal.SIGCHLD, signal.SIG_DFL)
# acr:end
    root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    # Validate before execution: neither lexical traversal nor resolved symlinks may escape.
    if os.path.isabs(path) or ".." in path.split("/"):
        return failure(agent, event, path, "hook path must be project-relative without .. segments")
    target = os.path.realpath(os.path.join(root, path))
    if os.path.commonpath((os.path.realpath(root), target)) != os.path.realpath(root):
        return failure(agent, event, path, "hook path resolves outside the realized project root")
    raw = sys.stdin.buffer.read()
    try:
        canonical = cursor_input(raw, event) if agent == "cursor" else raw
# acr:antigravity
        native: dict[str, Any] = {}
        if agent == "antigravity":
            native = antigravity_input(raw, event, os.path.realpath(root))
            canonical = json.dumps(native).encode("utf-8")
# acr:end
    except (ValueError, UnicodeError, TypeError) as error:
        return failure(agent, event, path, f"invalid native stdin ({type(error).__name__})")
    env = dict(os.environ, CLAUDE_PROJECT_DIR=root)
    try:
        # The realized command supplies its native deadline. Direct execution uses
        # the same native defaults; reserve time to emit a valid failure reply.
# acr:antigravity
# acr:legacy         timeout = int(os.environ.get("acr_hook_timeout", "3" if agent == "codex" and event == "session-end" else "600")) * 3 // 4
        default = "3" if agent == "codex" and event == "session-end" else "600"
        timeout = int(os.environ.get("acr_hook_timeout", "30" if agent == "antigravity" else default)) * 3 // 4
# acr:end
        hook: Union[subprocess.Popen[bytes], None] = None
        captured = False
        try:
            # A signal during construction must not hide the new child's owner.
            defer_cancellation = True
            try:
# acr:antigravity
# acr:legacy                 hook = subprocess.Popen(
                process: type[subprocess.Popen[bytes]] = AntigravityHookProcess if agent == "antigravity" else subprocess.Popen
                hook = process(
# acr:end
                    [os.path.join(root, path), *sys.argv[4:]], stdin=subprocess.PIPE,
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env,
# acr:antigravity
                    start_new_session=agent == "antigravity",
# acr:end
                )
            finally:
                defer_cancellation = False
            if cancelled:
                raise SystemExit(128 + cancelled)
            try:
                stdout, stderr = hook.communicate(canonical, timeout=timeout)
                code = hook.returncode
                captured = True
            except subprocess.TimeoutExpired:
                stdout, stderr = b"", b""
                raise
        finally:
            # Stay in the host's command group so native cancellation reaches
            # package descendants. Never SIGKILL that shared group: it contains
            # this translator and the shells that publish its reply/status.
            defer_cancellation = True
            try:
                if hook is not None:
                    try:
# acr:antigravity
# acr:legacy                         if not captured:
                        if agent == "antigravity":
                            if not captured and hook.returncode is None:
                                # The unreaped leader pins the private group ID.
                                # An empty group can disappear before either signal.
                                for signum in (signal.SIGTERM, signal.SIGKILL):
                                    try:
                                        os.killpg(hook.pid, signum)
                                    except ProcessLookupError:
                                        break
                                    except PermissionError:
                                        # Darwin reports EPERM when the group
                                        # contains only exited, unreaped members.
                                        if sys.platform != "darwin":
                                            raise
                                        break
                                    if signum == signal.SIGTERM:
                                        time.sleep(0.2)
                        elif not captured:
# acr:end
                            # Install this only after spawning: package hooks must
                            # inherit default SIGTERM, not an ignored disposition.
                            signal.signal(signal.SIGTERM, lambda signum, frame: None)
                            pgid = os.getpgrp()
                            if pgid == os.getpid() or os.environ.get("acr_hook_command_pid") == str(pgid):
                                os.killpg(pgid, signal.SIGTERM)
                            else:
                                hook.terminate()
                            time.sleep(0.2)
                            # Ignoring grandchildren may survive indefinitely.
                            # Escalation and reaping cover only the direct hook.
                            hook.kill()
                    finally:
                        try:
                            hook.wait()
                        finally:
                            for pipe in (hook.stdin, hook.stdout, hook.stderr):
                                if pipe is not None:
                                    pipe.close()
            finally:
                signal.signal(signal.SIGTERM, cancel)
                defer_cancellation = False
            if cancelled:
                raise SystemExit(128 + cancelled)
    except subprocess.TimeoutExpired:
        return failure(agent, event, path, "", "watchdog")
    except OSError as error:
        return failure(agent, event, path, f"cannot run hook ({type(error).__name__})")
    if code not in (0, 2):
        return failure(agent, event, path, f"exited {code}; stdout discarded")
    try:
# acr:antigravity
# acr:legacy         return translate(agent, event, path, stdout, stderr, code)
        return translate(agent, event, path, stdout, stderr, code, native)
# acr:end
    except (ValueError, UnicodeError, TypeError, AttributeError):
        return failure(agent, event, path, "printed invalid JSON or an invalid hook response")


if __name__ == "__main__":
    for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(signum, cancel)
    try:
        try:
            status = main()
        except BrokenPipeError:
            raise  # Host EPIPE never becomes a native failure reply.
        # outer-boundary-process-contract: hosts fail open on an exception/exit 1.
        # Emit native denial for gates, diagnostic-only for Stop; propagation loses the gate.
        except Exception as error:
            agent = sys.argv[1] if len(sys.argv) > 1 else ""
            event = sys.argv[2] if len(sys.argv) > 2 else ""
            path = sys.argv[3] if len(sys.argv) > 3 else "<unknown>"
            status = failure(agent, event, path, f"translator failed ({type(error).__name__})")
        sys.stdout.flush()
        sys.stderr.flush()
    except BrokenPipeError:
        os._exit(141)  # Do not flush again during interpreter shutdown.
    finally:
        if cancelled:
            os._exit(128 + cancelled)
    sys.exit(status)
