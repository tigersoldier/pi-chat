# pi-chat — Milestones & Working State

Reference file for resuming work after a context reset. Design and rationale live in
[`DESIGN.md`](./DESIGN.md); this file is the operational plan, the environment facts, and
the current position. **Update the status block and checkboxes as work lands.**

Last updated: 2026-09-28 · repo `/home/pi/code/pi-gchat` (module
`github.com/tigersoldier/pi-chat`) — M0–M4 done: the bot answers mentions and commands in
Slack threads through `pi-gatewayd`, with durable thread state and provisioned
workspaces.

---

## Status snapshot

| Item | State |
|---|---|
| Design | **Settled.** All 14 questions answered; `DESIGN.md` rewritten for the pi-gateway-based, Slack-first scope |
| M0 — gateway running | **Done and verified** (see M0 below) |
| M1 — Slack app | **Done for phase 0, verified live** (see M1 below): Socket Mode socket opens, bot token authenticates, phase-0 scopes granted. The phase-1 scopes are still to be added in one reinstall |
| M2 — repo prep | **Done** (see M2 below). Module is `github.com/tigersoldier/pi-chat`; no Google Chat artifact and no GCP dependency remains |
| M3 — phase-0 vertical slice | **Done and verified end-to-end** on 2026-09-27: a mention in a Slack thread streamed a real pi answer back into it (see M3 below) |
| M4 — phase-1 MVP | **Done and verified live** on 2026-09-28: commands, allowlist refusals, SQLite thread state, provisioned project directories, cold/resume, startup GC (see M4 below) |
| Code | `internal/config`, `internal/store` (SQLite), `internal/workspace` (provisioning + git cleanup), `internal/bot` (core), `internal/slack` (adapter), `cmd/pi-chatd`; five packages of tests |
| `pi-chatd` | **Installed and running** as a systemd user unit (`packaging/pi-chatd.service`, `make install install-unit`), enabled at boot |
| `pi-gatewayd` | **Installed and running** as a user unit; built from `~/code/pi-gateway` @ `119f0f2` (= tag `v0.1.2` content), self-reports `0.2.0` |
| GitHub repo rename | **Not done** — browser action; local `origin` still says `pi-gchat`, which keeps working through GitHub's redirect |
| Upstream rebind fix | **Not filed.** Owner is the user |

**Current position: M4 is done and verified live. Next is M5 (phase 2): `/delete` with
button confirmation and worktree cleanup, `/abort`, `/model`, interactive approvals
(buttons + modal via `pending_ui`), and the warm-session cap with eviction and the bounded
queue.**

---

## Milestones

### M0 — Gateway running (blocking prerequisite) — ✅ DONE

Prove the session plane end to end with no Slack code at all.

- [x] M0.1 Built and installed `pi-gatewayd` into `~/.local/bin` from `~/code/pi-gateway`
      @ `119f0f2` (byte-identical to tag `v0.1.2`); `--version` prints
      `pi-gatewayd 0.2.0 (gateway protocol 1)` — note the daemon's version const is
      ahead of the last tag, so the string is not the release name
- [x] M0.2 Provisioned two tokens into `~/.config/pi-gateway/tokens.json` and copied the
      values to `~/.config/pi-chat/gateway-{admin,thread}.token` (0600): `pi-chat-admin`
      (`role=admin`) and `pi-chat-thread` (`caps=observe,interject,prompt,ui,control`)
- [x] M0.3 Installed the upstream user unit plus a drop-in forcing
      `--pi %h/.local/bin/pi` and an explicit `PATH`; enabled at boot; running on
      `127.0.0.1:7331`, debug listener on `127.0.0.1:7332`, pi `0.85.1`
- [x] M0.4 Wrote `~/.config/pi-chat/config.toml` (0600) per DESIGN §11;
      `slack.access.allowed_users` still empty pending M1
