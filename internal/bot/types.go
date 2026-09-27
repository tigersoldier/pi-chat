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
