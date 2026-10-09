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

**Session context injection, and spawn configuration that survives a restart** — see
[`docs/pi-gateway-session-context.md`](docs/pi-gateway-session-context.md), which ended in
a release (**pi-gateway v0.1.3**) rather than an answer. Shipped: durable spawn
configuration (`gw_welcome.features: ["spawn_config"]`, `gw_list_sessions[].spawn`, a
sidecar under `<stateDir>/spawn/`, `gw_reload_session{piArgs}` to replace it). **Removed:**
the `inject` command, because no released pi exposes `send_message` over RPC — the
primitive is extension-only — so it could only ever answer `not_supported`. Upstream's
prescription for a standing instruction is therefore client-side, and it is what §4's
decisions now assume: the instruction goes in `--append-system-prompt` for sessions we
create (the record re-applies it, and compaction cannot touch it), and into the first
prompt we send in a session we adopted, wrapped in `<slack-specific-instructions>`
markers, re-sent after an observed `compaction_end`.

That same release also **fixes the defect recorded here**: because spawn configuration is
now persisted and re-applied, the project's `--append-system-prompt` survives a
`pi-gatewayd` restart even though the thread connection dials without `piArgs`. It needs
the upgraded daemon (this machine still runs `119f0f2`), and it applies to sessions
created from then on — sessions that predate the release were already created without a
record.

The stop-rebind ask above is **unchanged** by that release: the guard is still set for
`deleted` only, so a session-scoped command after a *stop* still creates a second
session. `unbound: true` in the stop response now tells the client immediately, and
`gwclient` clears its own binding from it, which makes the invariant easier to keep — but
it is still an invariant.

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

### Observation (built, 2026-09-27)

A session should not be blind to the room it is in, so a turn now reads the conversation
and puts it in front of the request.

- **Fetched, not buffered.** At turn time the core asks the platform for the thread with
  `conversations.replies`, `oldest` set to the watermark it remembers (§7). Nothing said
  while the daemon was down is lost, no message is buffered while the bot is idle, and no
  message text is stored at rest — the watermark is a timestamp.
- **The watermark moves only after a prompt is accepted**, and never backwards. A turn
  whose prompt failed leaves the conversation for the next turn to read again; a replayed
  turn cannot make the bot read the same messages twice.
- **The boundary is checked, not trusted.** Slack takes `oldest` as the start of a range
  rather than as an exclusive bound, so the message a previous prompt already carried
  comes back with the next fetch; the core drops anything at or before the watermark
  before it can become a line. Without that filter an earlier request would reappear as
  a remark somebody made.
- **The transcript rides in the prompt**, as a block between `<thread-conversation>`
  markers whose lines are `[Name (U123)] text`. The request follows the block, and the
  instruction installed once (§3, and the milestone that adds it) is what tells the agent
  that only the text after the block is addressed to it.
- **Bounded to ~10k characters, newest kept**, with a single message cut at ~2k. Every
  message the block dropped or shortened is in the file the block names:
  `<state dir>/threads/<session name>/observed-<ts>.md`, at most 20 of them per thread (§7).
- **The bot's own messages are left out** (its history already has them) and the trigger is
  not repeated as somebody's remark. A message that arrives while a turn is running is
  covered by the next turn's fetch, because the watermark only ever reaches what a prompt
  actually carried.
- **Names need `users:read`** and are resolved lazily, one `users.info` per person per
  process, cached both ways. Without the scope the transcript falls back to bare IDs —
  which is what the mention form `<@U123>` is built from, so the agent can still address
  somebody — and the adapter stops asking after the first refusal.
- **The first turn in a thread that already had a human conversation reads that
  conversation too** (an empty watermark means "from the beginning"), bounded the same way.
  That is the open question below, settled this way in the code: the mention usually refers
  to what was just said.

### Images from Slack (2026-10-03)

- Preserve file identities and names on inbound messages and thread replies, including
  Slack's `file_share` subtype. Images in the trigger and in retained conversation lines
  travel as pi's native `images` prompt content, not private URLs the model cannot open.
- The optional platform `ImageReader` resolves each file through `files.info` and downloads
  `url_private_download` (or `url_private`) with the bot token. Only HTTPS Slack file hosts
  are allowed, including redirects; pi-chat does not log or save tokens, private URLs or
  image bytes. pi retains native image prompts in its session history as usual.
  The install needs `files:read`, already declared in both manifests.