- [x] M0.5 `tools/gateway-probe` (`go run .` there) → **PASS**: admin token granted
      `observe,interject,prompt,ui,control,admin` and started session-less; thread token
      granted the five expected capabilities and `gw_new_session` was refused with
      `forbidden`; `SwitchSession` + `Prompt` on the second connection worked;
      `set_thinking_level` proved `control`; `gw_delete_session` (force=false) unbind both
      connections and a later prompt was refused with `unknown_session`; catalog row gone

**Incident during M0.3:** a leftover daemon from the earlier v0.1.2 e2e validation
(`/tmp/e2e/bin/pi-gatewayd`, up for a day) held port 7331 and made the unit restart-loop.
Killed it; no other instances remain.

### M1 — Slack app (user, browser, ~10 min)

Full walkthrough: **[docs/slack-app-setup.md](docs/slack-app-setup.md)**. The app is
configured by pasting **[slack/manifest.yaml](slack/manifest.yaml)** into Slack's app
manifest editor, so scopes, events, Socket Mode, Interactivity, the Messages Tab and the
`/pi` command are applied in one step. `slack/manifest-agent.yaml` is the variant that
declares the app as a Slack agent (see [slack/README.md](slack/README.md)).

- [x] M1.1 App created from scratch; **Socket Mode enabled**; app-level token with
      `connections:write` → `~/.config/pi-chat/slack-app-token`
- [x] M1.2 Bot scopes, phase 0: `app_mentions:read`, `chat:write` (verified granted)
- [x] M1.3 Subscribed to the `app_mention` bot event (no Request URL under Socket Mode)
- [x] M1.4 Installed to the workspace → `~/.config/pi-chat/slack-bot-token` (0600);
      `[slack.access] allowed_users` populated; both token files verified as real
      (correct prefixes, no trailing newline, `chmod 600`); app must still be invited to
      a channel with `/invite @pi` if that has not happened yet
- [ ] M1.5 Apply the remaining scopes, events and settings in one paste: app →
      **Settings → App Manifest** → replace with `slack/manifest.yaml` → Save → reinstall.
      Slack shows a diff first. This covers `channels:history`, `groups:history`,
      `im:history`, `mpim:history`, `files:read`, `reactions:write`, the four
      `message.*` events, the Messages Tab, Interactivity and the `/pi` command
- [ ] M1.6 Optional, after M1.5: decide on the agent variant. It adds
      `assistant:write` and the agent events for a DM timeline, real agent status
      (`agents.sessions.setStatus`) and a stop button, at the cost of requiring the
      workspace's agent feature (`feature_disabled` elsewhere) and an irreversible switch

**Verified on 2026-09-26** with read-only API calls (`auth.test`,
`apps.connections.open`); tokens were passed via a stdin curl config so they never
appeared in `ps`:

| Fact | Value |
|---|---|
| Workspace | `Test` (`T6K8Y3FRR`) |
| Bot user | `U0C4TM8KT5F` — the mention is `<@U0C4TM8KT5F>` |
| Granted scopes | `app_mentions:read`, `chat:write` — phase 0 only, so M1.5 is still open |
| Socket Mode | `apps.connections.open` returns ok, so the app token can open a real socket |
| Allowed users | the owner's member ID is set in the live config |

**Exit:** met for phase 0, which is what M3 needs. M1.5/M1.6 cost one reinstall.
**Owner:** user.

### M2 — Repo prep (mechanical, independent of M0/M1) — ✅ DONE

- [x] M2.1 Module renamed to `github.com/tigersoldier/pi-chat`; the GitHub repo rename is
      still **the user's browser action** — the local `origin` URL stays on `pi-gchat`,
      which keeps working through GitHub's redirect after the rename
- [x] M2.2 Deleted every Google Chat artifact: `cmd/probe`, `scripts/`,
      `docs/`, `assets/`, `.github/workflows/static.yml`,
      `pi-gchat.toml.example`, `PRIVACY.md`, `TERMS.md`
- [x] M2.3 Dropped the GCP/Pub/Sub tree — `go.mod` now requires only
      `BurntSushi/toml`, and `go.sum` has no Google entries. The `pi-gateway` require
      is deferred to M3 on purpose: nothing imports `gwclient` yet and `go mod tidy`
      would drop it again. `go.mod` carries the commented `replace` line to develop
      against `~/code/pi-gateway`
