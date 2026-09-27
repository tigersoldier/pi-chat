package bot

import (
	"context"
	"strings"
)

// Thread identifies one conversation. Sessions are thread-scoped, so a thread
// is also a session's identity (DESIGN.md §7).
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
// (DESIGN.md §10): the same thread always maps to the same session, so a
// restart reattaches instead of creating a duplicate.
func (t Thread) SessionName() string {
	return "slack-" + slugName(t.Workspace) + "-" + slugName(t.Channel) + "-" + slugName(t.ThreadTS)
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

// Message is one inbound message that addresses the bot.
type Message struct {
	EventID string // platform event ID, used to drop redeliveries
	Thread  Thread
	UserID  string // the human who sent it
	TeamID  string // workspace ID, needed when streaming to a channel
	Text    string // the prompt, with the bot's own mention removed
	Files   int    // attachments, which phase 0 cannot hand to pi yet
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

// Platform is the adapter half of the core/adapter split: it turns core
// intents into one chat platform's API calls.
type Platform interface {
	// StartTurn builds a renderer for a reply to m. It must not have a side
	// effect on the platform: the core calls Start on the first turn.
	StartTurn(ctx context.Context, m Message) (Renderer, error)
}

// helpText answers a mention that carries no prompt of its own.
const helpText = "Mention me with a prompt, for example: `@pi what does this repository do?`"

// trimPrompt reduces a prompt to what a chat platform can hold, so a
// pathological message cannot be rejected at the gateway.
func trimPrompt(s string) string {
	const max = 32000
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "…"
}
