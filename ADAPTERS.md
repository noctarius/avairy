# Adapter Investigation — Claude Code & Codex

> Reality-check of how each agent family can be driven as a long-lived, interruptible,
> *gated* worker. Input to the `Adapter` / `EnforcementBackend` interface design (§3/§7 of
> [DESIGN.md](DESIGN.md)). State: June 2026. **Both families are version-pinned and evolving
> — re-verify against the installed binary before relying on field/method names.**

## TL;DR — the control surfaces differ fundamentally

| Concern | Claude Code | Codex |
|---|---|---|
| Programmatic drive | `claude -p --output-format stream-json` (CLI) **or** Claude Agent SDK (TS/Py) | **`codex app-server`** (long-lived JSON-RPC v2: `thread/*`, `turn/*`) or `@openai/codex-sdk` |
| One-shot mode | `claude -p` | `codex exec --json` |
| **Gating mechanism** | **PreToolUse hook** (command hook gets tool call on stdin, calls out, returns allow/deny/ask) or SDK `canUseTool` | **in-protocol approval requests** (server→client JSON-RPC: `item/commandExecution/requestApproval`, `item/fileChange/requestApproval`) answered with a decision |
| Interrupt | SDK `interrupt()`; CLI = SIGTERM + `--resume` (no clean mid-turn input) | `turn/interrupt` (v2) / `InterruptConversation` (v1) |
| Mid-turn inject (steer) | limited/uncertain in CLI; SDK path preferred | native `turn/steer` |
| Turn-done signal | stream event `message_stop` w/ `stop_reason: end_turn` | event `turn.completed` (exec) / `turn/completed` (app-server) |
| MCP client | `.mcp.json`, `--mcp-config <json>`, `claude mcp add` (stdio/http) | `~/.codex/config.toml [mcp_servers.<id>]` (transport inferred); `codex mcp add` |
| Resume | `--resume <session-id>` / `--continue`; JSONL in `~/.claude/projects/...` | `codex resume [id]` / `--last`; rollout JSONL in `~/.codex/sessions/...` |
| Role / system prompt | `--append-system-prompt[-file]`, `CLAUDE.md` | `AGENTS.md`, `developer_instructions`, `model_instructions_file` |

**Design consequence:** drive **Codex via `app-server`, not `exec`** — exec is streaming-only and *cannot route approvals back*, which defeats our gating model. Drive **Claude via CLI stream-json or SDK**, with gating via the **PreToolUse hook calling back into avairy**.

## Claude Code — details

- **Invocation:** `claude -p "<prompt>" --output-format stream-json --verbose`. JSON output mode also exposes `session_id`, `usage`, `cost_data`.
- **Stream events:** NDJSON; `type: "stream_event"` with `event.type` ∈ `text_delta` (`event.delta.text`), `tool_use` (`event.name`, `event.input`, `event.id`), `tool_result`, `message_stop` (`stop_reason`). Also top-level `type: "system"` (subtype `system/init` lists plugins; `api_retry`).
  - **Done:** `message_stop` with `stop_reason: end_turn`. `stop_reason: tool_use` ⇒ a tool ran and the turn continues.
- **Interrupt / mid-turn:** CLI stream-json is **output-only** in practice — no reliable live input channel; interrupt = SIGTERM then `--resume <id>` with the new prompt. **Claude Agent SDK** exposes a true `interrupt()` and is the path for mid-reasoning injection. Killing a turn hard-kills any in-flight Bash/tool (no graceful drain).
- **Gating — PreToolUse hook (the mechanism we use):**
  - Configured in `.claude/settings.json` under `hooks.PreToolUse` (matcher by tool, `type: "command"`, `timeout`).
  - Hook **stdin**: `{tool_name, tool_input, tool_use_id, session_id}`.
  - Hook **stdout (exit 0)**: `{"continue": bool, "hookSpecificOutput": {"hookEventName": "PreToolUse", "permissionDecision": "allow|deny|ask|defer", "permissionDecisionReason": "...", "updatedInput": {...}}}`. Exit **2** = hard block (stderr fed to Claude).
  - **Routes to avairy:** the hook is a thin client that POSTs the tool call to a local avairy endpoint and returns the coordinator's decision. **Watch the hook `timeout`** (default ~30s) — slow human approvals can time the turn out; need async/long-timeout handling.
  - Coarser controls also exist: `permissions.{allow,ask,deny}` rules (deny-first), permission modes (`default|acceptEdits|plan|dontAsk|bypassPermissions`), `--allowedTools`/`--disallowedTools`.
