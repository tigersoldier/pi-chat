# Slack self-DM: step-by-step setup

This guide takes you from nothing to pi sessions driven from the Slack
conversation you have with yourself ("Notes to self"), with **no Slack app to
create or install**. It is the app-less surface; the walkthrough for the app
surface is [`slack-app-setup.md`](slack-app-setup.md).

Budget about 15 minutes, most of it in the Slack UI.

> **Setting this up for a team?** If several people in one workspace or org
> should each drive their own self-DM through one shared Slack app, use
> [`slack-user-token-setup.md`](slack-user-token-setup.md) instead: same surface,
> but the credential is a scoped, per-person OAuth token rather than a browser
> session.

## Before you start

Read this once. The self-DM is a supported-but-caveated surface, and two of the
caveats are not fixable:

- **No notifications, ever.** Every message in the self-DM is authored by you,
  so Slack never marks it unread and never pings you. The agent's answer is
  there when you look, and nowhere else.
- **The credentials are an account-wide secret.** The `xoxc` token plus the `d`
  cookie are your whole Slack session — every channel and DM you can see, not a
  scoped bot token — and they live on disk until they are rotated or you sign
  out everywhere.
- **Slack detects unofficial clients.** Its audit logs name `unexpected_client`,
  `spoofed_user_agent` and `unexpected_scraping`, and enterprise security teams
  watch for exactly this traffic. Use it on a **personal workspace you own**, not
  a work one.

Also missing by construction, and fine to miss: slash commands, buttons, modals,
the status indicator, and streamed rendering. Choices are numbered replies.

[`self-dm-feasibility.md`](self-dm-feasibility.md) is the full account of the
cost; [`self-dm-integration-options.md`](self-dm-integration-options.md) is why it
lives in the same daemon as the app.

## Pick your shape

| Shape | What you configure | What you get |
|---|---|---|
| **A. Self-DM only** | `[slack] enabled = false` + `[slack.self_dm] enabled = true` | one control channel in your own DM; nothing to install in Slack |
| **B. App only** | the defaults | mentions, DMs, slash commands, buttons, status, notifications ([walkthrough](slack-app-setup.md)) |
| **C. Both** | defaults + `[slack.self_dm] enabled = true` | the app where you have it, the self-DM as a second way in; one daemon, one session table |

Both surfaces share the store and the warm-session cap; the daemon routes by
channel id, so the app's DM with you and your self-DM are different
conversations with different sessions. This guide covers **A** first — the other
shapes are one line apart (Step 5).

---

## Step 1 — Run pi-gatewayd

pi-chatd is only the chat side; pi-gatewayd owns the sessions.

```bash
systemctl --user status pi-gatewayd
ls -l ~/.config/pi-gateway/tokens.json
```

If it is not installed yet, follow the pi-gateway packaging (the unit is at
`~/code/pi-gateway/packaging/pi-gatewayd.service`), then enable it:

```bash
install -Dm644 ~/code/pi-gateway/packaging/pi-gatewayd.service \
    ~/.config/systemd/user/pi-gatewayd.service
systemctl --user daemon-reload && systemctl --user enable --now pi-gatewayd
```

Mint the two tokens pi-chat needs — an admin one for lifecycle, and a thread one
with **no** admin capability:

```bash
install -d -m700 ~/.config/pi-chat
pi-gatewayd --provision-token --token-name pi-chat-admin --token-role admin \
    > ~/.config/pi-chat/gateway-admin.token
pi-gatewayd --provision-token --token-name pi-chat-thread \
    --token-caps observe,interject,prompt,ui,control \
    > ~/.config/pi-chat/gateway-thread.token
chmod 600 ~/.config/pi-chat/gateway-*.token
```

*Checkpoint:* `make probe` in the repository should smoke-test the gateway seam
without Slack being involved at all.

## Step 2 — Build and install pi-chatd

