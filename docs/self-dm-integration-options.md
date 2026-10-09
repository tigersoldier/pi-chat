# Self-DM integration shapes: one daemon (mux) or a separate service

Follow-up to [`self-dm-feasibility.md`](self-dm-feasibility.md). That doc judges
the *transport* (session credentials, polling, capability and policy costs).
This one judges the *structure*: given that a self-DM surface may exist, should it
live in `pi-chatd` beside the app surface, or in its own process?

**Verdict.** Option 1 — one daemon, a multiplexing `Platform`, a second ingress
loop — is the right shape: the codebase was built for it, the change is confined
to the adapter side of the seam, and it costs a few hundred lines more than a
single-surface self-DM build. A separate service is only justified when the two
surfaces must **not** share fate or state; if you take it, take the version with
its own database and projects root, because every shared-state variant collides
with four global operations documented in the code below.

Investigated 2026-01 · pi-chat @ `5d8bc66`.

**Status (2026-01): option 1 shipped.** One daemon; `slack.MultiPlatform` routes
by channel id; the self-DM is a second `bot.Platform` with a poller, a posted
ledger and numbered replies standing in for buttons; each ingress has its own
supervision; and both surfaces are independently optional in configuration (the
app surface turns itself off when its token files are missing, the self-DM is
opt-in and strict). Deliberately not shipped from the sketch: the internal client
websocket (polling needs no undocumented endpoint), `Observer` and the agent
surface capabilities (the mux reports the empty answer), and every shared-state
variant (2b/2c stay unbuilt).

---

## 1. What the code already gives you

The economics of both options are set by how surface-agnostic the existing code
already is. It is mostly good news:

| Fact | Where | Consequence |
|---|---|---|
| The core never imports an adapter; `Platform` is the whole adapter half | `internal/bot/types.go` `Platform`, `internal/bot/bot.go` package doc | A second surface is an adapter problem, not a core rewrite |
| Ingress is four plain calls — `HandleMessage`, `HandleCommand`, `HandleAction`, `HandleOpened` | `internal/slack/router.go` `Core` | A second ingress loop needs no new core entry point |
| Thread identity is `workspace:channel:thread_ts` | `internal/bot/types.go` `Thread.Key` | The self-DM `D…` channel and the bot DM `D…` channel are different rows; no key collision |
| Session name is `slack-<ws>-<channel>-<ts>` (`platformName` is a const) | `internal/bot/types.go` `SessionName` | Same, for pi session names; **keep the `slack` prefix for both surfaces**, changing it would orphan existing sessions |
| "The conversation is the address" is already modelled | `Message.Direct`, `Message.Mentioned` | The self-DM maps onto the DM grammar exactly (see feasibility doc §5) |
| Capabilities are optional interfaces: `Observer`, `StatusReporter`, `PromptReporter`, per-renderer `ProgressReporter` | `internal/bot/types.go:147-226` | A surface that cannot do something simply does not implement it |
| Dedupe/claim is central and message-keyed | `internal/bot/bot.go` `messageClaim`, `internal/store/seen` | Two ingresses in one core cannot double-answer |
| Durable state is keyed by thread (`threads`, `seen`, `retired`, `admissions`) | `internal/store/store.go` schema | Surfaces share state safely *if they share one store* — see §4 |

Two facts cut the other way:

- **`Bot` holds exactly one platform** (`plat Platform`) and type-asserts it for
  every capability (`bot.go:274`, `bot.go:625`, `observe.go:77`). Multi-surface
  means either a mux that satisfies those interfaces, or threading a
  surface→platform map through the core. The mux is strictly less invasive.
