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
- **Stream events (VERIFIED against 2.1.176 via live smoke test):** NDJSON, one object per line:
  - `{"type":"system","subtype":"init", session_id, model, tools, mcp_servers, permissionMode, memory_paths, ...}` — session start; capture `session_id`.
  - `{"type":"rate_limit_event","rate_limit_info":{status, rateLimitType, overageStatus, ...}}`.
  - `{"type":"assistant","message":{role, content:[{type:"text",text}|{type:"tool_use",id,name,input}], stop_reason, usage}}` — **full message** (content blocks), not deltas.
  - `{"type":"user","message":{content:[{type:"tool_result",tool_use_id,content}]}}` — tool results.
  - `{"type":"result","subtype":"success"|"error", is_error, result, stop_reason, total_cost_usd, usage, modelUsage, permission_denials, terminal_reason}` — **TURN DONE** signal + cost.
  - **Correction:** the docs/SDK describe `stream_event`/`message_stop` deltas — those only appear with `--include-partial-messages`. Default mode emits full `assistant` messages + a final `result`. Use **`type:"result"`** (not `message_stop`) as the done signal.
  - **Cost note:** loading the operator's global CLAUDE.md/memory/skills inflated a trivial turn to ~$0.20 (17.7k cache-creation tokens, Opus). Worker agents must launch lean (minimal context, explicit `--append-system-prompt` role, cheaper models).
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
- ✅ **Codex v2 protocol pinned** (0.142.1, via `codex app-server generate-json-schema`): framing = newline-delimited JSON-RPC; client requests `initialize`, `thread/start`, `turn/start`, `turn/steer`, `turn/interrupt`; notifications `item/completed`, `turn/completed`, `item/agentMessage/delta`, `error`; approvals `item/commandExecution/requestApproval` + `item/fileChange/requestApproval` + `item/permissions/requestApproval` (v2 decision `accept`/`acceptForSession`/`decline`; v1 `execCommandApproval`/`applyPatchApproval` → `approved`/`denied`). `thread/start` returns `{thread:{id}}`; `turn/start` returns `{turn:{id}}`; user input = `[{type:"text",text}]`. **initialize + thread/start handshake verified live** (free, no model turn). MCP via `-c mcp_servers.<name>.url/http_headers`. Implemented in `internal/adapter/codex`.
- ✅ **Full Codex turn verified live**: a real Codex agent received a bus message and called `post_task` over MCP, creating a task on the board (needed `mcp_servers.<id>.default_tools_approval_mode="approve"` — otherwise `approvalPolicy:never` force-denies MCP tool calls with "user rejected MCP tool call").
## Copilot — via ACP (generic engine)

GitHub Copilot CLI (`copilot` 1.0.x) speaks **ACP (Agent Client Protocol)** — Zed's open
JSON-RPC-2.0-over-NDJSON protocol — so it's driven by a *generic* engine
(`internal/adapter/acp`) that any ACP agent can reuse via a small `Profile`; `copilot.New()`
is the concrete family. **Verified live end-to-end** (initialize → session/new → session/prompt
→ session/update text → turn_done; one real `copilot --acp --stdio` turn returned "OK").

- **Invocation:** `copilot --acp --stdio` (also `--port` for TCP). Framing is JSON-RPC 2.0 / NDJSON.
- **Handshake:** `initialize{protocolVersion:1, clientCapabilities}` → `{protocolVersion,
  agentCapabilities{loadSession, mcpCapabilities{http:true,sse:true}, promptCapabilities}, authMethods}`.
  **Requires `copilot login`** — unauthenticated `session/new` stalls.
- **Drive:** `session/new{cwd, mcpServers}` → `{sessionId}`; `session/prompt{sessionId,
  prompt:[{type:"text",text}]}` → `{stopReason}` (the turn-done signal; `end_turn|cancelled|
  max_tokens|max_turn_requests|refusal`). One prompt/turn at a time.
- **Streaming:** `session/update` notifications: `agent_message_chunk` (text, accumulated &
  flushed), `agent_thought_chunk` (reasoning), `tool_call` (→ tool_use), `tool_call_update`
  (status → tool_result), `plan`, `available_commands_update`.
- **Gating (real):** agent→client `session/request_permission{toolCall{kind}, options[{optionId,
  kind: allow_once|allow_always|reject_once|reject_always}]}` → we map the §7 `gating.Decider`
  to an option id (Allow→allow_once, AllowForSession→allow_always, Deny→reject_once). So
  Copilot is `Enforcement=hooked`, not advisory.
- **Bus:** `mcpServers` in `session/new` accepts HTTP servers with header lists — the avairy
  bus joins as `{type:"http", name:"avairy", url, headers:[{name:"X-Avairy-Agent",value:id}]}`.
- **Interrupt:** `session/cancel` (notification) → in-flight prompt returns `stopReason:cancelled`.
- **Caveats:** persona/model aren't `session/new` fields (use `--agent`/instructions / the
  models offered in the `session/new` response); the plain `copilot -p` mode is text-only with
  no runtime-routed gating — **ACP is the integration surface**, not `-p`.
- **Bonus:** because the engine is generic, other ACP agents (Gemini, `claude-agent-acp`) are a
  new `Profile` away — but Claude Code & Codex stay on their richer native adapters.
