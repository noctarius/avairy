# avairy — Status & Backlog

Snapshot of what's built and what's still owed against [DESIGN.md](DESIGN.md). Update as items
land. Section references (§N) point at DESIGN.md.

## Working end-to-end

- **Four agent families** on the MCP bus, verified live: Claude Code and Codex (native
  adapters), Copilot and Grok (generic ACP engine + per-family profile).
- **MCP shared bus** (§4): addressed messaging (no self-echo), capability-gated task board,
  `report_status`, inbox delivery.
- **Single-machine and distributed paths**: `cmd/avairy` core + `cmd/avairy-node` daemon
  (token enrollment, rejoin, heartbeat, local MCP reverse-proxy).
- **File sync hub** (§9): per-file hub versions, stat+content-hash change detection, idempotent
  push, LF/mode normalization, `.gitignore`/`.dockerignore`/`.avairyignore` excludes. Deletes
  and moves (move = delete+add) propagate; symlinks replicate as links; empty dirs are pruned
  on delete. Conflicts are detected and routed to agents for reconciliation (#3).
- **Facilitator** (§5): journal-driven blocked/loop detection → rule-based nudges.
- **Human-in-the-loop gating** (§7): gated actions block and route to the operator's
  **Approvals** tab (allow/deny); Claude via PreToolUse hook, Codex via app-server approvals;
  unanswered requests fail closed. Layered timeouts (hook 300s > shim 290s > broker 280s).
- **TUI** (§3): fleet/cost, conversation, handovers, task board, approvals; human injection
  (steer/interrupt), enroll-token display/rotation.
- **Event-sourced journal** (§10): durable append-only `.avairy/journal.jsonl`.

## Backlog

00. ~~**Ephemeral / fresh-look sessions (§8).**~~ ✅ Done. The `Mode` field was dead; now
    adapters honor `SessionEphemeral` (force a fresh session, no resume), and the **`fresh_look`**
    MCP tool spins up a one-shot ephemeral session (same family/model, clean context seeded with
    the task board, tools denied) and returns an independent answer — the §8 anti-anchoring tool.
    A one-shot persists **nothing** (throwaway workspace, no session id), and node session
    persistence is guarded `!= SessionEphemeral`, so a fresh look can never overwrite the agent's
    real session. ✅ The **facilitator auto-invokes** fresh_look on a detected loop and delivers
    the independent take to the stuck agent (async, rate-limited by the nudge cooldown; gated to
    runs with a real agent so a `-demo` mock loop can't spawn a paid session). *Still open:*
    curating richer blackboard context for the fresh-look prompt.

0. ~~**Tool actions carried & shown with detail.**~~ ✅ Done. Tool calls now surface their
   identifying arg — `Bash: go test ./...`, `Read src/main.go` — in the TUI (`agent.ToolSummary`)
   instead of a bare repeated "Bash"/"Read". The node ships the (trimmed) tool input over the
   wire (`agent.TrimInput` drops file bodies/diffs), which it previously dropped — so remote
   agents no longer look like they're doing nothing, and loop detection keys on the full action
   (reading 100 different files is 100 steps, not a loop). ACP (Copilot/Grok) now map
   `rawInput` (and `locations` as a fallback for file ops) into `ToolCall.Input`, so all four
   families carry action detail.


Ranked roughly by value-to-effort within each group.

### Designed but not built (large)

1. ~~**Git integration (§9).**~~ ✅ Done. `internal/git` wraps the git CLI on core's canonical
   repo; MCP tools wired (enabled when `-workspace` is a repo):
   - ✅ `git_history(mode, ref, path, limit)` — log/show/diff/blame, read-only, any agent (for
     RCA); args validated against flag-injection.
   - ✅ `request_commit(message, paths)` — **gated** (routes to the operator's Approvals tab via
     the broker), executed **core-only and signed** (`git -S`; keys never ship to nodes).
   - ✅ `scratch_worktree(create|list|remove)` — disposable detached checkout of any ref, rooted
     off the synced tree (so bisect/build/repro doesn't disturb the canonical tree), tracked and
     pruned on shutdown. *Note:* the checkout lives on core; materializing it onto a node for
     on-node cross-OS build/repro is a further step.
   - ✅ TUI-initiated commit: the operator types `/commit <message>` to sign a commit directly
     (runs off the UI thread, result folds into the conversation).
   - ✅ On-node cross-OS bisect/build/repro: core serves the repo as a git **bundle**
     (`/repo/bundle`); the node maintains a **read-only mirror** under `.avairy/mirror.git`
     (refreshed every 5 min) and the agent's role tells it to `git --git-dir=<mirror> worktree
     add` past commits into `.avairy/scratch/`, build/bisect there, and commit via
     `request_commit` (no push rights to the mirror). Bundles are **incremental** — the node
     sends the shas it has and core ships only newer objects (`--all --not <have>`), or 204 when
     current. *Limitation:* fetch adds/advances refs but doesn't prune, so branch deletions /
     force-rewinds on core leave stale refs in the mirror (harmless for read-only RCA).

2. ~~**Hub persistence.**~~ ✅ Done. The hub snapshots to `.avairy/hub.json` (atomic
   temp+rename), restored on startup via `LoadHub`; persisted every 5s if dirty and on clean
   shutdown. The seed NodeView calls `ResumeFromHub` so a restored hub isn't re-conflicted or
   false-deleted (adopts versions only for files still present locally). *Future:* the snapshot
   is one JSON blob (whole-tree rewrite); a git-backed / per-file store would scale better.

3. ~~**Conflict reconciliation routing.**~~ ✅ Done. On a rejected (divergent) push, core
   routes a CONFLICT to the responsible agent over the bus — carrying **both** sides (hub
   version + the agent's rejected edit, since the node's SyncDown overwrites the local file
   with the hub version) — deduped per (agent, path, hub version). The agent merges and calls
   the `resolve_conflict(path, content)` MCP tool → `Hub.Resolve` lands it as the next version,
   and both nodes converge on SyncDown. ✅ **Non-clobbering hold (file locking):** a rejected
   push now writes **git-style 3-way markers** into the node's local file (the agent's edit is
   the "ours" side — nothing lost), **locks** the path (SyncUp won't push markers, SyncDown won't
   overwrite it even as the hub moves on), and adopts the hub version as base; the agent resolves
   in place by removing the markers, which then pushes and converges. *Still open:* routing an
   operator/seed conflict to the **human** (vs an agent) — #19.

### Gating — finish what's started

4. ~~**Copilot & Grok aren't gated to the human.**~~ ✅ Done. `copilot.New(decide)` /
   `grok.New(decide)` now take a decider (nil → fail-closed); the node path passes
   `gateDecider` and local `-live` passes `localGateDecider`, so ACP
   `session/request_permission` requests reach the operator's Approvals tab like Claude/Codex.

5. ~~**Local `-live claude` gating.**~~ ✅ Done. The hook shim + `--settings` builder moved to
   `internal/gating` (shared by both binaries; `avairy` gained a `hook` subcommand).
   `cmd/avairy` now serves a loopback `/gate` and registers the PreToolUse hook for local
   Claude — same broker path as a node, no more `--allowedTools`.

6. ~~**`AllowForSession`.**~~ ✅ Done. The Approvals tab adds `a` = "allow this kind from this
   agent for the session"; the broker remembers `(agentID, kind)` grants and auto-allows
   matching requests with no re-prompt (centralized, so local + node paths both benefit).

7. ~~**Live `--settings` hook validation.**~~ ✅ Done. Verified end-to-end against real `claude`
   2.1.181: a live run parses the injected `--settings`, fires the PreToolUse hook on a tool call,
   the shim relays it to `/gate`, and `HookHandler`'s decision governs — the gate saw the correctly
   mapped action (`command echo avairy-hook-ok`). Covered by `TestLiveClaudeHook` (gated behind
   `AVAIRY_CLAUDE_HOOK_LIVE=1`; spends ≈one cheap haiku turn, so it's skipped in normal/CI runs).

### Robustness / operational

8. ~~**Channel TLS.**~~ ✅ Done for the control channel, with self-managed CA + mTLS:
   - `-tls-cert`/`-tls-key` for operator-supplied certs, or **`-tls-auto`**: core generates a
     self-signed CA (persisted at `.avairy/ca.{crt,key}`, stable across restarts) and issues its
     own server cert.
   - **Join bundle**: core writes `.avairy/join` — one base64 string carrying core URL + CA
     pubcert + token. The node consumes it with `-join`/`-join-file` (so the CA travels with the
     token; no cert files copied by hand). TUI shows the join path.
   - **mTLS as an alternative to the token**: `avairy mint-join -id <node> -core <url>` issues a
     CA-signed client cert (node id in a URI SAN, `avairy:<id>`) embedded in a join; the node
     authenticates by certificate (core does VerifyClientCertIfGiven and enrolls by the SAN id,
     no token). `-ca`/`-insecure` remain for manual/dev trust.
   - **Auto-reenroll**: an mTLS node re-enrolls automatically on a 401 (e.g. after a core
     restart drops its session) and retries — cert auth is stateless on core, so it recovers
     without a node restart. (Token nodes can't: their binding is in-memory only — see #12.)
   - ✅ **MCP bus TLS + one port**: local agents get a dedicated plain loopback listener (never
     need TLS), while remote nodes reach the bus at `/mcp` on the **control listener** — so the
     bus, control channel, and operator API share one TLS port. A node's MCP proxy reuses its
     CA-trusting transport to reach it, so a remote agent's inter-agent traffic isn't cleartext.

9. ~~**Dead-node detection.**~~ ✅ Done. `Core.RunLiveness` marks a node offline when its
   heartbeats lapse past `LivenessTimeout` (15s) and online again on return, journaling each
   transition; the TUI fleet shows offline agents with a `⊘` dot. Built on the existing
   node→core heartbeat (no new keep-alive). *Still open:* core doesn't know a node's heartbeat
   interval, so `LivenessTimeout` must exceed it (fine at the 2s default).

10. ~~**fsnotify.**~~ ✅ Done. `workspace.Watch` recursively watches the tree (auto-adds new
    subdirs, honors Ignore, debounces bursts) and emits a coalesced signal; node + seed loops
    SyncUp on it immediately, ticker stays as the fallback poll + drives heartbeat/SyncDown.
    Paired with **content-hash change detection**: size+mtime is the cheap stat gate, but a
    real change now requires a content-hash difference — so our own SyncDown/reconcile writes
    (and metadata-only touches) seen by fsnotify don't ping-pong into re-pushes. Stamps record
    the hash; touched-but-identical files refresh their stamp without pushing.

11. ~~**Facilitator debounce + matchmaking.**~~ ✅ Done. A per-(agent, trigger) cooldown (45s)
    in `Observe` stops a flapping agent from being nudged on every status report; a progress
    report clears the agent's blocked cooldown so a genuine later block nudges promptly.
    Matchmaking is now **roster-driven** (`bestPeer`): the blocker text is matched against *any*
    declared capability (arch, qemu, gpu, docker, … — with value synonyms like aarch64↔arm64,
    boolean caps by key), picking the peer that's differentiated from the blocked agent — not the
    old OS-only keyword table.

12. **State-resume from journal — mostly done.** ✅ The **task board** resumes (`board.Restore`
    replays the journal, recovering each task's final state + continuing ids). ✅ The **TUI
    history** resumes too: `cmd` re-decodes the persisted records to their typed forms
    (`decodeRecords`) and seeds the in-memory log via `journal.Memory.Restore` before the TUI
    subscribes, so conversation / handovers / fleet / approvals replay on the backfill (seqs
    renumbered contiguously for stable de-dup). **By design, token enrollment state is NOT
    persisted** — one-time tokens are short-lived secrets; a node that must reliably reconnect
    across a core restart should use mTLS (#8), whose cert auth is stateless on core and
    auto-reenrolls. ✅ **Agent session resume**: the node persists the agent's session id under
    `.avairy/session` and passes it back as `ResumeID` on respawn, so a restarted agent continues
    its conversation — wired for **Claude** (`--resume`) and **Codex** (`thread/resume` by
    threadId, verified against the app-server schema; falls back to a fresh thread if the id
    can't be loaded) and **Copilot/Grok** (ACP `session/load` — both advertise `loadSession:true`
    and recognize the method, verified live; the replayed history is suppressed since it's
    already journaled, and it falls back to `session/new` if the id can't be loaded). All four
    families now resume. *(Not yet exercised: a full create→load round-trip with real history —
    rests on the capability + graceful not-found behavior.)*

14. **Loop detection — cycle-aware.** ✅ `trackLoop` now does **period/k-cycle detection**: it
    keeps a window of recent *action* signatures (tool calls only — interleaved reasoning is
    filtered out) and fires when the tail is a block of 1..4 actions repeated `loopN` (3) times.
    So it catches the classic back-to-back repeat (period 1), **A↔B oscillation** (period 2,
    e.g. ping-ponging between two fixes), and **interleaved retries** (reasoning between attempts
    no longer hides the repeat); two rounds of edit/test are *not* flagged (normal iteration).
    On a hit the facilitator auto-runs a fresh look (#0).

    - **(a) Circling without a clean period** — ✅ Done. `trackLoop` now also tracks a **novelty**
      signal: an action never seen in the recent window is progress (resets); `circleN` (6)
      consecutive actions that introduce nothing new fire a loop even with no period (`A B C  B A C
      C A B …`). A new action every few steps (editing many files, productive churn) keeps
      resetting it, so genuine iteration isn't flagged. Tests: `TestLoop_CirclingDetection`.
    - **(b) Semantic loops** — addressed by **design**, not a continuous LLM. Rather than an
      always-on LLM reading the whole transcript (token-heavy), the **deterministic detectors are
      the cheap trigger**; on a hit the facilitator **hands off to a local agent** (the fresh-look
      one-shot, #0) with a *summary* of the loop (the cycle's repeating block, or the circling
      detector's list of churned actions) to analyze the situation and advise — not the full
      history. So exact-string detection stays cheap, and the agent supplies the judgment for the
      "same intent, different surface form" cases when it's actually invoked.

### Usability / driving work

15. ~~**Per-edit (and per-session) human approval of file edits.**~~ ✅ Done. `-gate-edits`
    (on both `avairy` and `avairy-node`) sets `Policy.GateEdits`, which routes `ActionFileWrite`
    to the Approvals tab instead of auto-allowing. Per-session falls out of the broker's existing
    **allow-for-session** (`a` in the TUI): approve "edits by this agent" once and the rest of the
    session's edits auto-allow. Off by default. Precise per family: Claude (Edit/Write) and Codex
    (fileChange) already classified edits as `ActionFileWrite`; ACP was over-broad (reads bucketed
    as file writes), so reads now map to a new never-gated `ActionRead` — `-gate-edits` gates real
    edits, not reads.

16. ~~**Blackboard — durable shared memory (§4/§8).**~~ ✅ Done. `board.Blackboard` is keyed,
    latest-wins, journal-backed shared memory (`KindNote`), exposed over MCP as `note(key, text)`
    and `read_notes(prefix?)`. It restores from the journal on startup like the task board, and
    `fresh_look` now curates its clean context from a notes summary (was hardcoded to the task
    board). Agents and the operator can seed durable context — decisions, repro steps, findings —
    that survives restarts and feeds fresh-look sessions.

17. ~~**Web UI (browser operator console, alongside the TUI).**~~ ✅ Done. A single-page browser
    console, served by core at **`/operator/ui`** off the existing operator API (#18) — so it's a
    second client of `/operator/*`, not new plumbing. Chat-first layout modelled on common AI web
    chats (centered conversation with message bubbles, composer pinned at the bottom, human messages
    right-aligned), with the operator-specific bits in side rails: **Fleet** + **Tasks** (left),
    **Approvals** + **Conflicts** with action buttons (right). It consumes the SSE journal stream
    (live transcript + fleet status + cost, mirroring the TUI's `apply`) and polls `/operator/state`
    (2s) for tasks/approvals/conflicts/roster/control; the composer drives inject (broadcast or an
    agent), `Stop` (interrupt), and `/commit <msg>`; approvals get allow / allow-session / deny,
    conflicts get resolve-mine / delegate-to-agent. The page is a static embedded asset
    (`go:embed`, zero build step / no JS deps). Auth: the operator token via `?token=` (the page is
    public; its data calls carry it — the bearer-or-query auth accepts both). The web URL (with
    token) is shown in the attached TUI's control line and printed when headless. Verified
    end-to-end against the real binary (page served, query-token stream auth, 401 on bad token, JS
    syntax-checked). Stays single-operator (#13); multi-operator is still out of scope.

18. ~~**Detach the TUI from core (remote operator connection).**~~ ✅ Done. `tui.Deps` no longer
    holds concrete `*bus.Bus`/`*board.Board` pointers — it's all interface-level (a `journal.Log`
    plus `Inject`/`Interrupt`/`Tasks`/`Resolve*` func fields), so the same TUI runs either
    in-process or attached from another machine.
    - **(a) core + attached TUI** — as today; `avairy` builds `operator.Services` and runs
      `tui.Run(svc.Deps())` in-process.
    - **(b) core serving + remote TUI** — `avairy core serve --advertise … ` serves the **operator
      API** (`/operator/*`) on the one listener, sharing its TLS. `avairy tui connect --join-file
      .avairy/operator-join` (or `--core/--token/--ca`) attaches and renders the identical UI.

    New `internal/operator` package: `Services` (the live surface — journal + bus/board/approvals/
    conflicts actions) yields both `Deps()` (in-process) and `NewServer()` (HTTP). The API is a
    journal **SSE stream** (`/operator/stream`: backfill then live, with a `ready` sentinel) + a
    `/operator/state` snapshot (tasks/approvals/conflicts/roster/control) + action POSTs (inject,
    interrupt, approval, conflict, commit, token). `Client` dials it, keeps a local journal fed by
    the stream + a state cache refreshed on relevant records, and exposes a matching `Deps()`. Auth
    is a bearer **operator token** (`-operator-token`, else random, shown in the TUI / printed when
    headless), bundled with the core URL + CA into `.avairy/operator-join` (the node join machinery
    reused). Verified end-to-end against the real binaries (curl + `avairy-tui` connect).

    **Same API the web UI (#17) needs** — it's a second client of `/operator/*`, so #17 is now a
    rendering layer over an existing transport, not new plumbing.

19. ~~**Route operator/seed (and git) conflicts to the human.**~~ ✅ Done (seed conflicts). Some
    conflicts have no owning agent to hand to. The operator's **seed workspace** diverging from a
    node's edit is now detected in core's seed-sync loop: `NodeView` gained the same marker/lock
    machinery as a node (`MarkConflict`/`IsLocked`/`LockedPaths`) — the operator's local file gets
    git-style markers, the path is held (not pushed, not clobbered by SyncDown), and the conflict
    is raised on a new `control.Conflicts` broker. A **Conflicts** TUI tab shows pending ones; the
    operator presses **`m`** to take it (resolve in their editor — the next sync picks it up) or
    **`d`** to delegate it to the selected recipient agent (a steer message: "fix the markers in
    X and call resolve_conflict"); `ctrl+t` picks the agent. Once the markers are removed and the
    file syncs, the notification auto-clears (`ClearPath`).

    **Git rebase/merge conflicts** are deferred: avairy has no merge/rebase operation today
    (`git.Repo` does history reads + `Commit`, never a merge), so there's no trigger to route. The
    broker carries a `Source` field (`"seed"` | `"git"`) so a future merge op can raise into the
    same view without rework.

20. ~~**Scrollable conversation (and other views) in the TUI.**~~ ✅ Done. `render()` now windows the
    flattened body by a `scroll` offset (visual lines above the bottom; 0 = following the tail).
    **PgUp/PgDn** scroll a half-page, **End** jumps to the latest, switching tabs resets to the tail.
    While scrolled up, a new record grows the offset by the rows it added so the viewport stays
    anchored on what you're reading (no drift); a footer indicator shows when you're scrolled back.
    Applies to every tab (Conversation/Handovers/Tasks/Approvals/Conflicts). Test: `TestScrollback`.

21. ~~**Operator choice on startup conflicts (full resync vs. resolve).**~~ ✅ Done. A node that hits
    conflicts on its **first** sync (genuine divergence: offline, local edits, hub moved on) now
    holds those paths (`startupHeld` — frozen, no markers, not pushed/clobbered) and routes the
    choice to the **operator** instead of auto-marking. Wire: `PushRequest.FirstSync`; core raises
    one per-node entry via `OnNodeConflict` (Source `node-startup`, with an overview summary —
    paths + hub version + age from the new timestamp); the operator's verdict is stored
    (`SetNodeDirective`) and rides back on the node's heartbeat (`HeartbeatResponse.Directive`),
    which the node applies via `ApplyDirective`. Options, on the **operator** (Conflicts tab + web,
    extended): the node keeps running (agent works non-conflicted files) until the verdict lands.
    - **Resync** (`r` in the TUI / "Resync" in the web) — a **checksum-manifest reconcile, not a
      wipe**: `Hub.Manifest()` (served at `/sync/manifest`) gives every path's checksum + version +
      age; `Node.Resync` keeps files whose checksum already matches, overwrites diverged ones,
      deletes paths the hub dropped, fetches the rest, and rebuilds base — only the delta crosses
      the wire (scales to large repos). Local divergence discarded.
    - **Resolve** (`x` / "Resolve") — write the 3-way markers and reconcile as today (#19).
    - **Overview** (`o` / "Overview") — expand the entry to the per-path summary (hub version + age)
      so the operator can choose between the former two.

    `FileState` gained a persisted `Modified` timestamp for the "age". Built on #19's broker +
    ResumeFromHub; the manifest reconcile also stands alone as a "repair drift" sync path. Tests:
    `TestResyncReconcilesAgainstManifest`, `TestNodeStartupConflict{Resync,Resolve}`.

22. ~~**Conflict MCP tool — agents shouldn't grep for markers.**~~ ✅ Done.
    - **`list_conflicts`** (MCP) returns the calling agent's conflicted files authoritatively — the
      node reports its tracked set (`n.conflicts` + `n.startupHeld`) on each heartbeat
      (`HeartbeatRequest.Conflicts`), core stores it per-node (`Core.NodeConflicts`), and the tool
      returns it (agent id == node id). No grepping, no false-positives on decorative `=======`.
    - **`resolve_conflict` node-lock gap closed** — it advanced the hub but left the node locked with
      stale markers. Now resolving queues an **unlock** (`Core.ResolveNodeConflict` →
      `HeartbeatResponse.Unlock`); the node drops the lock and pulls canonical *before* the next
      SyncUp (`ApplyUnlocks`), so the merged content lands over the markers. Tests:
      `TestNodeConflictListAndResolveUnlock`, `TestListConflictsTool`. (`list_conflicts` added to the
      role prompts so agents use it instead of grep.)

23. ~~**Markdown rendering in the operator console.**~~ ✅ Done.
    - **TUI** → `charmbracelet/glamour`: agent text events render markdown→styled ANSI, width-wrapped,
      renderer cached + rebuilt on width change, falling back to raw text on error. (Bumped
      `x/cellbuf` to align glamour's transitive deps with the charm.land v2 stack.)
    - **WebUI** → `marked` + **DOMPurify** (sanitized — agent text → innerHTML) + **highlight.js**
      with the **diff** language, all **vendored** same-origin under `/operator/ui/vendor/` (embedded
      via `go:embed`, no CDN — self-contained / airgap-friendly, ~200KB incl. the highlighter).
      Messages and agent text render as markdown with fenced-code syntax highlighting (incl. diff);
      tool/system lines stay plain. Falls back to plain text if the libs don't load.

24. ~~**Operator-spawned ephemeral consult agents.**~~ ✅ Done. The operator can spin up a disposable agent to
    ask questions / get feedback (e.g. OS-specific path validation) — on **core** or, for OS-specific
    answers, on a **node** (runs there, with that OS/filesystem). Design (agreed):
    - **Disposable, multi-turn, never persisted.** A real bus participant you converse with
      (`@consult-…`), running in `SessionMode: Ephemeral` (no session id on disk); `/end` tears it
      down and it's *gone*. Since the transcript vanishes, **outcomes must be captured deliberately**
      to the blackboard (`note`) or task board (`post_task`) — nothing is auto-saved.
    - **Full bus citizen.** Has the normal MCP tools, so it can ask other agents
      (`@consult-linux ask @macos whether …`) and read replies.
    - **Naming/addressing** — location-encoded, deduped ids handed back at creation: `/consult`
      → `consult-core`; `/consult @<node>` → `consult-<node>` (e.g. `consult-linux`); a second on the
      same target → `consult-linux-2`. Address with the normal `@id` semantics; ephemeral consults
      show in the fleet tagged (so they're discoverable) and `/end <id>` removes them.
    - ✅ **core-local** — `/consult [family]` spawns `consult-core` (deduped) via `spawnLocalAgent` in
      `SessionEphemeral`; `/end <id>` cancels the session + `mcp.Server.Unregister`s it. Lifecycle
      is **journaled** (`consult_opened`/`consult_closed`) so the TUI and web both show it; fleet tags
      consults with `⟳`. Wired through `operator.Services` → `tui.Deps`.
    - ✅ **node-targeted** — `/consult @<node> [family]` registers the consult on the bus and queues
      an open command to the node (`Core.QueueConsult` → `HeartbeatResponse.Consults`); the node
      spawns it on its own ephemeral proxy (its OS/filesystem), ships events + pulls its inbox like
      any agent, and tears it down on the close command. `Core.NodeOnline` gates with a clear error.
      Tests: `TestConsultCommandDelivery`, `TestConsultCommands`.
    - ✅ **web / remote-TUI triggering** — operator API `/operator/consult` + `/operator/close`
      endpoints; the `operator.Client` exposes them so `avairy-tui` drives it, and the web composer
      gained `/consult [@node] [family]` and `/end <id>`. So both consoles can open/close consults,
      not just render them. (Operator command is **`/end`**; the HTTP route stays `/operator/close`.)
    - ✅ **peer discovery** — a `list_agents` MCP tool returns the other agents on the bus (id +
      roles + caps like `os`), excluding the caller, so an agent can find the right peer to
      `send_message` (e.g. "who's on linux?") instead of guessing ids. In all role prompts. Test:
      `TestListAgentsTool`.

### Robustness / scale (next)

25. ~~**Bus hardening — stop reply-storms.**~~ ✅ Done. Decoupled **delivered** from **triggers a
    turn** so a broadcast/role message no longer wakes the whole fleet into simultaneous turns:
    - **Direct** (`to: agent`) → wake & act. **Broadcast/role** → *context-only* (delivered to the
      inbox, no auto-turn) **except from `human`/`facilitator`**, which still wake (sender-aware: the
      storm source is agent→broadcast loops; operator/facilitator broadcasts stay bounded).
    - **Reply budget** — per-agent cap on *autonomous* (agent-originated) direct wakes within a
      window; beyond it, further agent messages are context-only until it goes quiet. (Realizes the
      "hop budget" intent without per-message reply-lineage threading, which the architecture doesn't
      carry — same runaway protection, simpler.)
    - **Dedup** — drop identical `(from,to,body)` within a short window.
    - Enforced at the **activation points** (a per-agent `bus.Waker` in the runner + node pull-loop)
      for the wake policy + budget; **dedup at the bus** (`publish`). Direct semantics and
      `read_inbox` unchanged; `InboxMessage` gained `ToKind` so the node can apply the policy.
      Tests: `TestWakerPolicyAndBudget`, `TestPublishDedup`.

26. ~~**Cost overview + budget guardrails.**~~ ✅ Done. Two halves:
    - **Overview (display).** Per-agent spend now shows in the fleet line (TUI + web), alongside the
      fleet total. Computed client-side by folding `turn_done` usage by actor off the journal stream,
      so it works identically in-process and remote — no operator-API plumbing.
    - **Guardrails (enforcement).** A core-side `cost.Monitor` (`internal/cost`) folds per-agent +
      total spend (cost & tokens) off the journal and, when `-agent-budget`/`-budget` (USD) is set
      and crossed, fires once: journals a `budget_exceeded` system event (rendered as a warning in
      both consoles, and the over agent's spend turns amber `⚠`) and `bus.Interrupt`s the runaway —
      the agent for an agent cap, broadcast for the fleet cap. Combined with the #25 wake-rate budget
      this caps a fan-out's burn. Tests in `cost_test.go` (accumulation + fire-once per scope).
      (Soft stop: interrupt halts the current turn; a hard "won't wake until the cap is raised" would
      need cross-process suppression at the activation points — a follow-up if needed.)

27. ~~**Blackboard view in the operator console.**~~ ✅ Done. A read view over `board.Blackboard`:
    a **Notes** tab in the TUI (key · author + text, sorted by key) and a **Notes** panel in the web
    left rail. Threaded `operator.Services.Notes` → `State.Notes` → client cache → `tui.Deps.Notes`
    (so it works in-process and remote, polled with the rest of the state). Test: round-trip asserts
    notes reach the client. (Read-only — agents still write via the `note` MCP tool.)

28. ~~**Idle teardown / lazy worker spawn.**~~ ✅ Done (core-local agents). New `internal/supervisor`
    owns a core agent's session lifecycle: it holds the bus subscription and drives the session like
    the runner, but adds an idle timer — after `-idle-sleep <dur>` quiet (and not mid-turn) it closes
    the subprocess (journals `agent_sleeping`, fleet shows it via a `◐` dot / "sleeping" status) and
    **lazily respawns** it (journals `agent_awake`) on the next wake-worthy directed message (same #25
    Waker gate, so broadcast chatter doesn't wake a sleeper). `idle == 0` ⇒ never sleep ⇒ behaves
    exactly like a runner, so all core-local agents (incl. #24 consults at `idle=0`) route through it;
    the family adapter + gate server are built once and reused across respawns. A crashed subprocess
    also drops to sleeping and respawns on demand. Default **off** (sleep drops in-session context;
    the agent re-reads the blackboard/journal on wake). Tests: `supervisor_test.go` (deliver-as-runner,
    sleep+respawn) with `-race`.
    - **Node agents too** ✅: `avairy-node -idle-sleep <dur>` mirrors the state machine over the HTTP
      pull/post transport (no local bus). The node reports `sleeping`/`awake` over the events channel;
      core translates those two pseudo-types into the same `agent_sleeping`/`agent_awake` system events
      the consoles render. A node respawn **resumes** the agent's session (ResumeID), so context
      survives sleep for families that support `--resume` — better than core-local (which loses it).
      Test: `TestE2E_NodeSleepLifecycleSurfaces` asserts the wire translation over real HTTP.

29. ~~**End-to-end distributed integration test.**~~ ✅ Done. New `internal/e2e` package: a black-box
    test that stands up a real core (bus + MCP bus HTTP server + control HTTP server + canonical hub)
    and a real `control.Node` talking over actual `httptest` HTTP, driving a mock agent through the
    node's pull/post loop (the same two goroutines `avairy-node` runs) — zero credits. Three tests:
    `TestE2E_MessageRoundTrips` (human→bus→MCP inbox→node→agent→PostEvents→core journal),
    `TestE2E_FileSyncRoundTrips` (file up to the hub and back down to a second node), and
    `TestE2E_ConflictRaisesAndResolves` (a diverged node raises a startup conflict to the operator,
    whose resync verdict rides the heartbeat back and reconciles it to canonical). Wired exactly as
    `cmd/avairy` does (`OnEnroll`→`RegisterAgent`, `InboxDrainer`→`DrainInbox`, `OnNodeConflict`).
    Passes under `-race`.

30. ~~**Browser-client mTLS + install option.**~~ ✅ Done (PWA optional, deferred). The web console
    (and `avairy-tui`, curl, …) can authenticate by **mTLS client certificate** instead of the URL
    token: the operator-API auth accepts a verified **operator** cert and falls back to the token.
    - Operator certs carry a **distinct SAN** (`avairy-operator:<name>`) so a node cert — though
      CA-signed — can **not** authenticate to the operator API (`control.OperatorIDFromCert`).
    - **`avairy mint-web-cert`** issues an operator cert from the self-managed CA and writes a
      password-protected **PKCS#12 (`.p12`)** (cert + key + CA chain, via `go-pkcs12`) to import into
      a browser/OS keychain. Then open the console with **no `?token=`** — the cert authenticates.
    - Tests: `TestOperatorCertDistinctFromNode`, `TestOperatorP12RoundTrip`, `TestOperatorMTLSAuth`
      (operator cert authes token-less; node cert + no-cert are rejected).
    - **Deferred (optional):** PWA-installable console (manifest + service worker + icons) — the
      `.p12` is the substantive "install option"; PWA is a UX nicety we can add later.

### Operator console & feedback (recent)

31. ~~**Live current-action spinner.**~~ ✅ Done (web). Each working agent's in-flight tool card
    shows a corner spinner (solid accent border), cleared the instant it does anything else / its
    turn ends / on Stop — so with several parallel agents you see each one's current action.

32. ~~**Single port for everything.**~~ ✅ Done. The MCP bus is mounted at `/mcp` on the control
    listener (mTLS), so control + operator API + bus share one reachable port; `--control-addr` was
    replaced by `--advertise` / `--advertise-port` (bind `0.0.0.0:7700` by default). `core add-node`/
    `add-operator` drop `--core`/`--mcp`; `node join` drops `--core-mcp` (defaults to `--core`).

33. ~~**Node reports agent family + model.**~~ ✅ Done. Enrollment caps now carry `family` (and
    `model` when pinned), flowing to `NodeInfo.Caps`, the `node_enrolled` journal record, and
    `list_agents`. An agent-driving node also shows **online the moment it enrolls** (TUI + web),
    instead of waiting for its first turn — keyed on the `family` cap; proxy-only nodes stay quiet.

34. ~~**Loop detection — content/region aware.**~~ ✅ Done (extends #14). The signature
    (`agent.ActionKey`) now folds in a digest of the edit content and the read region, so
    edit→read→edit with *different* edits isn't flagged as an A↔B loop; a repeated identical edit or
    re-reading the same span still is. `TrimInput` leaves a `_digest` (and a capped `_diff`) behind.

35. ~~**Quick-feedback reactions on agent messages.**~~ ✅ Done (web + TUI). 👍/👎 deliver
    context-only feedback the agent sees on its next turn without interrupting (a new `bus` NoWake
    delivery the drivers don't wake on); ❌ hard-stops it and steers a reconsider. Only the last 5
    text messages per agent are reactable (server-enforced). Web: hover buttons + persistent badge.
    TUI: `/react up|down|reject [@agent]`.

36. ~~**Present the requested patch (edit diffs).**~~ ✅ Done (web + TUI). A unified diff travels
    with file edits (`agent.PatchPreview`/`ToolDiff`, all families) — through gating →
    `control.Approval` → the consoles. Reviewable in a scrollable modal: web has a "▸ diff" link on
    edit approval cards **and** on every edit in the transcript; the TUI opens an overlay modal
    (lipgloss Canvas/Layer + a `viewport`) via `d` on an edit approval or `/diff [@agent]`. The
    allow-once / allow-for-session / deny set already existed (#6); this adds the patch presentation.

### Backlog (next)

37. **TUI mouse + viewport-backed conversation.** The conversation is still a hand-rolled `[]string`
    with a manual `scroll int`; migrate it to a `bubbles/v2 viewport` (mouse-wheel scrolling, cleaner
    windowing), then enable mouse input (`tea.WithMouseCellMotion` + zone hit-testing, e.g.
    `bubblezone`) for clickable diff links, 👍/👎/❌ reactions, and Allow/Session/Deny — point-and-
    click parity with the web. Do the viewport migration first; it's what makes inline per-message
    click affordances clean.

### Single operator

13. The TUI is single-operator by design (v1). Multi-operator is out of scope for now.
