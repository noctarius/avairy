# avairy — Design

> An open-source orchestration tool for AI coding agents that lets multiple agents —
> same family or across families (Claude Code, Codex, Gemini, …), local or **remote on
> other machines** — collaborate on real engineering work across OSes, languages, and
> services in complex distributed applications.

## 1. What it is (and isn't)

avairy is **collaborative engineering across machine/OS/environment boundaries**, not a
chat room. The unifying pain it removes: work that requires *being on a specific
machine/OS/environment*, where today you shuttle a single agent session back and forth by
hand. Two primary use cases:

1. **Distributed application** — agents each own a *slice* of a multi-service system across
   machines (a Claude on Linux owns the backend, a Codex on macOS owns the iOS client,
   something on Windows owns a service).
2. **Cross-OS collaboration on a single codebase** — multi-platform apps, cross-platform
   builds, OS-specific bugs: work where behavior differs by OS and you need an agent
   actually *on* each OS. Instead of ferrying context between, say, a macOS host and a
   Linux VM, an agent per OS-node collaborates on the *same* tree. (Examples: a defect that
   reproduces on Linux but not macOS; a feature that must be built and tested on each
   platform. A VM/emulator is just a node reached via SSH — even on the same physical host.
   In this mode agents touch the *same files* often, so the agent-reconciled conflict flow
   and continuous sync are load-bearing.)

In both, agents actively message and consult each other, share code/artifacts, and a
lightweight facilitator keeps them unstuck. A human can steer at any moment.