- [x] M2.4 Rewrote `Makefile`, `README.md` (Slack-first, prerequisites, the command
      grammar), `.gitignore` (credentials and `*.token` are never committable) and
      `pi-chat.toml.example`
- [x] M2.5 `go build ./...`, `go vet ./...` and `go test ./...` are clean

**Beyond the spec:** the module would otherwise have held no packages, so M2 also added
`internal/config` (loader, defaults, `~` expansion, strict validation, unknown-key
rejection, redacted `Summary`) with unit tests, and `cmd/pi-chatd` with `--check` and
`--version`. This is the config half of M3.1; `make check` already validates the live
file. Runtime logging must still use `log/slog` in M3 — the `fmt` prints in `--check`
output are the command's product, not daemon logs.

### M3 — Phase 0 vertical slice (DESIGN §13)
One allowed user, one channel, **no database, no policy**. Thin on purpose: it touches
both unknowns (Slack ingress/egress and the gateway seam) in one runnable path.

- [x] M3.1 `cmd/pi-chatd` skeleton: config load, slog logging, signal handling — run as a
      systemd user unit (`packaging/pi-chatd.service`, `make install install-unit`)
- [x] M3.2 `internal/slack`: Socket Mode connect, `apps.connections.open`, envelope ack
      immediately (well inside 3 s), mention parse, thread key
      `{workspace}:{channel}:{thread_ts}`, `chat.postMessage`. Verified live: `auth.test`
      reports workspace `Test`, and the socket connects (`app=A0C4RJKV8P4`,
      `connections=1`, self-refresh in 50 m, ping keepalive every 30 s)
- [x] M3.3 `internal/bot`: throwaway admin connection → `gw_new_session` →
      `SwitchSession(path)` → `Prompt`, events via the connection's `OnEvent` handler,
      author answer from `GetLastAssistantText`
- [x] M3.4 Render the turn into the thread — `chat.startStream` / `appendStream` /
      `stopStream` with a patched-placeholder fallback, decided per turn and remembered
      per process
- [x] M3.5 **Deliverable: a streamed pi turn visible inside a Slack thread** — verified by
      the owner on 2026-09-27: `@pi hello` in a channel thread produced the answer in that
      thread. Log evidence: session
      `slack-t6k8y3frr-c0c4tmqnr8r-1790529157-569279` created in `~/work`, **no streaming
      warnings** (so `chat.startStream` accepted the recipient fields on a thread reply),
      and three `setStatus` frames from `pi-lens-lsp` that nothing had to answer.

**Verified so far:** `make live-test` (`PI_CHAT_LIVE=1 go test ./internal/bot -run Live`)
drives one real turn through the running `pi-gatewayd` with the configured tokens and a
real `pi`: session created, connection bound, prompt sent, deltas streamed, final answer
`"PONG"`. That covers M3.3; M3.5 is the same path with Slack on both ends.

**Three bugs the live runs caught** (all fixed and pinned by tests):

1. `message_end` was treated as the end of the turn, but one prompt emitted *two*
   messages — the flusher stopped on the first one and dropped the answer entirely.
   Terminal signals are `agent_settled` / `gw_turn{state:"settled"}` (DESIGN §4).
2. The flusher only rendered on its ticker, so a whole answer that arrived inside two
   ticks (all four deltas in 21 ms of a 2.4 s turn) was never rendered. It now renders on
   the way out too (DESIGN §4).
3. Every `extension_ui_request` was logged as "pi asked for UI input, which phase 0
   cannot answer". Only *dialogs* need an answer; `setStatus`/`notify`/`setWidget` are
   fire-and-forget and arrive on every turn (`--approve` does not silence them). The
   adapter now splits them by method, so the journal has no false alarms (DESIGN §6).

**Settled by the live run:** Slack accepts `recipient_user_id`/`recipient_team_id` on a
`chat.startStream` that also carries `thread_ts`. The documented "required when streaming
to channels" is therefore about channel-level streams; the defensive retry-without-
recipient path stays as insurance and has never fired.

