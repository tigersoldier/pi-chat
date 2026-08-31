# Creating the pi-gchat Chat app — walkthrough

*Status: phase 0 (Google plumbing spike). This is the only manual setup step —
everything else is provisioned by `scripts/setup-gcp.sh`.*

## What you are creating, and why

The Google Chat app is the bot's identity in Google Chat and the one piece of
the system that cannot be created from the command line (Google exposes no
API to create Chat apps — only the console can do it). The app does two
things:

1. **Delivers events** — every message, mention, or space-join involving the
   bot is published by Google Chat to the Pub/Sub topic you select here
   (`projects/pi-gchat/topics/pi-gchat-events`). The daemon on your machine
   pulls those events from the matching subscription — this is the only
   "inbound" path; nothing listens on your machine.
2. **Gives the bot its reply identity** — the app is bound to the
   `pi-gchat` service account, so when the daemon calls
   `spaces.messages.create` with that account's key, the reply is attributed
   to this app in Chat.

Nothing else needs configuring in the console: the topic, subscription,
service account, and key already exist (created by the setup script), and
phase 1 (the real daemon) reuses this same app unchanged.

## Prerequisites (already done)

- Project `pi-gchat` with the Pub/Sub and Google Chat APIs enabled
- Topic `pi-gchat-events` and pull subscription `pi-gchat-sub`
- Service account `pi-gchat@pi-gchat.iam.gserviceaccount.com` with the
  subscriber role, and its key at `~/.pi/agent/pi-gchat-sa.json`
- Local config `~/.pi/agent/pi-gchat.toml` (written by the script)

## Steps

1. Open the Chat API configuration page for the project:
   `https://console.cloud.google.com/apis/api/chat.googleapis.com/hangouts-chat?project=pi-gchat`
   (or Console → project **pi-gchat** → **Google Chat API** → **Configuration**).

2. **App name:** `pi-gchat`

3. **Avatar:** optional — any image; it becomes the bot's icon in Chat.

4. **Description** (shown in the app directory): suggested text —

   > Control your local pi coding agent from Google Chat. This app is the
   > Google-side half of pi-gchat: it forwards chat events to a Pub/Sub
   > topic and posts replies from the daemon running on your machine.

5. **Connection settings:** select **Pub/Sub topic** and enter
   `projects/pi-gchat/topics/pi-gchat-events`.
   (This is what makes Chat publish interaction events to your topic.)

6. **App identity:** select **Service account** and choose
   `pi-gchat@pi-gchat.iam.gserviceaccount.com`.
   (Replies from the daemon will be attributed to this app.)

7. **Visibility:** restrict the app to **specific people/groups** and add
   your own Google account. This is defense-in-depth layer 1: even if the
   daemon's per-event allowlist (phase 1) is bypassed, nobody else can
   install or message this app.

8. Leave status at **TESTING** and click **Save/Activate**. Google applies
   the configuration within about a minute.

## Verify

1. Run the probe: `make probe` (it waits for an event, 10 min default).
2. In Google Chat (chat.google.com): **Apps** (left rail) → find
   **pi-gchat** → open a DM → send any message.
3. The probe logs the event and posts the reply
   `✅ pi-gchat probe: round-trip OK …` in the same DM thread.

Phase 0 is done when that reply appears.

## Troubleshooting

| Symptom | Fix |
|---|---|
| App not in the Apps list / search | Visibility in step 7 must include your account; status must be TESTING (not a draft); wait a few minutes for indexing. |
| Events never arrive in the probe | Re-check the topic in step 5 — it must match exactly; the app only starts publishing after it is created/activated. |
| `spaces.messages.create` errors in the probe | App identity (step 6) must be the same service account whose key the probe uses. |

## Notes for later phases

- Phase 1 adds slash commands (`/resume`, later `/delete`, `/abort`,
  `/status`, `/help`) — they are declared on this same Configuration page.
- The app's Pub/Sub delivery, identity, and visibility stay unchanged for the
  life of the project.