- Trigger images have priority, then the newest retained conversation images; deduplicate
  by file identity. Bound a turn to four images, 5 MiB per image and 10 MiB total before
  base64 encoding, safely below the gateway's 16 MiB frame cap. Accept PNG, JPEG, GIF and
  WebP, checking downloaded bytes rather than trusting Slack's MIME label.
- Text identifies each attached image's author/source and position. Conversation images
  remain context, not requests. A bare image request gets a default inspection prompt.
  Unsupported, inaccessible or oversized files are reported visibly rather than silently
  discarded; a text request can still proceed, but an image-only request with no readable
  images does not spend a model turn. Downloads happen only inside an authorized turn.

### The instruction that explains the conversation (built, 2026-09-27)

The transcript and the request arrive in one prompt, so the agent has to be told what the
block means. pi-gateway has no way to inject anything mid-conversation — no released pi
exposes one over RPC (§3) — so the instruction is client-side, and once per session
rather than once per turn.

- **A session pi-chat creates carries it in `--append-system-prompt`**, merged with the
  configured `gateway.pi_args` and the project's injected prompt into a *single* flag: pi
  takes the flag's value as the whole appended prompt, and which of two flags wins is its
  parser's business, not something to rely on. pi re-supplies the system prompt on every
  request, so this copy cannot be summarized away — no repetition per turn, none after a
  restart, none after a compaction.
- **A session pi-chat adopted cannot be changed that way**: its spawn parameters were
  recorded when somebody else created it, and replacing them would take away what that
  creator asked for. The instruction goes in front of the first prompt of the
  conversation instead, wrapped in `<slack-specific-instructions>` markers, and again
  after a successful compaction — which is what can summarize it away.
- **Whether it is already installed is asked of the catalog**, not guessed from the
  session's name: `gw_list_sessions[].spawn["append-system-prompt"]` is searched for the
  instruction's own marker (`(pi-chat instruction)`). A session created before this build
  has pi-chat's name and no instruction, and would otherwise never get one. A catalog that
  cannot answer means it is sent — a repetition costs a few hundred tokens, while a
  session that never learns how its prompts are shaped misreads every one of them.
- **`compaction_end` is applied to the thread rather than to the turn**, and only when it
  succeeded (not `aborted`, not `willRetry`): it can arrive between turns, its consequence
  outlives the turn it happened in, and the read goroutine that receives it must not
  block. A failed compaction changed nothing, so it discards nothing.
- **The instruction is static per session.** Rewording it reaches future sessions only;
  changing an existing one is `gw_reload_session{piArgs}`, which is refused while another
  client is attached (§3). It opens by naming itself, because the agent is expected to
  follow it, and a deployment that changes the rules should be able to say so.

### The grammar of a turn (built, 2026-09-28)

- **Only a mention turns in a room** — a channel thread or a group DM. Plain text there is
  conversation to observe: the bot sits in busy rooms, and a reply meant for somebody else
  must not spend a turn. Nothing is lost by not turning on it, because the next turn reads
  the thread back from its watermark.
- **In a one-to-one DM the conversation is the address**, so plain text turns — and a
  **top-level DM message starts a session of its own**, which the thread model gives for
  free: a new thread is a new session. When the DM already had one, a visible one-line
  notice says so, because a reset that shows nothing is how work gets lost.
- **A mention registers the thread but provisions nothing**: the worktree and the pi
  process are created by the first turn, so a bare `@pi` or `@pi /help` costs a row.
- **`@pi /new [<text>]` replaces the thread's session.** It retires the current one
  immediately — the connection is let go, its cursor saved, the row forgets the session —
  while the session file, its working directory and its pi process are left alone, because
  "new session" means a clean slate rather than "throw my work away" (that is `/delete`,
  with its confirmation button). The retired session is recorded (see `retired` in §7) so
  the startup sweep keeps its directory, and it stays adoptable through `/pi resume`. A
  following `<text>` becomes the new session's first prompt; without one, the next message
  here is. A turn already running in the thread is refused rather than waited for.
- **A thread's later sessions get a generation suffix** (`slack-<ws>-<ch>-<ts>-2`): one
  thread can now have more than one session, and two of them under one name would leave the
  catalog ambiguous — for `/pi resume`, and for anyone reading it. The suffix is the number
  of retired sessions, so a rebuilt database lands on the same names.

