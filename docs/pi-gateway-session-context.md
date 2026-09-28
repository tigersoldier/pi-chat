# Session context injection and durable spawn configuration

A proposal for **pi-gateway**, written from the client side by **pi-chat** (a Slack bot
integration that drives gateway sessions).

| | |
|---|---|
| Status | Proposal. Nothing in pi-chat is built on it yet. |
| Against | pi-gateway `119f0f2`, pi `0.85.1`, RPC protocol 1 |
| Author | pi-chat, after implementing status, stop and streaming against this daemon |

## Outcome (pi-gateway v0.1.3, `a47e0ad`)

| Ask | Result |
|---|---|
| P1 `inject` + `context` capability | **Implemented, then removed** (`a47e0ad`). No released pi exposes `send_message` over RPC — 0.85.1 has none in `rpc-types.d.ts` or `rpc-mode.js` — so the command could only ever answer `not_supported`, and advertising it in `features` would invite clients to depend on something that cannot work. |
| P2 durable spawn configuration | **Shipped.** Sidecar per session under `<stateDir>/spawn/`, written atomically; recorded spawn-only values win on a cold respawn, the requester fills keys the record never set; delete removes it, `fork`/`clone` copies it; credential values are redacted from the exposed view while the full argv stays internal; a sidecar that cannot be read is reported instead of silently meaning "nothing recorded". |
| P3 exposure | **Shipped.** `gw_list_sessions[].spawn` (canonical key→values, redacted, omitted when nothing is recorded; the unauthenticated debug `/catalog` omits it) and `gw_welcome.features: ["spawn_config"]`. |
| P4 replace on reload | **Shipped.** `gw_reload_session{session?, force?, piArgs?}` replaces the record, requires `control`, and the actor adopts the replacement; refused with `reload_busy` while a turn runs or another client is attached. |
| R5 (never choose between installing and attaching) | **Shipped, with a documented asymmetry.** A client that names a key the record never set is no longer refused; a *live* session with a *differing* value for a *recorded* key still answers `spawn_param_conflict`. |

Also in the release: stop/delete report `unbound: true` and `gwclient` clears its binding from it; `gwclient.SessionRow` is now an alias of `protocol.SessionRow`; `ReloadSession` takes optional `piArgs`.

**Still open:** the §3 stop-rebind ask. The guard that stops an unbound connection from lazily creating a session is still set for `deleted` only, so a session-scoped command after a *stop* still creates a second session. `unbound: true` tells the client immediately, which makes it easier to avoid, but the daemon-side lazy creation stands.

