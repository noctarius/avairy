# avairy — Architecture

This document describes avairy **as built**: its processes, subsystems, data flows, and the
decisions that hold them together. It is the map you want when changing the code or reasoning about
behaviour.

- For the **why** and the original product vision, see [DESIGN.md](DESIGN.md) (an intent spec — some
  of its names predate the implementation, e.g. it still says `avairy-node` and "no CA in v1").
- For **using** avairy (quick start, command reference, security walkthrough), see
  [README.md](README.md).
- For **building/releasing**, see [BUILD.md](BUILD.md). For **adapter specifics**,
  [ADAPTERS.md](ADAPTERS.md). For **status/backlog**, [STATUS.md](STATUS.md).

---

## 1. What avairy is

avairy orchestrates **multiple AI coding agents** — same family or mixed (Claude Code, Codex,
Copilot, Grok), **local or on remote machines/VMs** — collaborating on real engineering work across
OSes, languages, and services. Agents message each other over a shared bus, share a synced
workspace, claim tasks off a board, and a lightweight facilitator keeps them unstuck. A human
operator steers at any moment from a terminal UI or a browser.

The two load-bearing use cases: **distributed applications** (an agent per service/machine) and
**cross-OS work on one codebase** (an agent actually *on* each OS, e.g. a bug that only repros on
Linux). A VM/emulator reached over the network is just another node.

**Stack:** Go 1.26, pure-Go (`CGO_ENABLED=0`), single statically-linked binary that cross-compiles
to darwin/windows/linux/freebsd × arm64/amd64. TUI is Bubble Tea (Charmbracelet). Agent comms ride
**MCP** (Model Context Protocol). avairy **never handles model credentials** — each agent CLI
authenticates the way it already does on its machine.

---

## 2. The shape of the system

avairy runs as **one binary, `avairy`**, in one of a few roles selected by subcommand. There is a
**core** (one per session, on the operator's machine) and zero or more **nodes** (one per remote
agent host). The operator console can run in-process with the core or attach remotely.

```
┌─ operator machine ────────────────────────────────────────────────┐
│  avairy core run            (or: core serve  — headless)          │
│                                                                   │
│   ┌─ Presentation ─────────────────────────────────────────────┐  │
│   │  TUI (Bubble Tea)  +  Operator HTTP API  +  web console    │  │
│   └──────────────────────────┬─────────────────────────────────┘  │
│   ┌─ Coordination & state ───┴─────────────────────────────────┐  │
│   │  Bus · MCP server · Board · Blackboard · Facilitator ·     │  │
│   │  Dispatch · Cost monitor · Approvals · Conflicts · Journal │  │
│   └──────────────────────────┬─────────────────────────────────┘  │
│   ┌─ Edges ──────────────────┴─────────────────────────────────┐  │
│   │  Control plane (HTTP) · MCP bus listener · File-sync hub · │  │
│   │  Git repo (canonical, signed) · CA (mTLS)                  │  │
│   └───────────────────────────┬────────────────────────────────┘  │
└───────────────────────────────┼───────────────────────────────────┘
                  node→core outbound (TLS), NAT/firewall-friendly
                                │
        ┌───────────────────────┼────────────────────────┐
        │                       │                        │
┌─ node:linux ────────┐ ┌─ node:macos ───────┐ ┌─ node:windows ──────┐
│ avairy node join    │ │ avairy node join   │ │ avairy node join    │
│  ├ agent supervisor │ │  ├ agent (codex)   │ │  ├ agent (claude)   │
│  │   (claude)       │ │  │                 │ │  │                  │
│  ├ MCP reverse-proxy│ │  ├ MCP proxy + gate│ │  ├ MCP proxy + gate │
│  │   + /gate hook   │ │  ├ workspace sync  │ │  ├ workspace sync   │
│  ├ workspace sync   │ │  ├ git mirror (ro) │ │  ├ git mirror (ro)  │
│  └ heartbeat        │ │  └ heartbeat       │ │  └ heartbeat        │
└─────────────────────┘ └────────────────────┘ └─────────────────────┘
```

### Three planes (a useful mental model)

avairy's packages divide cleanly into three planes:

| Plane                   | Question it answers                                 | Key packages                                                        |
|-------------------------|-----------------------------------------------------|---------------------------------------------------------------------|
| **Coordination / data** | "what are the agents saying & doing?"               | `bus`, `mcp`, `board`, `journal`, `facilitator`, `dispatch`, `cost` |
| **Edge / transport**    | "how do remote nodes and agents reach core safely?" | `control`, `workspace`, `git`, `adapter/*`, `gating`                |
| **Presentation**        | "how does the human see and steer it?"              | `operator`, `tui`, `supervisor`, `runner`                           |