**Still open from the same interview:** how a message that arrives *while* a turn is
running is handled (the default, and what the code does, is the next turn's transcript;
steering it into the running turn is the alternative), and whether the first turn in a
thread that already had a human conversation should read that history (the code does,
bounded — see the observation section above).

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
6. **`@pi /delete` asks for confirmation with buttons**, then deletes: the confirmation
   names the session, both buttons carry its path, and a press whose path is no longer the
   thread's session (a `/new` landed in between) is refused rather than applied to the
   successor. It refuses while a turn is running, too: the answer is "stop it or wait",
   not a force delete of work in progress.
7. **A message nobody addressed to the bot is dropped before the allowlist is
   consulted.** It is not a request, so there is nothing to refuse: an unlisted person
   answering their colleague in a thread the bot happens to be in must not draw a visible
   reply about somebody's allowlist. Their *mention*, or their DM, is a request, and a
   refusal is the answer to it.
8. **A mention is acknowledged before work or refusal.** Slack reacts `:eyes:` when the
   sender is allowed; otherwise it reacts `:shrug:` and sends that person a private refusal.

What reaches a session, and what does not:

| Message | Behaviour |
|---|---|
| Channel root, plain text | **Dropped.** Roots are session-less, so a bare message must not start one — the bot sits in busy channels |
| DM (one-to-one), plain text | A prompt, rooted at that message. In a conversation the bot was added to, the conversation *is* the address — and this is the shape a suggested prompt takes |
| Group DM, plain text | **Observed only, never a prompt or notice**: with two or more people in it, the conversation is no longer only the bot's address. Only a mention turns there, exactly as in a channel |
| Channel root, `@pi <text>` | Starts a thread rooted at the mention, and a session in it |
| DM root, `@pi <text>` | Same, and the thread is a DM thread |
| Channel thread, plain text | **Observed only, never a prompt or notice**: the bot sits in busy channels, and a reply meant for somebody else must not spend a turn. It reaches the next turn as context, through the observation watermark |
| Channel thread, `@pi <text>` | A prompt; the mention is optional but harmless |
| DM thread, plain text | A prompt: in a DM the conversation is already the address |
| Any thread, a message that addresses nobody | **Dropped**, and before the allowlist is consulted: whoever sent it, it is not a request, so there is nothing to answer and nothing to refuse

### Command reference (v1)

| Command | Plane | Notes |
|---|---|---|
| `/pi help`, `@pi /help` | root / thread | usage + current thread state |
| `/pi status` | root | bot status |
| `@pi /status` | thread | session id, cwd, model, warm/cold, queue, last activity |
| `/pi resume` | root | list adoptable sessions; pick with buttons |
| `@pi /new [<text>]` | thread | retire this thread's session and start a fresh one; `<text>`, when given, is its first prompt. The old session stays in `/pi resume` |
| `@pi /abort` | thread | `Abort`; queued prompts survive by design |
| `@pi /delete` | thread | confirm → `DeleteSession` → worktree/branch cleanup → row deleted |
| `@pi /model` | thread | `SetModel` / `GetAvailableModels` |
| `@pi /stop` | thread | manual `StopSession` (cap debugging) |
| `@pi /compact`, `@pi /skill:<name>`, `@pi /<template>` | thread | forwarded to pi |
| `@pi <text>` | thread | prompt |

**A picker's buttons say what a session is about**, not what its file is called: the
catalog's title (the first thing asked in the session, with the wrapper pi puts around a
skill or template invocation cut away), or the session's own name when it has no title,
followed by the last element of the working directory — which together are what tell two
sessions apart. A live session is marked: adopting one that a terminal is already driving
makes it a shared session, and that is worth knowing before pressing, not after.

Adopting opens a thread, and the notice in it says how to continue **there**: a channel
thread answers mentions, a DM thread answers anything — and it says the one that applies
where it is posted, because the two rules are opposites (`Direct` travels with the command
and the button for exactly this). A notice that names only its thread also names its
channel, so the adapter always has somewhere to post it.

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

    **Suggested prompts are computed, not declared.** `app_home_opened` with the
    Messages tab is the signal to offer them, and the list names repositories that exist
    under `paths.repos_root` — two questions per repository, four at most, and nothing at
    all when there is nothing to name. A list hard-coded in the manifest cannot say
    anything about the machine the bot runs on, which is the entire point of offering one.
    `assistant.threads.setSuggestedPrompts` is the one thing here that needs
    `assistant:write`, and in an agent app it must **not** carry `thread_ts`: Slack
    documents that including it makes the call fail **silently**, which is why the adapter
    has a test asserting the parameter is absent. An install that cannot show them — a
    plain bot without the scope, or a workspace without the agent feature — is asked once
    and then left alone, exactly like the status.

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
        observed_ts,              -- newest message a prompt has already carried
        created_at, last_active)

