# Self-DM as a pi control surface, with no Slack app

**Question.** Can pi-chat drive pi sessions from a Slack *self-DM* (the personal
"Notes to self" conversation) without creating or installing a Slack app for the
agent at all?

**Verdict.** Technically yes, for a text-only control plane, using your own
browser-session credentials (`xoxc` + the `d` cookie) and polling the self-DM.
It is *not* a drop-in replacement for the app adapter, it gives up the features
that make the current UX good (notifications, slash commands, buttons, status),
and it walks into a detection surface Slack documents by name. Recommendation in
§9: optional personal-use mode, never on an Enterprise Grid workspace.

Investigated 2026-01 · pi-chat @ `5d8bc66`. No live Slack credentials were used;
every claim below is either documented in the evidence list (§11) or explicitly
marked **unverified**.

**See also:** [`self-dm-integration-options.md`](self-dm-integration-options.md) —
where this surface should live in the code (one daemon with a mux, or a separate
service).

**Status (2026-01):** built, as an optional surface that is off by default —
`slack.self_dm.enabled`, with the credentials and the walkthrough in
[`slack-self-dm-setup.md`](slack-self-dm-setup.md) and the shape it shipped in at
the top of [`self-dm-integration-options.md`](self-dm-integration-options.md).
The judgement below is unchanged: this is a documented option for a personal
workspace, not a recommended one.

---

## 1. The two different things "no app" can mean

1. **No app at all.** Nothing registered with Slack; the daemon acts *as the
   human*, in the human's own self-DM. This is the question asked, and the
   subject of §3–§7.
2. **No app *per agent*.** One app, many pi sessions. The current design already
   satisfies this: `slack/manifest.yaml` is installed once and every thread,
   DM and project reuses it. If (2) is the real requirement, there is nothing to
   build.

There is also a middle path that needs no *new* app: add user-token scopes to the
already-installed pi-chat app and use its `xoxp` token against the self-DM. §8B.

---

## 2. Why "no app" is a real constraint

