# pi-chat — Milestones & Working State

Reference file for resuming work after a context reset. Design and rationale live in
[`DESIGN.md`](./DESIGN.md); this file is the operational plan, the environment facts, and
the current position. **Update the status block and checkboxes as work lands.**

Last updated: 2026-09-26 · repo `/home/pi/code/pi-gchat` (module now
`github.com/tigersoldier/pi-chat`) — M0 and M2 done; the Slack app (M1) is what M3 waits
for.

---

## Status snapshot

| Item | State |
|---|---|
| Design | **Settled.** All 14 questions answered; `DESIGN.md` rewritten for the pi-gateway-based, Slack-first scope |
| M0 — gateway running | **Done and verified** (see M0 below) |
| M2 — repo prep | **Done** (see M2 below). Module is `github.com/tigersoldier/pi-chat`; no Google Chat artifact and no GCP dependency remains |
| Code | `internal/config` + `cmd/pi-chatd --check` (loads, validates, redacts). No Slack or gateway code yet; `tools/gateway-probe/` is the working seam smoke test |
| `pi-gatewayd` | **Installed and running** as a user unit; built from `~/code/pi-gateway` @ `119f0f2` (= tag `v0.1.2` content), self-reports `0.2.0` |
| Slack app (M1) | **Not created** — this is what unblocks M3 |
| GitHub repo rename | **Not done** — browser action; local `origin` still says `pi-gchat`, which keeps working through GitHub's redirect |
| Upstream rebind fix | **Not filed.** Owner is the user |

**Current position: M1 (Slack app) is the critical path; M3 follows it.**

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
- [x] M0.4 Wrote `~/.config/pi-chat/config.toml` (0600) per DESIGN §11; `allowed_users`
      still empty pending M1
- [x] M0.5 `tools/gateway-probe` (`go run .` there) → **PASS**: admin token granted
      `observe,interject,prompt,ui,control,admin` and started session-less; thread token
      granted the five expected capabilities and `gw_new_session` was refused with
      `forbidden`; `SwitchSession` + `Prompt` on the second connection worked;
      `set_thinking_level` proved `control`; `gw_delete_session` (force=false) unbind both
      connections and a later prompt was refused with `unknown_session`; catalog row gone

**Incident during M0.3:** a leftover daemon from the earlier v0.1.2 e2e validation
(`/tmp/e2e/bin/pi-gatewayd`, up for a day) held port 7331 and made the unit restart-loop.
Killed it; no other instances remain.

### M1 — Slack app (user, browser, ~15 min)

- [ ] M1.1 Create app from scratch; **enable Socket Mode**; app-level token with
      `connections:write`
- [ ] M1.2 Bot scopes, phase 0: `app_mentions:read`, `chat:write`
- [ ] M1.3 Install to workspace; invite `@pi` to a test channel
- [ ] M1.4 V1 scopes (add when phase 1 starts): `channels:history`, `groups:history`,
      `im:history`, `mpim:history`, `assistant:write`, `files:read`, `reactions:write`
- [ ] M1.5 V1 settings: enable **Interactivity** (no request URL needed under Socket
      Mode) and register the `/pi` slash command

**Exit:** tokens written to `~/.config/pi-chat/slack-{app,bot}-token`, bot reachable.
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

- [ ] M3.1 `cmd/pi-chatd` skeleton: config load, logging, signal handling
- [ ] M3.2 `internal/slack`: Socket Mode connect, envelope ack inside 3 s, mention parse,
      thread key `{workspace}:{channel}:{thread_ts}`, `chat.postMessage`
- [ ] M3.3 `internal/bot`: admin connection → `gw_new_session` → `SwitchSession(path)` →
      `Prompt`; subscribe to events
- [ ] M3.4 Render the turn into the thread — streaming (`chat.startStream` /
      `appendStream` / `stopStream`) with a patched-placeholder fallback
- [ ] M3.5 **Deliverable: a streamed pi turn visible inside a Slack thread**

**Exit:** the slice runs against the real daemon and a stub or real `pi`.
**Depends on:** M0, M1.

### M4 — Phase 1 MVP

- [ ] Allowlist enforcement **before any side effect**; denial message
- [ ] SQLite (`threads`, `seen`, `pending_ui`, `admissions`) — DESIGN §7; persist before
      ack; `seen` mandatory (upstream prompt idempotency still open)
- [ ] Session provisioning: project dir, `injected_prompt` (worktree convention),
      deterministic session names
- [ ] Thread-scoped sessions; root is session-less; warm/cold idle-close
- [ ] Prompt → streamed answer, cursor persistence, `Resume` on re-dial
- [ ] Commands: `/pi help`, `/pi status`, `/pi resume`, `@pi /status`, `@pi /<command>`
      pass-through, near-miss guard
- [ ] `--approve` mode; systemd user unit; startup orphan GC

### M5 — Phase 2

- [ ] `@pi /delete` with button confirm → `DeleteSession` → worktree/branch cleanup → row
- [ ] `@pi /abort`, `@pi /model`, `@pi /stop`
- [ ] Interactive approvals: buttons + modal via `trigger_id`; `pending_ui` → `RespondUI`
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
  streaming needs `recipient_user_id` + `recipient_team_id`; `assistant.threads.setStatus`
  / `setTitle` need `assistant:write`.

### Testing approach

Run the real `pi-gatewayd` pointed at a **stub `pi` binary** (`--pi`), so the gateway
contract is exercised for real while the agent is fake; drive the adapter with recorded
Slack payloads. Cheaper and less brittle than mocking `gwclient`.

---

## Next action

1. **M1 in the browser** (user) — the only thing gating M3: create the Slack app,
   enable Socket Mode, generate the app-level token (`connections:write`), add the
   phase-0 bot scopes (`app_mentions:read`, `chat:write`), install it, invite `@pi` to a
   test channel, write the two token values to
   `~/.config/pi-chat/slack-{app,bot}-token` (0600), and put your Slack member ID in
   `allowed_users` (`make check` shows whether the files are seen).
2. **M3** once those tokens exist: the phase-0 vertical slice.
3. **Optionally, whenever you like:** rename the GitHub repo to `pi-chat`, and file the
   upstream rebind fix (DESIGN §3) — neither blocks M3.

The gateway side is ready: `pi-gatewayd` is running, both tokens work, and the seam is
proven by `tools/gateway-probe`. The repo side is ready: `make check` validates the live
configuration, `make test` and `make vet` are clean.