Reference project: [agentpipe](https://github.com/kevinelliott/agentpipe) — borrow its
adapter registry, middleware pipeline, and TUI patterns. Key differences: agentpipe is
local-only, conversation-oriented, and agents are passive (prompt-injection). avairy adds
remote machines, active inter-agent messaging, engineering-task orientation, file sync,
and human-in-the-loop steering.

**Tech:** Go 1.26, Bubble Tea (Charmbracelet) for the TUI.

## 2. Topology

Peer workers that message each other over a shared bus, with a facilitator and the human
sitting *above* the bus and dropping in occasional steering. Same delivery machinery for
all three intervention sources (peer message / facilitator nudge / human injection).

```
   [worker]◄──MCP bus──►[worker]◄──MCP bus──►[worker]
       ▲          (peers talk directly)           ▲
       └──────── facilitator nudges (on trigger) ─┘
                         ▲
                  [human injection]
```

## 3. Components

```
┌─ operator machine ───────────────────────────────┐
│  avairy core                                     │
│   ├─ TUI (Bubble Tea)  ── human command line     │
│   ├─ Coordinator   (facilitator policy + human   │
│   │                 injection router + gating)   │
│   ├─ Bus           (addressing, priority,        │
│   │                 interrupt/steer delivery)    │
│   ├─ MCP server    (message/task/artifact tools) │
│   ├─ Task board / blackboard  (shared state)     │
│   ├─ File sync hub (canonical workspace + vvecs) │
│   ├─ State store   (persist + resume)            │
│   └─ Node manager  (transports)                  │
└──────────────────────┬───────────────────────────┘
    bootstrap: SSH | manual enrollment
    → node→core channel (TLS, gRPC/ws)
                       │
┌─ remote node (linux/macos/windows) ──────────────┐
│  avairy-node (daemon)                            │
│   ├─ Agent supervisor (spawn/monitor)            │
│   ├─ Adapter runtime  (claude / codex / gemini)  │
│   ├─ local MCP proxy  → tunnels to core bus      │
│   ├─ workspace sync   (fs-watch ↔ hub)           │
│   └─ heartbeat / health                          │
└──────────────────────────────────────────────────┘
```

### Core concepts
- **Node** — a machine *or VM/emulator* (incl. a QEMU VM on the same physical host) hosting
  agents; advertises a capability registry (OS, arch, emulator/toolchain, languages,
  services, repos) used for matchmaking — notably "who can reproduce this here?".
- **Agent** — an agent-family instance on a Node, bound to a workspace, with a persistent
  **role**.
- **Adapter** — per-family driver normalizing: start / send / stream / detect-done /
  handle-tool-prompt / **interrupt** / capabilities.
- **Transport / bootstrap** — local | SSH-bootstrap | manual enrollment → persistent node
  daemon (single cross-platform Go binary). Channel is always node→core outbound.
- **Bus / Coordinator / Blackboard / State store / File-sync hub** — see below.

### TUI views (Bubble Tea)
Single operator in v1. Core views:
- **Fleet / live progress** — every agent at a glance: node, current task, status
  (`working / idle / blocked / awaiting-approval`), and live activity (current step /
  streaming tool output). The whole fleet's state in one place, not one-agent-at-a-time.
- **Nodes & agents** — tree of nodes (OS/capabilities, health, enforcement level) and the
  agents on each.
- **Handover timeline** — first-class, chronological view of work changing hands: task
  claims/reassignments, `suggest_consult`, repro/build handoffs, conflict routing, and
  fresh-eyes ephemeral-session spawns. Rendered from the event-sourced log (§10) so it's a
  complete, replayable trace of *who picked up what, from whom, and why*.
- **Conversation** — unified, filterable multi-agent transcript (per-agent or merged);
  the human command line lives here (addressing + interrupt/steer indicator, §6).
- **Task board** — tasks with state, `requires`, deps, claimant (§4).
- **Approvals** — queue of gated actions awaiting human/facilitator decision (§7).
- **Conflicts** — pending file conflicts routed for agent/human reconciliation (§9).
- **Cost footer** — per-agent tokens & cost (§10).

## 4. Inter-agent communication — MCP shared bus

avairy core runs an **MCP server**; every agent connects to a **local MCP endpoint on its
own node**, which the daemon tunnels back to the core bus. Network topology is invisible
to the agent — it just sees "a localhost MCP server."

Agent-facing tools:
- `send_message(to, body)` / `read_inbox()` — direct, broadcast, or role-addressed.
- `post_task` / `claim_task` — shared work board.
- `share_artifact(path | blob)` — one-off file/blob sharing.
- `report_status` / `report_blocked` — feed the facilitator clean stuck-signals.

**Fallback:** prompt-injection (agentpipe-style transcript feeding) for agents without MCP.

**Task model (rich).** A task carries `id`, `title`, lifecycle `state`
(open/claimed/in-progress/blocked/done/failed), a `requires` capability set (e.g.
`{os: linux, arch: arm64, qemu: true}`) used to gate claims and for matchmaking, and
optional `deps`. The human seeds the top-level goal; agents decompose via `post_task`.
`claim_task` is **atomic** (no double-claim) and succeeds only if the claiming node meets
`requires`.

**Identity & addressing.** Each agent has a stable `agent_id`; role is a *non-unique* label
(two Claudes are distinct ids). Messages target an `agent_id`, a role (fan-out), or
broadcast. The bus stamps sender identity from the authenticated channel — an agent
**cannot post as another** (no spoofing).

## 5. Coordination — peer workers + minimal facilitator

Workers do the real work and decompose peer-to-peer. The **facilitator** is a privileged
role that only *nudges* — it reminds, never commands; workers may ignore it. It is
**trigger-invoked and stateless** (not a persistent watcher): the coordinator does cheap
deterministic stuck-detection, then wakes the facilitator to decide what (if anything) to
say.

**Stuck-detection triggers (coordinator):** repeated repro/test failures on the same
issue; loop detection (near-duplicate messages/tool calls); a task with no board progress
for N turns; agent self-declared `blocked`/low-confidence; an unresolved two-agent
disagreement for N turns.

**Facilitator actions (just bus messages):**
- `nudge(agent, hint)` — e.g. "agentB on Windows may reproduce this faster; consider
  handing off the repro," or "spin up an ephemeral session for a clean look."
- `suggest_consult(agentA, agentB, topic)` — get a peer's opinion.
- `escalate_to_human(question)` — surface a decision to the operator.

**Agent activation (run-loop).** Agents don't poll. The daemon keeps each agent in a
managed loop and parks it **idle (zero tokens)** when it has nothing to do. Inbound bus
events — a direct message, a claimable task matching the node's capabilities, a facilitator
nudge, or human injection — **wake** the agent by injecting a prompt via the same
interrupt/steer machinery as §6. Peer messaging, facilitator nudges, and human injection
are thus *one* delivery mechanism.

**Facilitator runtime.** A core-resident agent (model configurable), spun up only on a
coordinator trigger — not a persistent watcher.

## 6. Human injection

The human is a **first-class bus participant**. The TUI command line can address the
facilitator, a specific worker, or broadcast — at any time. Two delivery modes:

| Mode          | Behavior                                                       | Support                                                                  |
|---------------|----------------------------------------------------------------|--------------------------------------------------------------------------|
| **interrupt** | cancel current generation, inject, resume — true mid-reasoning | adapters with `supports_interrupt` (Claude **Agent SDK**, Codex **app-server** — *not* the bare `claude -p` / `codex exec` modes; see ADAPTERS.md) |
| **steer**     | queue, deliver at next turn/tool boundary                      | any agent                                                                |

The bus carries `delivery: interrupt | steer`; the TUI **shows which will happen before
you send**.

**Interrupt vs. a running tool.** Interrupt cancels in-flight *generation* cleanly. A
running child process (e.g. a build) is by default **let finish** (output keeps streaming)
and the message is delivered immediately after; a hard-kill variant is available per-action
where the adapter supports it.

## 7. Autonomy & gating

**Policy (what's gated)** — gate risky actions only:
- **Free + logged:** read, build, test, local edits, git history reads / bisect in a
  scratch worktree (§9).
- **Gated (need approval):** destructive commands (`rm -rf`, …), git history mutations
  (commit/tag/push — core-only & signed, see §9), actions affecting another node, package
  installs / `sudo`.

**Enforcement (how) — pluggable, native hooks first.** Policy and mechanism are decoupled
behind an `EnforcementBackend` interface, so stronger enforcement can be added later
*without touching policy or callers*:

- **v1 — native permission hooks.** Use each agent's own permission/hook mechanism (e.g.
  Claude Code hooks & permission prompts) to route gated actions to the coordinator.
  Flow: agent attempts a gated action → its permission hook fires → routed to avairy →
  human/facilitator approves or denies → agent proceeds or aborts.
  - *Caveat:* strength varies by family. Where an agent exposes no usable hook, that adapter
    declares a weaker level — down to **advisory** (allow + log + stream for the human to
    watch).
- **Future (out of scope now) — OS-layer sandbox + brokers.** Confine-by-default (Linux
  namespaces + seccomp-notify + Landlock; macOS Seatbelt; Windows AppContainer) so gated
  actions become structurally impossible, with brokers (git/network proxies) for
  allowed-but-gated ops. Drops in as another `EnforcementBackend` implementation. **Design
  for this now:** keep the policy spec backend-agnostic, express the workspace/network/
  device boundary declaratively, and never assume the agent's cooperation in the policy
  layer.

Each adapter/node **declares its enforcement level** (`sandboxed` | `hooked` | `advisory`),
surfaced in the TUI so the operator always sees how strongly a given agent is contained.
**Approval authority:** human always; the facilitator only for a configurable low-risk
subset.

## 8. Agent lifecycle — persistent role, chooseable session

- **Role = persistent, always** — system prompt / persona / capabilities; the agent's
  stable identity on the bus.
- **Session = chooseable per request:**
  - **persistent project session** — long-lived, accumulates context, periodically
    compacted. Default working mode.
  - **ephemeral session** — same role, clean context curated from the blackboard, *no*
    conversation history. A deliberate fresh look, unanchored by prior reasoning; the
    facilitator's primary tool against anchoring and loops.

The blackboard/task board is the durable shared memory feeding both.

## 9. Code & artifact sharing — avairy-mediated file sync

No git-as-bus; avairy owns propagation **and** conflict resolution.

- **Topology = hub:** core holds the canonical workspace + per-file version vectors; nodes
  sync diffs up/down over the daemon channel; core fans out. O(N), one conflict point.
- **Trigger = continuous on declared paths + ignores:** the daemon fs-watches a declared
  path set per workspace and auto-syncs, with gitignore-style excludes (`.git`, `build/`,
  `node_modules`, binaries). Plus explicit `share_artifact(path)` for one-off blobs.
- **Conflicts = agent-reconciled:** on divergent concurrent edits, avairy flags it and
  hands both versions to an agent to merge (ties to `suggest_consult`); escalates to the
  human if the agent can't.
- **Git: one canonical repo on core.** The canonical workspace IS the single git
  repository. Remote nodes hold *synced working trees, not clones* — `.git` is excluded
  from the live working-tree sync so writes stay single-sourced. Read and write are
  decoupled:
  - **History reads — available to any node agent** (independent of commit rights; for
    root-cause analysis). Two tiers: (a) lightweight queries (`log`/`blame`/`show`/`diff
    <commits>`) proxied to core's repo via an MCP `git_history(...)` tool; (b) bisect /
    historical checkout-and-build that must run *on a node's OS* — the daemon provisions a
    **disposable scratch worktree** (read-only history mirror + throwaway checkout)
    isolated from the synced canonical tree, so the agent can `bisect` → build → reproduce
    without disturbing live sync.
  - **History writes — core-only & signed.** All history-mutating ops (commit, tag, push)
    run **only at core**, because commits must be **signed** and signing keys live on the
    operator's machine and must never ship to nodes. A node agent requests a commit via MCP
    (`request_commit(paths, message)`) — gated (human/facilitator approval), executed
    core-side with signing — or the human commits via the TUI.
- **Cross-OS normalization & bootstrap (defaults).** Text files normalized to LF in transit
  (restored per-node on checkout); mode/executable bits preserved; symlinks synced as links
  where supported; case-insensitive (macOS/Windows) collisions are **flagged, not silently
  merged**. Sync is **debounced (settle window)** and writes **atomically** (temp + rename)
  so half-written files never ship. Large binaries (kernel images, ISOs) are excluded by
  default and moved only via explicit, chunked `share_artifact`. A node's initial tree is a
  full hub→node sync at attach time.

```
        ┌───────────── avairy core ──────────────┐
        │  canonical workspace + version vectors │
        └───┬───────────────┬───────────────┬────┘
      diff up/down    diff up/down    diff up/down
            │               │               │
       [node:linux]    [node:macos]   [node:windows]
       working copy    working copy    working copy
```

## 10. Cross-cutting decisions

- **Per-node credentials — use each node's existing local login.** avairy never handles
  secrets: each agent CLI authenticates the way it already does on that machine
  (subscription login or local env/API key); avairy just spawns the agent and inherits
  that auth. No secrets stored in avairy, none on the wire.
- **Persistence & resume — event-sourced.** Append-only log of bus events / tasks /
  messages + periodic snapshots; the blackboard is the materialized (replayable) view.
  Gives full resume after a core restart and a complete audit trail of the collaboration.
  Lives in the State store on core.
- **Cost/metrics — TUI-only.** Per-agent token & cost tracking surfaced live in the TUI;
  no Prometheus/exporter for now (can add later if wanted).
- **Node trust, enrollment & channel security.** Two bootstrap paths, both yielding the
  same per-node TLS channel credential; the persistent channel is **node→core outbound**
  (NAT/firewall-friendly — important for Windows and locked-down hosts):
  - **SSH bootstrap** (Unix-y hosts): SSH access is the trust root — core installs/launches
    the daemon and seeds its enrollment credential over the SSH session.
  - **Manual provisioning** (Windows, no-SSH, locked-down): the operator installs the
    `avairy-node` binary themselves (download / MSI / OS service) and starts it with a
    **one-time, single-use enrollment token** generated in the TUI; the daemon dials core
    and enrolls.
  No separate CA in v1 — mTLS can be added later. The daemon is a single cross-platform Go
  binary, so Windows is just another build target.

## 11. Build order

1. **Adapter contract** (highest risk) — define and *validate against reality*:
   `Start / Send / Stream / DetectDone / HandleToolPrompt / Interrupt / Capabilities`.
   Verify Claude Code stream-json and Codex exec actually deliver interrupt + the
   stuck-signals we assume. Design downstream of what they really do.
2. **MVP, single machine:** core + bus + MCP server + 2 local peer agents + TUI with human
   injection + blackboard. Get the collaboration loop working locally.
3. **File-sync hub** (local multi-workspace first).
4. **Remote transport:** node daemon + node→core channel; bootstrap via SSH **and** manual
   enrollment (token); MCP tunnel; workspace sync.
5. **Facilitator** triggers + nudges.
6. Hardening: gating UI, persistence/resume, conflict reconciliation flow, metrics.