**Revised plan for a standing instruction** (upstream's prescription, and it needs no gateway change):

1. Sessions the bot **creates**: put the instruction in the `--append-system-prompt` it already passes at `gw_new_session`. P2 keeps it installed across hibernation, daemon restarts and respawns triggered by other clients — and it is never compacted, because compaction summarizes messages, not the system prompt.
2. Sessions the bot **adopts**: prefix the instruction to the first prompt it sends in that session, wrapped in `<slack-specific-instructions>…</slack-specific-instructions>`, and prefix it again after an observed `compaction_end` — which the event stream already delivers.
3. **New participants** need nothing: the mapping is per line in the transcript block.

The record makes the instruction *static per session*: changing the text later does not reach an existing session unless it is recreated or `gw_reload_session{piArgs}` is used (which is the deliberate P4 path, and unavailable while another client is attached).

### Verified on this machine (v0.1.3 deployed)

- `gw_welcome.features` reports `["spawn_config"]`; grants are unchanged.
- A session created with `--append-system-prompt` shows it in `gw_list_sessions[].spawn`, and **still shows it after a daemon restart** — read from the sidecar, since nothing else survives. That is the durability promise, confirmed end to end.
- `gw_delete_session` removes the real record.
- pi-chat itself passes its live test against the new daemon with the old client library, so the upgrade is compatible in both directions.

**Defect found while verifying, reported upstream.** `handleGWNewSession` calls
`setCreated` *before* it asks pi for its state, so `setCreated`'s
`canonicalPath(a.Path())` runs with an empty path — and `canonicalPath("")` resolves to
the daemon's working directory, not to nothing. Every `gw_new_session` therefore writes
one bogus sidecar keyed `/home/pi`, holding whichever session was created last. The
correct record is written later by `onPath`, so durability is unaffected; but
`gw_delete_session` deletes by the session's real canonical path, so the bogus file
survives every delete, and `setCreated`'s `canon != ""` guard does not catch it because
`canonicalPath("")` is not empty.

```text
create a session with --append-system-prompt, then ls <stateDir>/spawn/
  -> two files: the real session path, and /home/pi
DeleteSession, then ls again
  -> the real record is gone; /home/pi remains, holding the deleted session's config
```

Suggested fix: persist only from `onPath`/`replaceSpawn`, or make `canonicalPath` refuse
an empty path (the guard then works as intended).

---

## 1. Context

pi-chat maps one pi session to one Slack thread. A Slack thread is not a terminal: the
person typing at any moment is only one of the participants, and the agent is expected to
know what the others said. Two things follow.

**The agent must see the conversation, not just the prompt.** Today pi-chat reads the
thread with `conversations.replies` (scoped to the messages posted since its last turn)
and folds a labelled transcript into the trigger prompt:

```text
[pi-chat] You are answering Alice (U6K8Y3FV1) in a Slack thread.
In this Slack thread since your last turn:
[Bob (U0C4TM8KT5F)] the table test is the flaky one
[Alice (U6K8Y3FV1)] whatever you do, don't touch the retry logic

fix it
```

**The agent needs a standing instruction about where it is.** The block above is not
self-explanatory: it is a format we invented, it identifies people by a Slack id, and it
tells the agent that `<@U123>` is how you address a human. That instruction should be
installed once per session — not re-sent with every turn — and it has to stay installed
across compaction and respawns.

pi-chat can do the first part today. The second part is this proposal, and it is not
Slack-specific: any integration with a conversation of its own (Discord, Matrix, email,
an issue tracker) has the same shape, and every one of them is currently forced to
prepend instruction text to prompts.

### What pi-chat does today, and why it is not enough

pi-chat creates sessions with `gw_new_session{piArgs: ["--append-system-prompt", …]}`
(the project's injected prompt), which shows the mechanism works. It cannot use it for
the Slack instruction because:

- adopted sessions (created from a terminal via pilish, then `/pi resume`d) can never
  receive a spawn parameter after the fact;
- the parameter is not persisted, so even sessions pi-chat created lose it on a daemon
  restart;
- pi-chat must not pass spawn parameters blindly, because a client that requests a
  spawn value the session does not have is **refused attachment** (§4.2).

## 2. Requirements

| # | Requirement |
|---|---|
| R1 | Install a standing instruction (a non-turn message) into a **live** session, including one pi-chat did not create. |
| R2 | Re-install or refresh it after compaction, without the client guessing. |
| R3 | Have an instruction installed at creation survive hibernation, a daemon restart, and a respawn triggered by another client. |
| R4 | Tell whether an instruction is installed, so a client does not inject twice and can detect that it was lost. |
| R5 | Never be forced to choose between "attach with spawn parameters" and "attach at all". |
| R6 | Keep all of it capability-gated and correct for sessions with several clients attached. |

## 3. What exists today

| Capability | Supported | How |
|---|---|---|
| System prompt addition at spawn | **Yes** | `gw_new_session{piArgs}`; `--append-system-prompt` is in the accepted set (`piargs/piargs.go`, `docs/protocol.md` §4.3) |
| Message appended to a session without a turn | **No** for clients | pi has the primitive, but only for extensions: `pi.sendMessage({customType, content}, {deliverAs: "nextTurn"})` — *"Queued for next user prompt. Does not interrupt or trigger anything."* |
| Compaction visible to clients | **Yes** | `compaction_start` / `compaction_end` are in the session event stream (`internal/session/hub.go`), delivered to attached clients |
| Spawn configuration durable | **No** | Held in memory: `entry.spawn` (`internal/daemon/daemon.go`) and `session.Params.PiArgs` (`internal/session/actor.go`) |
| Spawn configuration visible | **No** | `get_state` reports model/thinking/`isCompacting`/`messageCount`; catalog rows report path/name/title/cwd/live/createdBy/clients (`gwclient/gwclient.go`) |

Two near-misses that are *not* the primitive, and should stay out of it:

- `steer` is a **user** message ("Queue a steering message while the agent is running",
  `docs/rpc.md`); it participates as the user, and while idle a prompt-shaped message
  starts a turn.
- `follow_up` is queued work, not context.

## 4. Gaps

### G1 — No on-demand, non-turn message injection

Neither the gateway's passthrough set (`prompt`, `steer`, `follow_up`, `compact`, `bash`,
… `docs/protocol.md` §4.2) nor pi's RPC has a way to append a system/custom message to a
live session. The primitive exists **inside** pi (the extension API above), which proves
it is implementable in pi's model — it is simply not reachable by a client.

Consequence: a client that cannot pass spawn parameters must put its instruction in the
prompt text. Costs: tokens on every turn, and the instruction shows up as a user message
in every other client's transcript (a terminal attached to the same session sees the bot
talking to itself).

