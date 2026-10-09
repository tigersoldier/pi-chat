# Slack user-token self-DM: one app, one token per person

This is the team shape of the self-DM surface: **one Slack app**, shared, with
**one token per person**. Whoever owns the app runs a small token broker; each
person opens an install link once, approves the app, and pastes the token they
are shown into their own `pi-chatd`. From then on each daemon reads and writes
its owner's own "Notes to self" as that person, with nobody else's agent, bot or
secret involved.

If you are setting this up for one machine only and would rather not create an
app at all, use [`slack-self-dm-setup.md`](slack-self-dm-setup.md) (the
browser-session mode) instead. Both modes produce the same surface; they differ
only in where the credential comes from.

## What you get, and what it costs

| | |
|---|---|
| **One app for everyone** | The workspace installs the app once; each person authorizes it for themselves. `oauth.v2.access` returns a user token per authorization. |
| **Sanctioned, scoped tokens** | `im:history`, `im:read`, `im:write`, `chat:write`, `users:read` — not an account-wide session cookie. Revocable one person at a time. |
| **No expiry** | Plain OAuth tokens do not expire. Keep **token rotation off**; with it on, tokens last 12 hours and pi-chatd does not refresh them. |
| **Someone runs a broker** | The app owner hosts `pi-chat-oauth` behind HTTPS. It sees each token as it is issued (it is the OAuth client) and stores none of them. |
| **No Socket Mode** | Each daemon polls with its own token. Socket Mode belongs to the app and distributes payloads across *all* its connections, so it cannot be shared between people. |
| **No notifications, buttons or status** | Unchanged from the session mode: a message you post to yourself is never unread, so nothing pings you. |

Two roles: the **owner** (Steps 1–3, once) and each **person** (Steps 4–7, about
five minutes).

---

## Step 1 — The app (owner)

Use the repository's manifest, which already declares the user scopes for this
mode — [`slack/manifest.yaml`](../slack/manifest.yaml) (or
[`manifest-agent.yaml`](../slack/manifest-agent.yaml) if you also want Slack's
agent surface):

1. Create or update the app from the manifest (see
   [`slack-app-setup.md`](slack-app-setup.md) for the same walkthrough).
2. **OAuth & Permissions → Redirect URLs**: add the broker's callback —
   `https://pi-chat.example.com/callback` for this guide.
3. Confirm **Token Rotation** is **off**. pi-chatd has no refresh path; with
   rotation on, the broker detects the expiring token and refuses to show it.
4. On a work workspace, get the app and those scopes approved. Slack's app
   management policy can block self-install, and the consent screen names the
   DM access, so tell people what to expect.
5. On Enterprise Grid, distributing the app to the org (and to the workspaces
   that should have it) is done by an Org Admin; users then authenticate once
   for every workspace that has the app.
6. Copy the **Client ID** and **Client Secret** from *Basic Information*.

The user scopes are requested only by the broker's install link, never by the
app's own install, so an existing install's consent screen does not change.

## Step 2 — Run the broker (owner)

```bash
cd ~/code/pi-gchat
make install            # builds bin/pi-chatd and bin/pi-chat-oauth, installs both
make install-unit-oauth # installs the optional systemd unit
```

Put the credentials and arguments in `~/.config/pi-chat/oauth.env` (0600) —
never on a command line, where they would be visible in `ps`:

```bash
umask 077
cat > ~/.config/pi-chat/oauth.env <<'EOF'
SLACK_CLIENT_ID=1234567890.1234567890
SLACK_CLIENT_SECRET=paste-the-client-secret
PI_CHAT_OAUTH_ARGS=--addr 127.0.0.1:8080 --redirect-url https://pi-chat.example.com/callback
EOF
systemctl --user enable --now pi-chat-oauth
journalctl --user -u pi-chat-oauth -f
```

Expected:

```text
level=INFO msg="pi-chat-oauth is listening" addr=127.0.0.1:8080 callback=https://pi-chat.example.com/callback scopes=im:history,im:read,im:write,chat:write,users:read
```

