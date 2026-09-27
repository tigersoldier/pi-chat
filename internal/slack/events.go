package slack

import (
	"encoding/json"
	"strings"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

// parseMessage turns one Socket Mode event envelope into a core message. It
// reports false for everything pi-chat does not treat as a message: other
// event types, the bot's own posts, edits and joins, and messages that do not
// address the bot at all.
//
// What counts as addressing it:
//
//   - a mention, anywhere (`@pi …`);
//   - plain text in a DM thread, where the conversation is already the address;
//   - plain text in a channel thread, which the core accepts only for threads
//     it already owns — the bot sits in busy channels, and a stray reply must
//     not start a session.
//
// A bare message in a channel or DM *root* is not a prompt: roots are
// session-less by design (DESIGN.md §4), which is what keeps `@pi hello` in a
// channel from making every later message in it a prompt.
func parseMessage(env Envelope, botUserID string) (bot.Message, bool) {
	if env.Type != "events_api" {
		return bot.Message{}, false
	}

	var callback struct {
		TeamID  string `json:"team_id"`
		EventID string `json:"event_id"`
		Event   struct {
			Type        string `json:"type"`
			Subtype     string `json:"subtype"`
			BotID       string `json:"bot_id"`
			User        string `json:"user"`
			Text        string `json:"text"`
			TS          string `json:"ts"`
			ThreadTS    string `json:"thread_ts"`
			Channel     string `json:"channel"`
			ChannelType string `json:"channel_type"`
			Files       []struct {
				ID string `json:"id"`
			} `json:"files"`
		} `json:"event"`
	}
	if err := json.Unmarshal(env.Payload, &callback); err != nil {
		return bot.Message{}, false
	}

	event := callback.Event
	switch {
	case event.Type != "app_mention" && event.Type != "message":
		return bot.Message{}, false
	case event.BotID != "" || event.Subtype != "":
		// Our own post, an edit, a channel join: none of them is a prompt.
		return bot.Message{}, false
	case event.User == "", event.Channel == "", event.TS == "":
		return bot.Message{}, false
	}

	// Slack renders a mention as <@U123>. Removing ours is also how we learn
	// that we were addressed: a mention event may arrive for a message we were
	// mentioned in alongside others.
	text := stripMention(event.Text, botUserID)
	mentioned := event.Type == "app_mention" || text != event.Text
	direct := event.ChannelType == "im" || event.ChannelType == "mpim"

	switch {
	case mentioned:
	case direct && event.ThreadTS != "":
	case event.ThreadTS != "":
	default:
		return bot.Message{}, false
	}

	// A mention at the top of a channel, or in a DM root, starts the thread
	// pi-chat answers in; the mention's own timestamp roots it. A mention
	// inside a thread joins that thread's session (DESIGN.md §4).
	threadTS := event.ThreadTS
	if threadTS == "" {
		threadTS = event.TS
	}

	return bot.Message{
		EventID: callback.EventID,
		Thread: bot.Thread{
			Workspace: callback.TeamID,
			Channel:   event.Channel,
			ThreadTS:  threadTS,
		},
		UserID:    event.User,
		Workspace: callback.TeamID,
		Text:      strings.TrimSpace(text),
		Mentioned: mentioned,
		Direct:    direct,
		Files:     len(event.Files),
	}, true
}

// parseCommand turns a Socket Mode slash-command envelope into a core command.
//
// The two quirks of this payload shape are why the core takes a normalized
// form: it carries no thread timestamp (so a root command is necessarily
// session-less) and its text omits the slash the user typed.
func parseCommand(env Envelope) (bot.Command, bool) {
	if env.Type != "slash_commands" {
		return bot.Command{}, false
	}
	var payload struct {
		TeamID      string `json:"team_id"`
		ChannelID   string `json:"channel_id"`
		UserID      string `json:"user_id"`
		Text        string `json:"text"`
		ResponseURL string `json:"response_url"`
	}
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return bot.Command{}, false
	}
	if payload.ChannelID == "" || payload.UserID == "" {
		return bot.Command{}, false
	}
	return bot.Command{
		// A slash-command payload has no event id, so the envelope is the only
		// thing to dedupe on. Slack only redelivers an envelope it did not see
		// acknowledged, which is a narrower guarantee than event_id — and the
		// widest one this payload shape offers.
		EventID:   env.EnvelopeID,
		Channel:   payload.ChannelID,
		UserID:    payload.UserID,
		Workspace: payload.TeamID,
		Text:      "/" + strings.TrimSpace(payload.Text),
		ReplyTo:   payload.ResponseURL,
	}, true
}

