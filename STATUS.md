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

7. **Live `--settings` hook validation.** The shim + policy + broker are tested, but a live
   `claude` run actually parsing the injected `--settings` and calling the hook is unverified.

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
   - ✅ **MCP bus TLS too**: local agents get a dedicated plain loopback listener (never need
     TLS), while the remote-facing bus on `-mcp-addr` is served TLS with the same self-CA cert;
     a node's MCP proxy reuses its CA-trusting transport to reach it. So a remote agent's
     inter-agent traffic is no longer cleartext either.

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
    On a hit the facilitator auto-runs a fresh look (#0). Both deterministic cycle cases are
    covered; two loop *kinds* the period detector inherently can't see remain open:

    - **(a) Circling without a clean period** — *deterministic, buildable now.* An agent churns
      the same few actions in no fixed order (`A B C  A D B  A C B …`) — stuck, but no period, so
      `trackLoop` stays silent. Fix is a **novelty/progress** signal (not periodicity): track the
      set of distinct action signatures over a window and flag when the agent produces **no new
      (never-seen) action** for N steps. Tune the window so a repetitive-but-productive phase
      (edit many files, rerun the same test) doesn't trip it.
    - **(b) Semantic loops** — *needs an LLM.* Detection is exact string match on
      `tool:<name>:<arg>`, so conceptually-identical-but-textually-different steps slip through
      (`go test ./a` ↔ `go test ./b`; the same fix in different files; "try X" ↔ "attempt X
      again"). Catching "same intent, different surface form" needs judgment → an **LLM `Nudger`**
      (the design's pluggable seam, `RuleNudger` today) periodically asked "is this agent making
      progress or circling?". It's just another trigger feeding the existing fresh-look
      intervention (#0), so the deterministic detector handles cheap cases and the LLM the rest.

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
    - **(b) core serving + remote TUI** — `avairy -control-addr … -headless` serves the **operator
      API** (`/operator/*`) on the control listener, sharing its TLS. `avairy-tui -join-file
      .avairy/operator-join` (or `-core/-token/-ca`) attaches and renders the identical UI.

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

### Single operator

13. The TUI is single-operator by design (v1). Multi-operator is out of scope for now.
