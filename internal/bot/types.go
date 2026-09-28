package bot

import (
	"context"
	"strings"
)

// platformName is the platform this build speaks. It names the sessions
// pi-chat creates and tags its connections with; it is the core's only
// Slack-specific value, and a second adapter moves it into configuration
// (DESIGN.md §12).
const platformName = "slack"

// Thread identifies one conversation. A session is always thread-scoped, so a
// thread is also a session's identity (DESIGN.md §4, §7).
type Thread struct {
	Workspace string // workspace (team) ID, e.g. T6K8Y3FRR
	Channel   string // channel, private group, or DM ID
	ThreadTS  string // timestamp of the thread's root message
}

// Key is the thread's stable identity, and the key pi-chat persists state
// under.
func (t Thread) Key() string {
	return t.Workspace + ":" + t.Channel + ":" + t.ThreadTS
}

// SessionName is the deterministic pi session name for the thread
// (DESIGN.md §4): the same thread always maps to the same session, so a lost
// database can be rebuilt by scanning the catalog.
func (t Thread) SessionName() string {
	return platformName + "-" + slugName(t.Workspace) + "-" + slugName(t.Channel) + "-" + slugName(t.ThreadTS)
}

// slugName reduces a platform identifier to the alphabet session names are
// happiest with: a Slack timestamp's dot becomes a dash.
func slugName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// Message is one inbound message that addresses the bot: a mention, or plain
// text in a thread the bot already owns.
type Message struct {
	EventID string // platform event ID, used to drop redeliveries
	Thread  Thread
	UserID  string // the human who sent it
	// TS is the platform's identifier for this particular message, which is what
	// the observation watermark is compared against. It differs from
	// Thread.ThreadTS for a reply, and equals it for the message that started the
	// thread.
	TS string
	// Workspace is the workspace (Slack team) ID. It is the same identifier
	// Thread.Workspace carries, and streaming to a channel needs it.
	Workspace string
	Text      string // the prompt, with the bot's own mention removed
	Files     int    // attachments

	// Mentioned records that the sender addressed the bot by name. It is what
	// separates `@pi status` (which earns a "did you mean @pi /status?") from
	// the same word typed inside a conversation, where it is just a word.
	Mentioned bool

	// Direct records that the message arrived in a one-to-one (or group) DM
	// rather than in a channel. A DM thread needs no mention: the conversation
	// is already the address (DESIGN.md §5).
	Direct bool
}

// Command is one inbound command: a slash command at a channel or DM root, or
// `@pi /<command>` inside a thread (DESIGN.md §5). A root command has no
// Thread, and is therefore session-less.
type Command struct {
	EventID   string
	Channel   string
	Thread    *Thread // nil at a channel or DM root
	UserID    string
	Workspace string

	// Text is the command as written, slash included: "/status", "/skill:x".
	// Adapters normalize their platform's shape into this one form — Slack's
	// slash-command payload omits the slash, and its message payloads carry
	// it — so the core has one grammar to parse.
	Text string

	// ReplyTo is an adapter-owned handle for answering this particular
	// interaction without posting a new message (Slack's slash-command
	// response_url). It is opaque to the core and may be empty.
	ReplyTo string
}

// Action is one press of a button the bot posted.
type Action struct {
	EventID   string
	Channel   string
	Thread    *Thread
	UserID    string
	Workspace string
	ActionID  string // which button, as the core named it
	Value     string // the button's payload, which comes back through the platform
	// MessageTS is the message the button was attached to, so the bot can
	// replace it instead of leaving a stale picker behind.
	MessageTS string
	ReplyTo   string
}

// Said is one message of a thread's conversation: something said by somebody
// other than the agent. What was said between two of the agent's turns rides in
// front of the next prompt, so a session can follow a conversation it is only
// occasionally addressed in (DESIGN.md §4).
type Said struct {
	TS     string // the platform's message id, which orders the transcript
	UserID string // who said it, empty for a platform that has no identity to give
	Name   string // display name, empty when the platform cannot resolve one
	Text   string
	// FromBot marks pi-chat's own message. The core leaves them out of a
	// transcript — the agent has them in its own history — and only the adapter
	// can tell which messages are its own.
	FromBot bool
}

// Observer is implemented by a platform that can read a thread's conversation
// back. The core asks for everything said after the watermark it remembers and
// folds the answer into the next prompt; a platform that cannot answer is simply
// not asked (DESIGN.md §4).
//
// Pagination and sanity caps are the platform's business; what reaches a prompt
// is bounded by the core.
type Observer interface {
	Conversation(ctx context.Context, t Thread, oldest string) ([]Said, error)
}