### G2 — Spawn parameters are fixed at creation

`--append-system-prompt` is a spawn value, not a runtime value:

- `gw_reload_session{session, force}` restarts pi with the actor's recorded parameters;
  it takes no `piArgs`.
- A client attaching with a key whose value differs from the recorded one is refused:

  ```go
  // piargs/piargs.go
  func SpawnConflict(recorded, requested map[string][]string) (string, bool) {
      for key, want := range requested {
          if !sameValues(recorded[key], want) {
              return key, true
          }
      }
      return "", false
  }
  ```

  Requesting `--append-system-prompt X` against a session recorded without it is a
  conflict (empty vs `X`), answered `spawn_param_conflict` — the client cannot attach at
  all.

Consequence: an integration must decide between installing its instruction and attaching
to other people's sessions, and the safe choice is to never install one.

### G3 — Spawn configuration is not persisted

`attach` builds a new actor from **the requesting client's** spec whenever the session is
not live (`internal/daemon/daemon.go`):

```go
a, err := d.newActor(canon, spec, spawnCwd(canon, cwd))
```

and after a daemon restart the only thing recovered from a session file is `cwd`
(`internal/catalog/catalog.go`, `HeaderCwd`). So the first client to attach after a
restart decides how pi is spawned, and any spawn parameter the session was created with
is gone.

Consequence, reproduced inside pi-chat: sessions are created with the project's
`--append-system-prompt`, but the long-lived thread connection dials with no `piArgs`, so
after a `pi-gatewayd` restart the respawned pi process **no longer has the project
prompt**. Hibernation within one daemon lifetime is fine (the actor keeps its params); a
daemon restart is not. This is a live defect in our integration rather than a request
that pi-gateway is breaking, but it is caused by the same missing piece of state.

### G4 — Spawn configuration cannot be inspected

There is no way to ask "was my instruction installed, and will attaching conflict?" —
neither in `get_state` nor in the catalog rows. A client can only try and interpret the
error, and cannot tell a lost instruction from one that was never there.

## 5. Proposal

Four items, in the order I would implement them. P1 and P2 are the ones pi-chat actually
needs; P3 and P4 are small and make them usable.

### P1 — `inject`: append a message to a session without a turn

