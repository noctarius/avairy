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
- **File sync hub** (§9): per-file hub versions, stat-skip change detection, idempotent push,
  LF/mode normalization, `.gitignore`/`.dockerignore`/`.avairyignore` excludes. Conflicts are
  *detected* and surfaced (see backlog #3 for the missing resolution half).
- **Facilitator** (§5): journal-driven blocked/loop detection → rule-based nudges.
- **Human-in-the-loop gating** (§7): gated actions block and route to the operator's
  **Approvals** tab (allow/deny); Claude via PreToolUse hook, Codex via app-server approvals;
  unanswered requests fail closed. Layered timeouts (hook 300s > shim 290s > broker 280s).
- **TUI** (§3): fleet/cost, conversation, handovers, task board, approvals; human injection
  (steer/interrupt), enroll-token display/rotation.
- **Event-sourced journal** (§10): durable append-only `.avairy/journal.jsonl`.

## Backlog

Ranked roughly by value-to-effort within each group.

### Designed but not built (large)

1. **Git integration (§9) — entirely missing.** No code behind this whole section:
   - `git_history(...)` MCP tool — history reads on any node for root-cause analysis.
   - `request_commit(paths, message)` MCP tool — gated, executed **core-only with signing**
     (signing keys never ship to nodes).
   - disposable scratch worktree for bisect/checkout (read-only history mirror).
   - TUI commit affordance.
   - *Today only `.gitignore` parsing touches git.*

2. ~~**Hub persistence.**~~ ✅ Done. The hub snapshots to `.avairy/hub.json` (atomic
   temp+rename), restored on startup via `LoadHub`; persisted every 5s if dirty and on clean
   shutdown. The seed NodeView calls `ResumeFromHub` so a restored hub isn't re-conflicted or
   false-deleted (adopts versions only for files still present locally). *Future:* the snapshot
   is one JSON blob (whole-tree rewrite); a git-backed / per-file store would scale better.

3. **Conflict reconciliation routing.** Concurrent divergent edits are detected, journaled,
   and printed (`CONFLICT … needs reconciliation`), and `Hub.Resolve` exists — but nothing
   drives a resolution. The "surface" half is done; the "route to an agent/human to reconcile"
   half is not, so a real conflict sticks.

### Gating — finish what's started

4. ~~**Copilot & Grok aren't gated to the human.**~~ ✅ Done. `copilot.New(decide)` /
   `grok.New(decide)` now take a decider (nil → fail-closed); the node path passes
   `gateDecider` and local `-live` passes `localGateDecider`, so ACP
   `session/request_permission` requests reach the operator's Approvals tab like Claude/Codex.

5. **Local `-live claude` gating.** No local `/gate` endpoint (only the node serves one), so
   local Claude relies on `--allowedTools`, not the broker.

6. **`AllowForSession`.** The decision constant exists but the TUI only offers allow/deny, so
   the human re-approves identical actions every time. No "allow this kind for the session."

7. **Live `--settings` hook validation.** The shim + policy + broker are tested, but a live
   `claude` run actually parsing the injected `--settings` and calling the hook is unverified.

### Robustness / operational

8. **Channel TLS.** node↔core is plain HTTP; enrollment tokens and full file contents cross
   the wire in cleartext. Production flips to TLS (node→core outbound, NAT-friendly).

9. ~~**Dead-node detection.**~~ ✅ Done. `Core.RunLiveness` marks a node offline when its
   heartbeats lapse past `LivenessTimeout` (15s) and online again on return, journaling each
   transition; the TUI fleet shows offline agents with a `⊘` dot. Built on the existing
   node→core heartbeat (no new keep-alive). *Still open:* core doesn't know a node's heartbeat
   interval, so `LivenessTimeout` must exceed it (fine at the 2s default).

10. **fsnotify.** Sync is poll-based. Pattern: fsnotify as the trigger (kills latency + idle
    CPU) + a coarse fallback poll for the new-subdir race / dropped events / network FS.
    Supported on all targets (Linux inotify, macOS/BSD kqueue, Windows ReadDirectoryChangesW).

11. **Facilitator debounce + matchmaking.** Works cross-node, but no rate-limit/debounce — a
    repeatedly-`blocked` agent re-nudges on every status report (the loop trigger self-resets,
    `blocked` doesn't). Matchmaking (`neededCap`) is OS-keyword-only.

12. **State-resume from journal.** The TUI backfills its view, but a restarted core/agent
    doesn't reconstruct task-board / session state from the journal.

### Single operator

13. The TUI is single-operator by design (v1). Multi-operator is out of scope for now.