**Exit:** the slice runs against the real daemon and a real `pi`.
**Depends on:** M0, M1.

### M4 — Phase 1 MVP — ✅ DONE

- [x] Allowlist enforcement **before any side effect**; denial message. Refusals are
      ephemeral and name the fact, never the configuration
- [x] SQLite (`threads`, `seen`, `pending_ui`, `admissions`) — the whole schema in one
      artifact; `threads` and `seen` are written in this phase, the other two wait for the
      features that fill them. Persist before ack; a failed claim blocks the turn rather
      than risking a double prompt
- [x] Session provisioning: project dir `<date>-<slug>` (slug from the first prompt, a
      counter on collision), `injected_prompt` rendering, deterministic session names,
      `approvals = "auto"` adding `--approve`
- [x] Thread-scoped sessions; root is session-less; warm/cold idle-close; startup reset of
      the warm marker; stale-warm repair in the sweep
- [x] Prompt → streamed answer, cursor persistence (on settle and on the sweep), `Resume`
      on re-dial
- [x] Commands: `/pi help`, `/pi status`, `/pi resume` (picker with buttons), `@pi /status`,
      `@pi /<command>` pass-through, near-miss guard; reserved phase-2 commands are
      answered rather than forwarded to the agent
- [x] `--approve` mode; systemd user unit (landed early in M3); startup orphan GC
- [ ] Warm-session cap with eviction and the bounded queue — **moved to M5**; phase 1
      counts warm threads and warns at the cap

**What landed where:** `internal/store` (SQLite in WAL, one writer, schema version),
`internal/workspace` (provision, injected prompt, worktree cleanup, orphan sweep),
`internal/bot` (command vocabulary, gateway reads through a narrow `gateway` interface,
provisioning wiring, idle sweep), `internal/slack` (slash commands, buttons, DMs, plain
thread text, notices and ephemeral answers).

**Verified live** (`make live-test`, 3.7 s against the running daemon and real pi): the
first prompt created session `slack-t-live-c-live-…` in a project directory named from the
prompt (`~/work/2026-09-27-reply-with-exactly-the-single-word`) and streamed `PONG`;
closing the connection marked the thread cold; the next prompt re-bound **the same
session** at cursor seq 132 and answered `PONG2`; the cleanup deleted the session through
the gateway and removed the directory (`removed=true left=[]`) — which also exercises the
phase-2 delete path early.

**Bug a test caught:** the interaction `response_url` reply matched on the substring
`"ok"`, so `{"ok":false,"error":…}` — exactly what an expired response URL answers — read
as success. It now parses the answer, accepts both documented shapes (the bare `ok` and an
envelope), and never mistakes a failure for a success.

**Exit:** the slice runs against the real daemon and a real `pi`.
**Depends on:** M0, M1, M2, M3.

### M4 review findings — what they changed

Four independent reviews (design gap, correctness, tests, structure) ran against the M4
commit. They converged on the same two session-identity bugs, which is what made them
worth fixing before M5 builds `/delete` on top of this base.

**Fixed (each with a regression test):**

| Finding | Why it mattered |
|---|---|
| `ensure` trusted a connection that was alive but **unbound** | After an external `gw_stop_session`/delete the daemon unbinds clients without failing the connection, so the next prompt created a **second session** — the exact defect DESIGN §3 documents, and what invariant 1 exists to prevent. It now checks the binding, not just the connection |
| The 429 retry re-sent a **drained body** | An `http.Request` consumes its reader, so every retry posted no parameters, turning a rate limit into a failure |
| `threadFor` could **overwrite a row** it failed to read, and two concurrent new-thread turns could clobber each other's session identity | A lost row means a second session plus an orphaned worktree the next sweep would delete. A failed read is now an error, and new rows are written through the thread under its lock |
| The channel half of the allowlist refusal was **dead code** | A listed user in a disallowed channel was told to join the allowlist, and the journal recorded the wrong reason |
| `deleted` was never written | DESIGN §4 invariant 3 had no implementation: the row kept pointing at a tombstoned session. `forgetSession` now marks the thread deleted, keeps the key, and leaves the working directory alone |
| `/status` could **end a running turn early** | `get_state` also pokes gwclient's turn latch, which wakes `AwaitSettled`; the model is now read only between turns |
| Replay frames could leak into the first turn after a re-dial | The turn marks itself prompted before prompting, and anything earlier is dropped as a previous turn's |
| A failed session creation left its **project directory** behind | Only the next startup sweep would have found it |
| `update` flushed outside its lock | Two writes could land out of order, leaving the database with an older snapshot than memory |
| Dead code, two loaders for one row, `TeamID` vs `Workspace`, a hardcoded platform name, duplicated rune trimming | Structure findings; all removed or unified |
| Docs claiming unbuilt features (`files:read`, `reactions:write`, the agent events, `setStatus`/`rename`/`Abort`) | Now marked phase 3 in both manifests, the tutorial and DESIGN |

