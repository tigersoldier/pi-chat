# pi-chat

Chat integrations for [pi-gateway](https://github.com/tigersoldier/pi-gateway):
drive local [pi](https://pi.dev) coding-agent sessions from a chat platform.
Slack is the first integration; Google Chat is a planned second one.

`pi-chatd` runs on your machine and is fully outbound: one WebSocket to Slack
(Socket Mode) and one loopback connection to `pi-gatewayd`. No listening socket,
no tunnel, no public URL, no certificate. It does **not** own pi processes —
`pi-gatewayd` does, and is a prerequisite.

Architecture, the thread/session model, the access rules and the build phases:
**[DESIGN.md](DESIGN.md).** Current position and milestones: **[PLAN.md](PLAN.md).**

## Status

Design settled. M0 (the gateway seam), M1 (the Slack app, phase 0), M2 (repo prep), M3
(the vertical slice), **M4 (the phase-1 MVP)** and **M4.6 (the agent surface) are done**:
in Slack, a mention or a command starts a pi session in a thread, the answer streams back,
and the thread's state survives a restart. Phase 1 has a user allowlist, durable thread
state, project directories with the worktree convention, `/pi help|status|resume`,
`@pi /<command>` pass-through, idle close and cold resume. Declared as Slack's agent
messaging experience (the recommended manifest), the session also shows a real status
while it works and a stop button that aborts the turn. Still to come (phase 2):
`@pi /delete`, `/abort`, `/model`, interactive approvals, and the warm-session cap with
eviction. Details: `PLAN.md`.

## Surfaces

`pi-chatd` talks to Slack through one or both of two surfaces (DESIGN.md §12),
and they share one core, one store and one session pool:

| Surface | Needs | Gives | Off when |
|---|---|---|---|
| **App** (default) | an installed app: `xapp-…` and `xoxb-…` | mentions, DMs, slash commands, buttons, status, notifications | `slack.enabled = false`, or either token file missing |
| **Self-DM** (opt-in) | either your own `xoxc-…` token and `d` cookie, or a per-person `xoxp-…` token minted by the app | a private control channel in "Notes to self" | `slack.self_dm.enabled` is false (the default) |

The mux routes a call by channel id, so the app's DM with you and your own
self-DM are different conversations with different sessions, in the same
database. The self-DM has no notifications, no slash commands and no buttons (a
numbered reply stands in for a press). Its credential comes from one of two
modes: your own browser session (no app at all —
`docs/slack-self-dm-setup.md`), or a scoped per-person OAuth token from a shared
app (`docs/slack-user-token-setup.md`); `docs/self-dm-feasibility.md` is the
honest account of the first, and the second needs an app owner and an approval
conversation.

## Prerequisites

1. **pi-gatewayd** running, with pi reachable:

   ```bash
   install -Dm644 ~/code/pi-gateway/packaging/pi-gatewayd.service \
       ~/.config/systemd/user/pi-gatewayd.service
   systemctl --user daemon-reload && systemctl --user enable --now pi-gatewayd
   ```

2. **Two tokens**, admin and thread, minted into `~/.config/pi-gateway/tokens.json`:

   ```bash
   pi-gatewayd --provision-token --token-name pi-chat-admin  --token-role admin \
       > ~/.config/pi-chat/gateway-admin.token
   pi-gatewayd --provision-token --token-name pi-chat-thread \
       --token-caps observe,interject,prompt,ui,control \
       > ~/.config/pi-chat/gateway-thread.token
   chmod 600 ~/.config/pi-chat/gateway-*.token
   ```

   The thread token deliberately has **no** `admin`: only the lifecycle
   operations use the admin token, and the long-lived per-thread connections
   cannot destroy sessions.

3. **A Slack app** — created from **[slack/manifest.yaml](slack/manifest.yaml)**;
   walkthrough: **[docs/slack-app-setup.md](docs/slack-app-setup.md)**. The app is
   optional: with no app tokens, the daemon runs the self-DM surface alone
   (**[docs/slack-self-dm-setup.md](docs/slack-self-dm-setup.md)**) — and with
   neither, it refuses to start and says so.

## Configuration

The live file is `~/.config/pi-chat/config.toml`, mode 0600. Every key and its
defaults: **[pi-chat.toml.example](pi-chat.toml.example)**. Check it with:

```bash
make check          # loads, validates, prints a redacted summary
```

## Running it

```bash
make install        # build and install ~/.local/bin/pi-chatd
make install-unit   # install packaging/pi-chatd.service and daemon-reload
systemctl --user enable --now pi-chatd
journalctl --user -u pi-chatd -f
```

`pi-chatd` is deliberately single-instance: Socket Mode splits deliveries across
connections, so a second copy would answer some messages twice. The unit restarts on
failure and is safe to leave running.

## Talking to the bot

Two trigger tokens, one vocabulary. Slack forbids developer slash commands
inside message threads, which is why the two forms differ.

> **What works today:** the mention forms and `/pi help`, `/pi status`, `/pi resume`,
> `@pi /status`, and pass-through of pi's own commands. `/delete`, `/abort`, `/model` and
> `/stop` are part of the grammar but answer "not in this build yet" until phase 2 — they
> are never forwarded to the agent as text.

| Where | Form | Example |
|---|---|---|
| Channel or DM root | `/pi <command>` | `/pi status`, `/pi resume`, `/pi help` |
| Channel or DM thread | `@pi /<command>` | `@pi /status`, `@pi /skill:grill-me` |
| Channel root or group DM root | `@pi <text>` | starts a thread and a session |
| Channel or group DM thread | `@pi <text>` | continues that thread's session |
| One-to-one DM root | plain text | starts a thread and a session |
| One-to-one DM thread | plain text | continues that thread's session |

A session is always thread-scoped; a root command never touches one. `/pi help`
and `@pi /help` print the current list.

In a **one-to-one DM**, plain text starts a session at the root and continues it in a
thread — no mention needed. In a **channel or group DM**, only a message that mentions pi
starts work; other replies are silently ignored as requests and read as context the next
time pi is addressed. Mentions get an `eyes` reaction when access is allowed; requests
outside the access policy get a `shrug` reaction and a private refusal. Plain text at
channel or group DM roots is ignored, which keeps busy rooms from turning every later
message into a prompt.

State lives in SQLite at `paths.db_path` (created on first run, inside a 0700 directory). It
holds thread keys, session identities, cursors and handled event IDs — never
transcripts, prompts, or answers.

## Layout

```text
cmd/pi-chatd/          the daemon: wiring, flags, logging, signals
cmd/pi-chat-oauth/     the optional OAuth broker: one user token per person
internal/config/       configuration file loading and validation
internal/bot/          platform-independent core: threads, sessions, turns
internal/slack/        Slack adapter: Socket Mode, Web API, rendering
packaging/             systemd user unit
slack/                 Slack app manifests — the app's configuration as code
tools/gateway-probe/   nested module: pi-gateway seam smoke test
```

## Development

```bash
make test            # unit tests
make vet             # go vet
make fmt             # gofmt
make live-test       # one real turn through a running pi-gatewayd (spends a few tokens)
make probe           # exercise the gateway seam against a running pi-gatewayd
```
