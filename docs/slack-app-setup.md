# Creating the Slack app for pi-chat

A step-by-step tutorial for wiring a Slack app to `pi-chatd`. Budget about fifteen
minutes.

pi-chat uses **Socket Mode**, so the daemon opens an outbound WebSocket to Slack and
everything — mentions, buttons, modals, slash commands — arrives over that socket.
Consequences worth knowing before you start:

- No public URL, no tunnel, no TLS certificate, and **no signing secret**: Slack never
  makes an HTTP request to you, so there is nothing to verify signatures for.
- You need **two** tokens, not one: an *app-level* token that is only good for opening
  the socket, and a *bot* token that is used for everything you send back.
- A Socket Mode app **cannot be listed in the Slack Marketplace**. This is a personal
  app by design.

The exact sidebar labels in Slack's app settings move around over time. Every step below
names the page title, which does not change, and the settings pages have a search box.

---

## Step 0 — What you will end up with

| Item | Shape | Where it goes |
|---|---|---|
| App-level token | `xapp-…` | `~/.config/pi-chat/slack-app-token` |
| Bot token | `xoxb-…` | `~/.config/pi-chat/slack-bot-token` |
| Your member ID | `U…` | `allowed_users` in `~/.config/pi-chat/config.toml` |

Those gateway token files (`gateway-admin.token`, `gateway-thread.token`) are a
different system: they authenticate to `pi-gatewayd`, not to Slack. If they are missing,
do [PLAN.md](../PLAN.md) M0 first.

## Step 1 — Create the app

1. Open <https://api.slack.com/apps> and click **Create New App**.
2. Choose **From scratch**.
3. **App Name:** `pi`. This becomes the mention handle — people will type `@pi`, and
   Slack will render it as the app's display name, which you can change later.
4. Pick the workspace you want the bot in and click **Create App**.

You land on **Basic Information**. Keep this page open; you will come back to it.

> The name matters more than it looks. `/pi` for root commands and `@pi` for thread
> commands are both the display name, not a fixed string.

## Step 2 — Turn on Socket Mode and mint the app-level token

1. Go to **Socket Mode** (under *Features*; search for it if you cannot see it).
2. Toggle **Enable Socket Mode** on. Slack offers to walk you through creating a token —
   accept, and if it does not, use the manual path below.
3. Name the token `pi-chat-socket`, click **Add Scope**, choose **`connections:write`**,
   then **Generate**.
4. Copy the token — it starts with `xapp-` — and keep it somewhere temporary. Slack
   shows the value once.

Manual path, or to mint another one later: **Basic Information → App-Level Tokens →
Generate Token and Scopes**.

`connections:write` does exactly one thing: it lets the app call `apps.connections.open`
to obtain the WebSocket URL. It grants no access to messages, and it is why the socket
token is worth keeping separate from the bot token.

## Step 3 — Add the bot token scopes

1. Go to **OAuth & Permissions**.
2. Scroll to **Scopes → Bot Token Scopes** and **Add an OAuth Scope** for each of:

| Scope | Why pi-chat needs it |
|---|---|
| `app_mentions:read` | receive `app_mention` — someone typing `@pi …`, including in a thread |
| `chat:write` | post and update messages, and stream a turn with `chat.startStream` |

