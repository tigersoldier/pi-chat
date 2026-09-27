# Creating the Slack app for pi-chat

A step-by-step tutorial for wiring a Slack app to `pi-chatd`. Budget about ten minutes,
most of which is Slack's own screens.

The app is configured from a **manifest** — one YAML file in this repository that
declares every scope, event and setting the bot needs. Slack applies it in one step,
which is both faster and less error-prone than clicking settings on and off, and it means
the configuration in your workspace matches the adapter in this repository.

Instructions first, reference after. If you would rather click through the settings
yourself, [Appendix A](#appendix-a--doing-it-by-hand) covers that; the result is
identical.

## Step 0 — What you will end up with

| Item | Shape | Where it goes |
|---|---|---|
| App-level token | `xapp-…` | `~/.config/pi-chat/slack-app-token` |
| Bot token | `xoxb-…` | `~/.config/pi-chat/slack-bot-token` |
| Your member ID | `U…` | `[slack.access] allowed_users` in `~/.config/pi-chat/config.toml` |

The gateway token files (`gateway-admin.token`, `gateway-thread.token`) are a different
system: they authenticate to `pi-gatewayd`, not to Slack. If they are missing, do
[PLAN.md](../PLAN.md) M0 first.

pi-chat uses **Socket Mode**, so the daemon opens an outbound WebSocket to Slack and
everything — mentions, messages, buttons, modals, slash commands — arrives over that
socket. Consequences worth knowing before you start:

- No public URL, no tunnel, no TLS certificate, and **no signing secret**: Slack never
  makes an HTTP request to you, so there is nothing to verify signatures for.
- You need **two** tokens, not one: an *app-level* token that is only good for opening
  the socket, and a *bot* token used for everything you send back.
- A Socket Mode app **cannot be listed in the Slack Marketplace**. This is a personal app
  by design.

## Step 1 — Create the app from the manifest

1. Open <https://api.slack.com/apps> and click **Create New App**.
2. Choose **From an app manifest**.
3. Pick the workspace you want the bot in, and click **Next**.
4. Paste the whole of [`slack/manifest.yaml`](../slack/manifest.yaml) into the field.
   (Which manifest to choose: see [the agent variant](#the-agent-variant) below.)
5. Review the summary Slack shows — it lists the scopes, events and settings it is about
   to apply — and click **Create**.

The comments at the top of the manifest mark what is worth customizing:

| Field | Meaning |
|---|---|
| `display_information.name` | the app name, which is also the mention handle people type. `pi` in the file means `@pi`. 35 characters max |
| `display_information.description` | one line shown in Slack's app directory |
| `features.bot_user.display_name` | the `@handle` Slack shows on messages |
| `features.slash_commands[].command` | the root command name. Renaming it is fine — pi-chat uses whatever command name Slack delivers — but if you rename it, `/pi` in this documentation becomes your name |

Everything else is load-bearing: the scopes, the events, `socket_mode_enabled`, and the
Messages Tab. Edit them only if you know the adapter expects them.

> **Already created an app by hand?** You can apply the manifest to it instead of
> starting over: your app → **Settings → App Manifest** → replace the contents → **Save**.
> Slack shows a diff of exactly what changes and then asks you to reinstall. That is the
> one-paste way to pick up the remaining scopes.

## Step 2 — Generate the app-level token

A manifest cannot contain tokens — they are generated per install, and deliberately not
storable in configuration. So this one step stays manual:

1. Go to **Basic Information → App-Level Tokens → Generate Token and Scopes**.
2. Name it `pi-chat-socket`, click **Add Scope**, choose **`connections:write`**, then
   **Generate**.
3. Copy the value — it starts with `xapp-` — and keep it somewhere temporary. Slack shows
   it once.

`connections:write` does exactly one thing: it lets the app call `apps.connections.open`
to obtain the WebSocket URL. It grants no access to messages, which is why it is worth
keeping separate from the bot token.

## Step 3 — Install the app

1. Go to **Install App** (under *Settings*).
2. Click **Install to Workspace**, review the permissions, and click **Allow**.
3. Copy the **Bot User OAuth Token**, which starts with `xoxb-`.

From now on, **any change to scopes or events disables the app until you reinstall** — the
install page shows a reinstall banner. This is the most common cause of `missing_scope`.

## Step 4 — Store both tokens as files

pi-chatd reads token *values from files*, so neither token ends up in the config file,
your shell history, or a process listing. Use `printf`, not `echo`: a trailing newline is
harmless to pi-chatd, but a stray one confuses `curl` and most other tools you might test
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

## Step 5 — Invite the bot to a channel

In Slack, open the channel you want to use (start with a private test channel) and:

```text
/invite @pi
```

A bot that is not in the conversation receives no events from it, and cannot post there
either — `chat:write` grants the ability to speak, not the right to join.

## Step 6 — Allow yourself to use it

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

## Step 7 — Verify

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
fix that before starting the daemon.

You can also ask Slack directly whether the bot token is valid:

```bash
TOKEN=$(cat ~/.config/pi-chat/slack-bot-token)
curl -s -H "Authorization: Bearer $TOKEN" https://slack.com/api/auth.test \
  | python3 -m json.tool
```

`"ok": true` with your team name and a `user_id` starting with `U` means the token and the
install are good. (This puts the token in `curl`'s command line for the moment the request
runs — fine on your own machine, worth avoiding on a shared one.)

Then start the daemon and mention the bot in the channel:

```text
@pi hello
```

pi-chat opens a thread, starts a session through `pi-gatewayd`, and streams the answer
into that thread. That is the phase-0 milestone ([PLAN.md](../PLAN.md) M3).

---

## What the manifest configured, and why

Reference for what you just applied. Nothing here needs action.

### Bot token scopes

| Scope | What it allows | Used for |
|---|---|---|
| `app_mentions:read` | receive `app_mention` events | the `@pi …` form, in channels and threads |
| `chat:write` | post, update, stream as the app | every reply, streamed turn, and status update |
| `commands` | register slash commands and shortcuts | the `/pi` root command. Declaring `features.slash_commands` in a manifest **requires** this scope — without it Slack rejects the paste with `commands requires the commands bot scope` |
| `channels:history` | read public-channel messages | plain text in a public thread the bot is in |
| `groups:history` | read private-channel messages | the same, in a private channel |
| `im:history` | read direct messages | DMs with the bot, including DM threads |
| `mpim:history` | read group direct messages | a multi-person DM that includes the bot |
| `files:read` | download files shared with the app | handing an attached image to the agent (**phase 3**; the scope is requested now so enabling it needs no reinstall) |
| `reactions:write` | add emoji reactions | a 👀 receipt while the agent is working (**phase 3**) |
| `assistant:write` | act as a Slack agent | **agent variant only** — `setTitle`, suggested prompts, agent context events |

### Bot events

| Event | Comes from |
|---|---|
| `app_mention` | someone typing `@pi …` |
| `message.channels` | public channels the bot is in |
| `message.groups` | private channels the bot is in |
| `message.im` | direct messages with the bot |
| `message.mpim` | group direct messages with the bot |

`message.channels` delivers **every** message in every public channel the bot is in,
including conversations pi-chat does not care about. That is inherent to the Events API —
the daemon ignores anything that is not a thread it owns, and only responds when
mentioned or addressed in a thread it already has a session for.

The agent manifest adds `app_home_opened` (how a DM open is detected in that surface),
`app_context_changed`, `agent_session_stopped`, and `agent_session_title_changed`.

### Settings

| Setting | Value | Why |
|---|---|---|
| `socket_mode_enabled` | `true` | the whole transport; no request URLs anywhere |
| `interactivity.is_enabled` | `true` | approval buttons and the multi-line input modal |
| `app_home.messages_tab_enabled` | `true` | without it the bot cannot be messaged at all |
| `app_home.messages_tab_read_only_enabled` | `false` | users can send, not just read |
| `features.slash_commands` | `/pi` | the root command; its request URL is never called under Socket Mode |

Because Socket Mode carries everything over the socket, **no request URL is needed for
events, interactivity, or the slash command** — the manifest therefore sets none.

One platform rule shapes the entire command grammar: **Slack forbids developer slash
commands inside message threads.** That is why root control is `/pi status` and thread
control is `@pi /status`.

## The agent variant

[`slack/manifest-agent.yaml`](../slack/manifest-agent.yaml) declares the app as a Slack
agent and enables Slack's agent messaging experience. Instead of a plain DM, the bot's
Messages tab becomes a conversation timeline, and Slack renders a real agent status.

What that buys pi-chat:

| Capability | Instead of |
|---|---|
| `agents.sessions.setStatus` with real values — `processing` while a turn runs, `suspended` while waiting for your approval, `active` when idle, `closed` after a delete | no status at all, or a free-form string with a two-minute timeout |
| a **stop button** while the agent works (via the `agent_session_stopped` event), which a later phase wires to abort the turn | `/abort` typed by hand |
| `agents.sessions.rename` titles, suggested prompts in the composer | untitled threads |

Before choosing it, know the caveats:

- The workspace must have the agent feature enabled. Where it is not, calls fail with
  `feature_disabled`, and the bot still works but shows no status.
- Switching an app to the agent view **cannot be reversed**, and `assistant_view` (the
  older assistant surface) is deprecated — new apps can only use `agent_view`.
- The agent container is **thread-only by construction**, so a slash command cannot be
  used inside it. There, everything is the `@pi …` form. pi-chat's grammar already
  accounts for this: DM content is plain text.
- Declaring the app as an agent is what grants `assistant:write`.

To switch an existing app: paste `slack/manifest-agent.yaml` into **Settings → App
Manifest** and save. Slack will ask you to reinstall.

---

## Appendix A — doing it by hand

Everything the manifest does, as a checklist. If you have already created the app,
**Settings → App Manifest** is still the faster path; this table is for people who prefer
Slack's own pages, or who need to check what the manifest applied.

| Where | Setting |
|---|---|
| Create New App → From scratch | App name `pi`, pick the workspace |
| Features → **Socket Mode** | enable; app-level token with `connections:write` |
| Features → **OAuth & Permissions** → Bot Token Scopes | add the nine (or ten) scopes in [the table above](#bot-token-scopes) |
| Features → **Event Subscriptions** | enable; add the five (or nine) bot events; leave the Request URL empty |
| Features → **App Home** → Show Tabs | Messages Tab on, "allow users to send messages" on |
| Features → **Interactivity & Shortcuts** | enable; no Request URL needed |
| Features → **Slash Commands** | create `/pi`; under Socket Mode the Request URL is never called, but Slack still parses it, so any parseable `https://` placeholder works |
| Settings → **Install App** | install; copy the bot token |
| Features → **Agents & AI Apps** (optional) | enable the agent experience; equivalent to the agent manifest |

Then continue at [Step 2](#step-2--generate-the-app-level-token).

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `invalid_auth` | Wrong token in the wrong file, or a stray newline, quote or space. `printf '%s'` avoids it; check with `head -c 5` |
| `missing_scope` | A scope changed but the app was never reinstalled. Install App → Reinstall |
| `not_in_channel` | Invite the bot: `/invite @pi` |
| Nothing happens on `@pi hello` | The app is not a member of that conversation, `app_mention` is not subscribed, Socket Mode is off, or `allowed_users` does not contain you. Slack's app page shows each toggle; pi-chatd logs the refusal |
| DMs do not reach the bot | The Messages Tab setting is off, or `im:history` / `message.im` is missing |
| `feature_disabled` | The agent manifest was used on a workspace without the agent feature. Use `slack/manifest.yaml` instead |
| Events arrive twice or not at all | Two instances of `pi-chatd` are running. Socket Mode splits events across connections, so the daemon is deliberately single-instance |
| `/pi` worked once, then stopped | A scope or event change disabled the install. Reinstall |
| `invalid_url` / `dispatch_failed` on `/pi` | The placeholder Request URL is unparseable. Slack validates it even though Socket Mode never calls it; use a real-looking HTTPS URL |
| The socket drops overnight | Socket Mode is a long-lived connection and is sensitive to network churn; the app reconnects, and the unit restarts it on failure |
| Manifest paste rejected | Slack reports the offending field. The usual causes: editing a load-bearing value, or declaring a feature whose scope is missing — re-paste the file from this repository and customize only the fields listed in Step 1 |
| `commands requires the commands bot scope` | The manifest declares `features.slash_commands` but not the `commands` scope. Both manifests in this repository include it; a hand-edited copy may have dropped it |
| Cannot install the app | The workspace restricts app installation — an admin must approve it, or you need a workspace where you can install apps |
| Want to be listed in the Marketplace | Not possible with Socket Mode; that requires a public HTTP endpoint, which this project deliberately avoids |

## Rotating or revoking credentials

- **App-level token:** Basic Information → App-Level Tokens → revoke and generate a new
  one; write the new value to the same file and restart `pi-chatd`.
- **Bot token:** OAuth & Permissions → revoke the installation (or reinstall), then update
  `slack-bot-token`.
- Deleting the app entirely invalidates both tokens.

Never commit either file. The repository's `.gitignore` blocks `*.token`, `*-token` and
`config.toml` so an accidental `git add -A` cannot publish a token.

## Checklist

- [ ] App created from `slack/manifest.yaml` (or the agent variant)
- [ ] App-level token created with `connections:write` → `slack-app-token`
- [ ] App installed to the workspace, bot token → `slack-bot-token`
- [ ] `chmod 600` on both token files
- [ ] Bot invited to a channel with `/invite @pi`
- [ ] Your member ID in `[slack.access] allowed_users`
- [ ] `make check` reports both Slack tokens present and you as an allowed user
- [ ] `auth.test` returns `"ok": true`
- [ ] `@pi hello` in a thread streams an answer back
