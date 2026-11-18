# Adapter boundary

`internal/adapter` is the versioned boundary between native agent renderers and the transactional realization engine described in [`docs/realization.md`](realization.md). An adapter inspects resolved packages and the project tree and renders data-only `Output` values; it never writes files and never constructs a `realize.Intent` directly. `compileOutputs` is the only trusted bridge from adapter output to `realize.Intent`.

This package defines the generic contract, the compilation guard, and a `Coordinator` library that resolves a fixed set of adapters against resolved packages. The shipped native implementations are `claude-code` at adapter version `1.0.4`, and `codex` and `cursor` at `1.1.0`, all at boundary version `1`. `internal/adaptertest` supplies shared golden fixtures plus reference and hostile adapters used to exercise the boundary end to end.

## Native adapters

For a package source `github:owner/repo`, every native artifact name is `acr__owner__repo__<artifact-id>`. Generated trees contain regular files only. Standalone scripts and hooks use mode `0755`; ordinary rule and skill files use `0644`, while executable files inside a skill retain `0755`.

Skill files, and the rule bodies rendered into an instruction host, carry two supported forms of reference to a bundled file. Both are rebased to the installed native directory so a command copied out of realized content resolves from the project root. A package-root path `skills/<id>/...` addresses any skill the same package declares, so a cross-skill reference resolves as well as a same-skill one. A legacy Tessl-installed path — `.tessl/plugins/<workspace>/<package>/` followed by that same package-root path — is replaced whole, but only when it names an identity the package is evidenced to own: its recorded `source.tesslIdentity`, or its own package name. Any other identity addresses a different package's tree and keeps its bytes, because redirecting it would silently run somebody else's helper.

`source.tesslIdentity` is written by migration, which is the only place the original identity is known: `acr migrate tessl-plugin` records the Tessl plugin name it converted, and `acr migrate tessl --vendor-unmapped` records the `<workspace>/<package>` the tree was installed under. The manifest's own `name` cannot stand in on its own, because `source.repository` binds that name to the GitHub repository — a package republished under a different repository name loses the identity its files still address. A package converted before that field existed, or renamed away from its identity, keeps its package-root references working and preserves its legacy ones; converting the producer again and publishing a new release restores them.

A reference is rewritten only where one may begin, and that is decided from the structure the scanner is already inside rather than from the byte in front of it. One byte of lookbehind cannot tell a Markdown link destination from `archive](skills/<id>/...)`, where nothing opened a label, nor a quoted command from `https://host/a'skills/<id>/...`, where the apostrophe sits inside a word. The scanner therefore carries three pieces of enclosing context:

