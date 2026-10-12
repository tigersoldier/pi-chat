# Slack app manifests

Configuration as code for the Slack side of pi-chat: instead of clicking through half a
dozen settings pages, paste one of these files into Slack and the app is configured
correctly — scopes, events, Socket Mode, Interactivity, the Messages Tab, and the `/pi`
command.

| File | App shape | Use when |
|---|---|---|
| `manifest-agent.yaml` | **Agent messaging experience** | **Recommended.** Slack's agent UX for DMs: a conversation timeline, a real agent status while a turn runs, and an interactive stop button. Needs the agent feature; where it is unavailable, pi-chat logs it once and runs without a status. |
| `manifest.yaml` | Standard bot | A smaller app for a workspace where the agent feature is unavailable or an admin has it disabled. Same bot in every other respect — no timeline, no status, no stop button. |

They share the core messaging scopes and events. The agent manifest additionally declares
`features.agent_view`, `assistant:write`, and the agent-related events (`app_home_opened`,
`agent_session_stopped`, `app_context_changed`, and `agent_session_title_changed`). Nothing
in pi-chat requires the agent one: it is a surface, not a dependency.

Both are ordinary YAML; the comments mark the fields worth customizing (app name,
descriptions, the slash-command name) and the fields that are load-bearing (scopes,
events, `socket_mode_enabled`, the Messages Tab).

## Creating the app

<https://api.slack.com/apps> → **Create New App** → **From an app manifest** → pick the
workspace → paste the file → **Next** → review → **Create**.

## Updating an existing app

Your app → **Settings → App Manifest** → replace the contents → **Save**. Slack shows a
diff of what changes and then asks you to reinstall, so nothing changes silently. This is
the supported way to pick up a new scope or event: one paste instead of one click per
setting, and the reinstall is the same either way.

Moving an existing plain-bot app to `manifest-agent.yaml` is one such paste. The manifest
declares `features.agent_view`; the equivalent app-level setting is in the Slack API
dashboard at <https://api.slack.com/apps> → select the app → **Agents** (sometimes labeled
**Agents & AI Apps**). Separately, a workspace/org admin can allow or disable the experience
for the installed app: **Admin → Apps and workflows → Installed apps → [app] → App Settings
→ AI agent experience**. Availability depends on the workspace plan and admin settings.
Where the experience is disabled, the bot keeps working but Slack does not show agent
sessions/status. Reinstall after applying manifest scope or event changes; Slack may issue a
new bot token, so check the `xoxb-` file and update it if needed. Restart `pi-chatd` after
changing app/admin settings so it reloads credentials and retries any previously unsupported
agent status.

## What a manifest cannot do

Tokens are not part of a manifest, and cannot be: they are generated per install.

1. **App-level token** — Basic Information → App-Level Tokens → generate one named
   `pi-chat-socket` with the `connections:write` scope.
2. **Bot token** — Install App → Install to Workspace, then copy the Bot User OAuth
   Token (`xoxb-…`).

Also still manual, because they are per-deployment rather than per-app: writing both
tokens to `~/.config/pi-chat/slack-{app,bot}-token`, inviting the bot to a channel, and
putting your member ID in `[slack.access] allowed_users`.

Full walkthrough, including the manual click-through path for people who prefer it:
[docs/slack-app-setup.md](../docs/slack-app-setup.md).