That is the whole set for phase 0. Later phases add more (see
[Adding the phase-1 permissions](#adding-the-phase-1-permissions)); every addition
requires reinstalling the app (Step 5).

## Step 4 — Subscribe to the mention event

1. Go to **Event Subscriptions** and toggle **Enable Events** on.
2. Do **not** fill in a Request URL. Because Socket Mode is enabled, Slack says so
   itself: event payloads are delivered over the socket instead.
3. Under **Subscribe to bot events**, add **`app_mention`**.
4. Click **Save Changes**.

`app_mention` fires only when the bot is mentioned *and* the bot is a member of the
conversation. Nothing about plain messages or direct messages is delivered yet — that
arrives with the history scopes in phase 1, which is also why the phase-0 slice can only
be driven with `@pi …` in a channel thread.

## Step 5 — Install the app to the workspace

1. Go to **Install App** (under *Settings*).
2. Click **Install to Workspace**, review the permissions, and click **Allow**.
3. Copy the **Bot User OAuth Token**, which starts with `xoxb-`.

From now on, **any change to scopes or bot events disables the app until you reinstall** —
the install page will show a "reinstall" banner. This is the single most common cause of
`missing_scope` errors.

## Step 6 — Store both tokens as files

pi-chatd reads token *values from files*, so neither token ends up in the config file,
your shell history, or a process listing. Use `printf`, not `echo`: a trailing newline is
harmless to pi-chatd, but a stray one confuses `curl` and most other tools you may use to
test the token later.

```bash
install -d -m 700 ~/.config/pi-chat
printf '%s' 'xapp-your-app-level-token' > ~/.config/pi-chat/slack-app-token
printf '%s' 'xoxb-your-bot-token'       > ~/.config/pi-chat/slack-bot-token
chmod 600 ~/.config/pi-chat/slack-app-token ~/.config/pi-chat/slack-bot-token
```

Sanity check without printing the secrets:

```bash
wc -c ~/.config/pi-chat/slack-*-token     # ~80 bytes for xapp-, more for xoxb-
head -c 5 ~/.config/pi-chat/slack-app-token; echo   # prints "xapp-"
head -c 5 ~/.config/pi-chat/slack-bot-token; echo   # prints "xoxb-"
```

## Step 7 — Invite the bot to a channel

In Slack, open the channel you want to use (start with a private test channel) and:

```text
/invite @pi
```

A bot that is not in the conversation receives no `app_mention` events from it, and
cannot post there either — `chat:write` grants the ability to speak, not the right to
join.

## Step 8 — Allow yourself to use it

pi-chat denies every request until your Slack user is on the allowlist, because an agent
with shell access is exactly the thing worth gating.

1. In Slack, click your avatar → **Profile** → the **⋯** menu → **Copy member ID**.
   It starts with `U`.
2. Put it in `~/.config/pi-chat/config.toml`:

```toml
[access]
allowed_users = ["U0123456789"]
```

## Step 9 — Verify

```bash
make check
```

Every line should be what you expect. The relevant ones:

```text
config /home/pi/.config/pi-chat/config.toml is valid
  slack app token: /home/pi/.config/pi-chat/slack-app-token (present)
  slack bot token: /home/pi/.config/pi-chat/slack-bot-token (present)
  gateway admin token: /home/pi/.config/pi-chat/gateway-admin.token (present)
  gateway thread token: /home/pi/.config/pi-chat/gateway-thread.token (present)
  allowed users: U0123456789
```

Then start the daemon and mention it in the channel:

```text
@pi hello
```

pi-chat opens a thread, starts a session through `pi-gatewayd`, and streams the answer
into that thread. That is the phase-0 milestone ([PLAN.md](../PLAN.md) M3).

---

## Adding the phase-1 permissions

Add these when the corresponding feature lands, then reinstall:

| Scope | Enables |
|---|---|
| `channels:history`, `groups:history`, `im:history`, `mpim:history` | plain messages in threads the bot is in, and direct messages (DMs need `im:history`, and the app must be allowed to be messaged) |
| `assistant:write` | `assistant.threads.setStatus` and `setTitle` — the live "working on it" status and a derived thread title |
| `files:read` | downloading an image someone attaches, to hand to the agent |
| `reactions:write` | a 👀 reaction as an instant receipt while the agent works |

Two more features need Slack configuration, not scopes:

- **Interactivity & Shortcuts** — toggle it on for buttons and modals. Under Socket Mode
  no Request URL is needed; interactive payloads arrive on the socket.
- **Slash Commands** — create the `/pi` command. The creation form asks for a Request
  URL and will not always accept an empty one; under Socket Mode the URL is never
  called, so any `https://` placeholder is fine once Socket Mode is enabled. This is the
  command behind root control (`/pi status`), and note that Slack forbids developer
  slash commands inside message threads — that is why thread control is written `@pi
  /status`.

If you also want Slack's agent experience (the split-view assistant container), enable
**Agents & AI Apps** under *Features*. Be aware that container is thread-only, so a
developer slash command cannot be used in it at all: everything there is `@pi …`.

---

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `invalid_auth` | Wrong token in the wrong file, or a stray newline, quote or space. `printf '%s'` avoids it; check with `head -c 5` |
| `missing_scope` | A scope was added but the app was never reinstalled. Install App → Reinstall |
| `not_in_channel` | Invite the bot: `/invite @pi` |
| Nothing happens on `@pi hello` | The app is not a member of that conversation, `app_mention` is not subscribed, Socket Mode is off, or `allowed_users` does not contain you. Slack's app page shows each toggle; pi-chatd logs the refusal |
| Events arrive twice or not at all | Two instances of `pi-chatd` are running. Socket Mode splits events across connections, so the daemon is deliberately single-instance |
| The socket drops overnight | Socket Mode is a long-lived connection and is sensitive to network churn; the app reconnects, and the unit restarts it on failure |
| Cannot install the app | The workspace restricts app installation — an admin must approve it, or you need a workspace where you can install apps |
| Want to be listed in the Marketplace | Not possible with Socket Mode; that requires a public HTTP endpoint, which this project deliberately avoids |

## Rotating or revoking credentials

- **App-level token:** Basic Information → App-Level Tokens → revoke and generate a new
  one; write the new value to the same file and restart `pi-chatd`.
- **Bot token:** OAuth & Permissions → revoke the installation (or reinstall), then
  update `slack-bot-token`.
- Deleting the app entirely invalidates both tokens.

Never commit either file. The repository's `.gitignore` blocks `*.token`, `*-token` and
`config.toml` so an accidental `git add -A` cannot publish a token.

## Checklist

- [ ] App created from scratch, named `pi`
- [ ] Socket Mode enabled
- [ ] App-level token created with `connections:write` → `slack-app-token`
- [ ] Bot scopes added: `app_mentions:read`, `chat:write`
- [ ] `app_mention` subscribed (no Request URL)
- [ ] App installed to the workspace, bot token → `slack-bot-token`
- [ ] `chmod 600` on both token files
- [ ] Bot invited to a channel with `/invite @pi`
- [ ] Your member ID in `allowed_users`
- [ ] `make check` reports both Slack tokens present and you as an allowed user