// parseAction turns a Socket Mode interactive envelope (a button press) into a
// core action.
func parseAction(env Envelope) (bot.Action, bool) {
	if env.Type != "interactive" {
		return bot.Action{}, false
	}
	var payload struct {
		Type string `json:"type"`
		Team struct {
			ID string `json:"id"`
		} `json:"team"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
		Message struct {
			TS       string `json:"ts"`
			ThreadTS string `json:"thread_ts"`
		} `json:"message"`
		ResponseURL string `json:"response_url"`
		Actions     []struct {
			ActionID string `json:"action_id"`
			Value    string `json:"value"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return bot.Action{}, false
	}
	if payload.Type != "block_actions" || len(payload.Actions) == 0 {
		return bot.Action{}, false
	}
	if payload.User.ID == "" || payload.Channel.ID == "" {
		return bot.Action{}, false
	}

	// Buttons are pressed on a message, and that message may be in a thread —
	// which is where an answer belongs, and which a later approval button will
	// depend on.
	var thread *bot.Thread
	if payload.Message.ThreadTS != "" {
		thread = &bot.Thread{
			Workspace: payload.Team.ID,
			Channel:   payload.Channel.ID,
			ThreadTS:  payload.Message.ThreadTS,
		}
	}
	return bot.Action{
		EventID:   env.EnvelopeID,
		Channel:   payload.Channel.ID,
		Thread:    thread,
		UserID:    payload.User.ID,
		Workspace: payload.Team.ID,
		ActionID:  payload.Actions[0].ActionID,
		Value:     payload.Actions[0].Value,
		MessageTS: payload.Message.TS,
		ReplyTo:   payload.ResponseURL,
	}, true
}

// parseStopped turns Slack's agent stop button into a core action: the user
// pressed stop while a turn was `processing`, and Slack sends
// `agent_session_stopped` for it.
//
// It becomes an Action rather than a fourth kind of ingress because that is the
// path where the allowlist, the thread lookup and the dedupe already live — and
// a stop that skipped the allowlist would be a way to interrupt other people's
// work.
//
// The event also carries `streaming_message_ts`, the streams Slack has already
// stopped. Nothing here needs it: a later append to a stopped stream fails, and
// the renderer already answers an append failure by switching to message
// updates, which is how the partial answer still gets its final text.
func parseStopped(env Envelope) (bot.Action, bool) {
	if env.Type != "events_api" {
		return bot.Action{}, false
	}
	var callback struct {
		TeamID string `json:"team_id"`
		Event  struct {
			Type     string `json:"type"`
			Channel  string `json:"channel"`
			ThreadTS string `json:"thread_ts"`
			User     string `json:"user"`
		} `json:"event"`
	}
	if err := json.Unmarshal(env.Payload, &callback); err != nil {
		return bot.Action{}, false
	}
	// A stop without a thread is one pi-chat cannot route: the thread is the
	// session's whole identity.
	if callback.Event.Type != "agent_session_stopped" || callback.Event.Channel == "" || callback.Event.ThreadTS == "" {
		return bot.Action{}, false
	}
	thread := bot.Thread{
		Workspace: callback.TeamID,
		Channel:   callback.Event.Channel,
		ThreadTS:  callback.Event.ThreadTS,
	}
	return bot.Action{
		EventID:   env.EnvelopeID,
		Channel:   callback.Event.Channel,
		Thread:    &thread,
		UserID:    callback.Event.User,
		Workspace: callback.TeamID,
		ActionID:  bot.ActionStop,
	}, true
}

// stripMention removes mentions of the bot itself from message text, leaving
// the prompt. Slack renders a mention as <@U123>, and older payloads use the
// labelled form <@U123|pi>; mentions of other people are left alone.
func stripMention(text, botUserID string) string {
	if botUserID == "" {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		if strings.HasPrefix(text[i:], "<@") {
			if end := strings.IndexByte(text[i:], '>'); end > 0 {
				id := text[i+2 : i+end]
				if labelled := strings.IndexByte(id, '|'); labelled >= 0 {
					id = id[:labelled]
				}
				if id == botUserID {
					i += end + 1
					continue
				}
			}
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}
