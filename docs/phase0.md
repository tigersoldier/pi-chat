# Phase 0 — Google plumbing spike

Goal: prove the round-trip **Chat message → Chat app → Pub/Sub → probe →
`spaces.messages.create` → Chat reply** before building anything else. This is
the riskiest unknown in the whole design (see [DESIGN.md](../DESIGN.md) —
"Build phasing").

Definition of done: the probe (`go run ./cmd/probe`) receives an event and a
reply appears in the Chat thread. Nothing else.

## Step 1 — provision GCP plumbing (scripted)

```bash
./scripts/setup-gcp.sh jihanzi
```

Creates, idempotently:

| Resource | Name | Purpose |
|---|---|---|
| APIs | `pubsub.googleapis.com`, `chat.googleapis.com` | needed by everything below |
| Pub/Sub topic | `pi-gchat-events` | Chat app publishes interaction events here |
| Pub/Sub pull subscription | `pi-gchat-sub` | the daemon/probe pulls from here |
| Service account | `pi-gchat@<project>.iam.gserviceaccount.com` | subscriber on the subscription + Chat app identity |
| SA key | `~/.pi/agent/pi-gchat-sa.json` | local auth for the daemon (chmod 600) |
| Config | `~/.pi/agent/pi-gchat.toml` | `[gcp]` section consumed by the probe |

## Step 2 — create the Chat app (console, manual)

There is no gcloud/API equivalent; this is a one-time console step.

1. Google Cloud Console → project `jihanzi` → **Google Chat API** →
   **Configuration** (the API library page for Chat API has a Configuration tab).
2. **App name:** `pi-gchat`; add an avatar and description if you like.
3. **Connection settings:** select **Pub/Sub topic** →
   `projects/jihanzi/topics/pi-gchat-events`.
4. **App identity:** select **Service account** →
   `pi-gchat@jihanzi.iam.gserviceaccount.com`.
5. **Visibility:** restrict to **specific people/groups** → your own Google
   account. (Defense-in-depth layer 1 from the design; the per-event email
   allowlist comes in phase 1.)
6. Leave status at **TESTING**, **Save**, and wait ~1 minute for propagation.

## Step 3 — sanity-check the Pub/Sub plumbing (optional)

```bash
gcloud pubsub topics publish pi-gchat-events \
  --message='{"type":"MESSAGE","probe":"self-test"}' --project=jihanzi
gcloud pubsub subscriptions pull pi-gchat-sub \
  --limit=1 --auto-ack --project=jihanzi
```

You should see the message. This proves topic → subscription before involving
the Chat app.

## Step 4 — run the probe and DM the bot

```bash
go run ./cmd/probe
```

Then, in [Google Chat](https://chat.google.com): open **Apps** (left rail) →
find **pi-gchat** → open a DM → send any message.

Expected probe output:

```
probe: config: /home/you/.pi/agent/pi-gchat.toml
probe: spike: pulling 1 event(s) from projects/jihanzi/subscriptions/pi-gchat-sub (timeout 10m0s)
probe: event: type=MESSAGE space=spaces/AAAA... thread=spaces/AAAA.../threads/TTTT user=you@gmail.com
probe: replied: spaces/AAAA.../messages/MMMM
probe: done: 1 event(s) round-tripped
```

And in the DM thread, a reply: `✅ pi-gchat probe: round-trip OK …`.

Useful flags: `-count N` (process several events), `-timeout 30s`, `-project`,
`-subscription`, `-credentials` (all override the config file).

## Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| Probe times out, no events | Chat app not created yet, wrong topic in the console config, or app not yet added — re-check Step 2 and that you messaged the app itself (not a namesake). |
| `parse event: ...` / `no type field` | Payload isn't a Chat event (e.g. you published a self-test message). Ack it by running the probe once more, or pull+purge with `gcloud pubsub subscriptions pull ... --auto-ack`. |
| `spaces.messages.create: ...` error | App identity in console ≠ the SA whose key the probe uses; or app not a member of the space. |
| No reply in thread, but probe says `replied:` | Check the bot's message was created (it may land as a separate message if `thread.name` was missing). |
| Events pile up in the subscription | `gcloud pubsub subscriptions pull pi-gchat-sub --limit=10 --auto-ack` to drain. |

## Teardown (only if needed)

```bash
gcloud pubsub subscriptions delete pi-gchat-sub --project=jihanzi
gcloud pubsub topics delete pi-gchat-events --project=jihanzi
gcloud iam service-accounts delete pi-gchat@jihanzi.iam.gserviceaccount.com --project=jihanzi
rm ~/.pi/agent/pi-gchat-sa.json
# Chat app: Console → Google Chat API → Configuration → Delete
```

## Hand-off to phase 1

Once the round-trip works, phase 1 replaces the probe's `main` with the real
daemon: same subscription (streaming pull), same service account, plus TOML
`[access]`/`[paths]`/`[behavior]` sections, SQLite, sessions, and rendering.
The probe stays as a `cmd/` tool for plumbing checks.
