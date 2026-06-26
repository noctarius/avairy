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

Produces three commands: `avairy` (core + TUI), `avairy-node` (the node daemon), and
`avairy-tui` (the operator console as a standalone client that attaches to a remote core, #18).

## Run it — single machine

**Mock agents (zero credits)** — the fastest way to see the loop:

```sh
go run ./cmd/avairy -demo
```

A TUI opens with two mock agents, `alice` and `bob`. (Without `-demo`, avairy starts **no
local agents** — you bring them via `avairy-node` or `-live`.)

- Type `@alice <message>` to address an agent; a bare line broadcasts to everyone.
- **Enter** sends; **Shift+Enter** (Kitty-protocol terminals) / **Option·Alt+Enter** / **Ctrl+J** insert a newline.
- `tab` cycles **Conversation / Handovers / Tasks / Approvals / Conflicts**; **Esc** stops running agents; **Ctrl+C twice** quits.
- On the **Approvals** tab, `↑/↓` (or `j/k`) selects a pending gated action; **`y`** allows it once, **`a`** allows that kind from that agent for the rest of the session, **`n`** denies. The tab shows a `(N)` badge while any are waiting.
- On the **Conflicts** tab, owner-less conflicts (your seed workspace diverging from a node's edit) appear with a `(N)` badge; `↑/↓` selects, **`m`** takes it yourself (the file already has git-style markers — fix it in your editor and the next sync picks it up), **`d`** delegates it to the selected recipient agent (`ctrl+t` picks who).
- The fleet line shows each agent's status (working / idle / blocked / offline) and running cost.
- Type `/commit <message>` to sign a commit of the canonical repo yourself (when core has one).

**A real Claude agent on the bus:**

```sh
go run ./cmd/avairy -live                       # alice = real Claude Code
go run ./cmd/avairy -live -family codex          # alice = real Codex
go run ./cmd/avairy -live -model sonnet          # pick the model (default: haiku, cheapest)
```

Live agents launch lean (temp workspace, cheap model) to keep cost down. A one-shot,
non-interactive run that prints the event log and exits:

```sh
go run ./cmd/avairy -live -send "create a task titled ping that requires os=linux"
```

`-headless` is a different thing: run core **without the TUI** (serve the bus/control and block),
for a host that nodes connect to with no local operator console. Attach to it later from a remote
TUI or the browser — see [Remote operator console](#remote-operator-console-tui-or-browser).

Add **`-gate-edits`** to also require operator approval for file edits (not just risky commands);
combined with **`a`** (allow-for-session) in the Approvals tab you approve an agent's edits once
and the rest auto-allow.

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
`http://127.0.0.1:7800/mcp` — the agent only ever sees localhost. For remote nodes, serve the
channels over TLS: start core with `-tls-auto` (self-signed CA, written into the join bundle so
the node trusts it automatically) or `-tls-cert`/`-tls-key`. With `-tls-auto` both the control
channel and the remote-facing MCP bus are encrypted, and a node can authenticate by mTLS client
cert (`avairy mint-join`) instead of a token. Local agents always use a plain loopback bus.

With **`-family claude`** (or `codex`, `copilot`, `grok`) the daemon **spawns and drives the agent for you**:
core registers it on the bus at enrollment, inbound messages are pulled from core and fed to
the agent, and its activity is shipped back to the core journal/TUI. **Omit `-family`** to run
proxy-only and launch the agent yourself against `http://127.0.0.1:7800/mcp`. Use `-model` /
`-role` to tune the spawned agent, and `-gate-edits` to gate its file edits like the core flag.

A node can also join with a single **join string** instead of separate flags — core writes one to
`.avairy/join` (and refreshes it as the token rotates); it bundles the core URL, the CA to trust,
and the token/cert: `avairy-node -join-file path/to/join -id linux-box`. `avairy mint-join` issues
an mTLS client-cert join (no token).

## Remote operator console (TUI or browser)

The operator console isn't tied to the core machine. When core serves the control API
(`-control-addr`, with or without `-headless`), it also serves an **operator API** on the same
listener (sharing its TLS), and you can attach from anywhere two ways. Core mints an **operator
token** (`-operator-token <tok>`, else a random one) and writes an `.avairy/operator-join` bundle
(core URL + CA + token); both the attached TUI's control line and the headless startup output show
the token, the join file, and the ready-to-open **web URL**.

**Standalone TUI** — the same interface as the local one, attached over the network:

```sh
avairy-tui -join-file .avairy/operator-join          # one argument: URL + CA + token bundled
avairy-tui -core https://core:7700 -token <op-token> -ca core-ca.pem
avairy-tui -core http://localhost:7700 -token <op-token>   # plain HTTP (dev)
```

**Browser** — open the URL core prints (chat-first console mirroring the TUI):

```
http://<control-addr>/operator/ui?token=<operator-token>
```

The browser console shows the conversation, fleet, tasks, approvals, and conflicts, and lets you
message agents (broadcast or a specific one), interrupt, allow/deny approvals, resolve/delegate
conflicts, and `/commit` — the same actions as the TUI, over the same API. Both clients consume a
live SSE journal stream; auth is the operator token. Single operator for now.

## What's inside

- **Bus + task board** — agents `send_message`, `post_task`, `claim_task` over MCP; claims are
  gated by node capabilities (e.g. `os=linux`).
- **Blackboard** — durable shared memory: agents (and the operator) `note(key, text)` /
  `read_notes(prefix?)` to seed context, decisions, and findings that survive restarts and feed
  `fresh_look` second opinions.
- **File-sync hub** — a canonical workspace on core; nodes sync diffs, with cross-OS
  normalization (LF, mode bits) and conflicts surfaced for reconciliation: an agent's lost push is
  routed to that agent, while owner-less ones (your seed workspace vs. a node's edit) go to the
  operator's **Conflicts** tab to resolve or delegate.
- **Facilitator** — watches for stuck agents and nudges (e.g. "the Linux agent is better
  positioned to reproduce this") — it reminds, never commands.
- **Gating** — risky actions (destructive commands, git pushes, installs, optionally edits) are
  gated; safe actions run freely. Gated actions block and surface in the operator's **Approvals**
  tab for allow/deny (Claude via its PreToolUse hook, Codex via app-server approvals, Copilot/Grok
  via ACP); unanswered requests fail closed.
- **Operator console** — fleet/progress, conversation, handover timeline, task board, approvals,
  and conflicts, plus your command line — as a local TUI, a remote TUI (`avairy-tui`), or a
  browser console, all over one operator API.

## Status

Working end-to-end: **four agent families** verified live on the bus — Claude Code and Codex
on native adapters, **Copilot and Grok via a generic ACP engine** (a new ACP agent is just a
small profile) — plus single-machine and distributed paths, fsnotify-driven file sync (deletes,
moves, symlinks, empty-dir pruning, content-hash change detection), agent- and operator-reconciled
conflicts, the **blackboard** + `fresh_look`, facilitator (with loop detection), **human-in-the-loop
gating** across all families (PreToolUse hook / app-server / ACP approvals → operator Approvals tab,
with allow-for-session and optional per-edit gating), **git** (history reads, gated signed commits,
on-node read-only mirror + scratch worktrees for cross-OS bisect/build), **TLS** on both the control
channel and the MCP bus (self-managed CA + mTLS), journal **state-resume**, and a **remote operator
console** — standalone TUI (`avairy-tui`) and a browser UI over one operator API. See
[STATUS.md](STATUS.md) for the full picture and remaining work (semantic loop detection, live
PreToolUse hook validation).