```bash
cd ~/code/pi-gchat
make test          # optional, but this is the only moment it is free
make install
make install-unit
```

Expected:

```text
installed /home/YOU/.local/bin/pi-chatd
now: systemctl --user enable --now pi-chatd
```

Do not enable it yet: with no valid configuration it would fail and restart in a
loop. Steps 3–6 come first.

## Step 3 — Capture your Slack session credentials

You need the pair Slack's own web client uses: the `xoxc-…` session token and
the `d` cookie (`xoxd-…`).

1. Open the workspace in a browser where you are signed in.
2. Open DevTools (`F12`) → **Network**, and filter on `api/`.
3. Click any channel in Slack, so the client makes a request.
4. Pick any `https://WORKSPACE.slack.com/api/…` request.
5. From its **Payload / Form Data**, copy the `token` value (`xoxc-…`).
6. From its **Request Headers**, copy the `d` cookie value (`xoxd-…`).

Write them into two files, mode 0600:

```bash
umask 077
cat > ~/.config/pi-chat/slack-xoxc <<'EOF'
paste-the-xoxc-token-here
EOF
cat > ~/.config/pi-chat/slack-xoxd <<'EOF'
paste-the-d-cookie-value-here
EOF
chmod 600 ~/.config/pi-chat/slack-xoxc ~/.config/pi-chat/slack-xoxd
ls -l ~/.config/pi-chat/slack-xox*   # both must be -rw-------
```

Notes that save an hour later:

- Copy the cookie **as the browser sends it**. If you copy the decoded value from
  a cookie editor and it contains `+`, that is fine — pi-chat escapes it.
- Some workspaces need the sibling `d-s` cookie too. The file may contain a whole
  Cookie header instead of a single value: `d=xoxd-…; d-s=…`.
- The pair is a session credential with a lifetime of about a year (Slack
  shortened it in December 2025) and it dies immediately on a password change or
  "sign out everywhere". Step 9 is what to do then.

*Checkpoint:* `wc -c ~/.config/pi-chat/slack-*` — both are non-trivial; an empty
file is the same as a missing one.

## Step 4 — Find the self-DM's channel id and your member id

**The self-DM's conversation id (`D…`).** Open the "Notes to self" DM — click
**+** next to Direct messages, type your own name, press Enter — then either read
the id out of the browser URL (`app.slack.com/client/T…/D…`) or, from the desktop
app, right-click any message → **Copy link**, and take the `D…` segment from
`https://WORKSPACE.slack.com/archives/D0123456789/p…`.

**Your member id (`U…`).** Click your avatar → **Profile** → the ⋮ menu →
**Copy member ID**.

Neither is secret, and both are needed in Step 5.

## Step 5 — Write the configuration

The live file is `~/.config/pi-chat/config.toml`, mode 0600. To start from the
annotated template:

```bash
install -Dm600 pi-chat.toml.example ~/.config/pi-chat/config.toml
$EDITOR ~/.config/pi-chat/config.toml
```

For **shape A** (self-DM only), these are the sections that matter:

```toml
[slack]
enabled = false                 # no app at all

[slack.access]
allowed_users = ["U01234567"]   # your member id from Step 4 — required

[slack.self_dm]
enabled       = true
auth          = "session"                  # or "user_oauth": docs/slack-user-token-setup.md
xoxc_file     = "~/.config/pi-chat/slack-xoxc"
xoxd_file     = "~/.config/pi-chat/slack-xoxd"
workspace_url = "https://acme.slack.com"   # your workspace host
channel_id    = "D0123456789"              # the self-DM from Step 4
poll_interval = "5s"                       # how often the DM is read
```

For **shape C** (both), delete the `enabled = false` line — the app surface is
on by default and needs its own two token files, which
[`slack-app-setup.md`](slack-app-setup.md) provides.

The allowlist is not suspended because the conversation is private: deny by
default is what protects a machine that runs an agent with shell access.

*Checkpoint:* the file is 0600 and every path is absolute after `~` expansion.