**Accepted, not fixed (with the reason):**

- **`cmd/pi-chatd` has no tests** (0% coverage) and the startup order — open store, reset
  warm markers, sweep, then socket — is verified only by running it. Worth a stub-`pi`
  harness (the approach in DESIGN §13) so the socket and the wiring are testable without
  a workspace.
- **No concurrency test drives two turns through one thread.** `-race` covers the code
  that runs, but nothing runs two goroutines against one thread; the lock discipline is
  documented and reviewed, not exercised.
- **Slack payload fixtures are invented, not recorded.** The shapes match the docs and the
  interactions seen so far, but a captured `app_mention`/`block_actions` sample would lock
  them to Slack rather than to our reading of it.
- **`progress_ts` is written and never read.** Re-attaching to a half-written reply needs a
  policy decision (a restarted process cannot resume a stream it never held), so the
  column waits for phase 3; DESIGN §7 says so.
- **The transport acks before `seen` is written.** Socket Mode demands an ack within three
  seconds, so the claim cannot precede it; a crash in that window loses a message Slack
  will not redeliver. DESIGN §7 records the trade-off.

### M4.6 — the agent surface

- [x] `slack/manifest-agent.yaml` is the recommended app: Slack's agent messaging
      experience, with the plain manifest kept for workspaces that cannot have it
- [x] Status follows the turn: `agents.sessions.setStatus` on `busy` → `idle`, mapped from
      a platform-neutral vocabulary, best-effort, and asked once when the install cannot
      show one (`feature_disabled`, `missing_scope`)
- [x] The stop button works: `agent_session_stopped` → `ActionStop` → abort the session,
      with a stale status cleared when nothing was running
- [x] A DM message is a prompt: a plain message in a DM or group DM now roots a thread,
      because a conversation the bot was added to has no ambiguity about who is being
      addressed — and because a suggested prompt arrives exactly that way
- [x] Suggested prompts are computed from `paths.repos_root` when the user opens the
      conversation, not declared in the manifest, and nothing is offered when there is
      nothing to name (`assistant:threads.setSuggestedPrompts` needs `assistant:write`
      and, in an agent app, must omit `thread_ts` — Slack fails the call silently if it is
      present)

**Exit:** the status appears in Slack and the stop button stops the turn.
**Depends on:** M4.

Not built here, because each belongs with the thing that needs it: `agents.sessions.rename`
titles, `suspended` while a turn waits for an approval (M5), `closed` on `/delete` (M5).

### M4.7 — the thread conversation, and the instruction that explains it

Agreed in the 2026-09-27 interview (DESIGN §4 lists the decisions, and which parts are
still open). The upstream half is settled: pi-gateway v0.1.3 shipped durable spawn
configuration and removed the `inject` command, because no released pi exposes the
primitive over RPC — so the instruction is client-side, and there is nothing left to wait
for.