A new command, forwarded to pi (pi owns the session file and the LLM context; the gateway
must not write to either behind pi's back).

```json
{"type":"inject","id":"i1","deliverAs":"nextTurn","dedupeKey":"slack:T1:C1:1700000001.000100",
 "message":{"role":"custom","customType":"pi-chat/slack-context","display":false,
            "content":"[pi-chat] You are answering Alice (U6K8Y3FV1) in a Slack thread…"}}
```

Semantics:

- **It never starts a turn.** That is the entire point; a client that wants a turn calls
  `prompt`. `deliverAs` selects when the message joins the context:
  `nextTurn` (default — held until the next user prompt), `steer` (with the running
  turn, like `steer`), `followUp` (after the current turn finishes).
- **It participates in LLM context** (as pi's `custom`/`custom_message` role already
  does) but is not a user message, so it cannot be mistaken for the person typing.
- **`display`** decides whether an attached TUI renders it. Integrations want
  `display: false` for a format preamble and arguably `true` for "Bob said …", since the
  terminal client is looking at the same session.
- **Response**: `{success:true, data:{queued:true}}`, in the shape `prompt` already uses
  ("accepted or queued, not complete", §4.2).
- **Ordering**: FIFO with prompts from the same client, so an instruction injected just
  before a prompt is in the context of that prompt.
- **Idempotency**: an optional `dedupeKey`; the daemon and pi both see retries across
  reconnects. (Without it, a client that reconnects and re-injects duplicates its
  instruction — which is exactly the kind of bug that is invisible until the model starts
  repeating itself.)
- **Capability**: I would add a `context` capability rather than hang it off `prompt`.
  A prompt-capable client can already inject arbitrary text, so `prompt` would be
  defensible, but the permission being granted is "may write into this session's context
  without being seen", and it should be nameable.
- **Errors**: `unknown_session` when unbound; `session_deleted` semantics as today;
  `not_supported` when the pi in use has no such RPC command.

The pi-side dependency is the real work: pi's RPC needs a client-reachable equivalent of
the extension API's `pi.sendMessage` (a `send_message` command with
`{customType, content, display, deliverAs, triggerTurn:false}`). The gateway then
forwards it and gates the capability. Until pi exposes it, the gateway can answer
`not_supported` truthfully and P2 still helps.

### P2 — Persist spawn configuration with the session

Store the spawn parameters a session was created with, and re-apply them when a new pi
process is spawned for that session.

- **Where**: a daemon-owned sidecar keyed by the canonical session path (for example
  `<stateDir>/spawn/<hash>.json`), rewritten atomically. Not in pi's session file: that
  format belongs to pi, and a gateway-written field would be a compatibility promise we
  cannot keep. Not in memory: that is the bug.
- **What**: the accepted `piArgs` (in canonical key form, so `-a` and `--approve` cannot
  both appear), the spawn `cwd`, the creator, the creation time, and the session id when
  known.
- **Merge rule on attach**: recorded values win for keys that define how the session
  runs (extensions, tools, system prompt); the requester's values fill in what was never
  recorded; runtime-adjustable values (`model`, `provider`, `thinking`, `name`) keep
  today's `applyRuntime` behaviour. Rationale: a second client attaching must not silently
  drop the first client's instruction, and must not be able to change how a session runs
  by accident — only on purpose (P4).
- **Cleanup**: remove the sidecar on `gw_delete_session`; copy it on `fork`/`clone` (the
  new session starts from the same configuration).
- **Sessions predating this**: no sidecar means "nothing recorded", i.e. exactly today's
  behaviour.

P2 alone fixes R3 and, incidentally, the project-prompt loss described in G3.

### P3 — Expose what is installed

- Add the recorded spawn values to `gw_list_sessions` rows (`spawn: {key: [values]}`), so
  a client can check before attaching and can tell a lost instruction from an absent one.
  The daemon already merges daemon-side state into that answer.
- Add a `features` array to `gw_welcome` (e.g. `["inject", "spawn_config"]`), so clients
  detect support instead of probing with a command that may have side effects or reading
  an error string.

### P4 (optional) — Replace the configuration, deliberately

For the case where an instruction must be *upgraded* on an existing session:
`gw_reload_session{session, force, piArgs}` would replace the recorded spawn values and
restart pi. This is disruptive (a restart, and a re-attach for every client), and
requires `admin`. With P1 in place, pi-chat would use `inject` rather than reload; P4 is
here for completeness and for clients that need the instruction in the actual system
prompt.

## 6. What pi-chat builds when this lands

| Requirement | With P1–P3 |
|---|---|
| Instruction at creation, invisible, compaction-proof | `piArgs` + `--append-system-prompt` at `gw_new_session`; P2 keeps it installed across restarts |
| Instruction in an adopted session | one `inject{deliverAs:"nextTurn"}` on the first Slack turn in that session |
| Refresh after compaction | `compaction_end` is already in the event stream; re-inject then — not every turn |
| Identity line per turn | stays in the prompt (it changes every turn: who is being answered) |
| Terminal clients | see the transcript as rendered text, or as custom messages if `display: true` |
| Tokens | instruction paid once per session (and once per compaction), not per turn |