// Renderer renders one turn into its thread. A renderer is used by a single
// goroutine, but its methods may be called from different ones over a turn's
// life, so implementations must be safe for that.
type Renderer interface {
	// Start creates the reply: a streaming message, or a placeholder to patch.
	Start(ctx context.Context) error
	// Delta appends text pi has produced since the last call, in order.
	Delta(ctx context.Context, text string) error
	// Finish presents the authoritative final answer, which supersedes
	// whatever was streamed.
	Finish(ctx context.Context, final string) error
	// Fail presents a failure in place of an answer.
	Fail(ctx context.Context, cause error) error
}

// ProgressReporter is implemented by a renderer that can name the message it
// is writing into. The core persists that handle so a restart can recognize
// the reply it left half-written (DESIGN.md §7); a renderer that cannot name
// one is simply not asked.
type ProgressReporter interface {
	Progress() string
}

// Status is a thread session's lifecycle state, in the vocabulary the core
// reasons about. Mapping it onto a platform's own states is the adapter's job:
// a platform that cannot show one simply does not implement StatusReporter.
type Status string

const (
	// StatusBusy: a turn is running. This is what a platform shows as a loading
	// indicator.
	StatusBusy Status = "busy"
	// StatusWaiting: the turn is blocked until a human answers pi.
	StatusWaiting Status = "waiting"
	// StatusIdle: the session is attached or resumable, with nothing running.
	StatusIdle Status = "idle"
	// StatusClosed: the session is gone.
	StatusClosed Status = "closed"
)

// StatusReporter is implemented by a platform that can display a session's
// state outside the conversation (DESIGN.md §6).
//
// It is best-effort by construction: a status is a nicety, never a
// precondition, so the core logs a failure and carries on. A platform that has
// nowhere to show one — or an install that is not permitted to — is asked once
// and not again (the adapter remembers its own refusal).
type StatusReporter interface {
	SetStatus(ctx context.Context, t Thread, s Status) error
}

// Suggestion is one prompt a platform offers before the user types: a title for
// the choice, and the message choosing it sends.
type Suggestion struct {
	Title   string
	Message string
}

// PromptReporter is implemented by a platform that can show suggestions in its
// own UI. Slack's agent surface shows up to four at the top of the Messages tab.
//
// They are per conversation, not per thread: the agent experience moved
// suggestions out of threads and into the Messages tab, so a channel is the
// whole address (DESIGN.md §6).
type PromptReporter interface {
	SetSuggestedPrompts(ctx context.Context, channel, title string, suggestions []Suggestion) error
}

// Opened reports that a user opened the bot's own conversation. It is the moment
// to offer suggestions built from this deployment rather than from a list
// hard-coded in a manifest (DESIGN.md §6), and the reason `app_home_opened` is
// subscribed at all.
type Opened struct {
	EventID   string
	Channel   string
	UserID    string
	Workspace string
}

// Notice is a message the core posts outside a turn: a command answer, a
// refusal, a hint, or a picker.
//
// It is deliberately plain — text and buttons — because every chat platform
// has both, and because the alternative (letting the core build blocks) would
// put one platform's presentation language in the platform-independent half.
type Notice struct {
	// Thread is where the notice belongs. A nil Thread means the channel root,
	// which Channel then names.
	Thread  *Thread
	Channel string
	// Ephemeral notices are visible only to UserID, which is what refusals and
	// hints should be: a stranger typing the wrong thing should not make noise
	// in someone else's channel.
	Ephemeral bool
	UserID    string
	// ReplyTo answers one specific inbound interaction (see Command.ReplyTo).
	ReplyTo string
	// Update replaces the message with this timestamp instead of posting a new
	// one. It is an adapter-owned handle, like ReplyTo.
	Update string
	Text   string
	// Buttons, when present, are the choices attached to the text.
	Buttons []Button
}

// Button is one interactive choice on a Notice.
type Button struct {
	ActionID string // returned as Action.ActionID
	Text     string
	Value    string
	Style    string // "", "primary" or "danger"
}

// Platform is the adapter half of the core/adapter split: it turns core
// intents into one chat platform's API calls (DESIGN.md §12).
type Platform interface {
	// StartTurn builds a renderer for a reply to m. It must not have a side
	// effect on the platform: the core calls Start on the first turn.
	StartTurn(ctx context.Context, m Message) (Renderer, error)
	// Post posts a notice.
	Post(ctx context.Context, n Notice) error
	// OpenThread posts a top-level message in a channel and returns the thread
	// it starts, whose root is the new message. The core needs this when a
	// root command has no message of its own to answer under: adopting a
	// session with /pi resume continues in a thread, and that thread has to
	// begin somewhere.
	OpenThread(ctx context.Context, channel, text string) (Thread, error)
}

// mentionHelp answers a bare mention: the user introduced the bot without
// asking it anything.
const mentionHelp = "Mention me with a prompt, for example “@pi fix the failing test”. " +
	"`@pi /help` lists the commands."

// trimPrompt reduces a prompt to what a chat platform can hold, so a
// pathological message cannot be rejected at the gateway.
func trimPrompt(s string) string {
	const max = 32000
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "…"
}
