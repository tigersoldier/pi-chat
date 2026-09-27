# pi-chat — High-Level Design

Bot integrations for [pi-gateway](https://github.com/tigersoldier/pi-gateway): drive local
[pi](https://pi.dev) coding-agent sessions from chat platforms.

Slack is the first integration. Google Chat is a planned second one; nothing in the
core is Slack-specific except the adapter.

**Status:** design settled. Supersedes the previous `pi-gchat` design (Google Chat
only, with the bot owning sessions itself). See §13 for build phasing.

---

## 1. Scope

**Is:** a long-running local service that

1. receives messages, commands and interactions from a chat platform (Slack),
2. maps a chat *thread* to a pi session owned by `pi-gatewayd`,
3. streams the agent's work back into that thread,
4. enforces who may use it, how many sessions may run, and how they are cleaned up.

**Is not:** a session daemon. `pi-gatewayd` owns pi processes, the prompt queue,
hibernation, replay and the session catalog. This project never spawns `pi`.

Non-goals are listed in §14.

---

## 2. Architecture

```text
   Slack (thread / DM)                                        local machine
        │  Socket Mode (outbound WSS, no public endpoint)      ┌──────────────────────────┐
        ▼                                                     │        pi-chatd          │
   ┌───────────────┐   normalized events   ┌──────────────┐    │  ┌────────┐  ┌─────────┐ │
   │ slack adapter │ ────────────────────▶ │    core      │    │  │ slack  │  │  core   │ │
   │  (ingress)    │ ◀──────────────────── │   (policy)   │    │  └────────┘  └────┬────┘ │
   └───────────────┘   Post/Update/Ask     └──────┬───────┘    │                   │      │
                                                  │            │        gwclient (2 tokens)
                                                  ▼            │                   │      │
                                          ┌──────────────┐     │                   ▼      │
                                          │   SQLite     │     │        ┌─────────────────┐
                                          │ threads,     │     │        │  pi-gatewayd    │
                                          │ cursor, seen │     │        └────────┬────────┘
                                          └──────────────┘     │                 │
                                                               │        pi --mode rpc (per session)
                                                               └──────────────────────────┘
```

Everything is **outbound**: one WSS to Slack (Socket Mode), loopback TCP to the
gateway, plus the Slack Web API. No listening socket, no tunnel, no public URL, no
certificate. `pi-gatewayd` is a prerequisite, not a component of this repository.

### Two planes

| Plane | Transport | Purpose |
|---|---|---|
| **Chat plane** | Slack Socket Mode + Web API | events in, messages out |
| **Session plane** | `gwclient` over loopback TCP | sessions, prompts, events |

The core is platform-independent; the Slack adapter owns everything that knows
Slack's shapes (§12).

---

## 3. Upstream contract (pi-gateway v0.1.2)

Dependency: `github.com/tigersoldier/pi-gateway` at **v0.1.2+**, importing the exported
`gwclient`, `protocol` and `config` packages. Single local deployment; single
`pi-gatewayd`.

What this design relies on, all verified present in v0.1.2:

| Need | API |
|---|---|
| Create/bind a session | `NewSession`, `SwitchSession` (path or name) |
| Drive a turn | `Prompt`, `Steer`, `FollowUp`, `Abort`, `ClearQueue` |
| Know when a turn ends | `TurnRunning`, `AwaitSettled`, `gw_turn` |
| Stream progress | `Events()` / `Config.OnEvent`; daemon coalesces deltas (50 ms / 8 KB) |
| Survive drops/restarts | `Cursor`/`LastSeq`/`LeafID` + `Reconnect`, replay or `gw_snapshot` |
| Resume external sessions | `ListSessions(SessionFilter)`, `SwitchSession` |
| Approvals | `Event.UIRequest`, `BlockingUIMethod`, `RespondUI` (dialogs only, §6) |
| Release / destroy a session | `StopSession`, `DeleteSession` (v0.1.2) |
| Terminal notification | `gw_session_state{state:"deleted"}`, `stopped` + `reason` |
| Session state / models | `GetState`, `SetModel`, `GetAvailableModels`, `SetThinkingLevel`, `Compact` |

Notable upstream behaviour this design must respect:

- **`admin` is required** for `gw_new_session`, `gw_stop_session`, `gw_delete_session`;
  `control` is required for `set_model`, `compact`, `set_session_name`; `operator`
  alone lacks `control`.
- **One connection binds to exactly one session.** `SwitchSession` rebinds it, and
  **`NewSession` rebinds the creating connection to the session it just created**.
  There is no unbind command and no way to keep a connection while dropping its
  binding — only `Close`/`Bye` releases it (§10).
- **A delete tombstones the path** for the daemon's lifetime; a repeat delete or attach
  answers `unknown_session`. The session file is deleted after pi is reaped.
- **A stop/delete unbinds the affected connections** (they survive; they are not
  closed). `gwclient` clears `Session()` on the terminal event.
- **Attached clients never block a delete**; `gw_stop_session` refuses with
  `session_attached` unless forced. A running turn refuses with `session_busy` unless
  forced (which aborts, then stops after a 5 s grace). Since `NewSession` binds, a
  long-lived creator connection is a **permanently attached client** and would make
  every session it created unevictable (§8, §10).
- **Extension UI is two different things on one frame type.** Dialog methods
  (`select`, `confirm`, `input`, `editor`) are routed to the author of the running turn
  (`gw_turn.author`), falling back to the most recently active `ui`-capable client; a
  non-owner response is dropped with `ui_stale`. Fire-and-forget methods (`notify`,
  `setStatus`, `setWidget`, `setTitle`, `set_editor_text`) are broadcast to everyone.
- **Open upstream gaps** — durable creator tags (in-memory, lost on daemon restart),
  prompt idempotency, and connection/session caps. Their mitigations are ours (§7, §8).

### Pending upstream ask

**`gw_stop_session` leaves the connection lazily-creating sessions.** After a stop the
connection is unbound, but only `state:"deleted"` sets the daemon's "must rebind
explicitly" flag, so the next session-scoped command silently creates a **second**
session:

```text
registered after stop = 0
prompt after stop = {command:prompt gw_session:s_1790… success:true}
registered after prompt = 1
```

Fix requested: set that flag for `stopped` with `reason: requested|forced` too, with a
message that distinguishes *stopped* (file exists, attach again) from *deleted*
(gone). Until it ships, §4's attach-before-prompt rule is a hard invariant, not a
convenience.

---

## 4. Sessions and threads

**A session is always thread-scoped.**

| Context | Session |
|---|---|
| Channel thread | one session per thread (`workspace:channel:thread_ts`) |
| DM thread | one session per thread |
| Channel root, DM root | **no session** — only session-less commands are accepted |

- The bot creates the thread when one is needed (`@pi <text>` in a channel root, or the
  first `@pi <text>` / `@pi /<cmd>` in a DM root, which opens a DM thread).
- Session identity is stored in `threads` (§7). Bot-created sessions are **named
  deterministically** from the thread key (`slack-<workspace>-<channel>-<threadts>`) so
  a lost database can be rebuilt by scanning the catalog.
- Sessions started outside the bot (pilish, another integration) are adopted with
  `/pi resume`; those keep their own names and paths.

### Invariants

1. **Attach before prompt.** Every turn begins with `SwitchSession(path)` — a no-op when
   already bound. This is the §3 mitigation and the fix for the "stale connection forks
   a new session" failure class.
2. **Never prompt on an unbound connection** without attaching first; `Session() == nil`
   is a state transition, never an assumed binding.
3. **A deleted session is never re-attached.** On `state:"deleted"`, mark the thread
   `deleted` and tell the user; a later message starts a *new* session deliberately.
4. **pi is never stopped mid-turn by us.** Eviction and explicit stop use
   `force: false`.
5. **Creation is a throwaway connection.** A session is created on a fresh admin
   connection that is closed immediately afterwards, so the thread connection is the
   only client attached to it (§10).

### Lifecycle

- **Warm:** the thread has a bound connection; the daemon holds a pi process.
- **Cold:** the connection is closed after `thread_idle_close_minutes` of inactivity
  (only when the turn is idle). The daemon hibernates the process later; the file
  remains.
- **Re-dial:** on the next message, dial with `Resume{Cursor}` — replay, or a snapshot
  whose content is discarded (we render live turns, not history) while its watermark is
  adopted. The replayed frames are dropped because nothing is being rendered between
  turns; what they carry forward is the sequence number the next save records.
- **The cursor is saved on settle and on the sweep**, so a restart or an idle close
  resumes at the tail rather than replaying from the head.
- **A process starts with no warm threads.** No thread is warm in a fresh process —
  warm means a bound connection, and a new process holds none — so startup clears the
  marker. Without it, a crash would leave rows claiming to be warm, and both `/pi status`
  and the warm-session cap read that marker. The sweep also repairs the same lie for a
  thread whose connection died while the process kept running.
- **Deleted:** the session file is gone; the thread row is marked `deleted` and keeps
  the key reserved so the next message starts fresh.

### Turn boundaries (verified against the live daemon)

A turn is not over when a message ends. One prompt produced this sequence:

```text
agent_start, turn_start, message_start, message_end,   <- first message, no text
message_start, message_update×4 (text_start, "P", "ONG", text_end), message_end,
turn_end, agent_end, agent_settled, gw_turn(settled)
```

- **`message_end` ends a message, `turn_end` ends one turn of the agent loop.** A single
  prompt can produce several of each, so neither marks the end of work. The terminal
  signals are **`agent_settled`** (pi's own, and what the protocol docs nominate) or
  **`gw_turn{state:"settled"}`**; `gw_session_state{state:"deleted"}` is terminal for a
  different reason. Treating `message_end` as terminal stopped rendering before the
  answer had streamed at all — a bug that only shows up on the second message of a turn.
- **A whole answer can arrive between two flush ticks.** In the run above, all four
  `message_update` frames landed inside 21 ms of a 2.4 s turn. A flusher that only
  renders on its ticker drops that text, so it must also render **on the way out**
  (turn done, or stopping for the finish).
- **`agent_end` precedes `agent_settled`,** and pi emits `extension_ui_request` frames
  (here: fire-and-forget `setStatus`) interleaved with the turn. Neither changes what is
  rendered.

---

## 5. Interaction grammar

Two trigger tokens, one vocabulary. The vocabulary is always written with its slash in
threads and without a second slash in roots — because **Slack forbids developer slash
commands inside message threads** and its slash-command payload carries no `thread_ts`.

| Context | Form | Examples |
|---|---|---|
| Channel root, DM root | `/pi <command>` | `/pi status`, `/pi resume`, `/pi help` |
| Channel thread, DM thread | `@pi /<command>` | `@pi /status`, `@pi /skill:grill-me`, `@pi /compact` |
| Channel root | `@pi <text>` | `@pi fix the failing test` (starts a thread + session) |
| Channel thread | `@pi <text>`, or plain text in a thread the bot is already in | continue the session |
| DM thread | plain `<text>` | no mention needed |

### Rules

1. **Root commands are session-less only.** Legal: `/pi help`, `/pi status` (bot status:
   gateway reachable, warm sessions, cap, this channel's threads), `/pi resume` (list
   resumable sessions; picking one opens a thread and binds it). Anything else answers
   with an ephemeral hint pointing at the thread form. A root invocation never creates
   or touches a session as a side effect.
2. **Resolution order:** bot control command → agent command → prompt. Agent commands
   are the ones pi reports through `GetCommands()` (extensions, prompt templates,
   skills); they are **not** validated against that list before being forwarded, so
   an unknown `/X` reaches pi verbatim and pi's own error is the authoritative one.
   `GetCommands()` is used to *list* them in `@pi /help`.
3. **Near-miss guard:** a message that is *exactly* a control name without its slash
   (`@pi status`) gets an ephemeral `did you mean @pi /status?` instead of burning a
   turn. Only the whole message counts: "delete the old branch" is a prompt, and
   interrupting it would be worse than running it.
4. **Mentions are stripped** before the text reaches the agent; a leading `/` after the
   mention is preserved.
5. **A command the grammar reserves but this build does not implement yet** (`/delete`
   in phase 1) is answered rather than forwarded: pi would receive `/delete` as literal
   prompt text, and a clear "not in this build yet" beats that.
6. **`@pi /delete` asks for confirmation with buttons**, then deletes.

What reaches a session, and what does not:

| Message | Behaviour |
|---|---|
| Channel or DM root, plain text | **Dropped.** Roots are session-less, so a bare message must not start one |
| Channel root, `@pi <text>` | Starts a thread rooted at the mention, and a session in it |
| DM root, `@pi <text>` | Same, and the thread is a DM thread |
| Channel thread, plain text | A prompt **only if** that thread already has a session (the bot sits in busy channels) |
| Channel thread, `@pi <text>` | A prompt; the mention is optional but harmless |
| DM thread, plain text | A prompt: in a DM the conversation is already the address |
| Any thread, other people's messages | Dropped unless the thread is one of the bot's own, and unless the sender is allowed |

### Command reference (v1)

| Command | Plane | Notes |
|---|---|---|
| `/pi help`, `@pi /help` | root / thread | usage + current thread state |
| `/pi status` | root | bot status |
| `@pi /status` | thread | session id, cwd, model, warm/cold, queue, last activity |
| `/pi resume` | root | list adoptable sessions; pick with buttons |
| `@pi /abort` | thread | `Abort`; queued prompts survive by design |
| `@pi /delete` | thread | confirm → `DeleteSession` → worktree/branch cleanup → row deleted |
| `@pi /model` | thread | `SetModel` / `GetAvailableModels` |
| `@pi /stop` | thread | manual `StopSession` (cap debugging) |
| `@pi /compact`, `@pi /skill:<name>`, `@pi /<template>` | thread | forwarded to pi |
| `@pi <text>` | thread | prompt |

Command answers, hints, refusals and pickers are **ephemeral**: they are about the session,
not part of the conversation, and a thread full of status messages buries the answers. A
notice goes through the interaction's `response_url` when there is one (which is also the
only way to replace an ephemeral message), and `chat.postEphemeral` otherwise.

`@pi /help` lists pi's own commands (skills, templates, extensions) only when the thread is
already warm: a help request must not dial, create or wake a session to answer itself.

---

## 6. Rendering

**Native Slack streaming is the primary path**; a patched placeholder is the documented
fallback.

- `chat.startStream` on turn start; `chat.appendStream` with
  - `markdown_text` chunks for assistant text (standard markdown passes through — **no
    mrkdwn conversion table**),
  - `task_update` chunks for `tool_execution_start`/`_end` (`in_progress` →
    `complete`/`error`), so tool calls render as a real timeline;
- `chat.stopStream` finalizes the *same* message with any footer blocks.

Constraints and behaviours:

- Appends are coalesced on `flush_ms`, never per token (the daemon already coalesces at
  50 ms / 8 KB).
- `cannot_provide_both_markdown_text_and_chunks` per request — compose chunks.
- Streaming to a channel requires `recipient_user_id` + `recipient_team_id`; thread
  replies use `thread_ts`.
- **Fallback:** on `channel_type_not_supported`, `access_denied` or similar, degrade to
  `chat.postMessage` + `chat.update` for that turn. The adapter seam has one method per
  operation so the fallback stays inside the Slack adapter.
- **The recipient fields are sent defensively.** Slack documents
  `recipient_user_id`/`recipient_team_id` as required "when streaming to channels",
  while pi-chat always streams into a thread. The first attempt sends them, and a
  refusal is retried **once without** them before falling back to patching; the refusal
  is remembered per process so later turns do not repeat a doomed call. **Verified live
  on 2026-09-27:** a thread reply carrying both the recipient fields and `thread_ts`
  streams fine, so the documented requirement is about channel-level streams. The retry
  has never fired and stays as insurance.
- **Text that has arrived is always rendered before the answer is finalised.** Streaming
  is progress, not the record: `Finish` presents the authoritative answer
  (`GetLastAssistantText`) and *corrects* the message when it differs from what streamed.
  A turn that produced several messages therefore ends showing the final answer, not a
  concatenation of intermediates.
- **Status and title, two tiers:**
  - *Declared as an agent* (`slack/manifest-agent.yaml`, Slack's agent messaging
    experience): `agents.sessions.setStatus` takes real lifecycle values that map onto
    our states exactly — `processing` while a turn runs (Slack shows a loading UX), `suspended` while we wait for an approval, `active` when idle, `closed` on delete.
    Titles use `agents.sessions.rename`. Subscribing to `agent_session_stopped` (scope
    `chat:write`, which we already hold) is what makes the loading indicator
    **interactive** — without the subscription `setStatus` returns a
    `missing_agent_session_stopped_event_subscription` warning and Slack shows a
    non-interactive spinner instead. Two details the adapter must honour: the event
    arrives with `streaming_message_ts`, the streams Slack has *already* stopped, so we
    must not stop them again; and the status does **not** change on its own when the user
    presses stop, so we transition off `processing` ourselves.

    **Status is implemented.** The core owns a small platform-neutral vocabulary — `busy`,
    `waiting`, `idle`, `closed` — and the adapter maps it onto Slack's values
    (`processing`, `suspended`, `active`, `closed`); a platform with no status surface
    simply does not implement `StatusReporter`. `busy` goes up before the turn's first
    byte and `idle` comes down however the turn ends — a failure and a stop included — so
    a spinner cannot outlive its turn. An install that cannot show one (a workspace
    without the agent feature answers `feature_disabled`; an app that was never granted a
    scope answers `missing_scope`) is asked **once** and then left alone: a missing
    indicator is cosmetic, and a warning per turn would not be.

    **The stop button works.** Pressing it arrives as `agent_session_stopped`, which the
    adapter hands to the button path — allowlist, thread lookup and dedupe all apply
    unchanged — as `ActionStop`; the core aborts the session and the turn ends through its
    normal path. Slack has already stopped the reply's stream by then, and that needs no
    bookkeeping: a later append fails, and the renderer already answers an append failure
    by switching to message updates, so the partial answer still gets its final text. A
    stop that arrives with no turn running clears a stale status instead of doing nothing.

    **Still open:** `rename` titles, `suspended` while a turn waits for a human answer,
    and `closed` on `/delete` — the first two land with phase 2's approvals, the third
    with its delete.

    The scopes, precisely: `agents.sessions.setStatus` and `agents.sessions.rename` need
    **`chat:write` alone** — the agent surface is not what buys `assistant:write`.
    `assistant:write` is the required scope of `app_context_changed` and of the legacy
    `assistant.threads.*` methods, and pi-chat uses neither, so in the agent manifest it
    is declared only because Slack's agent feature asks for it and a scope change forces
    a reinstall. Caveats: the workspace must have the agent feature enabled
    (`feature_disabled` otherwise), and switching an app from the legacy assistant view
    to the agent view cannot be reversed.
  - *Plain bot* (the default `slack/manifest.yaml`): the only status primitives are
    `assistant.threads.setStatus` (a free-form string with a two-minute timeout) and
    `assistant.threads.setTitle`, both of which need `assistant:write` — a scope the
    default manifest deliberately does **not** request, so today this tier shows no
    status at all and *cannot*: adding it means adding the scope and reinstalling. Slack
    is migrating these methods to the `agents.sessions.*` names above, so treat both
    spellings as current, not settled.
- **`extension_ui_request` is two things on one frame type, and the adapter must split
  them by `method`:**
  - *Dialogs* — `select`, `confirm`, `input`, `editor` — need an answer and are routed
    to the **author of the running turn**, so the bot receives the ones it caused.
    `confirm`/`select` → Block Kit buttons carrying the dialog id; `input`/`editor` → a
    modal via the interaction's `trigger_id`; `pending_ui` (§7) maps a click back to
    `RespondUI`. If the same session is being driven from pilish, the dialog goes to
    *that* author instead and the bot never sees it — so the bot must never assume it
    owns approvals, and must treat `ui_stale` on a late response as normal.
  - *Fire-and-forget* — `notify`, `setStatus`, `setWidget`, `setTitle`,
    `set_editor_text` — are **broadcast and must never be answered**. Map `setStatus`
    and `setTitle` onto the Slack status/title; drop the rest. Phase 0 logs them at
    debug (a `warning`/`error` `notify` goes to the journal at warn); treating them as
    "unanswerable UI input" produced a false alarm on every single turn, since
    `pi-lens-lsp` emits `setStatus` continuously.
- `approvals = "auto"` passes `--approve` and pi resolves dialogs itself. That does
  **not** silence the frame type: sessions emit `setStatus`/`setWidget` regardless, so
  the classification above is always required.
- **Strip ANSI escapes** (and control characters) from every string a pi extension
  supplies before it reaches Slack — observed `setStatus.statusText` carries raw SGR
  sequences (`\x1b[38;5;241m…`), and Block Kit renders them literally.
- An unanswered dialog eventually resolves through pi's own timeout; the bot marks it
  expired rather than letting the thread stall silently.
- **Admission notices** ("queued — waiting for a free slot") are posted in-thread; the
  Slack ack always happens inside 3 s, before any work.

---

## 7. State

SQLite (WAL, single writer), `db_path` from config. The gateway owns session files,
processes and the catalog; **this database owns everything chat-side** — upstream's
`createdBy`/tags are in-memory and vanish on a daemon restart.

```sql
threads(thread_key PK,            -- workspace:channel:thread_ts
        workspace_id, channel_id, thread_ts,
        session_name, session_path, session_id, cwd, project_dir,
        state,                    -- warm | cold | deleted
        last_seq, leaf_id,        -- replay cursor (persist on settle and periodically)
        progress_ts,              -- Slack message being streamed/patched
        created_at, last_active)

seen(event_id PK, received_at)    -- Slack retry dedupe (TTL sweep)
pending_ui(dialog_id PK, thread_key, message_ts, created_at)
admissions(id PK, thread_key, text, event_id, queued_at)
```

Rules:

- **No conversation content is stored** — no transcripts, no assistant text, no tool
  output. `admissions` holds text only until dispatch. (This is what the privacy note
  will claim, and it must stay true.)
- **Persist before acking** anything that mutates state, so a crash cannot lose an acked
  message.
- **`seen` is mandatory**, not an optimisation: upstream prompt idempotency is still
  open, so this is the only guard against a Slack retry producing a double turn. A
  failure to *write* the claim blocks the turn: running the prompt once, late, beats
  running it twice.
- **`progress_ts` is persisted** (with the reply's timestamp) so that a later phase
  can recognize the message a restart left half-written. Nothing reads it back yet:
  a restarted process cannot resume a stream it was not holding, so the next turn
  writes a new message. The column is there so the recovery path is a change of
  policy rather than a migration.
- **`deleted` is a state, not a delete**, so the thread key stays reserved.
- **Phase 1 writes `threads` and `seen`.** `pending_ui` and `admissions` are created with
  the rest of the schema — one artifact, one migration — and are written from phase 2,
  when there are dialogs and a queue to put in them. The database carries a schema
  version and refuses to open one written by a newer pi-chat.

---

## 8. Concurrency and resources

The scarce resource is a **warm pi process**, held for every *bound* connection. The cap
counts warm sessions, not sockets; an unbound connection is nearly free.

- `max_warm_sessions` (default 8) — configurable.
- **Overflow: evict → queue → refuse.**
  1. Evict the least-recently-active **idle** session with `StopSession(force:false)`
     and close its connection. Skip candidates whose turn is running, and skip any
     session with another client attached (checked via `gw_list_sessions`' `clients`) —
     the user may be driving it from pilish, and stopping pi under them is exactly what
     `gw_stop_session` refuses.

  A session created by the bot must have exactly **one** attached client (its thread
  connection). This is why session creation uses a throwaway admin connection (§10):
  `gw_new_session` binds the connection that issued it, so a reused creator connection
  would show up in `clients` forever and render every session it ever created
  unevictable — a cap that silently never evicts.
  2. If nothing is evictable, queue the message for `admission_wait_seconds` (default
     120) with an in-thread notice; **re-check the allowlist at dequeue**; on expiry say
     so.
  3. Refuse with a clear message.
- **Never `force`.** A busy or attached session is skipped, never killed.
- **Every eviction is logged and counted** — an invisible cap that stops sessions is
  indistinguishable from a bug.
- **Phase 1 reports the cap but does not evict.** It counts warm threads and warns when
  the cap is reached; evict-then-queue belongs with the queue that gives it something to
  fall back on (phase 2). Session creation is never queued or refused before then.
- The daemon's own idle hibernation (15 min after the last client detaches) is the
  second tier and is not accounted; the idle-close sweep is what keeps the cap honest.

---

## 9. Workspace convention

New sessions get a fresh project directory; the agent works in a git worktree of a real
repository, so concurrent threads on one repo never collide and the user's own checkout
is never touched.

- Project dir: `{projects_root}/<date>-<slug>/`, passed to `gw_new_session.cwd`. The slug
  comes from the first prompt (the first six words, reduced to letters, digits and inner
  dashes) and a collision gets a counter rather than a shared directory: two threads must
  never share one working tree.
- Repos: `{repos_root}` (some may be bare).
- `injected_prompt` (config template; empty disables injection) instructs the agent to
  pick the repo, detect bare vs clone, resolve the default branch, and
  `git worktree add -b pi/{date}-{slug} ./repos/<repo> <ref>`, then work and push there.
- It is passed at spawn as `piArgs: ["--append-system-prompt", <rendered>]`.

Cleanup:

- `@pi /delete` → `DeleteSession` (gateway removes the session file) → for each dir under
  the project dir's `repos/*`: `git -C <mainrepo> worktree remove --force`, prune the
  `pi/<date>-<slug>` branch → delete the row → remove the project dir if empty.
- **Cleanup only touches what it can prove is pi-chat's**: a linked worktree (git reports
  a different `--git-dir` and `--git-common-dir`) sitting on a `pi/` branch. A real clone
  the agent made, a worktree on somebody else's branch, or a file the agent wrote outside
  `repos/` is reported as left behind, and a directory that is not empty is never
  removed. Deleting the wrong repository is worse than a directory outliving its session.
- **Startup GC:** for each directory under `projects_root` with **no** `threads` row:
  remove its worktrees, prune its branch, delete the directory. Orphans only — a cold
  thread with a live row is never touched, because `/pi resume` (and the work in its
  worktree) must still be there. Only names that look like pi-chat's own
  (`<YYYY-MM-DD>-<slug>`) are considered at all.
- Sessions adopted from outside the bot have no project dir, live outside `projects_root`,
  and are therefore structurally out of the GC's reach — as well as out of `/delete`'s.

---

## 10. Access control and credentials

**Deny by default.** Anyone who can talk to this bot can run an agent with shell access
on the machine and, through the admin token, destroy sessions.

- `slack.access.allowed_users` (Slack member IDs) is required and enforced **before any
  side effect** — no dial, no project dir, no session. The check uses the *event's* user,
  so edits and bot-authored messages cannot slip through.
- `slack.access.allowed_channels` is optional; empty means any channel the bot is in.
- **The allowlist lives under the platform's own section**, because the identity models
  differ: Slack has member and channel IDs, Google Chat would have email addresses and
  space names. Each adapter owns its access rules; the core enforces “default deny” and
  never assumes one platform's identifiers apply to another.
- `workspace_id` is recorded in every thread key from day one, so adding OAuth
  distribution later does not require re-keying.

Two gateway tokens (§3) — never the daemon's default token:

| Token | File | Capabilities | Where used |
|---|---|---|---|
| admin | `admin_token_file` | `admin` (+ everything) | **one throwaway connection per lifecycle operation** — dial → `gw_new_session` / `gw_stop_session` / `gw_delete_session` → `Close` |
| thread | `thread_token_file` | `observe, prompt, interject, ui, control` — deliberately **no `admin`** | one long-lived connection per warm thread |

Rationale: the many long-lived per-thread connections cannot destroy sessions even if a
rendering or dispatch bug abuses them; and lifecycle operations, which are always
issued with an explicit target, never depend on a connection's binding.

The admin connection must be **short-lived, and never reused for creation**:
`gw_new_session` rebinds the connection that issues it and there is no unbind command,
so a pooled creator connection would remain an attached client of every session it
ever created — pinning them warm and making them unevictable (§8). Dial, create,
close; the loopback dial costs nothing.

Mint with `pi-gatewayd --provision-token`; rotate via `tokens.json` + SIGHUP.

---

## 11. Configuration

`~/.config/pi-chat/config.toml`, mode 0600 (it holds token *paths*; token values live in
0600 files beside it). State in `~/.local/state/pi-chat/`.

```toml
[slack]
app_token_file = "~/.config/pi-chat/slack-app-token"   # xapp-…, Socket Mode
bot_token_file = "~/.config/pi-chat/slack-bot-token"   # xoxb-…

[slack.access]
allowed_users    = ["U01234567"]   # required; Slack member IDs
allowed_channels = []              # optional; empty = any channel the bot is in

[gateway]
state_dir         = "~/.config/pi-gateway"
admin_token_file  = "~/.config/pi-chat/gateway-admin.token"
thread_token_file = "~/.config/pi-chat/gateway-thread.token"
# Passed to gw_new_session, on top of what pi-chat adds itself: the injected
# prompt, and --approve when [behavior] approvals is "auto".
pi_args           = []

[paths]
projects_root = "~/work"
repos_root    = "~/code"
db_path       = "~/.local/state/pi-chat/pi-chat.db"

[concurrency]
max_warm_sessions         = 8
thread_idle_close_minutes = 10
admission_wait_seconds    = 120
eviction                  = "evict-then-queue"

[render]
mode     = "stream"                # stream | patch
flush_ms = 1000

[behavior]
approvals = "auto"                 # auto adds --approve to pi_args | interactive
# injected_prompt = """…"""        # {reposRoot} {projectsRoot} {date} {slug}; empty = none

[log]
level  = "info"
format = "text"
```

---

## 12. Adapter seam

Core is platform-independent; the Slack adapter owns platform shapes. The seam is drawn
from Slack only — it deliberately does not anticipate Google Chat's cards or Pub/Sub.

**Core:** gateway client pool and tokens, thread↔session binding and persistence,
cursor handling, admission and eviction, command resolution, project provisioning and
GC, the normalized progress model (turn started → tool activity → assistant text →
terminal).

**Slack adapter:** Socket Mode envelopes + ack, `event_id` dedupe, thread key and sender
extraction, allowlist input, mention stripping, rendering and chunking, streaming vs
fallback, interactive components and modals, file download → `protocol.ImageContent`,
slash-command registration.

The Slack app itself is configuration as code: `slack/manifest.yaml` (standard bot) and
`slack/manifest-agent.yaml` (agent messaging experience) are the two supported app
shapes, applied by pasting one into the app's manifest editor;
`docs/slack-app-setup.md` is the walkthrough. Keeping them in the repository means a
scope or event is added once, reviewed as a diff in Slack, and never drifts from the
adapter's expectations.

**The seam, as implemented** (`internal/bot/types.go`):

- *in* — `Message` (a mention or plain text, with `Mentioned`/`Direct` telling the core
  how it was addressed), `Command` (`/pi <cmd>` at a root or `@pi /<cmd>` in a thread,
  normalised so the text always carries its slash), `Action` (a button press).
- *out* — a `Renderer` per turn (`Start`/`Delta`/`Finish`/`Fail`, plus an optional
  `ProgressReporter` naming the message it writes into), `Notice` for everything that is
  not a turn (text plus plain buttons, plus the opaque adapter handles `ReplyTo` and
  `Update`), and `OpenThread` for the one case where a root command needs a thread.

The adapter side declares the core as a three-method `Core` interface and owns the
`Router` that turns envelopes into those calls, so main only wires components together
and the payload handling is testable with recorded envelopes. No Slack envelope, `ts`,
block or Socket Mode type crosses the seam; `ReplyTo` and `Update` are opaque strings the
core passes back untouched.

---

## 13. Build phasing

- **Phase 0 — vertical slice through the real seam.** One allowed user, one channel, no
  database: Socket Mode → `@pi hello` in a thread → admin connection creates a session →
  prompt → streamed reply rendered into that thread. The deliverable is a streamed pi
  turn inside Slack. It reads the real config file, so nothing is thrown away.
- **Phase 1 — MVP.** Allowlist + denial; SQLite binding, cursor, dedupe; session
  provisioning (project dir, injected prompt, worktree convention); thread-scoped
  sessions; prompt → streamed answer; `/pi help`; `/pi status`; `/pi resume`;
  `@pi /<command>`; `@pi /status`; `--approve` mode; startup GC; systemd user unit.
- **Phase 2.** `@pi /delete` (button confirm) + worktree/branch cleanup; `@pi /abort`;
  `@pi /model`; interactive approvals (buttons + modal); concurrency cap with eviction
  and bounded queue; streaming → patch fallback.
- **Phase 3.** Message shortcut for in-thread discovery; `setTitle`/`setStatus` and
  suggested prompts; image/attachment passthrough; `/pi resume` picker polish; metrics.

Testing: run the real `pi-gatewayd` pointed at a stub `pi` binary (`--pi`), so the
gateway contract is exercised for real while the agent is fake; recorded Slack payloads
drive the adapter without a workspace.

---

## 14. Non-goals (v1)

- Google Chat adapter (planned; the core is built to admit it).
- Multi-workspace OAuth distribution, Marketplace listing, and the privacy/terms
  artifacts a listing requires (Socket Mode apps cannot be listed, so there is no
  listing in v1).
- Per-user settings, model preferences, or quotas (single-owner deployment).
- Browsing or replaying transcripts inside Slack; Slack renders live turns only.
- Session groups across daemons, WebSocket transport (upstream M4).
- Multi-instance operation: Socket Mode distributes events across connections, so the
  bot is deliberately single-instance.

---

## 15. Decision log

| # | Decision | Why |
|---|---|---|
| 1 | Built on `pi-gateway` as a `gwclient` consumer (v0.1.2+) | The daemon owns sessions, queue, replay, hibernation; reimplementing that was the old design's largest cost. Upstream explicitly scopes the Slack bot to a separate repo. |
| 2 | Slack ingress: Socket Mode, wired directly (no transport interface) | Keeps the service fully outbound — no endpoint, tunnel or cert; slash commands and interactivity work. The platform seam below it is where reuse actually matters. |
| 3 | Keep the worktree-per-session convention; `/delete` cleans up; startup GC sweeps orphans only | Worktrees are what allow concurrent threads per repo without touching the user's checkout; GC covers crashes, and cold-but-known threads stay resumable. |
| 4 | Request the upstream stop-rebind fix; keep attach-before-prompt as an invariant | The defect silently forks a second session after a stop; the invariant is defence in depth either way. |
| 5 | Rename to `pi-chat`, Google Chat named as a deferred adapter, gchat spike code deleted | The name is the scope statement; the module path has no external importers, so the rename is free. The gchat transport code does not survive the gateway rewrite. |
| 6 | Core/adapter split, seam drawn from Slack, responsibilities written down | Avoids designing against a platform not yet implemented while keeping the second adapter cheap. |
| 7 | Access: required user allowlist, optional channel allowlist, deny by default | An agent with shell access plus session-destroying capability is the thing being protected. |
| 8 | Two gateway tokens: session-less admin, per-thread `operator + control` (no admin) | Least privilege on the hot path, and lifecycle ops that cannot be confused by a stale binding. |
| 9 | Cap warm sessions (default 8); evict idle LRU → bounded queue → refuse; never force | The resource is the pi process, not the socket; eviction must never kill a turn or a session someone else is driving. |
| 10 | Bound while hot, close after idle; persist the cursor; discard snapshot content | Fast within a conversation burst, with the tail decaying automatically; history is not replayed to a user who already saw it. |
| 11 | SQLite + deterministic session names | Durable authoritative chat-side state, rebuildable from the catalog if the database is lost. |
| 12 | Native Slack streaming primary, patched message as fallback | `task_update` chunks map tool executions onto a real timeline and pi's markdown passes through unmodified — strictly better than a mrkdwn conversion table. |
| 13 | Two trigger tokens, one vocabulary: `/pi <cmd>` at root, `@pi /<cmd>` in a thread; sessions are thread-scoped | Slack forbids developer slash commands in threads and omits `thread_ts` from their payload; this is the only consistent grammar the platform allows. |
| 14 | Phase 0 is the thinnest vertical slice through the real seam | The two unknowns (Slack ingress/egress, gateway integration) only meet at the seam; proving them one at a time proves neither. |