- **The word being read.** A quote or a bracket inside a run of non-whitespace bytes is part of that word, not an opener, so a URL, a query value and an unrelated filename stay whole.
- **An argument container.** A `'` or `"` that opens where an argument may start and has a closing partner delimits one argument: the whole interior belongs to it, so whitespace, a further quote and an escaped quote inside the argument all separate nothing. Only its first interior position begins a reference, and being eligible there does not put that position outside the argument, so `"'archive' skills/<id>/..."` keeps every byte it was written with. The partner is that argument's terminator and is consumed as one, so an argument ending in whitespace closes at its own quote instead of reopening through the next quote in the file, and `sh skills/<id>/... "label "` followed by a second command still rebases that second command. The word carrying the argument continues past the terminator, because a shell removes the quotes and joins what is left: `"archive "skills/<id>/...` is one word whose interior space is quoted, so it keeps its bytes, while `""skills/<id>/...` — an empty argument that contributes nothing to the word — is the path alone and is rebased. Inside a double-quoted argument a backslash escapes the byte after it, as it does in a shell, so `"archive \" skills/<id>/..."` stays one argument rather than two. A quote with no partner opens nothing, so an odd quote cannot swallow the rest of a file. Outside a quoted argument, a backtick at a token start acts as a Markdown code-span delimiter. Whitespace outside quoted arguments still separates tokens, so a backquoted command's unquoted arguments can begin references.
- **A label stack.** `](` opens a Markdown destination only where a `[` actually opened at a token start. Labels reset at a blank line, which is the one thing [CommonMark](https://spec.commonmark.org/0.31.2/#links) link text cannot contain, so a destination whose label wraps onto a second line is still rewritten while an unrelated `](` further down the file opens nothing.

Ordinary emphasis directly before a code span also preserves that token start: one, two, or three matching `*` or `_` markers followed immediately by a backtick, as in **`skills/<id>/references/guide.md`**. The markers must start a token outside a quoted argument. Markers inside filenames or URLs, escaped markers, and bare glob or filename prefixes keep their bytes. Existing generated files upgrade through ordinary `acr realize`; `acr check` reports the pending change until it is applied.

A leading escape and one shell assignment prefix (`NAME=` or `--flag=`) are carried through ahead of the reference; the assignment name is a bounded run, so a URL query's `?next=` never qualifies. An assignment prefix is also a place an argument may start, so `HELPER="skills/<id>/..."` and `--file='skills/<id>/...'` name the same installed helper their unquoted spellings do. Every reference outside the two supported forms is preserved unchanged rather than rewritten into a path that resolves nowhere.

`RebasePackageReferences` applies a whole package's references, including the legacy Tessl form, which needs an evidenced identity. `RebaseSkillReferences(content, sourceRoot, nativeRoot)` remains the original two-root entry point at its original signature, so an adapter built against boundary version `1` keeps compiling; it carries no identity and therefore never rewrites a legacy path.

A rule resolves against the installed tree of the adapter that owns its instruction host — `.claude/skills` for a Claude-owned host, `.codex/skills` for a Codex-owned one, `.cursor/skills` for Cursor's own `.mdc`. When Claude and Codex share one host, every client reading it reaches the owner's tree by the same project-relative path, so the single contribution stays reachable to both without duplicating the rule body.

An include naming an instruction host this run writes resolves even when that file is absent right now. Deselecting every Markdown adapter removes an ACR-created host while a user's `@AGENTS.md` in another file survives, and reselecting has to discover the host before it can regenerate it; without this, reselection refuses with `unresolved_include` and the only way out is for the user to delete their own import. An include naming anything else that does not resolve is still a refusal.

| Artifact | Claude Code | Codex | Cursor |
| --- | --- | --- | --- |
| Rules | Shared `CLAUDE.md`/`AGENTS.md` managed block | Shared `AGENTS.md` managed block | `.cursor/rules/<name>.mdc` |
| Skills | `.claude/skills/<name>/**` | `.codex/skills/<name>/**` | `.cursor/skills/<name>/**` |
| Scripts | `.claude/scripts/<name>/<basename>` | `.codex/scripts/<name>/<basename>` | `.cursor/scripts/<name>/<basename>` |
| Hook executable | `.claude/hooks/<name>/<basename>` | `.codex/hooks/<name>/<basename>` | `.cursor/hooks/<name>/<basename>` |
| Hook configuration | `.claude/settings.json` | `.codex/config.toml` | `.cursor/hooks.json` |

Claude and Codex rule bundles reuse the preservation compiler's include graph. If `CLAUDE.md` imports `AGENTS.md`, both selections contribute one package block to the shared `AGENTS.md` host; rule bodies are not duplicated. Path-activated rules include their declared globs in the block. Cursor emits one `.mdc` per rule with exactly one leading frontmatter document: `alwaysApply: true` for unconditional rules, or `globs` plus `alwaysApply: false` for path activation. Any source frontmatter is parsed and discarded so the manifest remains authoritative.

The neutral hook vocabulary maps as follows:

| Manifest event | Claude Code | Codex | Cursor |
| --- | --- | --- | --- |
| `session-start` | `SessionStart` | `SessionStart` | `sessionStart` |
| `session-end` | `SessionEnd` | `SessionEnd` | `sessionEnd` |
| `user-prompt-submit` | `UserPromptSubmit` | `UserPromptSubmit` | `beforeSubmitPrompt` |
| `pre-tool-use` | `PreToolUse` | `PreToolUse` | `preToolUse` |
| `post-tool-use` | `PostToolUse` | `PostToolUse` | `postToolUse` |
| `stop` | `Stop` | `Stop` | `stop` |

Hook entries are merged structurally. Claude uses command matcher groups in [native exec form](https://code.claude.com/docs/en/hooks#exec-form-and-shell-form): `command` is the unquoted `${CLAUDE_PROJECT_DIR}/…` executable path and `args` is always an array, including `[]` for a hook with no arguments. Claude substitutes path placeholders and spawns the executable directly; spaces, empty arguments, quotes and shell-special characters need no shell escaping. Existing ACR 0.1.4 settings upgrade through ordinary `acr realize`; `acr check` reports the pending change before realization and a current state afterward. The owned entries are replaced through their ledger identities, preserving user and Tessl siblings. Codex emits native `[[hooks.<Event>]]` and `[[hooks.<Event>.hooks]]` tables. Codex runs a command hook with the session working directory as its cwd and exposes no project-root variable to it, so the command first walks from `$PWD` to the nearest `agents.yaml` — the marker every initialized ACR project carries — then runs `python3 -I` with the managed translator under that root. The translator inherits the session cwd, passes stdin bytes unchanged, and sets `CLAUDE_PROJECT_DIR` to the ACR project root. A launch with no `agents.yaml` above its cwd is refused rather than resolved to another project, so an enclosing Git repository can no longer capture a nested ACR consumer's hook. The command leaves `hooks.state` trust data untouched. Codex discovers a project `.codex/` layer only from a trusted project root; nested non-Git launches need the recognized markers and trust its [advanced configuration](https://learn.chatgpt.com/docs/config-file/config-advanced) documents, and root startup is the supported default. Cursor uses command objects under `hooks.<event>`; it adds an owned root `version: 1` when missing, re-plans that field while the previous ledger records ACR ownership, preserves an existing unowned `version: 1`, and rejects any other schema value.

### Hook delivery

Package hooks use the [canonical command-hook contract](package-manifest.md#hook-contract). Codex and Cursor hooks require **python3 ≥ 3.9 on PATH**, including the PATH available to a GUI agent. ACR embeds a stdlib-only translator at `.codex/hooks/acr-hook.py` or `.cursor/hooks/acr-hook.py` (mode `0755`) only while that adapter has hooks. Translation runs in realized output; ACR never runs an agent. Shell guards report how to install Python and deny gating events even when the interpreter cannot start. Cursor gating definitions also set `failClosed: true` ([configuration contract](https://cursor.com/docs/hooks#configuration)). Runtime and translation failures deny before-tool and prompt events, report non-blocking errors for session/after-tool events, and allow Stop with an actionable diagnostic. Hook paths must be project-relative, contain no `..` segments and resolve inside the realized root.

Both adapters write a native `timeout` of 600 seconds, except Codex session end uses 3 seconds. [Codex documents](https://learn.chatgpt.com/docs/hooks#configuration) a 600-second default for most events and a one-second default with a three-second maximum for session end; [Cursor accepts an explicit timeout in seconds](https://cursor.com/docs/hooks#configuration) but leaves its platform default unspecified. The Python translator keeps each package hook in the native command’s process group. At three quarters of the realized deadline (rounded down to whole seconds), its watchdog sends SIGTERM to that shared group while protecting the translator and buffering shells with caught handlers. After a 0.2-second grace it SIGKILLs and reaps only the direct hook, closes capture pipes and discards partial output. Package hooks inherit default SIGTERM handling; the translator never SIGKILLs the shared group. This reserve lets ACR emit a native gate denial before the host's timeout can fail open. Stop timeouts allow termination with a diagnostic. Output capture buffers stdout and stderr until both streams reach EOF, so a timeout discards partial replies even when the launcher has already exited.

Python ≥ 3.9 is the only translator runtime requirement. Supervision stays in the translator through package launcher exit until both output pipes reach EOF or the deadline expires, covering background descendants holding either pipe. Interpreter startup hangs are outside this watchdog; shell guards still deny gates if `python3` is missing or exits unsuccessfully.

SIGTERM, SIGINT and SIGHUP cancel supervision silently with statuses 143, 130 and 129. The translator performs the same bounded group SIGTERM and direct-hook escalation before exiting; partial replies are discarded. Cancellation is deferred only while creating the child or cleaning up, so repeated signals cannot interrupt cleanup and waiting for stdin remains cancellable. A detected broken host output pipe exits 141 without a failure reply or shutdown flush. The shell wrapper preserves these four statuses and exits 141 if publishing the accepted reply fails. Ordinary stdin EOF completes the request. Internal timeouts and supervision errors still produce the native failure replies described above. Native cancellation reaches descendants remaining in the command group while the invocation is outstanding. Codex sends SIGKILL to that group ([runner cleanup](https://github.com/openai/codex/blob/a05444bbeb6bb07f2875fff0143f2e4cba994463/codex-rs/hooks/src/engine/command_runner.rs#L393)); SIGKILL runs no translator cleanup. Cursor’s documented timeout and `failClosed` options do not establish its cancellation signals, targeting or escalation; synthetic group-kill tests provide no Cursor vendor guarantee.

Translator-originated cleanup cannot guarantee termination of SIGTERM-ignoring grandchildren. They may survive indefinitely and perform side effects after the translator replies, with no guaranteed later host cleanup: [Codex clears its cleanup guard when the command completes](https://github.com/openai/codex/blob/a05444bbeb6bb07f2875fff0143f2e4cba994463/codex-rs/hooks/src/engine/command_runner.rs#L342-L348). This residual applies even to grandchildren that remain in the command group. Descendants deliberately leaving the group are also outside group cancellation. Package authors must arrange their own cleanup when these residual behaviors are unacceptable.

Codex replies must also fit its native JSON parser. ACR rejects integers outside `i64::MIN` through `u64::MAX`, non-finite `f64` values and container nesting that exhausts serde_json's 128-entry recursion budget (the 128th container is rejected). These limits follow [serde_json's number representation](https://docs.rs/serde_json/1.0.154/serde_json/struct.Number.html) and [deserializer depth check](https://docs.rs/serde_json/1.0.154/src/serde_json/de.rs.html#1372-1386). An unrepresentable allow with `updatedInput` denies the tool call with an ACR message naming the hook, event, runtime failure and recovery. Orphan `reason` fields without a blocking decision or `continue:false` are removed on every Codex event, preserving supported additional context. PostToolUse retains `reason` with `continue:false` for model feedback.

| Contract | [Claude Code](https://code.claude.com/docs/en/hooks) | [Codex](https://developers.openai.com/codex/hooks) | [Cursor](https://cursor.com/docs/hooks) |
| --- | --- | --- | --- |
| Input | Canonical JSON and `CLAUDE_PROJECT_DIR` | Stdin bytes unchanged; translator supplies `CLAUDE_PROJECT_DIR` | Native fields retained; event/session/cwd/source, tool response and stop guard added in canonical form |
| Session start | `hookSpecificOutput.additionalContext` or text | Context JSON filtered; plain text unchanged | `additional_context` |
| Session end | Native advisory output | JSON discarded; plain text advisory | Output discarded |
| Prompt submission | Block or context | Block/context JSON filtered; plain text unchanged | `continue:false` plus `user_message` blocks; context dropped with diagnostic |
| Before tool | `permissionDecision`, reason, `updatedInput` | `ask` becomes deny with guidance; unsupported stop/suppression fields removed; `defer` and allow without a rewrite become no-ops | `permission`, reason messages, `updated_input`; `ask` becomes deny with guidance; context dropped with diagnostic |
| After tool | Block reason beside original result, plus context | Block reason becomes `additionalContext` so the original result survives; `continue:false` retains `reason`; unsupported output rewrites removed | Reason and context become `additional_context`; MCP rewrite becomes `updated_mcp_tool_output` |
| Stop | `continue:false` terminates; otherwise block reason continues | Preserves `continue:false` and prepares `stopReason` as termination feedback; otherwise context becomes `decision:block` plus `reason`; plain text dropped with diagnostic | `continue:false` emits no followup; otherwise block reason/context become `followup_message`; `loop_count > 0` supplies `stop_hook_active` |
| Exit 2 | Block/deny via stderr where supported | Native gating/Stop denial; after-tool feedback becomes context and session events become diagnostics at exit 0 | Native denial, prompt block, stop continuation or after-tool feedback at exit 0 |
| Other failures | Native nonblocking error | Gates: exit 2 denial; other events: accepted diagnostic at exit 0 | Gates: native denial; other events: accepted no-op or diagnostic at exit 0 |

Every translated reply passes one final check against the target event's fields and types. Codex JSON is filtered to its [generated output schemas](https://github.com/openai/codex/tree/main/codex-rs/hooks/schema/generated) and parser constraints. In particular, the [Stop schema](https://github.com/openai/codex/blob/main/codex-rs/hooks/schema/generated/stop.command.output.schema.json) forbids `hookSpecificOutput`; ACR maps its context to `decision: "block"` and `reason` unless `continue:false` terminates the response. A deny response carries no `updatedInput`. Codex and Cursor denial messages name the hook, event and intentional outcome; missing or whitespace-only reasons request policy review without describing a hook failure. Unknown fields are dropped with a diagnostic. Exit-2 stdout bypasses decoding because the host ignores it. If a reply cannot be made valid, gates deny and Stop allows termination with a diagnostic.

Intentional feedback uses the same prepared message in every surviving sibling field and diagnostic: unsafe control characters are escaped and rendered detail is bounded to 16 KiB before adding hook identity, outcome and recovery. Ordinary successful context and stderr pass through unchanged; filtering diagnostics omit unsupported field names.

After upgrading Codex output, **re-trust the changed hook definitions once in `/hooks`**. Codex hashes each definition; ACR preserves `hooks.state` but cannot reuse approval for a changed command ([trust documentation](https://developers.openai.com/codex/hooks#trust-and-approval)).

Cursor also imports Claude hook configurations by default. Selecting both `claude-code` and `cursor` can run a hook twice; disable “Include Third-Party Plugins, Skills, and Other Configs” in Cursor when using its native ACR hook output ([third-party hooks](https://cursor.com/docs/reference/third-party-hooks)). Whether Cursor’s native `permission:allow` changes its approval UI still needs interactive conformance; the translator uses Cursor’s documented allow response for no decision.

### ACR freshness hook

Session-start freshness uses the same adapter contract without adding adapter-specific behavior. `internal/freshness` contributes one synthetic package after the locked dependency packages:

- source `github:jbaruch/agentic-context-registry`;
- hook artifact ID `freshness-session-start`;
- event `session-start`;
- source path `internal/freshness/session-start.sh`;
- arguments `--policy outdated|install`.

The existing renderers place the executable and merge the native hook entry exactly as they do for a dependency package. The stable ownership key makes repeated realization idempotent and lets a policy change replace the owned argument vector rather than add a duplicate. Policy `none` omits the synthetic package; the coordinator revisits the previous shared configuration target and removes only the owned hook entry. Unrelated user hooks and Codex trust state remain untouched.

The wrapper converts actionable notices and fail-open diagnostics into one session-start context payload beginning `Session-start status — `. Claude Code and Codex share the `hookSpecificOutput.additionalContext` response shape. The wrapper always emits the canonical contract; Cursor’s translator converts it to `additional_context`. Throttled and no-change runs emit nothing, and every outcome exits `0`.

Each adapter validates its complete candidate projection before the engine runs. Stable native error codes cover event spelling, duplicate configuration, malformed Cursor frontmatter, incomplete or unsafe skill trees, and non-executable scripts/hooks. Within the native skill roots being validated, a real directory named exactly `__pycache__` is tolerated when every direct child is a regular file with a case-sensitive `.pyc` suffix; an empty cache also qualifies. This exception does not verify bytecode contents. Loose `.pyc` files, uppercase `.PYC`, `.pyo`, and files in nested or impure caches receive no exemption from existing candidate and historical-file checks. Empty directory scaffolding remains allowed. Symlinks and special entries remain refused with `invalid_skill_tree`. Detection is read-only and reports sorted evidence from the agent's instruction, configuration, rule, and skill paths.

## Contract

`Descriptor{ID, Version, Boundary}` identifies an adapter implementation. `Boundary` is the exact API and semantic contract version (`CurrentBoundaryVersion`); a signature or semantic compatibility break to this package increments it, while an adapter's own output changes bump `Version` instead. `Register` validates every adapter before it can be coordinated: a unique, lowercase kebab-case ID; a valid semantic `Version`; sorted, duplicate-free `SupportedArtifacts()`/`SupportedEvents()`; and `Boundary == CurrentBoundaryVersion`, or an actionable `*BoundaryVersionError`.

The `Adapter` interface:

| Method | Purpose |
| --- | --- |
| `Descriptor()` | Identity and version, used for ledger stamping and diagnostics |
| `Detect(ctx, DetectRequest)` | Read-only project inspection for `acr init` preselection |
| `SupportedArtifacts()` / `SupportedEvents()` | The closed capability set the coordinator's preflight checks against |
| `Plan(ctx, PlanRequest)` | Maps resolved package artifacts onto native target paths and kinds, without bytes |
| `Render(ctx, RenderRequest)` | Produces the adapter's `Output` values for a `NativePlan` |
| `Validate(ctx, ValidateRequest)` | Adapter-owned semantic checks over compiled candidates, before intents reach `realize.Engine` |

### Snapshot implementations

`RootSnapshot` (`NewRootSnapshot(dir)`) is the production-safe `Snapshot`: it is backed by `os.OpenRoot`, so no path component and no symlink it follows can resolve outside the project directory — the same confinement `internal/realize`'s own write boundary relies on — and it rejects symlinks and special files at the leaf, rejects symlinks in parent path components, and caps read size. `FSSnapshot` (`NewFSSnapshot(fsys)`) is test-only scaffolding backed by any `fs.FS`; an `fs.FS` such as `os.DirFS` is explicitly permitted by its own contract to follow a symlink outside its root, so `FSSnapshot` must never back a real project tree.

`ReadFile` still rejects every symlink leaf, including a project-root `CLAUDE.md`. Detection and instruction-root planning do not follow that link through `ReadFile`. They accept a project-root `CLAUDE.md` symlink only when its canonical final target is a regular file inside the project, then plan the resolved file and leave the symlink in place. Native `Stat` enforces the OS’s symlink and non-directory traversal limits; `EvalSymlinks` supplies the canonical target, whose parent ancestors are compared with the opened project root using `os.SameFile`, then whose leaf is compared with rooted metadata before reading. Intermediate links may leave and reenter the project. A dangling link, a link whose final target is outside the project, a chain that ends on a non-regular file, and any other host path that is a symlink are refused with a message that names the path, the canonical target when known (otherwise the literal link target), and why the link was refused, with a repair instruction.

Before accepting an alias, rooted metadata checks reject destinations that identify `agents.yaml` or lie within the rooted `.git` or `.agents` entries, including filesystem-equivalent spellings. The planner applies the same check to ordinary intents and ledger destinations, preserving the coordinator-owned shared-skill exception. These checks do not read target contents.

After safety validation, a project-root `CLAUDE.md` alias requires exact on-disk component names for its resolved destination and all existing regular conventional roots (`CLAUDE.md`, `.claude/CLAUDE.md`, `AGENTS.md`, `AGENTS.override.md`), including unselected hosts. Layouts requiring case-equivalent instruction destination matching are refused with the alias, target, offending component, and guidance to align on-disk native-host names and link-target spelling. Root-directory prefix casing does not affect containment. No root spelling is recovered; ordinary non-alias roots retain their observed names. Distinct case-sensitive files and exact hardlink entries remain separate write destinations.

## Capability preflight

`Coordinator.Realize` checks every `(adapter, source, artifact ID, artifact kind, hook event)` implied by the resolved packages against every selected adapter's `SupportedArtifacts()`/`SupportedEvents()` before calling any adapter's `Plan`. Any miss produces a sorted `*UnsupportedError` (code `unsupported_adapter_capability`) naming every combination; the preflight itself only calls `Descriptor`, `SupportedArtifacts`, and `SupportedEvents` — `Plan`, `Render`, and `Validate` never run, and no files change. The package manifest's own `unsupported_hook_event` (see [`docs/package-manifest.md`](package-manifest.md)) is a different check: it rejects event names outside the neutral v1 vocabulary, while `UnsupportedError` rejects a valid event the *selected adapter* cannot realize.

## Output kinds

An adapter never supplies a whole shared document; it supplies only the pieces the trusted compiler needs to derive one safely:

| Kind | Adapter supplies | Ledger `artifactKind` |
| --- | --- | --- |
| `generated-file` | Whole file content | `file` |
| `markdown-include` | One include/block body per insertion | `managed-block` |
| `config-merge` | Structural `(container, kind, key)` entries | `structured-entry` |

`compileOutputs` (`internal/adapter/compile.go`) groups every adapter's outputs by native target and is the sole place `Content`, `ObservedHash`, `ManagedIntact`, and `PreservedContent` are set on a `realize.Intent`:

- A `generated-file` output is rejected outright for an existing target the previous ledger does not already record as `generated-only` — a whole-file adapter output can never replace shared or unproven content. Promoting a changed generated file to shared ownership is only reachable through a `markdown-include`/`config-merge` output.
- A `markdown-include` or `config-merge` output is compiled through a registered `SharedCompiler`; without one, both kinds fail closed rather than silently skipping preservation.
- Duplicate managed-block IDs and duplicate `(container, kind, key)` structural entries across every contributing adapter and package are rejected as `*DuplicateEntryError` (code `duplicate_config_entry`); multiple adapters may otherwise contribute to the same shared target. Duplicate detection and sort ordering both use one length-prefixed encoding of every container segment, the entry kind, and the key, so no separator byte inside a segment can make two structurally different tuples collide.
- A `config-merge` output with a non-nil `Config` but zero `Entries` is rejected outright: it would otherwise render zero plan/render-correspondence tuples, letting an adapter with a completely empty plan pass correspondence trivially (0 == 0) while still contributing to compilation for any target it names — all while its own `Validate` never receives it. An adapter with nothing to contribute must omit the output entirely.
- `compileOutputs` also visits every previously shared target that has no current output at all (a package or artifact was removed), so the compiler can express a safe partial or final removal instead of the plain generated-only delete path, which would silently drop any surviving unmanaged or still-owned content.
- Every output's target is rejected by `realize.ValidateTargetPath` (reserved paths, parent traversal, absolute paths) before anything else runs — defense in depth alongside `realize.Planner`'s own authoritative rejection of the same paths on any intent, whatever produced it.

### The SharedCompiler seam

```go
type SharedCompiler interface {
	CompileMarkdown(ctx context.Context, request MarkdownCompileRequest) (SharedCompilation, error)
	CompileConfig(ctx context.Context, request ConfigCompileRequest) (SharedCompilation, error)
}
```

For Claude Code or Codex rule bundles, the shared compiler must also implement the `IncludeGraphProvider.DiscoverIncludeGraph(project, selectedRoots)` seam. The provider returns a read-only `InstructionIncludeGraph` used to validate the selected roots and choose their deepest shared host; an `invalid_include`, `unresolved_include`, `include_cycle`, or `duplicate_include` in a selected root's connected component aborts `Coordinator.Realize` and therefore both `acr realize` and `acr check`, while findings in untouched components remain warnings. Discovery indexes modes across the project but reads content only for roots and reachable include targets.

`compileOutputs` reconciles the previous ledger `Target` (if any) against this run's desired insertions/entries — which are empty for a no-current-output revisit — and passes both to the compiler as `SharedTarget{Path, Observed, Previous, ExplicitDemotion, Force}`; `Observed`/`Previous` are `nil` when the native file or the ledger target is absent. `ExplicitDemotion` and `Force` are coordinator/user options, never adapter data: `Coordinator.Realize` and `compileOutputs` both take a variadic, optional trailing `map[string]TargetOptions` keyed by native target path (omit it entirely for the default of no per-target overrides), and stamp `SharedTarget.ExplicitDemotion`/`Force` from the entry for that target. The compiler answers with one `SharedCompilation{Action, Candidate, Managed, Proof, Notices}`: `Action` becomes the `realize.Intent`'s action; `Candidate` (nil only for a proven whole-target removal) supplies `Content`/`Mode`/`Ownership`; `Managed` is stamped into ledger `Entry` values using the registered descriptor that contributed each entry's owner, never compiler or adapter data; `Proof` is copied verbatim onto `ObservedHash`/`ManagedIntact`/`PreservedContent`. A `ConfigEntry` with `Kind: ConfigElement` names its array by `Container` alone; the compiler locates the previous element by its ledger `managedHash`, never by array position, so an array reordered by something else still resolves correctly.

Each `ManagedResult` must set `Identity` to the contribution-level identity of the `Desired` entry it confirms — a `MarkdownInsertion`'s own `BlockID`, or `CanonicalEntryKey(entry.Container, entry.Kind, entry.Key)` for a `ConfigEntry` — never just `Owner`. Two different adapters can legitimately contribute the same `OwnerRef` (the same package artifact) to one target under distinct block IDs or config keys; `compileOutputs` matches each `ManagedResult` back to the descriptor that contributed it by `Identity`, since `Owner` alone cannot tell them apart and duplicate detection already guarantees `Identity` is unique across every adapter for that target.

A config-merge target's structural format (JSON or TOML) is never guessed from its file extension — a caller-controlled path is not trustworthy evidence. When a current adapter output declares `Config.Format`, that declaration is used (and rejected as malformed if a caller-supplied `TargetOptions.ConfigFormat` disagrees with it). When `compileOutputs` revisits a target with no current output at all, `TargetOptions.ConfigFormat` is the only source of truth; without it, compilation fails closed rather than inferring anything from the path.

`internal/preserve` supplies the production `SharedCompiler` from issue #6: concrete JSON/TOML/Markdown preservation, marker grammar, include-graph discovery, and promotion/demotion mechanics. `internal/adapter` defines only the seam and its fail-closed guard.

## Engine defense in depth

The realization planner's `preserves()` check is vacuously true when an intent's `PreservedContent` is empty, so a hand-built intent that otherwise satisfies every other merge precondition could still whole-file-replace a shared target. `internal/realize`'s planner closes this independently of the adapter boundary: a shared `ensure` or `promote` whose observed file is non-empty conflicts unless `PreservedContent` carries at least one non-empty fragment and every non-empty fragment occurs byte-for-byte in the hash-bound observed file, whether or not the intent reached the planner through `compileOutputs`.

## Coordinator

`Coordinator.Realize(ctx, project, packages, previous)` runs the complete boundary as a library, without ever calling `realize.Engine` itself:

1. The capability preflight (above).
2. Each registered adapter's `Plan` then `Render`, in adapter-ID order.
3. For each adapter, an exact correspondence check between its `NativePlan.Items` and its own rendered contributions — every `(target, kind, mode, owner)` tuple the plan promises must appear in the render exactly as often, and vice versa, and `NativePlan.Adapter` must match the registered descriptor. A plan with no items can therefore never smuggle an arbitrary rendered output through compilation, and one adapter's output can never mask another adapter's planned-but-never-rendered target, even when both legitimately share one native file.
4. `compileOutputs` over every adapter's rendered `Output` values.
5. Each adapter's `Validate` over the compiled `CandidateFile`s for its own planned targets.

It returns `realize.Intent` values only when every stage succeeds. A caller must never invoke `realize.Engine.Run(ModeApply, ...)` when `Realize` returns an error — the returned intent slice is `nil` in that case, so there is nothing to apply. `internal/realizeapp` selects persisted or flag-supplied adapters, materializes and verifies immutable locks, invokes this coordinator, and runs the transactional engine for `acr realize` and `acr check`.

`ctx` is threaded verbatim into every `Plan`/`Render`/`Validate` call and into `compileOutputs`'s own `SharedCompiler` calls — never replaced with `context.Background()` — so a caller's cancellation or deadline actually reaches the compiler. `Realize` drops any `SharedCompilation.Notices` a compiler returned; `RealizeWithNotices` returns the identical result plus every notice gathered across all compiled targets, in target-path order — the way a caller learns about #6's `shared_file_requires_commit` promotion notice, for example.

An adapter version bump alone — identical rendered bytes, only `Descriptor.Version` changed — still regenerates every affected ledger entry, because `compileOutputs` stamps `Adapter`/`AdapterVersion` from the currently registered descriptor. The next plan is non-empty and reviewable (`ledgerChanged: true`, an `update` operation with equal before/after hashes) even though no bytes changed; no file is rewritten until `ModeApply`.

## Golden fixtures

`internal/adaptertest.RunGolden` drives an adapter and `SharedCompiler` through every case directory under `testdata/`:

```text
testdata/<case>/
  package/agent-plugin.yaml   # plus every declared artifact file
  project/                    # starting native files (omit for an empty project)
  want/plan.json              # success cases: the realize.Plan, content omitted by design
  want/files/<path>           # success cases: the complete resulting project tree
  want/error.json             # error cases only: {"error": "<exact Coordinator.Realize error>"}
```

`go test ./internal/adaptertest/... -run TestReferenceAdapterGolden -update` rewrites `want/` from the adapter's actual output; without `-update`, the harness compares the plan JSON and every realized file's bytes, and fails on any extra or missing file. Fixtures are UTF-8 text, built once and checked in, never generated from wall-clock time or unseeded randomness.

`RunGoldenFixture` supports a shared package/project fixture with separate adapter-specific `want/` trees. The checked-in `all-agents` and `freshness-session-start` matrices exercise Claude Code, Codex, and Cursor against identical inputs, including modes, preservation, idempotence, cleanup, and event vocabulary.

`internal/adaptertest.NewReferenceAdapter` is a fixture/hostile test double, not a production adapter.
