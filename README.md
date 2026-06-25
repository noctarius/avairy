# avairy

Orchestration for AI coding agents. Run multiple agents — same family or across families
(**Claude Code**, **Codex**, **Copilot**, **Grok**), local or on **remote machines/VMs** — collaborating over one
shared bus on the same project: messaging each other, working a capability-gated task board,
sharing a synced workspace, with a human able to steer at any moment.

Two use cases it's built for:
1. **Distributed apps** — agents each own a slice (backend / iOS / a service) across machines.
2. **Cross-OS work on one codebase** — multi-platform apps or OS-specific bugs, where you need
   an agent actually *on* each OS instead of ferrying context between a Mac and a Linux VM.

See [DESIGN.md](DESIGN.md) for the architecture and [ADAPTERS.md](ADAPTERS.md) for how each
agent family is driven.

## Requirements

- Go 1.26+
- For live agents: the relevant CLI installed and logged in — `claude`, `codex`, `copilot`
  (`copilot login`), and/or `grok` (xAI auth). avairy uses each CLI's own auth; it never
  handles your credentials.

## Build & test

```sh
go build ./...
go test ./...
```

Produces two commands: `avairy` (core + TUI) and `avairy-node` (the node daemon).

## Run it — single machine

**Mock agents (zero credits)** — the fastest way to see the loop:

```sh
go run ./cmd/avairy -demo
```

A TUI opens with two mock agents, `alice` and `bob`. (Without `-demo`, avairy starts **no
local agents** — you bring them via `avairy-node` or `-live`.)

- Type `@alice <message>` to address an agent; a bare line broadcasts to everyone.
- **Enter** sends; **Shift+Enter** (Kitty-protocol terminals) / **Option·Alt+Enter** / **Ctrl+J** insert a newline.
- `tab` cycles **Conversation / Handovers / Tasks / Approvals**; **Esc** stops running agents; **Ctrl+C twice** quits.
- On the **Approvals** tab, `↑/↓` (or `j/k`) selects a pending gated action and **`y`** allows / **`n`** denies it; the tab shows a `(N)` badge while any are waiting.
- The fleet line shows each agent's status and running cost.

**A real Claude agent on the bus:**

```sh
go run ./cmd/avairy -live                       # alice = real Claude Code
go run ./cmd/avairy -live -family codex          # alice = real Codex
go run ./cmd/avairy -live -model sonnet          # pick the model (default: haiku, cheapest)
```

Live agents launch lean (temp workspace, cheap model) to keep cost down. A one-shot,
non-interactive run that prints the event log and exits:

```sh
go run ./cmd/avairy -live -headless "create a task titled ping that requires os=linux"
```

Everything that happens is appended to an event-sourced journal at `.avairy/journal.jsonl`.

## Run it — across machines

On the **operator machine**, start core with the node control API bound to a reachable
interface, and advertise the host/IP remote nodes should dial:

```sh
avairy -control-addr 0.0.0.0:7700 -mcp-addr 0.0.0.0:7701 -advertise <operator-ip> -workspace ./myproject
```

`-workspace ./myproject` seeds the **canonical workspace** from your project and keeps it
synced both ways — so each remote node receives a working copy on its first sync, and node
edits flow back to your directory. (Without it the hub starts empty and nodes get nothing
until some node pushes content.)

The TUI header shows the **control URL**, the **MCP bus base**, and an **enroll token**. The
token **auto-regenerates each time a node uses it**, so each new node gets its own; the token
a node first enrolls with is **bound to that node** and stays valid for it — so a restarted
daemon (e.g. under systemd) **rejoins with the same `-token`** with no operator action.
(`ctrl+e` rotates the current token manually; omit `-advertise` for local-only, and the TUI
warns when the bus host isn't reachable.)
On each **remote machine/VM**, run the daemon (a single cross-platform binary):

```sh
avairy-node \
  -core    http://<operator>:7700 \       # control URL
  -core-mcp http://<operator>:<busport> \  # MCP bus base
  -token   <enroll-token> \
  -id      linux-box \
  -workspace ./repo \
  -proxy   127.0.0.1:7800 \
  -family  claude                          # optional: spawn & drive the agent here
```

The daemon enrolls (node→core, NAT-friendly), continuously syncs `./repo` to/from the
canonical workspace on core, heartbeats, and serves a local MCP endpoint at
`http://127.0.0.1:7800/mcp` — the agent only ever sees localhost. (The channel is plain HTTP
today; TLS is the production flip.)

With **`-family claude`** (or `codex`, `copilot`, `grok`) the daemon **spawns and drives the agent for you**:
core registers it on the bus at enrollment, inbound messages are pulled from core and fed to
the agent, and its activity is shipped back to the core journal/TUI. **Omit `-family`** to run
proxy-only and launch the agent yourself against `http://127.0.0.1:7800/mcp`. Use `-model` /
`-role` to tune the spawned agent.

## What's inside

- **Bus + task board** — agents `send_message`, `post_task`, `claim_task` over MCP; claims are
  gated by node capabilities (e.g. `os=linux`).
- **File-sync hub** — a canonical workspace on core; nodes sync diffs, with cross-OS
  normalization (LF, mode bits) and conflicts surfaced for agent reconciliation.
- **Facilitator** — watches for stuck agents and nudges (e.g. "the Linux agent is better
  positioned to reproduce this") — it reminds, never commands.
- **Gating** — risky actions (destructive commands, git pushes, installs) are gated; safe
  actions run freely. Gated actions block and surface in the operator's **Approvals** tab for
  allow/deny (Claude via its PreToolUse hook, Codex via app-server approvals); unanswered
  requests fail closed.
- **TUI** — fleet/progress, conversation, a handover timeline, the task board, and your
  command line.

## Status

Working end-to-end: **four agent families** verified live on the bus — Claude Code and Codex
on native adapters, **Copilot and Grok via a generic ACP engine** (a new ACP agent is just a
small profile) — plus single-machine and distributed paths, file sync, facilitator, and
**human-in-the-loop gating** (Claude PreToolUse hook + Codex app-server approvals → operator
Approvals tab). Known follow-ups: routing local-Claude gating through the hook too, conflict
auto-merge routing, typed state-resume from the journal, fs-watch (currently poll), and
channel TLS.