A second consumer appears for free: the thread transcript itself (the block in §1) could
become a sequence of injected custom messages instead of quoted text inside the prompt,
which is what makes a shared session readable in a terminal. That is pi-chat's Q4-B
option, and it needs exactly this primitive.

## 7. Interim

If this proposal waits, a client has two honest options:

1. **Hold the feature** until P1/P2 exist (pi-chat's current choice), or
2. **Ship with prompt-prefixed instructions** and delete the prefix when the primitive
   lands: ~90 tokens per turn, visible in shared transcripts, and still bounded above by
   the same compaction behaviour.

P2 is worth doing on its own even for clients that never inject: it removes a whole class
of "the session quietly lost its configuration" failures, of which we have already
reproduced one.

## 8. Alternatives considered

| Alternative | Why not |
|---|---|
| Use `steer`/`follow_up` as the injection | They are user messages: they queue as work and can start a turn. `steer` cannot be withdrawn except by a session-wide `clear_queue`. |
| Write the instruction into the first prompt, once | Compaction summarizes messages away; and it is indistinguishable from the human typing it. |
| Ship a pi extension instead (`pi.sendMessage` in-process) | The only zero-upstream-change path today, and genuinely viable for a single deployment: install a user-level extension, drive it via `prompt /command` (extension commands bypass the agent loop). Costs: a second runtime and language in the product, an install step on every machine that runs pi, no help for sessions whose pi was already spawned, and the injection dies with the "no spawn parameter can be added later" rule for anything other than the extension's own logic. Worth doing only as a private stopgap. |
| Have the gateway append to pi's session file directly | pi owns the file and its in-memory state; the gateway would be writing to a format it does not own, behind pi's back. |
| Put the instruction in the gateway's own per-turn prefix | The gateway is not in the prompt path by design: prompts are forwarded, not composed. Making it compose them would put integration-specific text into every client's session. |

## 9. Compatibility

- P1 is additive: a new command type, gated by a new capability, absent from
  `granted` on older tokens. An older daemon forwards it to pi (which will answer its own
  unknown-command error); an older pi means `not_supported`. `gw_welcome.features` (P3)
  makes that check cheap and side-effect-free.
- P2 is invisible to clients that pass no spawn parameters, and is what makes the
  documented behaviour of `gw_new_session{piArgs}` ("created with `piArgs`") actually hold
  over time. The merge rule should be documented in §4.3 alongside the accepted set.
- P3 adds fields to existing responses; both the catalog and `gw_welcome` are already
  extensible JSON objects, and pi-chat decodes them into structs with the fields it
  knows.

## Appendix: evidence

All line references are against pi-gateway `119f0f2`.

| Claim | Evidence |
|---|---|
| `--append-system-prompt` is an accepted spawn parameter | `piargs/piargs.go` (`valueFlags`), `docs/protocol.md` §4.3 |
| The gateway keeps spawn values only in memory | `internal/daemon/daemon.go` (`entry{actor, spawn: spec.SpawnValues()}`), `internal/session/actor.go` (`PiArgs []string`) |
| A non-live session is respawned with the attaching client's spec | `internal/daemon/daemon.go`, `attach`: `d.newActor(canon, spec, spawnCwd(canon, cwd))` |
| Only `cwd` is recovered from a session file | `internal/catalog/catalog.go`, `HeaderCwd` |
| Requesting a differing spawn value refuses attachment | `piargs/piargs.go`, `SpawnConflict`; `internal/daemon/daemon.go`, `attach` returns `CodeSpawnParamConflict` |
| The gateway has no non-turn injection command | `docs/protocol.md` §4.2 (passthrough set); `internal/daemon/conn.go` dispatch |
| pi has the primitive, extensions only | pi `docs/extensions.md`: `pi.sendMessage(message, options)` with `deliverAs: "nextTurn"` — *"Queued for next user prompt. Does not interrupt or trigger anything."* |
| pi's client-facing messages are user messages | pi `docs/rpc.md`: `prompt`, `steer`, `follow_up` |
| Compaction is visible to clients | `internal/session/hub.go` (`compaction_start`, `compaction_end`), delivered on the session event stream |
| Spawn configuration is not readable by clients | pi `docs/rpc.md` `get_state`; `gwclient/gwclient.go` `SessionRow` |