Slack's redirect URL must be HTTPS, so terminate TLS in front of it. Any reverse
proxy will do; with Caddy it is one command:

```bash
caddy reverse-proxy --from pi-chat.example.com --to 127.0.0.1:8080
```

*Checkpoint:* open `https://pi-chat.example.com/` — you should see the broker's
intro page and a **Continue to Slack** link. If the page loads over plain HTTP,
go no further: the token would travel unencrypted.

**Broker flags** (all optional; `-client-id`/`-client-secret` also read
`SLACK_CLIENT_ID`/`SLACK_CLIENT_SECRET`):

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `:8080` | listen address |
| `-redirect-url` | derived from the request `Host` | the registered callback; set it explicitly behind a proxy |
| `-scopes` | `im:history,im:read,im:write,chat:write,users:read` | user scopes to request |
| `-state-secret` | derived from the client secret | HMAC key for install links; set it if you run more than one replica |
| `-slack-api`, `-slack-authorize` | `slack.com` | override for GovSlack or a test double |
| `-title` | `pi-chat` | the service name on the pages |

## Step 3 — Invite people (owner)

Send them `https://pi-chat.example.com/` (the bare host, not `/install`: the
landing page explains what they are approving, and the link itself is a signed,
short-lived invitation). Each person continues from there on their own.

## Step 4 — Authorize (each person)

1. Open the link and click **Continue to Slack**.
2. Slack shows the consent screen for the app, naming the access it wants
   ("view messages in your direct messages", "send messages as you" and so on).
   Approve it. If your workspace requires admin approval, Slack says so here
   instead of granting anything.
3. The broker shows a page with **your token**, once. Copy it: the broker does
   not store it and the page cannot be reloaded to get it back (start again if
   you lose it).

The page also shows the configuration below with your own member id filled in,
which is the piece most people forget.

## Step 5 — Configure pi-chatd (each person)

```bash
umask 077
cat > ~/.config/pi-chat/slack-xoxp <<'EOF'
paste-your-xoxp-token-here
EOF
chmod 600 ~/.config/pi-chat/slack-xoxp
ls -l ~/.config/pi-chat/slack-xoxp   # must be -rw-------
```

In `~/.config/pi-chat/config.toml`:

```toml
[slack.access]
allowed_users = ["U01234567"]      # your own member id, from the broker's page

[slack.self_dm]
enabled       = true
auth          = "user_oauth"
xoxp_file     = "~/.config/pi-chat/slack-xoxp"
channel_id    = "D0123456789"      # open "Notes to self"; D… is in its URL
poll_interval = "5s"
```

`workspace_url` and the `xoxc`/`xoxd` files are not used in this mode: a user
token resolves its own workspace.

## Step 6 — Check and restart (each person)

```bash
make check
```

Expected (paths abbreviated):

```text
config /home/YOU/.config/pi-chat/config.toml is valid
  slack app surface: off (a missing token disables it)
  slack self-DM surface: on (user OAuth token)
  self-DM xoxp token: /home/YOU/.config/pi-chat/slack-xoxp (present)
  self-DM xoxc token: /home/YOU/.config/pi-chat/slack-xoxc (missing, not used)
  self-DM xoxd cookie: /home/YOU/.config/pi-chat/slack-xoxd (missing, not used)
  self-DM channel: D0123456789, workspace: slack.com, poll: 5s
  …
  allowed users: U01234567
```

```bash
systemctl --user restart pi-chatd
journalctl --user -u pi-chatd -f
```

Expected in the log:

```text
level=INFO msg="connected to Slack as yourself for the self-DM" auth="user OAuth token" workspace_id=T0123ABCD user_id=U01234567 channel=D0123456789 poll=5s
level=INFO msg="pi-chatd is ready" surfaces="[slack self-DM poller]" …
```

## Step 7 — Verify in Slack (each person)