- **Four operations are global, not per-thread**: `MarkThreadsCold` (all rows),
  the warm count (`store.Threads` → cap), `adoptable` (gateway catalog minus all
  bound thread rows), and `SweepWorkspaces` (every row's project dir). These are
  exactly the operations that make shared-state multi-process setups fight.

---

## 2. Option 1 — one daemon, both surfaces

### Shape

```text
cmd/pi-chatd
├── slack app surface       Socket Mode ─┐
│   (existing Platform)                  │
├── self-DM surface         poller ──────┤→ bot.Bot (one core, one store, one gateway pool)
│   (new Platform)                       │
└── MultiPlatform (mux) ←────────────────┘   routes by channel id
```

- **Ingress.** The socket loop stays. A second goroutine polls the self-DM and
  calls the same `Core` methods. The core's per-thread locking and the `seen`
  claim already make concurrent ingresses safe; no router changes beyond a second
  mapper (history message → `bot.Message`/`bot.Command`).
- **Egress.** A `MultiPlatform` implements `bot.Platform`. Routing is by channel:
  the configured self-DM channel id → self-DM platform, everything else → app
  platform. `StartTurn(m Message)` routes on `m.Thread.Channel`, `Post(n Notice)`
  on `n.Thread.Channel` then `n.Channel`, `OpenThread(channel, …)` on `channel`.
  That is the whole rule, and it is one config value.
- **Capabilities.** The mux implements `Observer`/`StatusReporter`/
  `PromptReporter` by delegating *only to surfaces that have them*; the self-DM
  platform simply does not implement them. The core's best-effort status path
  (`setStatus`) then costs a type assertion and nothing else. Per-surface refusal
  latches (`statusRefused`, `promptsRefused` in `internal/slack/platform.go`) stay
  where they are — inside each surface — so a plain-bot app never poisons the
  agent surface, and vice versa.
- **Identity.** Each surface carries its own: the app's `slack.Identity{TeamID,
  UserID, BotID}` from `auth.test`, the self-DM's `{TeamID, UserID: <the human>}`
  with no bot id. `Identity.UserID` is what strips mentions and what marks
  "own messages" in a transcript — so for the self-DM, "own messages" must come
  from the daemon's post ledger, not from authorship (feasibility doc §5). That
  ledger is shared state between the poller and the renderer and belongs in the
  self-DM platform object.
- **Transport asymmetry.** The app surface streams via `chat.startStream`/
  `chat.appendStream`; the self-DM cannot (agent APIs are app-only) and must
  patch via `chat.update`. `renderer.wantStream` is set from the *global*
  `cfg.Render.Mode` (`platform.go:116`), so the self-DM renderer has to force
  patch mode itself or the config grows a per-surface render mode. One line, but
  it is the kind of asymmetry the mux has to own.
- **Failure policy — the one real change to `main.go`.** Today the socket dying
  cancels the whole service ("this is one service, not two processes sharing a
  token"). A self-DM cookie *will* expire (about yearly) and *will* return
  `invalid_auth`. So ingress failure must become per-surface: retry with backoff,
  log loudly, mark that surface down, keep the other running. That is a change in
  supervision, not in the core.
- **Single instance.** Unchanged: Socket Mode still forbids a second app-surface
  connection in the same app. The poller adds no such constraint, but the
  instance rule stays global for simplicity.

### Effort

| Work | Est. |
|---|---|
| `MultiPlatform` + route registry + tests (two fakes) | ~200 lines, ½ day |
| Self-DM adapter: session auth/cookie HTTP client, history read, poller with jitter/backoff, self-post ledger, patch renderer, degraded notice rendering (buttons → text) | ~500–700 lines, 1.5–2 days |
| Config surface (`[slack.self_dm]`, creds files, channel id, poll interval) + `--check` summary | ~100 |
| `main.go` wiring, per-surface supervision, capability plumbing | ~150 |
| Text-command/approval vocabulary in the core (the gated phase-2 work) | ~200 |
| Tests, docs, manifest/DESIGN updates | ~400 |

Total a bit above the transport-only estimate in the feasibility doc; the mux
itself is the cheapest part of it.

### Risks specific to this shape

- One process, one blast radius: a panic or a hang in the self-DM adapter takes
  the app surface with it (unless the supervisor restarts only that loop, which
  goroutines make easy but sloppy).
- One store, one writer connection (`SetMaxOpenConns(1)`): fine — the load is
  rows per turn — but a pathological poller must not hold the store.
- Global config now describes two surfaces (`render.mode`, access rules). Config
  ergonomics, not safety.
- `Bot`'s doc comment ("one instance per process, because Socket Mode distributes
  deliveries") becomes half-true and should be reworded rather than left as a lie.

---

## 3. Option 2 — a separate service

Three sub-variants, and the distinction matters more than "separate process".

### 2a. Separate process, separate state (own DB, own projects root)

A second binary (`cmd/pi-chat-selfdm`) or the same binary with a mode flag, its
own config, its own SQLite file, its own projects root, its own gateway
connections and warm pool. Code is shared through the existing `internal/`
packages (config, store, bot, slack API helpers, workspace); wiring is not.

- **Clean.** No shared mutable state, no locking, real failure isolation, and the
  self-DM's yearly credential failure cannot touch the app path.
- **Costs.** Two systemd units, two configs, two warm caps (each
  `max_warm_sessions`, so effectively 2× against one pi-gatewayd), two versions
  to keep in step, and no cross-surface state (which is mostly a non-feature:
  sessions are channel-scoped anyway).
- **Hard requirements.** `paths.db_path` **and** `paths.projects_root` must be
  distinct. Otherwise each process's `SweepWorkspaces` keeps only the dirs *its*
  rows name (`bot.go:715`) and deletes the other's projects, and `adoptable`
  (`command.go` `pickResumable`) offers the other surface's live sessions for
  adoption — two clients on one pi session.
- **Effort.** Roughly the same adapter work as option 1 minus the mux, plus a
  small amount of wiring duplication — unless `run()` is extracted into an
  `internal/daemon` package, which is worth ~150 lines and removes the duplication.

### 2b. Separate process, shared database

Tempting ("one view of the state"), and it does keep dedupe and `adoptable`
coherent. But four globals fight:

1. **`MarkThreadsCold` is `UPDATE threads SET state='cold' WHERE state='warm'`**
   (`internal/store/threads.go:195`). Starting process B marks every thread
   process A holds warm as cold. `/pi status` and the warm count then lie about
   A, and phase 2's eviction reads exactly that state.
2. **The warm cap is counted from all rows** (`bot.go:405-430`): shared, which is
   fine today, but eviction is per-process and two evictors will race for a cap
   neither owns.
3. **`adoptable` and session ownership** work only because thread rows are
   visible; that holds in 2b, so this one is fine here — it is 2a's problem.
4. **Maintenance runs twice**: two sweeps (same keep set, benign) and two idle
   loops that each only know their own in-memory threads (`Bot.threads`), so
   neither closes the other's idle sessions. Warm sessions outlive their
   conversations until the owning process notices.

Plus SQLite: WAL + `busy_timeout(5000)` makes two processes *work*, but the
package comment's "one writer connection" invariant is per process, and lock
contention becomes a real failure mode the unit tests never exercise.

### 2c. Shared database plus a `surface` (and owner) column

The honest version of 2b: schema v4 adds `surface` and an owner/lease, and the
four globals become scoped — `MarkThreadsCold(surface)`, cap count per surface or
per lease, sweep and resume filtered by surface, and a rule for who may own a
row. That is design work you cannot undo cheaply (the schema is forward-only:
a newer database is refused by an older binary, so upgrading the two binaries
out of order breaks the loser on restart).

- **Effort.** mux-and-adapter work of option 1 **plus** a migration, store API
  changes, lease/heartbeat semantics, and two-process integration tests.
- **Verdict.** Only if shared state across surfaces is a genuine requirement —
  and nothing in the design needs it, because threads are channel-scoped.

---

## 4. Comparison

| | 1 · one daemon, mux | 2a · separate, own state | 2b · shared DB | 2c · shared DB + surface |
|---|---|---|---|---|
| Failure isolation | none (per-loop recovery only) | full | partial (SQLite contention couples them) | partial |
| Shared code | all, naturally | packages only, wiring duplicated | packages + state | packages + state + new rules |
| New schema work | none | none | none, but breaks 4 invariants | v4 migration + lease |
| Cross-surface session view | automatic | none (fine) | automatic | automatic, filtered |
| Warm cap | one, correct | 2× (documented) | one number, two evictors (phase 2) | scoped |
| `SweepWorkspaces` safe | yes | only with separate projects roots | yes | yes |
| `adoptable` safe | yes | only with separate DB | yes | yes |
| Ops surface | one unit, one config | two units, two configs, version skew | two units, coupled versions | two units, coupled versions + migration risk |
| Instance rules | Socket Mode: one | independent | independent | independent |
| Effort over self-DM-only | +mux | +wiring, no mux | ⚠ +invariant work | ⚠ +schema/lease work |
| Test story | two fake platforms | two configs, no new concurrency | multi-process SQLite tests | multi-process + lease tests |

---

## 5. Recommendation

1. **Build option 1.** A `MultiPlatform` keyed by channel id, the self-DM as a
   second `bot.Platform`, a second ingress goroutine, per-surface supervision in
   `main.go`. The adapter seam is the intended place for this and the core keeps
   one store, one gateway pool, one truth about warm sessions.
2. **If failure isolation is a hard requirement** — self-DM on another machine,
   another OS user, or a credential whose yearly expiry you refuse to let near
   the app path — take **2a with its own database and its own projects root**.
   Extract the wiring into `internal/daemon` so the two mains are ten lines each.
3. **Do not take 2b.** Sharing the database without a `surface`/owner column
   silently breaks `MarkThreadsCold`, warm accounting, and idle close.
4. **Take 2c only with eyes open**: forward-only schema, lease semantics,
   two-process tests, and an upgrade order rule.
5. Neither option changes the conclusions of the feasibility doc: no
   notifications, no interactivity, no slash commands, and the same policy
   exposure. Structure is not the blocker; §7 of that doc is.

---

## 6. Code-level work list (option 1)

1. `internal/bot/types.go` — optional `Capabilities` interface (or per-surface
   flags) so the core can render a text fallback for buttons on a surface that
   cannot receive a press; reword the `Bot` "one instance per process" comment.
2. `internal/slack/api.go` — allow a per-instance base URL and an auth mode
   (Bearer *or* `d` cookie) so one `API` type serves both surfaces; add
   `conversations.history` with `oldest`/`cursor`.
3. `internal/slack/selfdm/` — session-pair auth, credential loading and expiry
   detection, poller (cursor, jitter, backoff, 429 handling), history →
   `bot.Message`/`bot.Command` mapping, self-post ledger, `Platform`
   implementation with patch-mode rendering and button→text degradation.
4. `internal/slack/mux.go` — `MultiPlatform` and the channel→surface route,
   including the capability delegation rule.
5. `internal/config` — `[slack.self_dm]` (`xoxc_file`, `xoxd_file`,
   `workspace_url`, `channel_id`, `poll_interval`) plus per-surface render mode
   and a `--check` summary line; deny-by-default stays as is.
6. `cmd/pi-chatd/main.go` — construct both surfaces, the mux, the extra ingress
   goroutine, and the supervisor policy (per-surface failure, loud logs, no
   global `stop()` for a recoverable surface error).
7. `internal/store` — no schema change. Only if polling needs a cursor distinct
   from the observation watermark: reuse `observed_ts` or add a self-DM field
   behind a v4 migration.
8. Docs: DESIGN §12 (the seam now has a mux), `pi-chat.toml.example`, README
   ("running both surfaces"), and an operator note that the self-DM surface is
   unofficial and degrades by design.

## 7. Open questions

- Does the poller need its own cursor, or is the existing per-thread
  `observed_ts` watermark enough once every poll is keyed by `ts`? (Probably
  enough for threads it owns; a channel-level cursor is still needed to notice
  *new* roots.)
- Do we want `/pi resume` in the self-DM to see app-surface sessions? With option
  1 the answer is naturally yes (shared store, channel-scoped listing); decide
  whether that is desirable or confusing.
- Should the self-DM surface appear in `--check` as "degraded" when the cookie is
  missing/expired, and should the daemon start at all in that case? (Recommended:
  start, log, retry, keep the app surface healthy.)
- Phase-2 approvals: text-only on the self-DM means the core needs a
  text-approval path before this surface is genuinely useful, independent of
  which option ships.

## 8. Credential alternative: the app's user token (`xoxp`), and many people on one app

A different way to build the same self-DM surface: keep the app — it is the only
source of an OAuth user token — but drive the self-DM with the *user* token the
app receives when a person authorizes it, instead of a bot token and the bot's
DM. The question this answers is whether one app can serve **many people, each
with their own self-DM, in one workspace or Enterprise org**.

**Status (2026-01): shipped** as `slack.self_dm.auth = "user_oauth"` plus
`cmd/pi-chat-oauth` (the broker) and `docs/slack-user-token-setup.md` (the team
walkthrough). The PKCE/local-callback variant described below is not
implemented; the broker uses the confidential-client flow.

### The token model says yes

- `oauth.v2.access` returns `authed_user.access_token` (`xoxp-…`) for the person
  who authorized. User tokens are "issued for the user who installed the app and
  for users who authenticate the app" (tokens doc), and each authorization is
  independent — the uninstall docs describe an app disappearing only "if no user
  tokens for the same app exist", which is only meaningful if several can.
- The granularity is one token per **installation**: app × user × workspace for a
  workspace install, and app × user × organization for an org-ready (Grid) app —
  "users only have to authenticate with an app once for all workspaces that have
  access to the app". Re-authorizing the same user in the same workspace
  accumulates scopes on that installation rather than minting a second token
  (with token rotation it becomes an expiring access token plus a refresh token,
  still per installation), and revocation is per installation too — `auth.revoke`
  kills exactly one. A bot token, by contrast, has no user dimension at all: one
  per app per workspace, shared by everyone.
- Slack has a user-scopes-only OAuth flow for exactly this shape: the `v2_user`
  authorization endpoint and `oauth.v2.user.access`, "used to initiate the OAuth
  flow using just user scopes (no bot scopes)", and pointed at MCP-style clients.
  That is the flow this mode should be built on.
- Which endpoint the owner's link points at decides what a click creates. The
  standard `oauth/v2/authorize` with bot scopes installs the app **into the
  workspace** (one shared bot token, admin-visible) *and* authorizes the clicker;
  `oauth/v2_user/authorize` creates only the clicker's personal grant. Slack's own
  summary of the second: "you only need user tokens, not bot tokens". For a
  self-DM deployment the link should be the user-only one, so one person's click
  never installs anything workspace-wide — the workspace still records the app
  against that user, which is what the uninstall rule ("if no user tokens for the
  same app exist, the app will appear to be uninstalled") is describing.
- A user token acts as that user. `im:history` (read) and `chat:write` (post) as
  *user* scopes are what make a self-DM reachable at all: the bot is not a member
  of it, so no bot token can read it. `chat:write` on a user token is current and
  supported, not the retired `chat:write:user` of the workspace-app era.
- N authorizations therefore give N isolated tokens, N self-DMs, N daemons that
  share nothing but the app registration. Same workspace or same org makes no
  difference for the user tokens themselves; Grid adds org-level installation and
  admin approval, and can present `W…` enterprise user ids where the allowlist
  expects `U…`.

### The ingress is what breaks

Socket Mode belongs to the *app*, not to an authorization: "When multiple
connections are active, each payload may be sent to *any* of the connections"
(Socket Mode doc). App-level tokens do not partition it — even several `xapp-`
tokens of one app still receive that app's payloads spread across connections.
So N daemons sharing the app's Socket Mode would see user A's self-DM message
arrive at user B's daemon, and possibly never at A's. This is the same fact that
makes pi-chatd single-instance today (DESIGN §12).

The HTTP Events API is smarter about *visibility* — "You will only receive
events that users who have authorized your app can 'see'", and when several
users can see one event Slack sends it once with an
`authorizations.user_id`/`apps.event.authorizations.list` pointer — but receiving
it at all means one public request URL, i.e. a central service holding everyone's
tokens. That is a different product from "each person runs their own
gateway".

So the multi-user shape has exactly one clean form: **ingress by polling each
user's own token** — which is already how `internal/slack/selfdm` works. No
app-level token, no shared socket, no fan-out.

### What it would take here

The surface is credential-agnostic already, so the change is small:

- `slack.API` needs nothing: it already supports no cookie and the default
  `https://slack.com/api/` base, which is where a user token lives.
