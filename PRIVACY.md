# Privacy Policy

**Last updated:** 2026-08-30

## Overview

pi-gchat is a personal Google Chat bot that lets its owner control a local
[pi](https://pi.dev) coding agent from Google Chat. The daemon runs on the
owner's own computer and uses the owner's own Google Cloud project. This
policy describes what data the service processes, where it lives, and how it
is protected.

This is a personal, single-owner service. It is not offered to the public and
is not designed to collect data from anyone other than its owner.

## What we process and why

**Chat messages sent to the bot.** When someone sends the bot a message, the
following is delivered to the owner's infrastructure via Google Chat →
Pub/Sub events:

- the message text,
- the sender's email address and display name,
- identifiers for the space and thread (e.g. `spaces/…`, `spaces/…/threads/…`),
- timestamps.

This data is used only to operate the service: verify the sender against the
owner's allowlist, route the message to the correct session, forward the
prompt to the pi agent, and post replies in the right thread.

**Session transcripts and tool output.** Each conversation with the bot is
stored as a pi session file (JSONL) on the owner's machine, with a thread→
session mapping in a local SQLite database. Because the bot controls a coding
agent, these transcripts can include file contents the agent read, commands
it ran, and their output — i.e., anything a message causes the agent to
touch on the owner's machine.

**Logs.** The daemon and probe write short local logs (event types, message
names, errors).

**Infrastructure metadata.** Pub/Sub message IDs, API request metadata, and
the Chat messages themselves are processed by Google under Google's own
terms; see [Google's privacy policy](https://policies.google.com/privacy).

## How data is used

- Solely to operate the service (receive, route, process, reply).
- No advertising, no analytics, no profiling, no selling, no sharing with
  third parties. Only the owner has access to the daemon, its data, and its
  credentials.

## Where data is stored

- **On the owner's machine:** session files under `~/.pi/agent/sessions/`,
  the SQLite database, logs, and the configuration (which includes the
  service account key). The service account key is stored with owner-only
  file permissions and is never committed to the repository.
- **In the owner's Google Cloud project:** Pub/Sub messages (transient —
  pulled and acknowledged promptly), Chat API request metadata.
- **Google Chat:** messages are retained per Google Chat's retention rules.

## Retention and deletion

- Session data and the database are kept until the owner deletes them —
  manually, or via the bot's own deletion command when available.
- Pub/Sub messages are acknowledged as soon as they are processed and are
  not stored beyond Google's transient retention.
- Chat messages follow Google Chat's retention settings.

## Security

- The daemon is fully outbound: it pulls from Pub/Sub and calls the Chat
  API. It opens no listening ports and requires no tunnel or public
  endpoint.
- Every event is checked against the owner's email allowlist (and
  optionally a space allowlist) before any action is taken.
- The bot authenticates as a service account whose key exists only on the
  owner's machine.
- The bot executes code through an AI agent by design. This is inherently
  not fail-safe: do not send it secrets, credentials, or anything you would
  not want executed or recorded.

## Your choices and rights

Because this is a personal service, data subjects are effectively limited to
the owner. If you believe data about you has reached this service, you can
request access or deletion by opening an issue in the
[repository](https://github.com/tigersoldier/pi-gchat/issues).

## Changes to this policy

Changes are reflected by updating this document in the repository, with the
"Last updated" date at the top. Material changes will be called out in the
commit history.

## Contact

- GitHub issues: <https://github.com/tigersoldier/pi-gchat/issues>