- [x] Upgrade: build and install `pi-gatewayd` from v0.1.3 (`119f0f2` was running), bump
      `go.mod` to `v0.1.3` (pi-chat compiles and its tests pass against it), and confirm
      `gw_welcome.features` carries `spawn_config`
      — done and verified: the daemon is v0.1.3 (`a47e0ad`), features report
      `["spawn_config"]`, a `--append-system-prompt` survives a daemon restart (the
      catalog still reports it, i.e. it came from the sidecar), `go test ./...` and
      `-race` are green, and the live test passes against the new daemon. Verification
      also found a bogus sidecar keyed by the daemon's cwd on every `gw_new_session`,
      which `delete` cannot remove — written up in the proposal's Outcome section and
      [filed as pi-gateway issue #1](https://github.com/tigersoldier/pi-gateway/issues/1).
- [ ] The instruction: add it to the `--append-system-prompt` that `bot.piArgs` already
      builds for sessions we create, and prefix it to the first prompt we send in an
      adopted session inside `<slack-specific-instructions>` markers — re-sent after an
      observed `compaction_end`, never per turn
- [ ] Observation: fetch the thread with `conversations.replies` from the previous turn's
      trigger, fold the labelled transcript into the prompt, bound it, and write the
      omitted text to a per-thread file (DESIGN §4, decisions 4 and 5)
- [ ] Grammar: DM messages turn, channel-thread plain text is observed only, mentions
      register a thread without provisioning, and a top-level DM message starts a new
      session with a visible notice
- [ ] `users:read` in both manifests for `[Name (U123)]` labels; the app must be
      reinstalled for the scope change
- [ ] Use the new client surface where it simplifies what exists: `unbound: true` from
      stop/delete (gwclient now clears its binding), and `spawn` from the catalog to tell
      whether a session already carries our instruction

**Exit:** a channel thread where the bot answers mentions, sees what other people said
since its last turn, and knows what the transcript means — with the instruction installed
once, not per turn.
**Depends on:** M4.6, pi-gateway v0.1.3.

### M5 — Phase 2

- [ ] `@pi /delete` with button confirm → `DeleteSession` → worktree/branch cleanup → row
- [ ] `@pi /abort`, `@pi /model`, `@pi /stop`
- [ ] Interactive approvals: buttons + modal via `trigger_id`; `pending_ui` → `RespondUI`
- [ ] Status `suspended` while a turn waits for an answer; `closed` on delete; titles via
      `agents.sessions.rename`
- [ ] Warm-session cap (default 8) with evict-idle → bounded queue → refuse; never force;
      skip attached sessions (DESIGN §8)
- [ ] Streaming → patch fallback hardening

### M6 — Phase 3

- [ ] Message shortcut for in-thread discovery; `setTitle`/`setStatus`; suggested prompts
- [ ] Image/attachment passthrough; `/pi resume` picker polish; metrics

### Parallel — upstream fix (user, in the pi-gateway repo)

- [ ] Set the daemon's "must rebind explicitly" flag for `stopped`
      (`reason: requested|forced`), not only `deleted`; distinct message for
      stopped-vs-deleted. Repro and detail: DESIGN §3 "Pending upstream ask"

Not required for M3, but required before `@pi /delete` and eviction (M5) are safe;
`DESIGN.md` §4's attach-before-prompt invariant is the interim guard.

---

## Environment facts

| Thing | Value |
|---|---|
| Repo | `/home/pi/code/pi-gchat`, origin `git@github.com:tigersoldier/pi-gchat.git` |
| Module | `pi-gchat` (to become `github.com/tigersoldier/pi-chat`), Go 1.26.4 |
| Design | `DESIGN.md` (14 sections + decision log §15) |
| `pi` binary | `~/.local/bin/pi` |
| `pi-gatewayd` | **not installed**; build source `/tmp/pigw` @ `v0.1.2` |
| Gateway state | `~/.config/pi-gateway` (port + tokens + `tokens.json`) |
| Bot config | `~/.config/pi-chat/config.toml` (0600), state in `~/.local/state/pi-chat/` |
| Scratch gwclient consumers | `/tmp/gwtest2` (earlier validation) |

### Upstream facts to keep in mind (verified against v0.1.2, M0 probe)

- Exported packages usable: `gwclient`, `protocol`, `config`, `piargs`.
- `admin` required for `gw_new_session` / `gw_stop_session` / `gw_delete_session`;
  `control` for `set_model`, `compact`, `set_session_name`. `operator` lacks `control`,
  so the thread token is minted from explicit capabilities, not the role.
- One connection binds to exactly one session; `SwitchSession` rebinds, and
  **`gw_new_session` rebinds the connection that issued it**. There is no unbind
  command: only closing the connection drops the binding. **Consequence: the admin
  connection must be throwaway, not pooled** — otherwise every session it created keeps
  a phantom attached client and can never be evicted (`session_attached`).
- `extension_ui_request` is one frame type with two meanings, split by `method`:
  dialogs (`select`, `confirm`, `input`, `editor`) are routed to the turn's author
  (`gw_turn.author`) with a fallback to the most recently active `ui` client and
  `ui_stale` for non-owners; fire-and-forget (`notify`, `setStatus`, `setWidget`,
  `setTitle`, `set_editor_text`) are broadcast and must never be answered. `--approve`
  does **not** stop `setStatus`/`setWidget` frames. `statusText` carries raw ANSI SGR
  sequences — strip them before rendering.
- Delete tombstones the path for the daemon's lifetime; a repeat delete/attach answers
  `unknown_session`; session file removed after pi is reaped.
- Stop/delete **unbind** connections rather than closing them; attached clients never
  block a delete; `session_attached` / `session_busy` gate a non-forced stop;
  `defaultStopGrace = 5s`.
- Still open upstream (bot-side mitigations are ours): durable creator tags (in-memory),
  prompt idempotency, connection caps.
- v0.1.2 shipped the delete/stop requirement this project specified, including
  `TestDeleteUnbindsEveryClient`, `TestDeleteWaitsForPiReap`, `TestConcurrentAttachAndDelete`,
  `TestCommandsAfterDeleteDoNotCreateASession`, tombstones and the bridge `errSessionDeleted` fix.

### Slack facts that constrain the design

- **Developer slash commands cannot be invoked in message threads** (built-ins and Giphy
  only), and the slash-command payload has no `thread_ts`. This forces the two-form
  grammar: `/pi <cmd>` at root, `@pi /<cmd>` in threads. Solved — see `DESIGN.md` §5.
- Socket Mode apps cannot be Marketplace-listed; Socket Mode distributes events across
  connections, so the bot must stay single-instance.
- Slack retries events by `event_id` — the reason `seen` is mandatory.
- Streaming: `chat.startStream`/`appendStream`/`stopStream`, chunks accept
  `markdown_text` and `task_update` (256-char limit) with `task_display_mode`; channel
  streaming needs `recipient_user_id` + `recipient_team_id`; `assistant.threads.setTitle`
  needs `assistant:write`, while `assistant.threads.setStatus` accepts
  `chat:write` (Slack is narrowing it to `chat:write` alone), and Slack is migrating
  both to `agents.sessions.setStatus` / `agents.sessions.rename`.

### Testing approach

Run the real `pi-gatewayd` pointed at a **stub `pi` binary** (`--pi`), so the gateway
contract is exercised for real while the agent is fake; drive the adapter with recorded
Slack payloads. Cheaper and less brittle than mocking `gwclient`.

---

## Next action

1. **M3 — the phase-0 vertical slice.** Both halves are verified: `pi-gatewayd` is up with
   working tokens, and the Slack app opens a Socket Mode socket with `app_mentions:read`
   and `chat:write`. Nothing is blocking it.
2. **When convenient (one reinstall):** the phase-1 scopes and settings (M1.5/M1.6) — see
   [docs/slack-app-setup.md](docs/slack-app-setup.md), which now includes them in the main
   walkthrough.
3. **Whenever you like:** rename the GitHub repo to `pi-chat`, and file the upstream
   rebind fix (DESIGN §3) — neither blocks M3.

The gateway side is ready: `pi-gatewayd` is running, both tokens work, and the seam is
proven by `tools/gateway-probe`. The repo side is ready: `make check` validates the live
configuration, `make test` and `make vet` are clean.