- Config: an auth mode for `[slack.self_dm]` (`auth = "session" | "user_oauth"`,
  or infer from the `xoxp-` prefix), making `xoxd_file` and `workspace_url`
  optional in that mode.
- A one-time token acquisition: `pi-chatd --slack-oauth` (or a hosted callback) to
  run the authorization flow and write the token file. With PKCE (below) the
  acquire step is a loopback listener inside the daemon and needs no client
  secret and nothing pasted; without it, a broker owns the callback. Scopes:
  `im:history`, `im:read`/`im:write`, `chat:write`, `users:read`; the manifest
  grows an `oauth_config.scopes.user` section.
- Everything else — poller, ledger, numbered replies, mux, store — is unchanged.

### Costs that do not go away

- **OAuth plumbing — smaller than it first looks.** Only the app's redirect URI
  ever sees a user token, so the flow needs *a* callback wherever it runs. With
  Slack's PKCE support, now generally available as an app setting, that callback
  can be the daemon's own loopback listener: a `localhost` redirect is treated as
  a *desktop* redirect, the exchange sends `code_verifier` instead of a
  `client_secret` ("the client should call the `oauth.v2.access` API method, but
  should not include `client_secret`"), and nothing is hosted or pasted. The
  costs move rather than vanish: PKCE marks the app as a public client and
  "cannot be disabled without contacting Slack support", desktop redirects "are
  not allowed to request bot scopes" — so a PKCE app cannot also be the agent
  app with its bot user — and the redirect URL still has to be registered in
  advance. A broker flow (owner-hosted HTTPS callback, token shown to the user
  once) remains the alternative for an app that must keep its secret; PKCE and a
  broker pull in opposite directions, because the verifier belongs with whoever
  will use the token.
- **Scopes and approval.** Reading DMs and posting as the user in one app is
  exactly what enterprise app-approval flows question; Grid installs need admin
  approval, and a user-scope-only app is the newest part of the platform
  (`v2_user` authorization / `oauth.v2.user.access`).
- **The UX is identical to the `xoxc` self-DM**: a message authored by you is
  never unread and never notifies. Since this alternative *requires* the app, the
  bot DM remains the better surface for anything you want to be told about — and
  if the app has no bot user at all, content, which is the only surface it can
  offer, is also the weakest one.

### Comparison

| | `xoxc` self-DM (built) | `xoxp` self-DM (this section) | bot DM (app surface, built) |
|---|---|---|---|
| One app, many people | no — each person has their own session | **yes** — one token per authorization | yes for DMs, but one daemon per app (Socket Mode) |
| Token kind | unofficial browser session | sanctioned OAuth user token | sanctioned bot token |
| Ingress | poll | poll (or a broker + Events API) | Socket Mode push |
| Notifications | no | no | **yes** |
| Slash commands, buttons, status | no | no | yes |
| Setup per person | paste `xoxc` + cookie | authorize once, receive a token | install the app |
| Account-wide secret on disk | yes (session cookie) | no (scoped, revocable) | no |

### Verdict

- **Many people, one app, each their own self-DM: yes** — token-wise it is exactly
  what Slack's per-authorization user tokens are for, and it needs no new protocol
  work in this repository beyond credential acquisition and configuration.
- **"But"**: it can never use Socket Mode for ingress, so it can never carry the
  app's other surfaces (mentions, slash commands, buttons) for N people at once;
  and someone must host the OAuth callback and own the app's secret.
- If the goal is one shared agent *service* for an org, that is the broker shape
  above, with the privacy review that follows from holding everyone's tokens.
- If the goal is one person's control channel, the app's bot DM still does more,
  and the `xoxp` self-DM is a cleaner-credentialed version of what the `xoxc`
  self-DM already does — worth building as a *credential mode* of that surface,
  not as a separate surface.

**Unverified, and cheap to test with one live user:** that
`conversations.history` with `xoxp` + `im:history` returns the *self*-DM (expected
but untested), how to discover its `D…` id with a user token
(`conversations.list`/`conversations.open` versus `search.messages` with
`from:me to:me`), and whether Socket Mode delivers `message.im` for the
authorizing user's self-DM at all.
