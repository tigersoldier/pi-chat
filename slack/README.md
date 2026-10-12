# Slack app manifest

[`manifest.yaml`](manifest.yaml) is pi-chat's single supported Slack app manifest. It
configures Socket Mode, messaging, slash commands, the Messages tab, and Slack's agent
messaging experience. When the workspace allows that experience, DMs get an agent timeline,
lifecycle status, and a stop button while pi is working.

If Agent Sessions are unavailable or disabled for the workspace, the bot still handles
messages, but Slack will not show the agent status/timeline or stop button. There is no
separate plain-bot manifest; ask a workspace admin to enable the experience if it should be
available.

## Creating the app

<https://api.slack.com/apps> → **Create New App** → **From an app manifest** → pick the
workspace → paste `manifest.yaml` → **Next** → review → **Create**.

## Updating an existing app

Your app → **Settings → App Manifest** → replace the contents with `manifest.yaml` →
**Save**. Slack shows a diff and asks you to reinstall, so scope and event changes are
reviewed before taking effect.

The manifest declares `features.agent_view`. In the Slack API dashboard, the equivalent
app-level setting is under **Agents** (sometimes labeled **Agents & AI Apps**). Separately,
a workspace/org admin can allow or disable the experience for an installed app at **Admin →
Apps and workflows → Installed apps → [app] → App Settings → AI agent experience**. Availability
depends on workspace plan and admin settings.

Reinstall after applying manifest scope or event changes. Slack may issue a new bot token, so
check the `xoxb-` file and update it if needed. Restart `pi-chatd` after changing app/admin
settings so it reloads credentials and retries any previously unsupported agent status.
Switching an existing app from the legacy assistant view to the agent view cannot be reversed.

## What a manifest cannot do

Tokens are not part of a manifest; they are generated per install:

1. **App-level token** — Basic Information → App-Level Tokens → generate one named
   `pi-chat-socket` with the `connections:write` scope.
2. **Bot token** — Install App → Install to Workspace, then copy the Bot User OAuth Token
   (`xoxb-…`).

Also manual, because they are per-deployment rather than per-app: writing both tokens to
`~/.config/pi-chat/slack-{app,bot}-token`, inviting the bot to a channel, and putting your
member ID in `[slack.access] allowed_users`.

Full walkthrough, including the manual click-through path:
[docs/slack-app-setup.md](../docs/slack-app-setup.md).
