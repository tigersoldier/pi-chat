# Creating the Slack app for pi-chat

A step-by-step tutorial for wiring a Slack app to `pi-chatd`. Budget about fifteen
minutes.

The setup installs **everything at once**, including the permissions the later phases
need. Slack invalidates an app whenever its scopes change and makes you reinstall it, so
adding scopes one phase at a time would mean four or five reinstall round-trips for no
benefit. If you want the absolute minimum instead, see
[Installing only the phase-0 minimum](#installing-only-the-phase-0-minimum).

pi-chat uses **Socket Mode**, so the daemon opens an outbound WebSocket to Slack and
everything — mentions, messages, buttons, modals, slash commands — arrives over that
socket. Consequences worth knowing before you start:

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
| Your member ID | `U…` | `[slack.access] allowed_users` in `~/.config/pi-chat/config.toml` |

Those gateway token files (`gateway-admin.token`, `gateway-thread.token`) are a
different system: they authenticate to `pi-gatewayd`, not to Slack. If they are missing,
do [PLAN.md](../PLAN.md) M0 first.

## Step 1 — Create the app

1. Open <https://api.slack.com/apps> and click **Create New App**.
2. Choose **From scratch**.
3. **App Name:** `pi`. This becomes the mention handle — people will type `@pi`, and
   Slack renders it as the app's display name, which you can change later.
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
to obtain the WebSocket URL. It grants no access to messages, which is why the socket
token is worth keeping separate from the bot token.

## Step 3 — Add the bot token scopes

Go to **OAuth & Permissions**, scroll to **Scopes → Bot Token Scopes**, and add all nine.
The third column says which part of pi-chat breaks without it, so you can judge for
yourself if you want to drop one.

| Scope | What it allows | Used for |
|---|---|---|
| `app_mentions:read` | receive `app_mention` events | the `@pi …` form, in channels and threads |
| `chat:write` | post, update, stream as the app | every reply, streamed turn, and status update |
| `channels:history` | read public-channel messages | plain text in a public thread the bot is in |
| `groups:history` | read private-channel messages | the same, in a private channel |
| `im:history` | read direct messages | DMs with the bot, including DM threads |
| `mpim:history` | read group direct messages | a multi-person DM that includes the bot |
| `assistant:write` | act as a Slack agent | `setTitle` and suggested prompts; also the `app_context_changed` event. Note `setStatus` alone works with `chat:write` today, but `setTitle` does not |
| `files:read` | download files shared with the app | handing an attached image to the agent |
| `reactions:write` | add emoji reactions | a 👀 receipt while the agent is working |

Do not install yet — Slack shows a reinstall banner until you do, and you are about to
change more settings.

## Step 4 — Subscribe to the events

Go to **Event Subscriptions**, toggle **Enable Events** on, and under **Subscribe to bot
events** add:

| Event | Comes from |
|---|---|
| `app_mention` | someone typing `@pi …` |
| `message.channels` | public channels the bot is in |
| `message.groups` | private channels the bot is in |
| `message.im` | direct messages with the bot |
| `message.mpim` | group direct messages with the bot |

Do **not** fill in a Request URL. Because Socket Mode is enabled, Slack says so itself:
payloads are delivered over the socket instead.

Two things to expect:

- Each `message.*` event needs its matching history scope from Step 3; Slack refuses the
  event otherwise.
- `message.channels` delivers **every** message in every public channel the bot is in,
  including conversations pi-chat does not care about. That is inherent to the Events
  API — the daemon ignores anything that is not a thread it owns, and only responds when
  mentioned or addressed in a thread it already has a session for.

Click **Save Changes**.

## Step 5 — App Home: let people send direct messages

1. Go to **App Home**.
2. Under **Show Tabs → Messages Tab**, enable **Allow users to send Slash commands and
   messages from the messages tab**.

Without this the bot cannot be messaged at all, and DM threads — one of the two places a
session can live — are unreachable even with `im:history` granted.

**Optional: the agent experience.** Also on this page (or under *Features → Agents & AI
Apps*, depending on when your workspace was migrated) you can enable Slack's agent/assistant
surface, which gives the split-view container with your bot's DM history. Be aware that
this container is thread-only by construction, so a developer slash command cannot be
used inside it: everything there is the `@pi …` form. pi-chat works either way.

## Step 6 — Interactivity (buttons and modals)

Go to **Interactivity & Shortcuts** and toggle it on. No Request URL is needed under
Socket Mode — interactive payloads arrive on the socket.

This is what carries the approval buttons and the modal pi-chat opens when the agent
needs multi-line input, so leave it on if you intend to use interactive approvals rather
than `--approve`.

## Step 7 — The `/pi` slash command

Go to **Slash Commands** and create one:

- **Command:** `/pi`
- **Short description:** `Run pi-chat commands` (anything you like)
- **Request URL:** the form will not always accept an empty value. Under Socket Mode the
  URL is **never called**, so any `https://` placeholder is fine — for example
  `https://example.invalid/slack/events`. If a placeholder is later reported as
  `invalid_url` or `dispatch_failed`, re-save the command with a parseable HTTPS URL:
  Slack validates the string even though it will not use it.

This is the command behind the root form (`/pi status`). Remember the platform rule that
dictates pi-chat's whole command grammar: **Slack forbids developer slash commands
inside message threads**, so thread control is written `@pi /status` instead.

## Step 8 — Install the app

1. Go to **Install App** (under *Settings*).
2. Click **Install to Workspace**, review the permissions, and click **Allow**.
3. Copy the **Bot User OAuth Token**, which starts with `xoxb-`.

From now on, **any change to scopes or bot events disables the app until you reinstall** —
the install page shows a reinstall banner. This is the most common cause of
`missing_scope` errors.

## Step 9 — Store both tokens as files

pi-chatd reads token *values from files*, so neither token ends up in the config file,
your shell history, or a process listing. Use `printf`, not `echo`: a trailing newline is
harmless to pi-chatd, but a stray one confuses `curl` and most other tools you may test
the token with later.

```bash
install -d -m 700 ~/.config/pi-chat
printf '%s' 'xapp-your-app-level-token' > ~/.config/pi-chat/slack-app-token
printf '%s' 'xoxb-your-bot-token'       > ~/.config/pi-chat/slack-bot-token
chmod 600 ~/.config/pi-chat/slack-app-token ~/.config/pi-chat/slack-bot-token
```

Sanity check without printing the secrets — confirm each file is non-empty and starts
with the right prefix:

```bash
test -s ~/.config/pi-chat/slack-app-token && echo "app token file is non-empty"
test -s ~/.config/pi-chat/slack-bot-token && echo "bot token file is non-empty"
head -c 5 ~/.config/pi-chat/slack-app-token; echo   # xapp-
head -c 5 ~/.config/pi-chat/slack-bot-token; echo   # xoxb-
```

## Step 10 — Invite the bot to a channel

In Slack, open the channel you want to use (start with a private test channel) and:

```text
/invite @pi
```

A bot that is not in the conversation receives no events from it, and cannot post there
either — `chat:write` grants the ability to speak, not the right to join.

## Step 11 — Allow yourself to use it

pi-chat denies every request until your Slack user is on the allowlist, because an agent
with shell access is exactly the thing worth gating. The allowlist lives under the
platform's section — `[slack.access]`, not a top-level `[access]` — because each chat
integration has its own way of naming people.

1. In Slack, click your avatar → **Profile** → the **⋯** menu → **Copy member ID**.
   It starts with `U`.
2. Put it in `~/.config/pi-chat/config.toml`:

```toml
[slack]
app_token_file = "~/.config/pi-chat/slack-app-token"
bot_token_file = "~/.config/pi-chat/slack-bot-token"

[slack.access]
allowed_users    = ["U0123456789"]   # required — Slack member IDs
allowed_channels = []                # optional — empty means any channel the bot is in
```

While you are there, `allowed_channels` is worth filling in if the bot is in a busy
workspace: it is the difference between "any channel the bot was invited to" and "only
the channels I chose".

## Step 12 — Verify

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

If a token file says `missing`, or `allowed users` says `none — every request is denied`,
fix that before starting the daemon. Then start it and mention the bot in the channel:

```text
@pi hello
```

pi-chat opens a thread, starts a session through `pi-gatewayd`, and streams the answer
into that thread. That is the phase-0 milestone ([PLAN.md](../PLAN.md) M3).

---

## Installing only the phase-0 minimum

If you would rather prove the round-trip before granting anything else, create the app
with only:

- Socket Mode on, app-level token with `connections:write`
- Bot scopes `app_mentions:read` and `chat:write`
- Bot event `app_mention`
- The Messages Tab setting from Step 5 — required for DMs, and needed later anyway
- Install, store the tokens, invite the bot, allowlist yourself

Then come back and do Steps 3, 4, 6 and 7 in one sitting, and reinstall once. Nothing
else in the tutorial changes; skipping these just delays DMs, in-thread plain text,
buttons, and the `/pi` command.

---

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `invalid_auth` | Wrong token in the wrong file, or a stray newline, quote or space. `printf '%s'` avoids it; check with `head -c 5` |
| `missing_scope` | A scope was added but the app was never reinstalled. Install App → Reinstall |
| `not_in_channel` | Invite the bot: `/invite @pi` |
| Nothing happens on `@pi hello` | The app is not a member of that conversation, `app_mention` is not subscribed, Socket Mode is off, or `allowed_users` does not contain you. Slack's app page shows each toggle; pi-chatd logs the refusal |
| DMs do not reach the bot | The Messages Tab setting (Step 5) is off, or `im:history` / `message.im` is missing |
| Events arrive twice or not at all | Two instances of `pi-chatd` are running. Socket Mode splits events across connections, so the daemon is deliberately single-instance |
| `/pi` worked once, then stopped | A scope or event change disabled the install. Reinstall |
| `invalid_url` / `dispatch_failed` on `/pi` | The placeholder Request URL is unparseable. Slack validates it even though Socket Mode never calls it; use a real-looking HTTPS URL |
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
- [ ] Nine bot scopes added (Step 3)
- [ ] Five events subscribed: `app_mention`, `message.channels`, `message.groups`, `message.im`, `message.mpim`
- [ ] Messages Tab enabled in App Home
- [ ] Interactivity enabled
- [ ] `/pi` slash command created
- [ ] App installed to the workspace, bot token → `slack-bot-token`
- [ ] `chmod 600` on both token files
- [ ] Bot invited to a channel with `/invite @pi`
- [ ] Your member ID in `[slack.access] allowed_users`
- [ ] `make check` reports both Slack tokens present and you as an allowed user