The **journal** is the spine that ties them together: nearly everything that happens becomes an
append-only record, and most views (board, cost, TUI, web console) are *materialized projections*
of that log.

### Command tree (`cmd/avairy`, urfave/cli/v3)

| Command                    | Role                  | Purpose                                                                    |
|----------------------------|-----------------------|----------------------------------------------------------------------------|
| `avairy core run`          | core + in-process TUI | the default operator workflow                                              |
| `avairy core serve`        | core, headless        | attach a console later via `tui connect`                                   |
| `avairy core add-node`     | minting               | issue an **mTLS client-cert** join bundle for a node                       |
| `avairy core add-operator` | minting               | issue operator certs (`.p12` for browsers + join bundle for `tui connect`) |
| `avairy node join`         | node daemon           | enroll, run MCP proxy + workspace sync + (optionally) an agent             |
| `avairy tui connect`       | remote console        | attach the operator UI to a remote core over mTLS                          |
| `avairy hook` *(hidden)*   | gating shim           | PreToolUse bridge Claude executes per tool call                            |
| `avairy version`           | —                     | print build metadata (`internal/buildinfo`)                                |

See README/BUILD for the full flag reference. The remainder of this document goes subsystem by
subsystem, then walks the end-to-end flows.

---

## 3. Core concepts & glossary

- **Node** — a machine or VM hosting agents. Identified by a stable `id` that is *also the agent's
  identity on the bus*. Advertises capabilities (`os`, `family`, …) used for matchmaking.
- **Agent** — a running agent-family instance bound to a workspace, with a persistent **role**
  (system prompt/persona). One agent per node in the common case; its bus id == the node id.
- **Adapter** — a per-family driver normalizing start/send/stream/interrupt/capabilities to a common
  contract (`internal/agent.Adapter`/`Session`).
- **Family** — `claude` (Claude Code), `codex`, `copilot`, `grok`. The latter three share gating via
  a `gating.Decider`; copilot/grok ride a generic **ACP** engine.
