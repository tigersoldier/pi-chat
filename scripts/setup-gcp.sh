#!/usr/bin/env bash
# Provision the phase-0 GCP plumbing for pi-gchat (idempotent).
#
# Creates (or verifies): Pub/Sub API + Chat API, the events topic, the pull
# subscription, the daemon service account with its subscriber binding, the
# service account key, and ~/.pi/agent/pi-gchat.toml ([gcp] section only).
#
# The Chat app itself is created in the Cloud Console — there is no gcloud
# equivalent: Google Chat API → Configuration. See docs/phase0.md.
#
# Usage: ./scripts/setup-gcp.sh [PROJECT_ID]
# Env overrides: PROJECT_ID TOPIC SUBSCRIPTION SA_NAME CONFIG_DIR KEY_FILE
set -euo pipefail

PROJECT_ID="${1:-${PROJECT_ID:-}}"
if [[ -z "$PROJECT_ID" ]]; then
  PROJECT_ID="$(gcloud config get-value project 2>/dev/null || true)"
fi
if [[ -z "$PROJECT_ID" ]]; then
  echo "usage: $0 [PROJECT_ID]" >&2
  exit 1
fi

TOPIC="${TOPIC:-pi-gchat-events}"
SUBSCRIPTION="${SUBSCRIPTION:-pi-gchat-sub}"
SA_NAME="${SA_NAME:-pi-gchat}"
SA_EMAIL="$SA_NAME@$PROJECT_ID.iam.gserviceaccount.com"
CONFIG_DIR="${CONFIG_DIR:-$HOME/.pi/agent}"
KEY_FILE="${KEY_FILE:-$CONFIG_DIR/pi-gchat-sa.json}"
CONFIG_FILE="$CONFIG_DIR/pi-gchat.toml"

say()  { printf '\n==> %s\n' "$*"; }
have() { "$@" >/dev/null 2>&1; }

say "project: $PROJECT_ID"

say "enabling APIs: pubsub, chat"
gcloud services enable pubsub.googleapis.com chat.googleapis.com --project="$PROJECT_ID"

say "topic: $TOPIC"
if have gcloud pubsub topics describe "$TOPIC" --project="$PROJECT_ID"; then
  echo "  already exists"
else
  gcloud pubsub topics create "$TOPIC" --project="$PROJECT_ID"
fi

say "service account: $SA_EMAIL"
if have gcloud iam service-accounts describe "$SA_EMAIL" --project="$PROJECT_ID"; then
  echo "  already exists"
else
  gcloud iam service-accounts create "$SA_NAME" \
    --display-name="pi-gchat daemon" \
    --description="pi-gchat: pulls Chat events from Pub/Sub, posts replies via the Chat API" \
    --project="$PROJECT_ID"
fi

say "pull subscription: $SUBSCRIPTION (subscriber: $SA_EMAIL)"
if have gcloud pubsub subscriptions describe "$SUBSCRIPTION" --project="$PROJECT_ID"; then
  echo "  already exists"
else
  gcloud pubsub subscriptions create "$SUBSCRIPTION" --topic="$TOPIC" --project="$PROJECT_ID"
fi
if gcloud pubsub subscriptions get-iam-policy "$SUBSCRIPTION" --project="$PROJECT_ID" 2>/dev/null | grep -q "roles/pubsub.subscriber"; then
  echo "  subscriber binding already present"
else
  gcloud pubsub subscriptions add-iam-policy-binding "$SUBSCRIPTION" \
    --member="serviceAccount:$SA_EMAIL" --role="roles/pubsub.subscriber" \
    --project="$PROJECT_ID" >/dev/null
  echo "  granted roles/pubsub.subscriber"
fi

say "service account key: $KEY_FILE"
mkdir -p "$CONFIG_DIR"
if [[ -f "$KEY_FILE" ]]; then
  echo "  already exists (not regenerated; delete it and re-run to rotate)"
else
  gcloud iam service-accounts keys create "$KEY_FILE" --iam-account="$SA_EMAIL" --project="$PROJECT_ID"
  chmod 600 "$KEY_FILE"
fi

say "config: $CONFIG_FILE"
KEY_IN_CONFIG="$KEY_FILE"
if [[ "$KEY_FILE" == "$HOME/"* ]]; then
  KEY_IN_CONFIG="~/${KEY_FILE#"$HOME"/}"
fi
cat > "$CONFIG_FILE" <<EOF
# pi-gchat daemon configuration. Phase 0: only the [gcp] section exists;
# phase 1 adds [access], [paths], [behavior] — see DESIGN.md.
[gcp]
project_id = "$PROJECT_ID"
subscription_name = "$SUBSCRIPTION"
credentials_file = "$KEY_IN_CONFIG"
EOF

cat <<EOF

Done. Remaining step is console-only (no gcloud equivalent):
  Google Cloud Console → project "$PROJECT_ID" → Google Chat API → Configuration
    • App name: pi-gchat
    • Connection settings: Pub/Sub topic → projects/$PROJECT_ID/topics/$TOPIC
    • App identity: service account → $SA_EMAIL
    • Visibility: restricted to your own Google account (defense in depth)
    • Save, then run:  go run ./cmd/probe
    • DM the bot in Google Chat — the probe logs the event and posts a reply.

Full walkthrough: docs/phase0.md
EOF
