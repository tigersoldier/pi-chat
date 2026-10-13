# pi-chat

Chat integrations for [pi-gateway](https://github.com/tigersoldier/pi-gateway):
drive local [pi](https://pi.dev) coding-agent sessions from a chat platform.
Slack is the first integration; Google Chat is a planned second one.

`pi-chatd` runs on your machine. Network traffic is outbound: Slack Socket Mode,
the Slack Web API, and a loopback connection to `pi-gatewayd`. It does not own pi
processes — `pi-gatewayd` does, and is a prerequisite. When the opt-in read-only
chat tools are enabled, pi-chatd also opens a private Unix-domain socket for Pi
extensions; it is owner-only and never exposed over TCP or a public endpoint.

Architecture, the thread/session model, the access rules and the build phases:
**[DESIGN.md](DESIGN.md).** Current position and milestones: **[PLAN.md](PLAN.md).**

## Status

M0 (the gateway seam), M1 (the Slack app, phase 0), M2 (repo prep), M3 (the vertical
slice), **M4 (the phase-1 MVP)** and **M4.6 (the agent surface) are done**. In Slack, a
mention or command starts a pi session in a thread, streams its answer back, and keeps
thread state across restarts. Phase 1 includes an allowlist, durable thread state, project
worktrees, `/pi help|status|resume`, pass-through commands, idle close and cold resume.
The Slack app manifest enables Agent Sessions, including status and a stop button when the
workspace allows the agent experience. Opt-in read-only chat-tools v1 is implemented and
unit-tested, but has not yet been verified against a live Slack workspace. Phase 2 is
underway: `/abort`, `/model`, and `/stop` are implemented; interactive approvals, warm-session
eviction/queueing, and streaming fallback hardening remain. Details: `PLAN.md`.

## Surfaces

`pi-chatd` talks to Slack through one or both of two surfaces (DESIGN.md §12),
and they share one core, one store and one session pool:

| Surface | Needs | Gives | Off when |
|---|---|---|---|
| **App** (default) | an installed app: `xapp-…` and `xoxb-…` | mentions, DMs, slash commands, buttons, notifications; agent status and stop button when enabled for the workspace | `slack.enabled = false`, or either token file missing |
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

3. **A Slack app** — created from **[slack/manifest.yaml](slack/manifest.yaml)**,
   which declares Slack's agent experience and status/stop button. If the workspace
   disables the agent experience, the bot still works without those indicators. Walkthrough:
   **[docs/slack-app-setup.md](docs/slack-app-setup.md)**. The app is
   optional: with no app tokens, the daemon runs the self-DM surface alone
   (**[docs/slack-self-dm-setup.md](docs/slack-self-dm-setup.md)**) — and with
   neither, it refuses to start and says so.

## Configuration

The live file is `~/.config/pi-chat/config.toml`, mode 0600. Every key and its
defaults: **[pi-chat.toml.example](pi-chat.toml.example)**. Check it with:

```bash
make check          # loads, validates, prints a redacted summary
```

### Read-only chat tools

The `[chat_tools]` feature is off by default. When enabled, new sessions receive three
read-only tools (`chat_read_thread`, `chat_read_message`, `chat_get_user`) through a
versioned Pi extension and a private local broker. The gateway must advertise durable
`spawn_config` support; Pi and pi-chatd must be able to access the same mode-0600 Unix
socket. Existing/adopted sessions without an active binding remain usable without the
tools. See [docs/chat-tools.md](docs/chat-tools.md) for scope, limits, session sharing,
and failure behavior.

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
> `@pi /status`, `@pi /resume` for restoring a session retired in that thread, `/abort`,
> `/model [provider/model]`, `/stop`, `/delete` (with confirmation), and pass-through of
> pi's own commands. Control commands are handled by pi-chat, never forwarded as agent text.

| Where | Form | Example |
|---|---|---|
| Channel or DM root | `/pi <command>` | `/pi status`, `/pi resume`, `/pi help` |
| Channel or DM thread | `@pi /<command>` | `@pi /status`, `@pi /resume`, `@pi /skill:grill-me` |
| Channel root or group DM root | `@pi <text>` | starts a thread and a session |
| Channel or group DM thread | `@pi <text>` | continues that thread's session |
| One-to-one DM root | plain text | starts a thread and a session |
| One-to-one DM thread | plain text | continues that thread's session |

A session is always thread-scoped; a root command never touches one. `/pi help`
and `@pi /help` print the current list.

In a **one-to-one DM**, plain text starts a session at the root and continues it in a
thread — no mention needed. In a **channel or group DM**, only a message that mentions pi
starts work; other replies are silently ignored as requests and read as context the next
time pi is addressed. On the app surface, accepted mentions and one-to-one DM requests
get an `eyes` reaction; successful turns replace it with a `white_check_mark`, and
failed turns replace it with a `warning`. Requests outside the access policy get a
`shrug` reaction and a private refusal. Plain text at channel or group DM roots is
ignored, so busy rooms do not turn every later
message into a prompt.

### Images

Attach a PNG, JPEG, GIF or WebP to a mention (or a DM message) and the agent
receives the actual image, including image-only requests. Screenshots posted earlier
in the thread are included as conversation context when you next address the bot.
A turn carries up to four images, at most 5 MiB each and 10 MiB total; unavailable,
oversized and unsupported attachments produce a visible warning. Other file types
are not supported yet. The bot needs the `files:read` scope (included in both app
manifests); reinstall the Slack app if your existing token does not have it.

State lives in SQLite at `paths.db_path` (created on first run, inside a 0700 directory). It
holds thread keys, session identities, cursors, handled event IDs and chat-tool binding
metadata — never transcripts, prompts, app credentials, or answers. Pi's own session file
may retain tool results under normal gateway retention and deletion behavior.

## Layout

```text
cmd/pi-chatd/          the daemon: wiring, flags, logging, signals
cmd/pi-chat-oauth/     the optional OAuth broker: one user token per person
internal/config/       configuration file loading and validation
internal/bot/          platform-independent core: threads, sessions, turns
internal/slack/        Slack adapter: Socket Mode, Web API, rendering, read tools
internal/chattools/    private Unix-socket broker and embedded Pi extension
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