seen(event_id PK, received_at)    -- inbound dedupe (TTL sweep); see the rule below
pending_ui(dialog_id PK, thread_key, message_ts, created_at)
admissions(id PK, thread_key, text, event_id, queued_at)
retired(session_path PK,          -- a session /new replaced, kept resumable
        thread_key, session_name, project_dir, retired_at)
```

Rules:

- **No conversation content is stored** — no transcripts, no assistant text, no tool
  output. `admissions` holds text only until dispatch. (This is what the privacy note
  will claim, and it must stay true.)
- **The one exception is deliberate and bounded**: when a prompt had to drop or shorten
  part of a conversation, the whole of it is written to
  `<state dir>/threads/<session name>/observed-<ts>.md` and the prompt names the file.
  It is the agent's way to read what the prompt could not carry, it is capped at
  `transcriptKeep` files per thread, and it belongs to the session's directory, so
  deleting the session can take it with it. `observed_ts` itself is a timestamp, not
  text: the database stores how far the conversation has been read, never what it said.
- **Persist before acking** anything that mutates state, so a crash cannot lose an acked
  message.
- **`seen` is mandatory**, not an optimisation, and it holds two kinds of key: a
  **message's** identity — `msg:<workspace>:<channel>:<ts>`, which is what a turn is
  claimed under — and, for input a message cannot identify (a slash command, a button,
  an event the platform gave no timestamp), the event or envelope id. A message is the
  unit of work, and one message can arrive as several events: Slack reports a mention
  both as `app_mention` and as `message.channels`, with the same `ts` and two different
  `event_id`s. Claiming by event id spent two turns, and posted two answers, on one
  request. A failure to *write* the claim blocks the turn: running the prompt once, late,
  beats running it twice.
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
- **Schema changes are migrations**, not a new schema: `init` migrates a database whose
  `user_version` is behind, stamping each step's version only after its statements
  succeed, so a failure is retried on the next start rather than half-applied. A fresh
  database gets the whole schema and skips the steps. The version statements are
  literals in an allowlist (SQLite takes no bound parameter there), and a test fails
  when a new version forgets its entry.
- **`observed_ts` is the observation watermark** (§4): the newest message a prompt has
  already carried. It advances only after a prompt was accepted, so a failed prompt
  leaves the conversation to be read again, and it never moves backwards, so a replayed
  turn cannot make the bot read the same messages twice.
- **`retired` is what `/new` keeps alive** (§4): a session it replaced still exists, so its
  working directory is somebody's work rather than litter, and the sweep has to be able to
  tell the two apart. It is also where the next session's generation number comes from.

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

- `@pi /delete` asks first, with buttons, and the confirmation names the session it would
  delete: both buttons carry that session's path, so a press that arrives after the thread
  has moved on refuses instead of deleting the successor. It refuses while a turn is
  running as well. Then `DeleteSession` (the gateway stops pi, reaps it, removes the
  session file) → for each dir under the project dir's `repos/*`: `git -C <mainrepo>
  worktree remove --force`, prune the `pi/<date>-<slug>` branch → the row is marked
  `deleted` (its key stays reserved, so the next message here starts a fresh session) →
  remove the project dir if it ended up empty. An adopted session has no project directory
  and only the session goes. A session the gateway no longer has is not an error: the
  cleanup still runs, and the answer says the session was already gone.
- **Cleanup only touches what it can prove is pi-chat's**: a linked worktree (git reports
  a different `--git-dir` and `--git-common-dir`) sitting on a `pi/` branch. A real clone
  the agent made, a worktree on somebody else's branch, or a file the agent wrote outside
  `repos/` is reported as left behind, and a directory that is not empty is never
  removed. Deleting the wrong repository is worse than a directory outliving its session.
- **Startup GC:** for each directory under `projects_root` with **no** `threads` row and
  no `retired` record: remove its worktrees, prune its branch, delete the directory.
  Orphans only — a cold thread with a live row is never touched, and neither is the
  directory of a session `/new` replaced, because `/pi resume` (and the work in its
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
# true (the default) runs the app surface; false runs only the self-DM below.
# A missing app or bot token file disables this surface at startup with a
# warning rather than failing the daemon.
enabled        = true
app_token_file = "~/.config/pi-chat/slack-app-token"   # xapp-…, Socket Mode
bot_token_file = "~/.config/pi-chat/slack-bot-token"   # xoxb-…

[slack.access]
allowed_users    = ["U01234567"]   # required; Slack member IDs
allowed_channels = []              # optional; empty = any channel the bot is in

# The app-less surface: your own "Notes to self" conversation, polled with your
# browser session. Off by default; enabling it is a deliberate act with a real
# cost (no notifications, an account-wide credential — see docs/).
[slack.self_dm]
enabled       = false
auth          = "session"          # session | user_oauth
xoxc_file     = "~/.config/pi-chat/slack-xoxc"   # auth = session
xoxd_file     = "~/.config/pi-chat/slack-xoxd"   # auth = session
xoxp_file     = "~/.config/pi-chat/slack-xoxp"   # auth = user_oauth
workspace_url = "https://acme.slack.com"         # auth = session
channel_id    = "D0123456789"      # the self-DM's D… id
poll_interval = "5s"

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

**One message, one delivery.** Slack reports a mention twice — as `app_mention` and as
the `message` event for the same `ts` — so which delivery answers is decided by where the
message was sent, and the other one is dropped rather than deduped:

- **A DM (`im`)** is answered by `message.im`. The conversation is the address, so every
  message in it is a request; `app_mention` is not what Slack sends for a DM at all.
- **A channel or group DM** is answered by `app_mention`, which is Slack's own statement
  that the message mentions the bot rather than our reading of its text — and that is what
  makes it right to trust for a mention composed in rich text. The `message` event with
  the same `ts` is that request arriving again; everything else in a room — plain text, a
  reply in a thread — is conversation the next turn fetches as context, which is why no
  event is needed for it.

The core still claims a turn by the message's own identity (`msg:<workspace>:<channel>:<ts>`):
the drop is the mechanism, the claim is the net under it, for a redelivery or a platform
that sends one message twice in a way an adapter cannot see. The subscriptions stay as
they are — the manifest says what the app *may* receive, the adapter says what pi-chat
*answers*, and a workspace that subscribes to more does not change that.

**A dropped message that the bot could have been asked about is not dropped in silence.**
A room `message` event that does *not* mention the bot is handed to the core **unaddressed**
rather than discarded in the parser, because the core is the side that knows whether that
thread has a session — and only the core can explain the rule to the person who typed.
Plain text at a channel root *is* discarded here: a root message belongs to no thread, so
there is nothing to read it back into and nobody to explain anything to. Every inbound
envelope is logged at debug level with its event type and whether the bot was mentioned,
so "did Slack deliver it, and did we take it" is answerable from the journal — twice now,
silence in that path was indistinguishable from a message that never arrived.

**Arguments travel form-encoded**, and that is a requirement rather than a preference:
Slack reads a JSON body for some Web API methods and ignores it for others, where the
arguments then look absent instead of malformed — `conversations.replies` answers
`invalid_arguments` and `users.info` answers `user_not_found` for users that exist. Both
methods pi-chat added for observation are in that second group, so the history a turn
should read and the names it should show were silently missing until the client sent
forms (scalars as text, and a structured value such as `blocks` or `prompts` as the JSON
string Slack documents for a form body). The test stub parses what Slack parses, and
answers the strict methods the way Slack answers, so this cannot return unnoticed — the
stub decoding JSON was what let it live.

**Buttons are rendered with unique action ids**, because Slack refuses a whole message
with `invalid_blocks` when one actions block repeats an `action_id`. A picker is exactly
that shape — every button means the same thing and carries its choice in its `value` — and
`/pi resume` was rejected for it the first time it had two sessions to offer. Measured
against the live API: two buttons with one id are refused, the same two with distinct ids
are accepted, and one id repeated across two separate blocks is accepted as well. The core
keeps naming a button by what it means (`resume`, `delete`, `delete-cancel`); the adapter
adds the button's position to the id it renders and takes it off again when the press comes
back, so the vocabulary the core switches on is unchanged. A refused reply path may not
swallow the answer either: when the interaction's `response_url` rejects a notice, the
adapter posts it instead — the picker's failure was reported through that same URL, which
is why the command was silent at both ends.

**A notice that names its thread has named its channel**, and the adapter takes the
channel from either: a notice carrying a thread and an empty channel is posted into the
thread's channel rather than into `""`. Posting to the empty one is how the "attached to
this session" notice after `/pi resume` was answered with `channel_not_found` — a session
that had in fact been adopted, announced as if nothing had happened. For the same reason
the reply path is never the only path: a `response_url` that refuses a notice, or refuses
the *replacement* of one (the outcome of a confirmed delete), falls back to posting, so a
button always answers somewhere.

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

**Two credentials, one surface.** The self-DM surface acts as a person either way,
and the adapter does not care which: `auth = "session"` supplies a pasted browser
session (`xoxc` + `d`), `auth = "user_oauth"` an OAuth user token from the team's
app. The second mode has a second binary beside the daemon: `cmd/pi-chat-oauth`,
the token broker the app owner runs, which exchanges the authorization code and
shows each person their own token once. It is deliberately the confidential-client
flow (`oauth/v2/authorize` with `user_scope`, exchanged through `oauth.v2.access`,
token read from `authed_user`), so it works with an app that also has a bot user and
needs no PKCE; Slack's `v2_user`/`oauth.v2.user.access` flow is the public-client
variant and is not required. Ingress stays polling in both modes: Socket Mode belongs
to the app and spreads payloads across all of its connections, so it cannot be shared
by several people's daemons (`docs/slack-user-token-setup.md`).
`Platform`, and `MultiPlatform` (internal/slack/mux.go) is what stands in front of them:
it routes `StartTurn`, `Post` and `OpenThread` by channel id, with the app as the default
and the configured self-DM channel as the one exception. Routing by channel rather than
by process is what keeps one store, one gateway pool and one warm-session cap, and it
works only because a thread's channel is part of its key. The optional capabilities
(§4 observation, §6 status and suggestions) are claimed by the mux on behalf of the
surfaces: it delegates where the surface implements the interface and reports the empty
answer — no conversation, no status, no prompts — where it does not, which is exactly
what the core sees from a platform that never implemented it.

The app surface is optional in a soft sense (a missing token file turns it off with a
warning) because it is the pre-existing default and an upgrade must not break it. The
self-DM surface is opt-in — off unless `slack.self_dm.enabled` asks for it — and strict
about *configuration*: a shape it cannot use is a startup error rather than something
ignored. A credential it cannot *use* is a different thing: at startup that disables the
surface with an error log while the app surface can still serve, and is fatal only when it
is the only surface, because a stale cookie must neither take the app down nor make
systemd flap on a restart loop. Once running, the poller retries with backoff and says
what to fix, so an expiry is a log line rather than an outage. Its differences from the app are not
incidental and the adapter owns them rather than hiding them: a reply is patched into one
message instead of streamed, a `Notice`'s buttons become a numbered list answered by a
number (the synthesized `Action` carries an event id so the core's dedupe still works),
notices are posted rather than shown ephemerally, and there is no `Observer` — in a self-DM
every message is addressed to the agent, so there is nothing to overhear. Its durable
state is its own: a cursor and a per-thread cursor, plus a ledger of what the daemon
posted, because in that conversation the agent and the human are the same author and only
the ledger can tell them apart (`surface_kv` and `surface_posted`, DESIGN.md §7).

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
  suggested prompts; image passthrough (built); general attachments; `/pi resume` picker
  polish; metrics.

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
| 15 | Two optional surfaces in one daemon, routed by channel; the app surface is disabled by a missing token, the self-DM is opt-in and strict about configuration | One store, one warm cap, one place to reason about senders; the seam already routes by thread, and the channel is part of a thread's key. Missing app tokens are a switch rather than a fault; a self-DM that cannot start is disabled with an error log while the app can still serve, and is fatal only when it is the only surface — a stale cookie must not take the app down or flap under systemd. |
| 16 | The self-DM is polled, not websocketed, and its interactivity is numbered replies | Polling needs no undocumented client endpoints; the surface has no notification (Slack never marks your own message unread) and no response URL, so text is the honest interface. The costs — an account-wide credential and Slack's documented detection of non-official clients — are why the surface is off by default and documented rather than recommended. |
| 17 | Self-DM credentials come in two modes, with a per-person OAuth broker as the team shape | The session mode needs no app and stays the single-machine default; the user-token mode is scoped, revocable and one-per-person, which is what lets one app serve many people. The broker is a separate tiny binary because it is the only part that must be reachable from the internet; each daemon still polls its own token, because Socket Mode's fan-out cannot be partitioned per authorization. |