- **Role vs Session** — *role* is the agent's durable identity; *session* is chooseable per request:
  a long-lived **persistent** session (default) or an **ephemeral** clean-context session (the
  facilitator's "fresh look").
- **Bus address** — every message targets one of: `agent:<id>`, `role:<name>`, `team` (one claims),
  `broadcast`/`all` (everyone), or `facilitator` (the triage loop only).
- **Operator** — the human, a first-class bus participant (`from = "human"`), steering via TUI or web.

---

## 4. Subsystems

### 4.1 Journal — the event-sourced spine (`internal/journal`)

Everything observable is an append-only `Record`:

```go
type Record struct {
    Seq   uint64    // monotonic
    Time  time.Time
    Kind  Kind      // see below
    Actor string    // agent id, "human", "facilitator", or "" (system)
    Data  any       // kind-specific payload (typed in memory, JSON on disk)
}
```

`Kind` is one of `message`, `agent_event`, `task`, `handover`, `approval`, `note`, `system`. The
`Log` interface is tiny: `Append(kind, actor, data) Record`, `Records() []Record`, and
`Subscribe() (<-chan Record, cancel)` for live consumers.

Two implementations:
- **`Memory`** — in-process ring of records with non-blocking fan-out to subscribers (a slow
  subscriber is skipped, never stalls the writer).
- **`File`** — embeds `Memory` and *also* durably appends JSONL to `.avairy/journal.jsonl`
  (`OpenFile`). On startup `ReadFile` returns `[]PersistedRecord` (payload as `json.RawMessage`) to
  replay into the board, blackboard, and cost monitor — this is how avairy **resumes** after a core
  restart and how it gives a complete audit trail.

The journal is *the* integration seam: the bus journals every message; the board journals every
task/claim; agents' streamed events are journaled; the operator API streams the journal to remote
consoles over SSE. Materialized views (board, blackboard, cost, TUI conversation) are pure
projections and can be rebuilt by replay.

### 4.2 Bus — the message router (`internal/bus`)

The bus routes addressed `Message`s between agents, the facilitator, and the human, journaling each
as `KindMessage`. `Publish(from, to, body, delivery)` assigns a sequential id (`m<n>`), dedups, and
fans out to matching subscribers (non-blocking — a full inbox drops rather than blocking the
publisher). `Subscribe(id, roles...)` returns a buffered channel (64) + cancel.

**Addressing & `matches()`.** A subscriber receives a message when `matches()` is true. Key rules:

- The sender never receives its own message.
- The **facilitator** subscription (`agentID == "facilitator"`) receives **only** `facilitator`
  messages — never `team`/`broadcast`/`role`. (This is load-bearing: otherwise the facilitator
  re-dispatches a direct `@team` request as a duplicate.)
- Working agents receive `broadcast`/`team` (everyone), `agent:<their id>`, and `role:<a role they
  hold>` — but never `facilitator`.

**`Waker` (autonomous-wake budget, #25).** Not every message should *wake* an idle agent and spend
tokens. `Waker.Wake(from, kind, interrupt, now)` decides:
- interrupts and human/facilitator messages always wake;
- agent→broadcast/role is **context-only** (delivered, but doesn't trigger a turn);
- agent→direct and agent→team wake within a refilling per-agent budget (so a chatty peer can't spin
  the fleet).

**`AnnotateDelivery(id, kind, body)`.** When a message *wakes* an agent it arrives via the adapter's
`Send` as bare text — it loses the `to:team` marker that `read_inbox` exposes. So for a `team`
request this helper prefixes the body with an explicit instruction to call `claim_response(<id>)`
before acting and stand down if denied. Without it, every agent woken by a team request just starts
working in parallel.

`Interrupt(from, to)` publishes a control signal (`Interrupt: true`, never deduped) that tells
recipients to cancel their current turn.

### 4.3 MCP server — the agent-facing surface (`internal/mcp`)

avairy core runs an **MCP server** (wrapping `mark3labs/mcp-go`) exposed at `/mcp` over HTTP. Every
agent connects to a **localhost MCP endpoint on its own node**; the node daemon reverse-proxies that
to core and stamps the caller's identity (`X-Avairy-Agent`). The agent sees only "a local MCP
server" — network topology is invisible.

**Identity & no-spoofing.** `agentFromContext(ctx)` resolves the caller from the injected header;
`Bus.Publish` is stamped with that id. An agent cannot post as another.

**Agent-facing tools** (registered in `registerTools`; some gated behind `Enable*`):

| Tool                  | Params                         | Effect                                                                                                     |
|-----------------------|--------------------------------|------------------------------------------------------------------------------------------------------------|
| `send_message`        | `to`, `body`, `delivery?`      | publish to bus (`agent:`/`role:`/`team`/`facilitator`/`broadcast`); rejects directed sends matching nobody |
| `read_inbox`          | —                              | drain & return this agent's buffered messages                                                              |
| `list_agents`         | —                              | the *other* peers (id, roles, caps) for discovery                                                          |
| `post_task`           | `title`, `requires?`, `deps?`  | create an open task `t<n>` on the board                                                                    |
| `claim_task`          | `task_id`                      | atomic claim if the node's caps satisfy `requires` (journals a handover)                                   |
| `list_tasks`          | —                              | all tasks with state/requires/claimant                                                                     |
| `claim_response`      | `thread_id`                    | claim sole ownership of a `team` request (5-min TTL); `granted`/`denied`                                   |
| `note` / `read_notes` | `key`,`text` / `prefix?`       | write/read the durable **blackboard**                                                                      |
| `report_status`       | `status`, `detail?`            | feed the facilitator clean stuck-signals                                                                   |
| `git_history`         | `mode`,`ref?`,`path?`,`limit?` | read-only `log`/`show`/`diff`/`blame` on core's repo *(EnableGit)*                                         |
| `request_commit`      | `message`, `paths?`            | **gated** signed commit, executed core-side *(EnableGit)*                                                  |
| `scratch_worktree`    | `action`,`ref?`,`id?`          | manage disposable checkouts for bisect/build *(EnableGit)*                                                 |
| `fresh_look`          | `question`                     | ask an ephemeral, clean-context session for an independent take *(EnableFreshLook)*                        |
| `resolve_conflict`    | `path`, `content`              | submit merged content as the next canonical version *(EnableConflicts)*                                    |
| `list_conflicts`      | —                              | this agent's files with unresolved markers *(EnableConflictList)*                                          |

Each agent is registered with two bus subscriptions: `ch` (drained by `read_inbox`) and `wakeCh`
(drained by the node daemon's wake loop). Keeping them separate means the daemon's wake-filtering
never empties what `read_inbox` would read. `AgentRoles(id, caps)` makes an agent reachable as
`role:<id>`, `role:<os>`, and `role:backend`.

### 4.4 Board & Blackboard (`internal/board`)

Both are journal-backed materialized views (`Restore(records)` replays them on startup).

- **`Board`** — tasks with a state machine (`open → claimed → in_progress → blocked → done/failed`).
  `Post` creates an open task; `Claim(taskID, agentID, caps)` is **atomic** and succeeds only if the
  task is open *and* `caps` satisfy `Requires` (`ErrNotClaimable`/`ErrCapMismatch` otherwise),
  journaling a `handover`. `requires` is a `map[string]string` (e.g. `{"os":"linux"}`); `deps` are
  tracked but not enforced in v1.
- **`Blackboard`** — durable shared memory: `Write(author, key, text)` (latest write per key wins),
  `Read(prefix)` (sorted). This is the curated context an ephemeral "fresh look" session draws from.

### 4.5 Adapters — the agent contract (`internal/agent`, `internal/adapter`)

The contract every family implements:

```go
type Adapter interface {
    Family() Family
    Capabilities() Capabilities
    Start(ctx, SessionConfig) (Session, error)
}
type Session interface {
    ID() string
    Send(ctx, text string, d Delivery) error   // Delivery: steer | interrupt
    Events() <-chan Event
    Interrupt(ctx) error
    Close() error
}
```

`Event` normalizes every family's stream into `turn_start | text | reasoning | tool_use |
tool_result | turn_done | usage | error`, carrying optional `*ToolCall` and `*Usage` (tokens +
`CostUSD`). `Capabilities` advertises `SupportsInterrupt`, `SupportsSteer`, `SupportsResume`,
`MCPClient`, and an **`Enforcement`** level (`sandboxed | hooked | advisory`) surfaced in the TUI so
the operator sees how strongly each agent is contained. `SessionConfig` carries `AgentID`, `Role`,
`Mode` (persistent/ephemeral), `Workspace`, `ResumeID`, `MCP` servers, and `Model`.

`adapter.NewGated(family, decider)` builds codex/copilot/grok with gating wired; claude is built
separately (its gate is an external PreToolUse hook).

| Family      | Driven via                                                         | Interrupt                     | Steer          | Gating mechanism              |
|-------------|--------------------------------------------------------------------|-------------------------------|----------------|-------------------------------|
| **claude**  | `claude -p --input-format stream-json --output-format stream-json` | ✗ (no in-band cancel)         | ✓ (stdin)      | PreToolUse hook → `/gate`     |
| **codex**   | `codex app-server --stdio` (JSON-RPC)                              | ✓ `turn/interrupt`            | ✓ `turn/steer` | in-protocol approval requests |
| **copilot** | `copilot --acp --stdio` (ACP engine)                               | ✓ `session/cancel`            | ✗ (new prompt) | `session/request_permission`  |
| **grok**    | `grok agent stdio` (ACP engine)                                    | ✓ `session/cancel`            | ✗              | `session/request_permission`  |
| **mock**    | in-process, scriptable                                             | configurable (`InterruptErr`) | ✓              | advisory                      |

codex and the ACP engine (copilot/grok) share a single line-delimited **`jsonrpc.Peer`**
(`Call`/`Send`/`Write`/`Run`/`Close` + a `Handler` with `OnNotification`/`OnServerRequest`). ACP is
generic: `acp.New(Profile{Family, Command, Args})` — copilot and grok are ~20-line wrappers that
supply a profile and a `Decide` func.

> **Why claude can't be interrupted in-band:** `claude -p` has no turn-cancel. avairy's drivers
> therefore *hard-stop* it (see §5.4): the supervisor/node loop closes the subprocess and respawns
> it (resuming via `--resume` on a node) on the next message. Interruptible families just cancel the
> turn.

### 4.6 Gating — autonomy guardrails (`internal/gating`)

Policy (what is gated) is decoupled from mechanism (how) behind a `Decider`:

```go
type Decider func(ctx, Request) (Decision, error)   // Allow | Deny | AllowForSession
```

`Policy.Decide` lets free actions through (read, build, test, local edits) and consults an
`Approve` func for gated ones. `Gated()` always gates `ActionGitMutate`, `ActionCrossNode`,
`ActionInstall`; for shell commands it pattern-matches destructive/`git push|commit`/install
strings. `GateEdits` opts file writes into approval too.

Three enforcement frontends feed the same `Decider`:
- **claude**: `gating.HookHandler` serves a `/gate` endpoint; `avairy hook` is the PreToolUse shim
  Claude runs per tool call, POSTing the tool to `/gate` and **failing closed** (deny) on any error.
  Layered timeouts: hook 300s > shim 290s > broker 280s.
- **codex**: app-server approval server-requests → `ApproverFromDecider`.
- **ACP**: `session/request_permission` → mapped to a `Request`, answered with the matching option.

On a node, the `Decider` calls `Node.RequestApproval`, which blocks on core's **Approvals** broker
until the operator allows/denies (or it times out → deny).

### 4.7 Agent lifecycle drivers — supervisor, runner, node loop

Three pieces drive a `Session` against the bus; all share the same shape (subscribe → on a
wake-worthy message, `Send`; stream events to the journal):

- **`runner`** (`internal/runner`) — the simplest driver; never sleeps. Used only for the in-process
  **mock** agents (`-demo`).
- **`supervisor`** (`internal/supervisor`) — drives a **core-local** real agent and adds **idle
  teardown**: after a quiet period it closes the subprocess (`agent_sleeping`, freeing
  context/credits) and lazily respawns it (`agent_awake`) on the next wake-worthy directed message.
  With `idle == 0` it behaves like a runner.
- **node run-loop** (`cmd/avairy/node.go`) — the same lifecycle for a **remote** agent, polling
  `PullInbox` each tick, with the same idle-sleep/respawn (respawn resumes the session via
  `--resume`).

All three implement two subtle behaviours uniformly:
1. **Wake policy** via `bus.Waker` — context-only chatter doesn't burn a turn.
2. **Interrupt = real stop** — `sess.Interrupt(ctx)`; if the family can't (claude), **hard-stop** by
   closing the subprocess and respawning on the next message.

### 4.8 Facilitator — keeping agents unstuck (`internal/facilitator`, `internal/dispatch`)

The facilitator is a **trigger-invoked, stateless** observer that *nudges* — it never commands. It
folds journal records (`Observe`) and detects two stuck conditions:

- **`TriggerBlocked`** — an agent self-reports `blocked`/`low_confidence` via `report_status`.
- **`TriggerLoop`** — repeated tool actions. `trackLoop` keys each step on `ToolSummary` (tool +
  identifying arg) and fires on either a **periodic cycle** (a block of 1–4 actions repeated
  `loopN=3`×, catching A↔B oscillation and back-to-back repeats) or **aperiodic circling**
  (`circleN=6` consecutive steps introducing nothing new). A **file-mutating tool repeated
  back-to-back is treated as progress** (each edit changes state), not a loop — only read-only
  repetition and interleaved oscillation count.

On a trigger it asks a `Nudger` (rule-based by default) what to say, debounced by a 45s per-(agent,
trigger) cooldown. On a loop it also runs **`fresh_look`**: an ephemeral, clean-context session gives
an unanchored take, delivered to the stuck agent.

**Dispatch** (`internal/dispatch`) is the pure routing cascade for `@facilitator` requests:
`Decide(workers, pick)` → `no-agents` | `sole-candidate` (one worker) | `matched` (an LLM picker
chose a known id) | `team` (open a claim). It touches no bus or model, so it's fully testable; the
caller publishes the result and journals `facilitator_dispatch`.

### 4.9 Cost monitor (`internal/cost`)

`Monitor` subscribes to the journal and folds `turn_done` usage into per-agent and fleet-total
`Spend` (`CostUSD`, input/output tokens). When a scope crosses its cap it fires `OnExceed(agentID,
scope, spent)` **once** — wired to interrupt the overspending agent (and surfaced in the TUI cost
footer with a ⚠). Caps of `0` mean uncapped.

### 4.10 Control plane — enrollment, identity, sync transport (`internal/control`)

The core's `Core.Handler()` is the node-facing HTTP API. Routes (auth = `Bearer <sessionToken>`
except `/enroll`):

| Route                            | Purpose                                                              |
|----------------------------------|----------------------------------------------------------------------|
| `POST /enroll`                   | join (token or verified mTLS cert) → `SessionToken`                  |
| `POST /heartbeat`                | liveness; report conflicts; receive directives + consult commands    |
| `POST /sync/push` / `/sync/pull` | file-sync diffs up/down                                              |
| `POST /sync/manifest`            | canonical fingerprint (checksum/version/age per path) for resync     |
| `POST /inbox/pull`               | drain this agent's bus messages                                      |
| `POST /events`                   | ship agent stream events to the journal (+ `agent_sleeping`/`awake`) |
| `POST /approve`                  | block on the operator's gating decision                              |
| `POST /repo/bundle`              | incremental git bundle of the canonical repo                         |

**Enrollment & identity.** A node joins with either a one-time **token** or a minted **mTLS client
cert**. `verifiedNodeID` extracts identity from the cert's `avairy:<id>` URI SAN. The
**`JoinBundle`** (base64) carries everything a node/operator needs — `Core`, `Bus`, `CA` (PEM),
`Token`, `NodeID`, and optionally `ClientCert`/`ClientKey` — so "the pubcert travels with the
token", no manual copying. `ReadJoin(join, joinFile)` resolves it from an inline string or file.

**Ephemeral vs persistent.** `NodeInfo.Ephemeral` is true for **token-joined** nodes: when
heartbeats lapse they are *forgotten* (dropped from the roster, `OnForget` fired, `node_forgotten`
journaled). **Cert-joined** nodes are durable: they're marked offline (`node_offline`) and kept, so
they rejoin cleanly after a restart. `touch` ignores unknown ids so a stray heartbeat can't
resurrect a forgotten node.

**Liveness.** `RunLiveness` ticks at `LivenessTimeout/3` and `sweepLiveness` flips `Live` based on
`LastSeen`, journaling only transitions.

**Callbacks** wire the control plane to the rest of core: `OnEnroll` (register the agent on the bus
*before* journaling, so the fleet sees it), `OnForget`, `OnConflict` (mid-run push conflict → notify
the responsible agent with both sides), `OnNodeConflict` (a node's *first* sync conflicts → route to
the operator for a verdict), and `InboxDrainer`.

**mTLS / CA.** `-tls-auto` makes core self-manage a CA under `.avairy` (`EnsureCA` → `ca.crt`/
`ca.key`, stable across restarts). It mints: server certs (`CN=avairy-core`), **node** client certs
(URI SAN `avairy:<id>`), and **operator** client certs (URI SAN `avairy-operator:<name>`, distinct
scheme so the two are never confused). `add-operator` also emits a password-protected `.p12` for
browser/keychain import. The node-side `Node` client mirrors all of this: `Enroll`, `Heartbeat`,
`SyncUp`/`SyncDown`/`Resync`, `PullInbox`, `PostEvents`, `RequestApproval`, `MCPProxy`, plus
`ReenrollOnExpiry` for stateless recovery on a 401.

### 4.11 Workspace sync hub & git (`internal/workspace`, `internal/git`)

**Hub topology.** Core holds the **canonical workspace** in a `Hub` (`map[path]*FileState` with a
monotonic per-file `Version`). Nodes sync diffs up/down; core fans out. O(N), one conflict point.
The per-file counter is the collapsed form of a version vector — sufficient because the hub is the
single linearization point.

**Change detection.** A node tracks per-path `FileStamp{Size, ModNano, Hash}`. `ScanChanges` uses a
cheap stat gate (size+mtime) and only reads + hashes (FNV-1a) when that trips — so atomic renames,
`touch`, git checkouts, and sync-writes don't ping-pong. A 200ms `Watch` debounce coalesces editor
bursts.

**Push/pull & conflicts.** A `Push` carries the node's `Base` version; if `Base != hub.Version` the
hub rejects it as a **`Conflict`** (returns both sides, hub unchanged). The node then writes
**3-way git-style markers** (`MergeMarkers`, line-level Myers diff — only differing spans get
`<<<<<<< local / ======= / >>>>>>> hub vN`, no spurious blank lines), **locks** the path from sync
until an agent/human removes the markers, then pushes the merged content as the next version. Pulls
skip locked paths so they never clobber in-progress merges. A node's *first* sync routes conflicts
to the operator for a `resync` (discard local, pull delta) or `resolve` (write markers) verdict
delivered on the heartbeat.

**Cross-OS hygiene.** Text is LF-normalized in transit (binaries left alone); mode/exec bits
preserved; symlinks replicated as links (not followed); writes are atomic (temp + rename); empty
parents pruned on delete; deletes/moves propagate. Ignores merge `DefaultIgnore()` (`.git`,
`node_modules`, build dirs, binaries, …) with real `.gitignore`/`.avairyignore`/`.dockerignore`
parsing. The hub snapshot persists to `.avairy/hub.json` (`SaveIfDirty`).

**Git (`internal/git`).** The canonical workspace *is* a single git repo on core. **History reads**
(`git_history`: log/show/diff/blame, arg-sanitized) are available to any node agent. **History
writes are core-only and signed** — `Commit` runs at core (`-S`), so signing keys never ship to
nodes; a node agent requests one via gated `request_commit`. For OS-specific bisect/build, nodes get
a read-only **mirror** (`UpdateMirror` from an incremental `Bundle`, refreshed ~5 min) and can spin
**disposable scratch worktrees** (`AddWorktree`/`RemoveWorktree`, pruned on shutdown) isolated from
the live tree. `.git` is excluded from the working-tree sync so writes stay single-sourced.

### 4.12 Operator API, TUI & web console (`internal/operator`, `internal/tui`)

The operator surface is designed so the **same UI runs in-process or remote**. `Services` (on core)
exposes callbacks (`Inject`, `Interrupt`, `ResolveApproval`, `ResolveConflict`, `Commit`,
`NewToken`, `Consult`/`CloseConsult`, `NodeDirective`) and a journal. `Services.Deps()` adapts them
to `tui.Deps`. The remote path is identical: `operator.Server` exposes `Services` over HTTP, and
`operator.Connect` returns a `Client` whose `Deps()` satisfies the same `tui.Deps`.

**Operator HTTP API** (`/operator/*`, auth = bearer token or operator mTLS cert):

| Route                                 | Purpose                                                              |
|---------------------------------------|----------------------------------------------------------------------|
| `GET /operator/stream`                | SSE journal (backfill, then live; `ready` event marks backfill done) |
| `GET /operator/state`                 | snapshot: tasks, notes, approvals, conflicts, roster, control info   |
| `POST /operator/inject`               | publish a human message (supports leading `@<id>`)                   |
| `POST /operator/interrupt`            | stop running agents                                                  |
| `POST /operator/approval`             | resolve a gated action (`allow`/`deny`/`allow_for_session`)          |
| `POST /operator/conflict`             | resolve/delegate a conflict                                          |
| `POST /operator/commit`               | signed commit → `{Hash, Error}`                                      |
| `POST /operator/token`                | rotate the enrollment token                                          |
| `POST /operator/consult` / `close`    | spawn / tear down an ephemeral consult agent                         |
| `GET /operator/ui` (+ `/ui/vendor/*`) | the browser console (opt-in via `-web`)                              |

The **web console** (`web/index.html`) is a chat-first UI with vendored libs (marked, DOMPurify,
highlight.js, diff) embedded via `go:embed` — no CDN, airgap-friendly. It consumes the same SSE +
`/state` as the TUI.

The **TUI** (`internal/tui`, Bubble Tea) is event-sourced from the journal subscription. Tabs:
**conversation**, **handovers**, **tasks**, **notes**, **approvals**, **conflicts**; a fleet line
shows each agent's status (working/idle/blocked/offline/sleeping) and spend. The command line
addresses an agent or broadcasts (with a steer/interrupt indicator) and supports slash commands
(`/commit`, `/consult [@node] [family]`, `/end <id>`); `@<id>` mentions of known agents are
highlighted.

---

## 5. End-to-end flows

### 5.1 Bootstrap & node join

```
operator: avairy core run --tls-auto
   → EnsureCA (.avairy/ca.{crt,key}); start bus, MCP, control API, hub, facilitator, TUI
operator: avairy core add-node --id linux        → prints a JoinBundle (CA + client cert/key)
on node:  avairy node join --join <bundle> --family claude --workspace ~/proj --core-mcp …
   → ReadJoin → Enroll (mTLS) → ResumeFromHub → start MCP proxy + /gate → spawn agent
   → heartbeat/sync loop every 2s
```
Token join is the opt-in alternative (`core run --allow-token-join`): the node enrolls once with a
one-time token and is **ephemeral** (forgotten on disconnect). Cert nodes are durable. Default
posture is **mTLS-only** (`mtlsEnabled := !allowTokenJoin`).

### 5.2 A message's lifecycle

```
agentA calls send_message(to="agent:linux", body="…")    [on its node's localhost MCP]
  → node proxy stamps X-Avairy-Agent: agentA → core /mcp
  → mcp.send_message → bus.Publish(from=agentA, agent:linux)   [journaled KindMessage]
  → bus.matches → linux's wakeCh + ch
  → linux's node loop PullInbox → Waker.Wake? yes (direct) → sess.Send(body)  [agent wakes, turns]
  → linux streams events → POST /events → journal → SSE → operator console
```

### 5.3 A `@team` request (single owner)

```
human → @team "fix the leak"   → bus (team)   → reaches every agent (NOT the facilitator)
each agent woken with AnnotateDelivery: "[team request m3 — call claim_response("m3") first…]"
  → agentA claim_response("m3") → granted (journaled response_claimed)
  → agentB claim_response("m3") → denied → stands down
```

### 5.4 Human injection: steer, interrupt, Stop

`delivery: steer` queues to the next turn/tool boundary (any agent). `delivery: interrupt` cancels
the current generation where supported. The **Stop** button broadcasts `bus.Interrupt`. Each driver
calls `sess.Interrupt`; for an interruptible family (codex/copilot/grok) the turn is cancelled
in-band. For **claude** (`SupportsInterrupt: false`) `Interrupt` errors, so the driver **hard-stops**
— closes the subprocess and respawns it on the next message (resuming the session on a node). So Stop
actually stops every family.

### 5.5 Gating approval round-trip

```
agent attempts a gated action (rm -rf / git push / file write w/ --gate-edits)
  claude:  PreToolUse → `avairy hook -gate` → node /gate
  codex:   app-server approval request
  acp:     session/request_permission
  → node gateDecider → Node.RequestApproval → core /approve  [blocks]
  → Approvals broker → TUI/console Approvals tab → operator allow/deny
  → verdict returns to the agent (timeout or core-down → deny, fail-closed)
```

### 5.6 File sync & conflict

```
node edits files → Watch debounce → SyncUp: ScanChanges → Push(base)        [→ hub Version+1]
peer's SyncDown: Pull(base) → ApplyFile (atomic)                            [gets the new version]
divergent edit: Push(base != hub) → Conflict → node writes 3-way markers, locks path,
  OnConflict notifies the responsible agent (both sides) → agent edits out markers →
  resolve_conflict / next SyncUp pushes merged content as the next canonical version
```

### 5.7 Idle-sleep / wake / resume

An idle agent's subprocess is torn down after a quiet window (`agent_sleeping`, zero tokens). The
next wake-worthy directed message respawns it (`agent_awake`), resuming the persistent session
(`--resume`) so context survives sleep. Context-only chatter does **not** wake it (Waker).

### 5.8 Loop → fresh look; budget → interrupt

The facilitator detects a loop (excluding back-to-back edits), runs an ephemeral `fresh_look`, and
delivers the unanchored take to the stuck agent. Independently, the cost monitor fires `OnExceed`
when an agent/fleet crosses its budget, interrupting the overspender.

---

## 6. Cross-cutting concerns

### Security model
- **mTLS by default.** Self-managed CA (`-tls-auto`); nodes and operators authenticate by minted
  client certs with distinct SANs (`avairy:` vs `avairy-operator:`). Token join is opt-in and
  ephemeral.
- **Channel is node→core outbound** — NAT/firewall-friendly; no inbound ports on nodes.
- **No model credentials, ever.** Each agent CLI uses its own local login; avairy spawns it and
  inherits that auth. Nothing secret is stored or on the wire (beyond the agents' own traffic).
- **Signing keys stay on core.** History writes are core-only and signed.
- **Fail-closed gating.** Any error in the approval path denies.

### Concurrency
- The journal fan-out is non-blocking (slow subscribers skipped). The bus drops to a full inbox
  rather than blocking publishers. Per-subsystem state is mutex-guarded (`Core.mu`, `Hub.mu`,
  `Server.claimMu`, `Monitor.mu`, facilitator `mu`). Each agent runs in its own goroutine driven by
  a `for { select … }` loop over its bus subscription / inbox ticker.

### Persistence & resume
- `.avairy/journal.jsonl` (event log), `.avairy/hub.json` (canonical workspace), `.avairy/ca.*`
  (CA), `.avairy/*.join` (bundles). On restart, board/blackboard/cost replay the journal and the hub
  reloads its snapshot; cert nodes rejoin statelessly.

### Extensibility
- **New agent family:** implement `agent.Adapter`/`Session` (or, for an ACP server, just supply an
  `acp.Profile`), advertise `Capabilities`, map gating to a `Decider`, and register in
  `adapter.NewGated`. The mock adapter shows the minimal shape.
- **Stronger enforcement:** the `gating.Decider` seam is backend-agnostic by design — an OS-sandbox
  backend can drop in without touching policy or callers.

### Testing
- Pure logic (`dispatch`, `board`, `bus` matching/waker, facilitator detection) is unit-tested
  without a live model. The **mock** adapter exercises the bus/driver/journal loop deterministically
  at zero cost. The project convention is **RED-before-GREEN**: a failing reproducer precedes every
  bug fix (see the bus/supervisor/facilitator tests for examples).

---

## 7. Package map

| Package                                   | Responsibility                                                                                         |
|-------------------------------------------|--------------------------------------------------------------------------------------------------------|
| `cmd/avairy`                              | CLI command tree; wires & starts core / node / tui / hook                                              |
| `internal/agent`                          | the adapter/session contract, `Event`, `Capabilities`, `ToolSummary`                                   |
| `internal/adapter/*`                      | per-family drivers: `claudecode`, `codex`, `acp`(+`copilot`,`grok`), `mock`, shared `jsonrpc`          |
| `internal/bus`                            | message router: addressing, `matches`, `Waker`, dedup, `AnnotateDelivery`, interrupt                   |
| `internal/mcp`                            | MCP server + all agent-facing tools; identity injection; `@team` claims                                |
| `internal/board`                          | task board (capability-gated claims) + blackboard (durable notes)                                      |
| `internal/journal`                        | event-sourced log (`Memory`/`File`), the integration spine                                             |
| `internal/facilitator`                    | stuck-detection (blocked/loop), nudges, fresh-look trigger                                             |
| `internal/dispatch`                       | pure `@facilitator` routing cascade                                                                    |
| `internal/cost`                           | per-agent/total budget tracking + interrupt-on-exceed                                                  |
| `internal/supervisor` / `internal/runner` | core-local agent lifecycle (idle-sleep / simple)                                                       |
| `internal/gating`                         | autonomy policy + `Decider` + PreToolUse hook shim                                                     |
| `internal/control`                        | node-facing control plane: enrollment, mTLS/CA, heartbeat, sync transport, approvals/conflicts brokers |
| `internal/workspace`                      | file-sync hub, change detection, conflict markers, ignores, normalization                              |
| `internal/git`                            | canonical repo: signed commits, history reads, bundles, scratch worktrees, mirrors                     |
| `internal/operator`                       | operator HTTP API + `Services` + remote `Client` + embedded web console                                |
| `internal/tui`                            | Bubble Tea operator UI (fleet/conversation/tasks/approvals/conflicts/notes)                            |
| `internal/buildinfo`                      | version/commit/date stamped at link time                                                               |

---

*Keep this in sync as the architecture evolves. Subsystem details (exact tool params, route bodies,
wire structs) live next to the code; this document is the map, not the territory.*