## Step 6 — Validate

```bash
make check
```

Expected (paths abbreviated):

```text
config /home/YOU/.config/pi-chat/config.toml is valid
  slack app surface: off (a missing token disables it)
  slack app token: /home/YOU/.config/pi-chat/slack-app-token (missing, not used)
  slack bot token: /home/YOU/.config/pi-chat/slack-bot-token (missing, not used)
  slack self-DM surface: on
  self-DM xoxc token: /home/YOU/.config/pi-chat/slack-xoxc (present)
  self-DM xoxd cookie: /home/YOU/.config/pi-chat/slack-xoxd (present)
  self-DM channel: D0123456789, workspace: https://acme.slack.com, poll: 5s
  …
  allowed users: U01234567
  database: /home/YOU/.local/state/pi-chat/pi-chat.db
```

Look for three things: `self-DM surface: on`, both credential files `(present)`,
and your own id under `allowed users`. `make check` reads the file only — it does
not call Slack — so a wrong-but-well-formed credential pair is caught by the
daemon at startup instead (Step 7 and Troubleshooting).

## Step 7 — Start the daemon

```bash
systemctl --user enable --now pi-chatd
journalctl --user -u pi-chatd -f
```

A healthy start looks like this (log order can vary; the watching line comes from
the poller's goroutine):

```text
level=INFO msg="connected to Slack as yourself for the self-DM" workspace_id=T0123ABCD user_id=U01234567 channel=D0123456789 poll=5s
level=INFO msg="pi-chatd is ready" config=/home/YOU/.config/pi-chat/config.toml surfaces="[slack self-DM poller]" render=stream …
level=INFO msg="the self-DM surface is watching" channel=D0123456789 workspace=T0123ABCD poll=5s cursor=1700000000.000000
```

*Checkpoint:* `systemctl --user is-active pi-chatd` says `active`, and there is no
`level=ERROR` line. Leave `journalctl -f` open for Step 8 — it is the fastest way
to see what the daemon is doing.

## Step 8 — Try it in Slack

In the self-DM, in this order:

1. **`/pi help`** — the daemon answers with the command list. This proves the
   round trip without spending a single token: the answer is posted into the
   conversation as you.
2. **`fix the failing test`** (or any prompt) — a session starts and the answer
   appears **in a thread** on your message, patched in place as it grows.
3. **Reply once more inside that thread** — the same session continues; a new
   message at the top level would start a new one.
4. **`/pi status`** — the session's state, and how close the warm cap is.
5. Optional: **`/pi new`** retires the session and starts a fresh one;
   **`/pi resume`** offers the sessions pi-chat can adopt as a numbered list.

What you will *not* see: a notification, an unread badge, a spinner, or a button.
That is the surface, not a fault.

## Step 9 — Keep it running

**When the cookie expires** (usually a year, sooner after a password change or
"sign out everywhere"): the poller logs

```text
level=WARN msg="cannot read the self-DM; retrying" error="slack conversations.history: invalid_auth" in=5s hint="the Slack session cookie or xoxc token is stale: sign in to the workspace in a browser, copy them again into slack.self_dm.xoxc_file and slack.self_dm.xoxd_file, and restart"
```

It keeps retrying with a growing backoff and keeps the app surface working if you
also run one. Re-do Step 3, then `systemctl --user restart pi-chatd`.

**Rotating or removing the credentials** is the same three files and a restart.
Setting `slack.self_dm.enabled = false` and restarting leaves everything else —
sessions, threads, project directories — intact and resumable from the app
surface or the gateway.

**Where state lives**, in case you need to inspect or back it up:

| What | Where |
|---|---|
| thread↔session bindings, cursors, the posted ledger | `~/.local/state/pi-chat/pi-chat.db` (SQLite) |
| the work for each session | `~/work/…` (or `paths.projects_root`) |
| pi's own session files | the pi-gateway state dir, `~/.config/pi-gateway` |

**Uninstalling the surface** is `enabled = false` plus a restart; uninstalling
pi-chatd itself is `make uninstall`, and the database is yours to delete.

## Troubleshooting

Every message below is the daemon's own, quoted exactly. Config mistakes fail
`make check`; anything about credentials or the conversation is found at startup.

| What you see | What it means | Fix |
|---|---|---|
| `slack.self_dm.channel_id must be the self-DM's D… conversation id, got "C0123456789"` | that is a channel, not the self-DM | Step 4; the id starts with `D` |
| `slack.self_dm.workspace_url must not be empty` | the workspace host is missing | set `https://YOUR-WORKSPACE.slack.com` |
| `slack.self_dm.workspace_url must be an https workspace URL like https://acme.slack.com, got "ftp://…"` | wrong scheme; a bare host is fine (`acme.slack.com` is normalized) | use `https://` or just the host |
| `slack: no surface is enabled — turn on slack.enabled (the app) or slack.self_dm.enabled (the self-DM)` | everything is switched off | shape A turns the app off, so the self-DM must be on |
| `slack.self_dm.xoxc_file: read token /…/slack-xoxc: open …: no such file or directory` | the credential file is missing | Step 3; check the path in the config |
| `slack.self_dm: the session token was refused: slack auth.test: invalid_auth` | the `xoxc`/`xoxd` pair is wrong, stale, or not a pair | re-copy **both** from the same browser session |
| `slack.self_dm: cannot read channel D…: slack conversations.history: channel_not_found` | the id is not a conversation that token can see | re-check the `D…` from Step 4 |
| `slack.self_dm is enabled as U…, who is not in slack.access.allowed_users: every message would be refused` | the allowlist does not contain you | add your member id to `slack.access.allowed_users` |
| `level=ERROR msg="the Slack self-DM surface is off: it could not start, and the daemon keeps running without it"` | the self-DM is configured but unusable, while the app surface is up | the attached error names the cause; the fix is in this table |
| `level=WARN msg="the Slack app surface is off: its bot token is not readable"` | expected for shape A | nothing; set `slack.app_token_file` only if you want the app |
| `msg="cannot read the self-DM; retrying"` | the cookie went stale while running | Step 3, then restart |
| The daemon exits on startup with one of the messages above, and systemd restarts it | the self-DM is your **only** surface, so its failure is fatal | fix the cause, then `systemctl --user reset-failed pi-chatd` if the unit latched |

If nothing answers in Slack, check the poller is alive
(`journalctl --user -u pi-chatd | grep watching`) and that the message was typed
in the right conversation: only the configured `D…` is read.

## What it deliberately does not do

- **No notifications and no unread state** — the messages are yours, so Slack
  treats them as read; look at the DM.
- **No slash commands of its own, no buttons, no modals, no status.** `/pi …`
  typed as text is recognized; numeric replies replace button presses.
  If the app surface is *also* installed and registers `/pi`, Slack delivers the
  slash command to the app instead — the answer still lands in the DM, and plain
  prompts are handled by the self-DM as usual.
- **No conversation context for the agent.** Every message you send is a turn,
  and the agent's own session history carries its side of the conversation.
- **Polling, not push.** Messages are picked up within `poll_interval` (5s by
  default), and a message is read once — the cursor is durable, so a restart
  resumes where it left off rather than replaying the DM. The very first run
  starts at the newest message instead of replaying your history.

## Where to read more

- [`slack-self-dm-setup.md`](slack-self-dm-setup.md) — this guide
- [`self-dm-feasibility.md`](self-dm-feasibility.md) — what the no-app route costs
- [`self-dm-integration-options.md`](self-dm-integration-options.md) — one daemon
  versus a separate service
- [`slack-app-setup.md`](slack-app-setup.md) — the app surface
- [`../DESIGN.md`](../DESIGN.md) §11 (configuration) and §12 (the adapter seam)