Socket Mode, the Events API, slash commands, modals and block interactivity all
require an app-level token (`xapp-`, `connections:write`) and an installed app.
Slack offers no supported API path that is not "an Application" ([API
ToS](https://slack.com/terms-of-service/api): the licence is granted "only as
necessary to develop, test, use and support an application"). So "no app at all"
leaves exactly one route: the browser/desktop session credentials the Slack
client itself uses.

---

## 3. How the no-app path works

### 3.1 Credentials

- **`xoxc-…`** — the workspace session token the Slack web client puts in the
  `token` form field; acts as *you*, with all your access.
- **`xoxd-…`** — the `d` cookie; must accompany `xoxc` on every request (some
  workspaces also want the sibling `d-s`). It is URL-encoded; a literal `+` in
  the value is a classic `invalid_auth` (slackdump #65).
- Both are workspace-scoped: requests go to `https://<workspace>.slack.com/api/`
  (Enterprise Grid: `https://<corp>.enterprise.slack.com/`), not `slack.com/api`.
- Historically the `d` cookie lasted ~10 years; since Dec 2025 Slack shortened
  it to roughly a year ([papermtn, updated Dec
  2025](https://www.papermtn.co.uk/retrieving-and-using-slack-cookies-for-authentication/)).
  It dies on password change, "sign out everywhere", or admin action, and
  re-capture is a manual browser step.
- Capture options: browser DevTools (Network → any `/api/` call), the Slack
  desktop app's local store ([macOS LevelDB + Keychain
  extractor](https://gist.github.com/turlockmike/da757caa7cacabc4816f393628d047dd)),
  [slackdump's EZ-Login
  3000](https://github.com/rusq/slackdump/wiki/EZ-Login-3000), or pasting both
  values by hand. **Note for this machine:** no Slack desktop and no browser
  profile is present here, so capture would be paste-from-another-machine into a
  0600 file.

### 3.2 Ingress: there is no supported push without an app

| Option | Reality |
|---|---|
| Socket Mode | Needs `xapp-` from an app. Impossible. |
| Events API via public URL | Needs an app. Impossible. |
| `rtm.connect` (RTM) | Closed to new apps; docs say "new Slack apps may not use **any** RTM method". For a *client* token it sometimes works, but Enterprise Grid answers `enterprise_is_restricted`. **Unverified for our workspace.** |
| Internal client websocket | The real client opens `wss://wss-primary.slack.com/…` after calling internal `client.getWebSocketURL` (a real implementation uses exactly this because `rtm.connect` fails on Grid). Works, but is undocumented protocol. |
| **Poll `conversations.history`** | Simplest, no internals: `channel=<self-DM id>`, `oldest=<last cursor ts>`. Tier 3 (50+/min) for user/internal tokens; the May 2025 "1 request per minute" cut applies to newly-created *commercially distributed apps*, not to a user session (**verify by measurement**). |

Polling is the recommended baseline: 3–5 s pickup, no reverse-engineered
protocol. The internal websocket is a later optimisation.

One consequence of self-DM semantics: **the conversation can never be unread.**
Unread state is per user, and Slack will not mark your own messages unread, so
the `client.counts` shortcut is useless here; a poller must always poll. (Same
fact kills notifications — see §4.)

### 3.3 Egress

`chat.postMessage`, `chat.update`, `conversations.replies`, `users.info` and
`files.*` all work with the session pair, acting as you. Editing own messages is
allowed ("Posted as the user — messages are editable in Slack"). Practical
limits: `chat.postMessage` is a special tier — **1 message/second/channel** plus
a workspace-wide cap; plan edit-based streaming at roughly 1 update/s.

### 3.4 Addressing the self-DM

The self-DM is a real `D…` conversation id. With a user token,
`chat.postMessage` also accepts your own `U…` id and resolves it to the self-DM
(with a *bot* token the same call lands in the bot's DM with you instead); the
`D…` id is what the read methods need.
Discovery: open the DM in Slack and read the id from the URL
(`app.slack.com/client/T…/D…`) — the only always-reliable route; or
`conversations.open`/`conversations.list` with the session token, `search.messages`
with `from:me to:me`, or internal `client.userBoot`/`client.counts`. Expect to
make this a config value pasted once.

---

## 4. Capability matrix

| Capability | Current app adapter | Self-DM, no app |
|---|---|---|
| Start / continue a session | mention, plain text in DM thread | plain text at root / in thread — same shape, see §5 |
| Streamed answers | `chat.update` | `chat.update` ✅ |
| `/pi …` slash commands | ✅ | ❌ no app to register them; text commands only |
| Buttons, modals, `/pi resume` picker | ✅ | ❌ a message with no owning app has no `response_url` |
| Interactive approvals | ✅ (phase 2) | ❌ must become a text protocol (`approve <id>`) |
| Status / "working" spinner (`agents.sessions.setStatus`) | ✅ | ❌ app-only method |
| Suggested prompts | ✅ | ❌ app-only |
| Files / screenshots | ✅ | likely ✅, unverified (download needs the cookie; upload via async flow) |
| **Notifications to the human** | ✅ real DM, unread badge | ❌ **messages you post to yourself are authored by you: never unread, never notified** |
| Ask-user flow (agent needs input) | status + buttons | pull-only; must be noticed by eye |
| Access control | user + channel allowlist | trivially "author is me", but see §5 |
| Realtime | Socket Mode push | poll (3–5 s) or internal websocket |
| Durability across restart | socket + SQLite cursors | polling cursor in SQLite — same |

The notification row is the fatal one for "manage": a control plane that cannot
tell you it needs you, or that the agent finished, is a scratchpad. Slack itself
is explicit on the sibling case — one MCP server ships a separate bot-DM tool
because "a user-token self-DM … Slack marks already-read"
([mindstone/mcp-servers](https://github.com/mindstone/mcp-servers/commit/c3a1da02668050754f250efcce01149b7e33ab31)),
and users report the same for scheduled self-DMs
([r/Slack](https://www.reddit.com/r/Slack/comments/1ihfl3o/help_how_to_get_notifications_for_scheduled/)).
Workarounds worth testing: a `<@yourself>` mention in the self-DM (does a
self-mention notify? **unverified**), or `reminders.add` at "now" as a Slackbot
doorbell (**unverified, and an API used off-label**). Neither is a real
notification channel.

---

## 5. The impersonation problem, and how the existing core absorbs it

With `xoxc` the daemon *is* you, so human commands and agent output have the same
author. A returned message can no longer be classified by `user`. The workable
rule:

1. Persist the `ts` of every message the daemon posts (the store already holds
   dedupe/cursor state). `ts` in the ledger ⇒ agent output, drop.
2. Otherwise, `thread_ts` present and the thread owns a session ⇒ continuation
   turn.
3. Otherwise a root message ⇒ new session.

This is almost exactly the grammar `internal/bot` already implements for DMs: a
`bot.Message` with `Direct: true` means "the conversation is the address", and
the core already distinguishes "started with a mention" from "plain text in the
DM thread continues it" (`internal/bot/types.go`). The adapter can synthesise
`Mentioned: true` for a self-DM root (or on `<@me …>`) and the core needs no new
concepts. Likewise `/pi status` typed at a self-DM root can be handed to the core
as a `bot.Command` exactly as `@pi /status` is today. The impersonation cost is
constrained to the adapter.

Failure modes to design for: a wiped database replays the conversation and reads
agent output as commands (mitigate with a first-run `oldest=now` plus a content
marker); edits arrive as `message_changed` and must be ignored; and any
out-of-band post made *as you* (e.g. a script, another tool) is indistinguishable
from agent output.

---

## 6. What it would take in this repo

The adapter seam was drawn for exactly this kind of swap ("Core is
platform-independent; the Slack adapter owns platform shapes", DESIGN.md §12).
A `self_dm` ingress mode would add, roughly:

| Piece | File(s) | Est. |
|---|---|---|
| Session-pair auth, cookie-bearing HTTP client, workspace base URL | `internal/slack/api.go` (currently fixed Bearer + `slack.com/api`) | ~200 lines |
| Poller transport with jitter/backoff, cursor persistence, 429 handling | new `internal/slack/selfdm` (replaces `socket.go` role) | ~250 |
| History message → `bot.Message`/`bot.Command` mapping + self-post ledger + capability flags through the seam | new mapper, `internal/bot/types.go` (flags), `internal/slack/router.go` | ~250 |
| Config: `mode`, creds paths, self-DM channel, poll interval | `internal/config`, `pi-chat.toml.example`, DESIGN §11 | ~100 |
| Text-based approvals / command vocabulary, capability-gated rendering | `internal/bot/command.go`, `turn.go`, `internal/slack/blocks.go` | ~200 |
| Tests + docs | `internal/slack/*_test.go`, README/DESIGN | ~400 |

**~1.5–2.5k lines including tests; a focused 2–4 days for a text-only mode.**
The hidden cost is not the transport — it is teaching the core that a renderer
may have no buttons, no status and no notification, and deciding what the UX is
when the agent must ask a question. The current `HandleAction` path and the
phase-2 approval design assume clicks; a text protocol is a second design, not a
flag.

---

## 7. Risks

**Policy.** The API ToS grants API access "only as necessary to develop, test,
use and support an application"; the Developer Policy prohibits "attempting to
reverse engineer … the Slack API", and "circumventing Slack's intended
limitations (including pricing, features and access structures)" — and states
violations may result in "token revocation, developer suspension, **User
notification**, legal action". A session token used where an app install would be
required is at best outside the licence, at worst the second clause.

**Detection is a documented product feature, not a theory.** Slack's audit-log
anomaly reference names the exact signals this pattern emits: `unexpected_client`
("an anomalous Slack client is detected"), `session_fingerprint`,
`spoofed_user_agent` ("characteristics of the client do not match the user
agent"), `unexpected_scraping` ("high fidelity"), and `api_call_volume`. Slack
Engineering describes correlating them into a scraping verdict, and AlphaSOC
ships a matching detection ("Unexpected Slack API calls indicating scraping
activity"). On Enterprise Grid an admin sees these; the `xoxc` route is the
loudest possible way to ask a locked-down workspace for agent access.

**Enforcement is observed.** Slack has invalidated `xoxc`/`xoxd` pairs on
instrumented access (bulk `users.list`), with the maintainers concluding `xoxp`
is the safe token type
([slack-mcp-server #86](https://github.com/korotovsky/slack-mcp-server/issues/86));
Enterprise users report "instant sign-out" after first use
([#62](https://github.com/korotovsky/slack-mcp-server/issues/62)) and corporate
security teams flagging the tooling. `users.list` is exactly the kind of call to
avoid; `users.info` lazily is the documented workaround.

**TLS fingerprinting.** Slack's Cloudflare-fronted edge inspects the TLS
ClientHello and can answer `invalid_auth` *before reading the token* when the
fingerprint is not a browser's. Go's `net/http` has a non-browser fingerprint;
the Go-based MCP server ships `SLACK_MCP_CUSTOM_TLS=1` (uTLS Chrome ClientHello)
for Enterprise workspaces, and a Rust client hard-depends on a
fingerprint-emulating HTTP stack for Slack only. So a Go implementation may need
uTLS — i.e. deliberate detection evasion, which makes the policy exposure worse
rather than better. **Unverified whether plain Go passes in our workspace.**

**Blast radius.** The `d` cookie is a full-account credential: every channel and
DM you can see, not a scoped bot token. It must live at 0600 next to the daemon
that already runs shell-capable agents; a leak is an account compromise, not a
bot compromise. Rotation is manual and annual-ish.

**Brittleness.** Undocumented endpoints, silent internal API drift, cookie
encoding traps, Enterprise-only TLS workarounds, rate-limit behaviour that is
app-specific and may change again (May 2025's history cut), and no support path
when it breaks.

**Confidentiality.** In a company workspace, treat the self-DM as company data:
Slack's discovery/export tooling covers DMs under legal process, and pi sessions
placed there may be in scope. **Verify with the admin before using a work
workspace.**

---

## 8. Alternatives

- **A. Keep the app (status quo).** One app, many sessions; no per-agent apps.
  Interactive, notifying, supported. Cost: app creation and install approval.
- **B. No *new* app: existing app + user-token scopes (`xoxp`).** Add
  `im:history`/`im:write`/`chat:write` user scopes to the already-installed
  pi-chat app and use the user token for the self-DM. Sanctioned token type,
  scoped permissions, normal audit trail, no cookie theft, and — under the
  user-events model — **Socket Mode might deliver `message.im` for the self-DM in
  real time (unverified)**. Interactivity from a user-token post is plausible but
  **unverified**; notifications are still absent (same authorship problem). If
  the motivation is "less app bureaucracy" rather than "zero apps", this is the
  best risk-adjusted answer, and it is a scope change, not a new program.
- **C. A different chat surface.** Telegram (bot created by talking to
  `@BotFather`, no review, no admin), Matrix, Google Chat (already planned in
  DESIGN §1). If the real goal is "a phone-friendly pi control channel without
  Slack app paperwork", these beat self-DM on every axis except being Slack.
- **D. Local control surface.** pi-gateway already speaks a loopback protocol;
  a tiny authenticated web/PWA view plus desktop notifications gives push-class
  UX with zero third-party policy risk.
- **Ruled out:** Slack Workflow Builder (cannot call out without an app step),
  incoming webhooks (app), Slack AI/agent surfaces (app), reading the desktop's
  local cache (read-only, no send).

---

## 9. Recommendation

1. **Do not build this as a replacement** for the app adapter. The losses in §4
   are structural, not cosmetic.
2. **If it is built, build it as an optional `self_dm` ingress mode**, off by
   default, with polling, a self-post ledger, text-only commands and approvals,
   capability flags through the seam so the core never renders a button it cannot
   receive, conservative rate limits, no uTLS evasion (fail loudly on
   `invalid_auth` instead), and a startup notice naming it unofficial.
3. **Scope the decision by workspace.** Personal/free workspace you own: fine,
   accept session-revocation risk. Company workspace, especially Enterprise
   Grid: no — the audit-log signals are exactly the ones Slack watches for, and
   the discovery/export caveat applies.
4. **Prefer Option B** if something must ship: it removes the "new app" and
   session-cookie problems while keeping a supported token, and it is a small
   change to the existing manifest and adapter.
5. If the underlying goal is simply "a chat channel for pi without Slack
   paperwork", evaluate Telegram or Google Chat before spending days on
   undocumented Slack internals.

---

## 10. Experiments to run before committing

Cheap, ordered, each decides a section above:

1. `auth.test` with the cookie pair from **plain Go `net/http`** → does this
   workspace accept a non-browser TLS fingerprint? (§7)
2. `chat.postMessage` to the self-DM (`U…` and `D…`) → confirm it lands and is
   visible in the client.
3. Poll `conversations.history` on the self-DM for 10 minutes → confirm human
   messages appear, measure 429s/`Retry-After` at 2–5 s intervals.
4. Post `<@yourself> hello` and a `reminders.add` at "now" → do either notify?
   (§4)
5. Confirm the daemon's own posts are visible in the poll and that a ts ledger
   cleanly separates them from human input (§5).
6. If Option B is on the table: does a user-token post support interactive
   buttons/`response_url`, and does Socket Mode deliver `message.im` for the
   self-DM? (§8B)
7. In a work workspace: confirm with the admin that self-DM content is in scope
   for discovery/export, and whether `unexpected_client` anomalies are alerted
   on. (§7)

## 11. Evidence

- Slack token types (xoxb/xoxp/xoxc semantics): <https://docs.slack.dev/authentication/tokens>
- Session cookie → `xoxc`, TTL change Dec 2025: <https://www.papermtn.co.uk/retrieving-and-using-slack-cookies-for-authentication/>
- Developer Policy (reverse engineering, circumvention, enforcement): <https://api.slack.com/developer-policy>
- API ToS (licence "as necessary to … support an application"): <https://slack.com/terms-of-service/api>
- Audit-log anomaly names (`unexpected_client`, `spoofed_user_agent`, `unexpected_scraping`): <https://docs.slack.dev/reference/audit-logs-api/anomalous-events-reference>
- Slack Engineering on correlating anomalies: <https://slack.engineering/slack-audit-logs-and-anomalies/>
- Third-party detection rule: <https://docs.alphasoc.com/detections_and_findings/alphasoc_detections/slack_scraping_anomaly.md>
- Token invalidation on instrumented access: <https://github.com/korotovsky/slack-mcp-server/issues/86>
- Enterprise instant sign-out reports: <https://github.com/korotovsky/slack-mcp-server/issues/62>
- uTLS `SLACK_MCP_CUSTOM_TLS` in a Go client: <https://github.com/korotovsky/slack-mcp-server> (README)
- Browser-emulating HTTP client rationale: <https://github.com/dohooo/helmor/blob/main/src-tauri/src/slack/api.rs>
- Internal websocket / `client.getWebSocketURL` on Enterprise Grid: <https://gist.github.com/turlockmike/da757caa7cacabc4816f393628d047dd>
- Web client internals (`client.userBoot`, `client.counts`, `wss-primary`): <https://github.com/ethanyc216/personal-slack-agent/blob/main/docs/slack-client-findings.md>
- Rate-limit tiers and the May 2025 history cut: <https://docs.slack.dev/apis/web-api/rate-limits>, <https://docs.slack.dev/reference/methods/conversations.history.md>
- RTM status for new apps: <https://docs.slack.dev/reference/methods/rtm.connect>
- Cookie URL-encoding trap: <https://github.com/rusq/slackdump/issues?q=invalid_auth>
- Self-DM is always read / no notification: <https://github.com/mindstone/mcp-servers/commit/c3a1da02668050754f250efcce01149b7e33ab31>, <https://www.reddit.com/r/Slack/comments/1ihfl3o/help_how_to_get_notifications_for_scheduled/>
- Posting to the personal DM by user id: <https://stackoverflow.com/questions/56701667/slack-api-sending-a-direct-message-to-personal-space-you>
- bolt-on "no app" tooling for reference: <https://github.com/rusq/slackdump> (Go, `xoxc`/`xoxd`, read-only by design)
