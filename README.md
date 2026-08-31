# pi-gchat

Control local [pi](https://pi.dev) coding-agent sessions from Google Chat.

The daemon runs on your machine (fully outbound — no listening socket, no
tunnel): it pulls Chat events from a Pub/Sub subscription, manages
`pi --mode rpc` subprocesses per session, and replies through the Chat API.
Architecture, data model, and build phasing: **[DESIGN.md](DESIGN.md)**.

## Status

**Phase 0 — Google plumbing spike** (the riskiest unknown, done first):
a tiny Go probe pulls one Chat event from Pub/Sub and posts one reply,
proving the round-trip before anything else is built.

## Quick start (phase 0)

```bash
./scripts/setup-gcp.sh <PROJECT_ID>   # topic, subscription, service account, config
# …create the Chat app in the console (see docs/phase0.md)…
make probe                            # or: go run ./cmd/probe
# DM the bot in Google Chat → the probe replies to your message
```

No project name is hard-coded anywhere; pass it as an argument (or `PROJECT_ID`
env var — the script falls back to `gcloud config get-value project`).

## Configuration

The daemon reads `~/.pi/agent/pi-gchat.toml` (path configurable via the probe's
`-config` flag). It is written locally by `scripts/setup-gcp.sh`, contains your
service-account key path, and is **never checked into git** (`pi-gchat.toml` is
gitignored). See **[pi-gchat.toml.example](pi-gchat.toml.example)** for a
reference copy with placeholders.

## Layout

```
cmd/probe/              phase-0 spike: pull one event, reply, exit
scripts/setup-gcp.sh    idempotent GCP provisioning (gcloud)
docs/phase0.md          console walkthrough, verification, troubleshooting
pi-gchat.toml.example   reference config with placeholders (copy, don't commit)
DESIGN.md               full high-level design
```

## Requirements

- Go ≥ 1.24
- `gcloud` CLI, authenticated as owner/editor of the GCP project
