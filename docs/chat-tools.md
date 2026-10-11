# Read-only chat tools

`[chat_tools] enabled = true` opts newly created Pi sessions into a small, read-only Slack tool set. The default is off. It exposes `chat_read_thread`, `chat_read_message`, and `chat_get_user`; it does not expose search, arbitrary Slack API calls, or writes.

## How access is bound

Each new tool-enabled session receives a versioned pi-chat TypeScript extension through `--extension`. pi-gateway persists that spawn argument, so its Pi process can reload the extension after hibernation or restart. At creation pi-chat verifies the gateway advertises durable `spawn_config`, checks that the session catalog records the installed extension path, checks the live extension health marker, then commits the thread row and a separate session-path binding in one SQLite transaction before sending the first prompt. A pending create recovered after a lost reply passes the same checks before it is bound.

The binding records only the canonical session path, Slack provider/surface/workspace, originating channel/thread, read capabilities, extension version and active/revoked state. It contains no credentials or chat content. Every call resolves the current Pi session path against that table; a missing, revoked or unsupported-version binding is denied. Provider, workspace, credential surface and allowed-channel policy come from the stored binding and daemon configuration, never tool arguments.

The extension makes bounded HTTP requests only over the private Unix-domain socket. The socket parent and extension directory are mode `0700`, the socket and extension file are `0600`, and pi-chatd rejects replaceable path ancestors or a socket already owned by another live listener. The extension contains no Slack credentials and cannot call Slack directly. Pi, pi-chatd and pi-gatewayd must share a host and OS identity. This local boundary does not protect against arbitrary code already running as that user.

## Inputs and bounds

Slack references may be a workspace permalink or a `CHANNEL_ID:THREAD_TS` reference; `chat_read_message` also accepts `CHANNEL_ID:THREAD_TS:MESSAGE_TS`. Permalink hosts must match the authenticated workspace. User references are Slack user IDs or `<@USER_ID>` mentions. The provider rejects unsupported shapes, foreign workspace links, disallowed channels and items Slack says are inaccessible.

Calls read one Slack page only, with configurable limits of at most 50 messages and 16 KiB of result text by default. The default deadline is 15 seconds. Request bodies are capped at 32 KiB, with per-session and per-app rate limits. Profiles contain only a display name and title; email and other profile fields are not returned. Results mark chat/profile content as untrusted data. Message and profile contents, arguments, and tokens are not logged.

Pi session files normally persist tool results. Anyone else attached to the same Pi session can see them, and—because tools are session-wide—can invoke the loaded extension too. A user allowlist does not narrow the Slack app's own visibility; configure `slack.access.allowed_channels` for that policy.

## Resume, deletion and opt-out

Bindings survive daemon restart and are keyed separately from thread ownership so retired sessions can still be resumed by their originating thread. Deleting a session or receiving a definitive gateway deletion revokes its binding. Reusing an active session from a different origin is not silently rebound.

Existing or externally adopted sessions without a pi-chat binding remain usable as normal Pi sessions, but have no chat-tool authority; root `/pi resume` says so and points to `@pi /new` for a new tool-enabled session. Disabling the feature stops the broker; any already-running extension then fails closed with a broker-unavailable tool error. Re-enabling it restores access only to sessions with an active binding and a matching extension version.

## Configuration

See `[chat_tools]` in [`pi-chat.toml.example`](../pi-chat.toml.example). The daemon installs the embedded extension atomically at `extension_path` on startup and creates the broker socket at `socket_path`. Keep both paths stable while tool-enabled sessions exist: the gateway persists the extension path, and a warm Pi process has the socket path embedded in its loaded extension. Recreate/restart those sessions after changing either path. The configured gateway must be reachable at startup and advertise the `spawn_config` feature; otherwise enabling chat tools is a startup error.
