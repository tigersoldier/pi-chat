# pi-gchat — High-Level Design

Control local [pi](https://pi.dev) coding-agent sessions from Google Chat.

## Goals (v1 feature set)

1. Start a new pi session from chat.
2. Interact with that session from chat.
3. Only the owner's Google account can use it.
4. Resume pi sessions that were started *outside* the chat bot.

---

## Architecture

```
                    Google Cloud                                Local machine
 ┌─────────────────────────────────────────┐      ┌─────────────────────────────────────────┐
 │  Google Chat (you, on any device)        │      │                                         │
 │     │  messages / mentions / commands    │      │                                         │
 │     ▼                                    │      │                                         │
 │  Chat app  ──publishes events──▶  Pub/Sub topic          Pub/Sub subscription (pull)      │
 │     ▲                                    │      │     │  (streaming pull, low latency)    │
 │     └── spaces.messages.create ◀───────────────┼─────┼─────┤                                 │
 │            (app auth / service account) │      │     ▼                                 │
 └─────────────────────────────────────────┘      │  ┌──────────────┐   spawn/kill   ┌─────────────┐
                                                  │  │  pi-gchat    │ ─────────────▶ │ pi --mode   │
                                                  │  │  (Go daemon) │  stdin/stdout  │ rpc (per    │
                                                  │  └──────┬───────┘    JSONL       │ session)    │
                                                  │         │  SQLite               └─────────────┘
                                                  │         ▼                                     │
                                                  │  ~/.pi/agent/pi-gchat.db                      │
                                                  └─────────────────────────────────────────────────┘
```

- The daemon is **fully outbound**: it pulls Pub/Sub and calls the Chat API. No listening socket, no tunnel, no public endpoint.
- Each active session runs as its **own `pi --mode rpc` subprocess** (process isolation, natural concurrency).

---

## Components

### Google Cloud side (configured once, in the console)

- **GCP project** — owns everything below.
- **Chat app** — a bot. Configured to deliver events to a **Pub/Sub topic** (not an HTTP endpoint).
  - Slash commands (`/resume`, later `/delete`, `/abort`, `/status`, `/help`) are declared in the app's "Chat API configuration" page in the console.
  - Install scope: restricted to the owner's account (defense-in-depth layer 1).
- **Pub/Sub topic + pull subscription** — the Chat app publishes interaction events; the daemon pulls from the subscription.
- **Service account** — dual purpose:
  - Pub/Sub Subscriber on the subscription (read events).
  - Chat app identity for `spaces.messages.create` (app auth, `chat.bot` scope).
  - Key file stored locally (path in config).

### pi-gchat daemon (Go)

Libraries: `cloud.google.com/go/pubsub`, `google.golang.org/api/chat/v1`, `modernc.org/sqlite` (pure-Go, no cgo), `BurntSushi/toml`, `os/exec` + `bufio.Scanner` for the RPC subprocess.

Responsibilities:
1. **Pull** Chat events from Pub/Sub (streaming pull), ack them.
2. **Verify** every event: sender email in `allowedEmails`; if `allowedSpaces` is set, space id in it.
3. **Route** by event type:
   - `MESSAGE` in a DM → new one-shot session.
   - `MESSAGE` in a space → only act when the bot is @-mentioned; strip the `@BotName` prefix; start-or-continue the thread's session.
   - `MESSAGE` with a slash command → dispatch (MVP: `/resume` only).
   - `ADDED_TO_SPACE` → post a short help blurb.
4. **Manage sessions**: spawn/kill `pi --mode rpc` subprocesses, keep the thread↔session mapping in SQLite.
5. **Forward** pi's RPC events to Chat as rendered messages.
6. **Create** project directories and inject the repo/worktree system-prompt convention for new sessions.

### pi subprocess

`pi --mode rpc`, one per active session. Key commands used:

| Purpose | RPC command |
|---|---|
| Send prompt | `{"type":"prompt","message":...}` |
| Abort (Phase 2) | `{"type":"abort"}` |
| State (Phase 2) | `{"type":"get_state"}` |

Events consumed from stdout: `message_update` (`text_delta`, `thinking_delta`), `tool_execution_start/end`, `turn_end`, `agent_end`, `compaction_*`, errors.

Spawn forms:
- New session (DM or new space thread): `pi --mode rpc --name "<slug>" --append-system-prompt "<injected>"` with `cwd` = freshly created project dir.
- Resume: `pi --mode rpc --session <file>` with `cwd` = the session's recorded cwd. **No** system-prompt injection.

---

## Configuration

`~/.pi/agent/pi-gchat.toml` (TOML; `~` is expanded by the daemon in all path values):

```toml
[access]
allowed_emails = ["you@gmail.com"]          # required — feature #3
allowed_spaces = ["spaces/ABC123..."]        # optional; omit = any space I'm in

[gcp]
project_id = "my-project"
subscription_name = "pi-gchat-sub"
credentials_file = "~/.pi/agent/pi-gchat-sa.json"

[paths]
projects_root = "~/work"                    # new session project dirs live here
repos_root = "~/code"                       # git repos live here
db_path = "~/.pi/agent/pi-gchat.db"

[behavior]
idle_timeout_minutes = 15
max_concurrent_sessions = 10
progress_update_interval_ms = 1000
injected_prompt = """                        # template; {reposRoot} {projectsRoot} {date} {slug}
Repositories live under {reposRoot} (some may be bare). To work on a task:
1. List {reposRoot} and pick the repo this task needs.
2. Detect layout: git -C <repo> rev-parse --is-bare-repository
3. Normal clone only: git -C <repo> fetch origin   # read-only; updates remote-tracking refs, never local
   Bare repo: skip this step (never fetch/pull; use refs already present).
4. Resolve the default branch:
   - normal clone: git -C <repo> rev-parse --abbrev-ref origin/HEAD   # e.g. origin/main
   - bare repo:    git -C <repo> symbolic-ref --short HEAD             # e.g. main
   (fallback: git -C <repo> ls-remote --symref origin HEAD)
5. Create the worktree: git -C <repo> worktree add -b pi/{date}-{slug} ./repos/<repo-name> <default-branch-ref>
6. cd ./repos/<repo-name>, do all work there, commit and push when done.
"""
```

Notes:
- `injected_prompt` empty ⇒ no injection.
- `allowed_spaces` empty ⇒ the per-event email check is still enforced everywhere.

---

## Data model (SQLite)

```sql
CREATE TABLE threads (
  thread_name  TEXT PRIMARY KEY,   -- Chat thread name: spaces/{space}/threads/{thread}
  session_file TEXT NOT NULL,      -- ~/.pi/agent/sessions/--cwd--/x.jsonl
  session_id   TEXT NOT NULL,      -- from session header
  cwd          TEXT NOT NULL,      -- project root
  pid          INTEGER,            -- live RPC subprocess, or NULL when idle
  status       TEXT NOT NULL,      -- running | idle | dead
  created_at   TEXT NOT NULL,
  last_active  TEXT NOT NULL
);
```

- DM one-shot sessions have **no** `threads` row (the DM is a stateless launcher).
- "Bound to chat" ⇒ has a row. `/resume` lists sessions with
  `session_file NOT IN (SELECT session_file FROM threads)`.

---

## Interaction model

| Surface | Trigger | Behavior |
|---|---|---|
| DM | any message | Always a fresh one-shot session; stream result back; session saved but not bound to a thread |
| Space | @mention in a thread | Unknown thread → new session + project dir; known thread → continue that session |
| Space | @mention, main conversation | New session in that message's (new) thread |
| Space/other | non-mention | Ignored |
| Space | slash command | Dispatched (see below) |

### Slash commands

| Command | Phase | Behavior |
|---|---|---|
| `/resume` / `/resume <n>` | MVP | List **unbound** sessions (~10 most recent across all projects); bind this thread to the chosen one |
| `/abort` | 2 | Abort the running agent (RPC `abort`) |
| `/status` | 2 | Session id, cwd, model, running/idle, file |
| `/delete` | 2 | Kill → remove worktrees (+prune branches) → delete session file + row → delete project dir if empty (double-confirm) |
| `/help` | 2 | List commands |

`/resume` enumeration: daemon reads session JSONL files under `~/.pi/agent/sessions/` directly (header line for `cwd`/`id`/`timestamp`; title = `session_info` name, else first user message).

`/delete` details:
1. Abort + terminate the RPC subprocess if running.
2. For each dir under `<projectRoot>/repos/*`: resolve main repo via `git rev-parse --git-common-dir`, `git worktree remove --force <path>` from that repo, then prune the `pi/<date>-<slug>` branch.
3. Delete the session JSONL and the `threads` row.
4. Delete the project dir only if empty; otherwise leave it and report.
5. `/delete` asks for a second `/delete` to confirm.

---

## Session & subprocess lifecycle

- **Spawn** lazily on first @mention in a thread (or per DM message for one-shots). Keep warm between messages (spawning is slow — model/auth/extension init).
- **Idle timeout:** 15 min without activity → kill subprocess, `pid = NULL, status = idle`.
- **Concurrency cap:** 10 simultaneous sessions; beyond that reply "N sessions already running, try later."
- **Restart recovery:** on daemon startup, mark all `running` rows `dead` (stale pids). Next message in a thread whose pid is dead → respawn `pi --mode rpc --session <file>` (pi resumes from the last auto-saved state).
- DM one-shots: spawn per message, stream to `agent_end`, then exit.

---

## Output rendering (pi → Chat)

- On prompt receipt: post a **placeholder** message ("Working…") in the thread.
- During the run: **patch that one message in place** (`spaces.messages.patch`) at coarse boundaries (turn end, tool start/end, compaction), rate-limited to `progress_update_interval_ms`. Never per-token.
- On `agent_end`: post the **final answer** as a new message + footer with the session id (for later `/resume`).
- Truncate to Chat's ~4 KB text limit at a block boundary; full output lives in the session JSONL.
- **Markdown → Chat dialect** conversion applied to all rendered text:

| Source (LLM markdown) | Chat text |
|---|---|
| `**bold**` | `*bold*` |
| `*italic*` / `_italic_` | `_italic_` |
| `~~strike~~` | `~strike~` |
| `` `code` `` | `` `code` `` |
| ``` fenced ``` | ``` fenced ``` |
| `- item` | `* item` |
| `1. item` | `1. item` |
| `[label](url)` | `<url\|label>` |
| `# Header` | `*Header*` |

Tool activity rendered as terse lines: `✓ bash: npm test` / `✗ bash: npm test (exit 1)`.

---

## Security (feature #3 — defense in depth)

1. **Console:** Chat app installable only by the owner's account/domain.
2. **Transport:** Pub/Sub subscription readable only by the service account (GCP IAM).
3. **Per-event:** reject unless `event.user.email` and `event.message.sender.email` ∈ `allowedEmails`.
4. **Space:** if `allowed_spaces` set, reject events from other spaces.

Outbound: app auth (service account); replies attributed to the bot.

---

## Message flow (example)

**New session in a space thread:**
1. You post a top-level message mentioning the bot → Pub/Sub event.
2. Daemon verifies sender + space; `thread.name` unknown.
3. Daemon creates `~/work/<date>-<slug>/`, renders `injected_prompt`, spawns `pi --mode rpc --name <slug> --append-system-prompt <rendered>` (cwd = project dir).
4. Inserts `threads` row (`running`).
5. Daemon posts "Working…" and forwards the prompt (mention stripped) via RPC `prompt`.
6. RPC events stream → daemon patches the progress message (rate-limited).
7. `agent_end` → final answer posted; row updated (`idle`).

**Resume an external session:**
1. In a thread: `/resume` → daemon lists unbound sessions.
2. `/resume 3` → daemon reads that session's header (cwd, id), spawns `pi --mode rpc --session <file>` with that cwd, binds `threads` row. No injection.

---

## Build phasing

- **Phase 0 — Google plumbing spike (riskiest unknown, first):** GCP project, Chat app, Pub/Sub topic + pull subscription, service account; a tiny Go probe pulls one event and posts one reply. Nothing else until this round-trips.
- **Phase 1 — MVP (all four core features):**
  - TOML config, SQLite, allowlists.
  - DM one-shot; Space @mention start/continue.
  - Project-dir creation + injected worktree convention.
  - Progress message + final answer; Markdown→Chat formatting.
  - `/resume` (unbound sessions).
  - Idle timeout, cap = 10, systemd `--user` service (`~/.config/systemd/user/pi-gchat.service`, `Restart=on-failure`, `loginctl enable-linger`), restart recovery. tmux window for dev.
- **Phase 2 — remaining commands:** `/delete`, `/abort`, `/status`, `/help`.
- **Phase 3 — polish:** button-cards for `/resume` picker, `/model`, `/projects`, image passthrough, richer status cards.

---

## Verified assumptions

- `pi --mode rpc --append-system-prompt "<text>"` is accepted and flows into the session system prompt (checked against the installed package; `get_state` round-trips cleanly).
- RPC framing is strict JSONL, LF-only; use `bufio.Scanner` with a raised buffer (tool output lines can be large).
- Sessions are JSONL under `~/.pi/agent/sessions/--<cwd>--/…`; header carries `cwd`/`id`/`timestamp`; `session_info` entries carry the display name.
- Google Chat: events can be delivered via Pub/Sub; async replies via `spaces.messages.create` with `thread.name`; messages carry `thread.name` in both spaces and DMs (DM inline threading rolled out Nov 2025).