In **Notes to self**: `/pi help` should answer without spending any tokens; then
send a prompt to start a session, and reply inside the thread to continue it.
Same grammar and limits as the session-credential mode — see
[`slack-self-dm-setup.md`](slack-self-dm-setup.md) for the command table.

## Maintenance

- **Revoke your own access**: Slack → your profile → *Apps* → the app → remove.
  The daemon then logs `cannot read the self-DM; retrying` with
  `invalid_auth` and keeps the rest of pi-chatd running.
- **Re-authorize**: open the broker link again. Scopes accumulate on the same
  grant; there is no way to remove one without revoking the whole token.
- **Scope changes** are an app change: reinstall/approve, then everyone
  re-authorizes to pick them up.
- **Add a person**: send the link. **Remove a person**: they revoke, or an admin
  uninstalls — one token at a time, unlike the session mode.
- **Rotate the client secret**: update `oauth.env` and restart the broker.
  Existing tokens keep working; they are not derived from the secret.

## Troubleshooting

Each message is quoted exactly as the daemon or broker prints it.

| What you see | Meaning | Fix |
|---|---|---|
| `slack.self_dm.auth must be "session" or "user_oauth", got "browser"` | a typo in `auth` | one of the two values |
| `slack.self_dm.xoxp_file: read token /…/slack-xoxp: open …: no such file or directory` | the token file is missing | save the token as in Step 5 |
| `slack.self_dm.xoxp_file: this is a bot token (xoxb-…); the self-DM needs a person's user token (xoxp-…)` | the wrong token was pasted | copy the token the broker showed, not the app's bot token |
| `… this is a browser session token (xoxc-…)` | the session credential was paired with `auth = "user_oauth"` | either paste the `xoxp` token, or set `auth = "session"` |
| `… this is an expiring token (xoxe…): the app has token rotation enabled …` | rotation is on | turn it off in the app settings, reinstall, authorize again |
| `slack.self_dm: the session token was refused: slack auth.test: invalid_auth` | the token was revoked or removed | open the broker link again |
| `slack.self_dm: cannot read channel D…: channel_not_found` | wrong `channel_id` | read `D…` from the "Notes to self" URL |
| `the page should contain…`-style broker error `Slack refused the exchange: invalid_code` | the link was already used or is older than 15 minutes | start again from the broker's page |
| The broker page says *Access was granted, but the token cannot be used* | rotation, or the app is missing the user scopes | fix the app, then authorize again; nothing was stored |
| `pi-chat-oauth: no client id: pass -client-id or set SLACK_CLIENT_ID` | `oauth.env` is incomplete or unreadable | check the file is 0600 and contains both values |

## How this differs from the session-credential mode

| | `auth = "session"` (xoxc + `d` cookie) | `auth = "user_oauth"` (xoxp) |
|---|---|---|
| App needed | no | yes, plus someone to own it |
| Per person | paste two secrets from DevTools | authorize once, paste one token |
| Credential lifetime | ~1 year, dies on sign-out-everywhere | no expiry (rotation off) |
| Scope of the secret | your whole Slack session | the granted scopes, revocable alone |
| Approval | none — unofficial | admin approval on work workspaces |
| Multi-person | one pair per person, each captured by hand | one app, one link, one token each |
| Ingress | poll | poll |

## Security notes

- Treat the token like a password: 0600 file, never in a ticket or a chat. If it
  leaks, revoke it in Slack (profile → *Apps*) and authorize again.
- The broker is a credential-handling service. It does not store tokens, logs
  them or leaves them in caches (`Cache-Control: no-store`, `Referrer-Policy:
  no-referrer`), and it refuses tokens pi-chatd cannot use — but its operator
  does see each token as it is issued, because it is the OAuth client. Keep the
  install link on HTTPS, and prefer an operator you would trust with a
  short-lived credential.
- Install links are signed and expire after 15 minutes, so a forwarded link is
  not a standing invitation. The person who opens it authorizes *their own*
  account; a leaked link cannot be redeemed as somebody else.
- The person's token cannot read anything they cannot: it is not a workspace
  install and not a bot.