- **MCP:** `.mcp.json` (project) / `~/.claude.json` (user) / inline `--mcp-config '{...}'`. stdio (`command`+`args`) or http (`url`). Tools namespaced `mcp__<server>__<tool>`; permission rules can scope them. Tool defs consume context (consider tool-search to defer loading).
- **Resume/compaction:** `--resume <id>`/`--continue`/`--fork-session`; auto-compaction summarizes old turns (early context can be lost — blackboard is our durable memory, per design §8).
- **Caveats:** SDK interrupt/streaming-input details need confirmation against installed version; deny-first precedence means hooks can't un-deny a denied rule; Windows symlinked config files can misbehave.

## Codex — details

- **Invocation:** persistent → `codex app-server` (line-delimited JSON-RPC 2.0 over stdio); one-shot → `codex exec --json` (NDJSON to stdout, progress on stderr; `-o/--output-last-message`, `--output-schema`). TS SDK `@openai/codex-sdk` spawns the binary; Python `openai-codex` (beta).
- **app-server v2 protocol:** `thread/start`, `turn/start`, `turn/interrupt`, `turn/steer`, `thread/fork`, `thread/list|read`; streaming deltas `item/agentMessage/delta`, `item/commandExecution/outputDelta`; `turn/completed` with `status: completed|interrupted` + `usage`.
- **exec JSONL events:** `thread.started`, `turn.started`, `turn.completed` (+`usage`), `turn.failed`, `item.started`, `item.completed`; item types `agent_message`, `reasoning`, `command_execution`, `file_change`, `mcp_tool_call`, `web_search`.
  - **Done:** `turn.completed` / `turn/completed`.
- **Gating — in-protocol approvals (the mechanism we use):**
  - Server→client requests: v2 `item/commandExecution/requestApproval` (`itemId, threadId, turnId, command, reason?`), `item/fileChange/requestApproval` (`grantRoot?`), `item/permissions/requestApproval`, `item/tool/requestUserInput`. v1 analogues `execCommandApproval`, `applyPatchApproval`.
  - **Decision enum** (`ReviewDecision`): v1 `approved | approved_for_session | denied | abort | timed_out` (+ structured amendments); v2 docs phrase as `accept | acceptForSession | decline | cancel | acceptWithExecpolicyAmendment`. **Names differ v1↔v2 — regenerate schema.**
  - **CRITICAL operational rule:** an *unanswered* approval request **hangs the turn forever** — avairy must answer every one.
  - Policy/sandbox knobs: `--ask-for-approval untrusted|on-request|never`, `--sandbox read-only|workspace-write|danger-full-access` (macOS Seatbelt / Linux bwrap+seccomp). `--full-auto` deprecated; `--yolo` disables sandbox.
- **MCP:** client via `[mcp_servers.<id>]` in `config.toml` — **transport inferred** (stdio: `command`/`args`; http: `url`+`bearer_token_env_var`, *not* a literal token). `codex mcp add|list|get|remove`. Can also run **as** an MCP server: `codex mcp-server` (experimental, exposes v2 thread/turn).
- **Resume/compaction:** `codex resume [id]|--last`, `codex exec resume`; rollout JSONL `~/.codex/sessions/YYYY/MM/DD/...`; auto-compact via `model_auto_compact_token_limit`. IDs auto-generated (`thr_*`), not user-nameable.
- **Identity/config:** `AGENTS.md`; `[profiles.NAME]`, `-c key=value` overrides, `-m/--model`, `model_reasoning_effort`, `-C/--cd`.
- **Caveats:** v1↔v2 protocol split is the main trap (target v2 `thread/turn`, but v1 approval methods still live); experimental features need `capabilities.experimentalApi: true` at `initialize`; rollout files world-readable (#21660); regenerate types with `codex app-server generate-ts`.

## What this fixes in DESIGN.md

- §6 interrupt support is **Claude Agent SDK** + **Codex app-server**, *not* the bare `claude -p` / `codex exec` modes. The `supports_interrupt` capability flag already models the variance; examples corrected.
- §7 enforcement: native-hook backend for Claude (PreToolUse → avairy), in-protocol-approval backend for Codex (app-server). Both route to the coordinator at runtime → confirms the `EnforcementBackend` abstraction is correct and necessary.
- Codex adapter targets **app-server**, not exec, as the primary transport.

## Open verification items (before/while building the adapters)

- Confirm Claude Agent SDK `interrupt()` + streaming-input signatures against the installed SDK version (decides whether Claude is `interrupt`- or `steer`-only).
- Confirm Claude PreToolUse hook async/long-timeout behavior for human-speed approvals.
- Pin Codex app-server v2 method/field/enum names via `codex app-server generate-ts` against the installed binary.