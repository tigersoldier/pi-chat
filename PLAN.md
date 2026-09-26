# pi-chat — Milestones & Working State

Reference file for resuming work after a context reset. Design and rationale live in
[`DESIGN.md`](./DESIGN.md); this file is the operational plan, the environment facts, and
the current position. **Update the status block and checkboxes as work lands.**

Last updated: 2026-09-26 · repo `/home/pi/code/pi-gchat` @ `d01ec9c` (DESIGN.md rewritten,
uncommitted).

---

## Status snapshot

| Item | State |
|---|---|
| Design | **Settled.** All 14 questions answered; `DESIGN.md` rewritten for the pi-gateway-based, Slack-first scope |
| Code | **Old Google Chat spike still present.** `cmd/probe`, `scripts/setup-gcp.sh`, GCP/Pub/Sub deps — none of it survives. No Slack or gateway code written yet |
| `pi-gatewayd` | **Not installed** (`command not found`). Blocks everything |
| Slack app | **Not created** |
| Repo rename | **Not done** (module is still `pi-gchat`) |
| Upstream rebind fix | **Not filed.** Owner is the user |

**Current position: start at M0.**

---

## Milestones

### M0 — Gateway running (blocking prerequisite)

Prove the session plane end to end with no Slack code at all.

- [ ] M0.1 Build + install `pi-gatewayd` from the existing clone `/tmp/pigw` (checked out
      `v0.1.2`) into `~/.local/bin`; `pi-gatewayd --version` reports v0.1.2
- [ ] M0.2 Provision two tokens into 0600 files: one `admin` (lifecycle), one with
      `operator` + `control` and **no** `admin` (per-thread). Upstream doc example
      `{"name":"slack","role":"operator"}` is **wrong** for this bot — `operator` alone
      lacks `control`
- [ ] M0.3 Start the daemon; confirm state dir (`~/.config/pi-gateway`), port and token
      discovery
- [ ] M0.4 Write `~/.config/pi-chat/config.toml` (shape: DESIGN §11)
- [ ] M0.5 Scratch `gwclient` program (throwaway, e.g. `/tmp/gwtest*/`): `NewSession` →
      `SwitchSession` → `Prompt` → print events. Verifies both tokens' capabilities
      before any Slack work

**Exit:** a prompt driven entirely through `gwclient` prints a real pi turn.
**Owner:** assistant (user confirms daemon config).

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

### M2 — Repo prep (mechanical, independent of M0/M1)

- [ ] M2.1 Rename module `pi-gchat` → `github.com/tigersoldier/pi-chat` (no external
      importers; also rename the GitHub repo and update the remote)
- [ ] M2.2 Delete Google Chat artifacts: `cmd/probe`, `scripts/setup-gcp.sh`,
      `scripts/render-assets.sh`, `docs/phase0.md`, `docs/chat-app-setup.md`,
      `pi-gchat.toml.example`, `assets/` (Chat artwork),
      `.github/workflows/static.yml`, `PRIVACY.md`, `TERMS.md` (no Marketplace listing
      in v1 ⇒ no listing artifacts)
- [ ] M2.3 Drop the GCP/Pub/Sub dependency tree; add `pi-gateway v0.1.2`; `go mod tidy`
- [ ] M2.4 Rewrite `Makefile`, `README.md` (Slack-first, `pi-gatewayd` prerequisite),
      `.gitignore`, and add a config example matching DESIGN §11
- [ ] M2.5 `go build ./...` clean with zero Google dependencies

**Exit:** repo builds, nothing Google-Chat-specific remains outside DESIGN/history.

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

### Upstream facts to keep in mind (verified against v0.1.2)

- Exported packages usable: `gwclient`, `protocol`, `config`, `piargs`.
- `admin` required for `gw_new_session` / `gw_stop_session` / `gw_delete_session`;
  `control` for `set_model`, `compact`, `set_session_name`. `operator` lacks `control`.
- One connection binds to exactly one session; `SwitchSession` rebinds.
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

Start **M0.1** (build and install `pi-gatewayd`), then M0.2–M0.5, then **M2** while the
user does **M1**. Ask before starting M3.
