package slack

import (
	"encoding/json"
	"strings"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

// parseMessage turns one Socket Mode event envelope into a core message. It
// reports false for everything pi-chat does not treat as a message: other event
// types, the bot's own posts, edits and joins, and the *delivery* of a message
// that another delivery already owns.
//
// Slack sends both `app_mention` and a `message` event for the same mention, so
// which one answers is decided by where the message was sent:
//
//	DM                message.im owns it. The conversation is the address, so
//	                  every message in it is a request — and Slack does not
//	                  deliver app_mention for a DM at all.
//	channel, group DM app_mention owns it. In a room only a mention turns, and
//	                  app_mention is Slack's own statement that this message
//	                  mentions the bot, rather than our reading of its text. The
//	                  message event carrying the same ts is that same request
//	                  arriving a second time; everything else in a room — plain
//	                  text, a reply in a thread — is conversation, which the next
//	                  turn fetches as context (DESIGN.md §4, §5).
//
// Dropping the other delivery is what makes one message one turn, instead of two
// turns deduped after the fact. The core still claims a turn by the message's
// own identity, so a redelivery — or a platform that sends something else twice
// — is still one turn: that is the net, not the mechanism.
//
// A bare message in a channel *root* is not a prompt: channel roots are
// session-less by design, which is what keeps `@pi hello` in a channel from
// making every later message in it a prompt. A DM root is different — plain text
// there starts a session, rooted at that message (DESIGN.md §4).
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
			Files       []File `json:"files"`
		} `json:"event"`
	}
	if err := json.Unmarshal(env.Payload, &callback); err != nil {
		return bot.Message{}, false
	}

	event := callback.Event
	switch {
	case event.Type != "app_mention" && event.Type != "message":
		return bot.Message{}, false
	case event.BotID != "" || (event.Subtype != "" && event.Subtype != "file_share"):
		// Our own post, an edit, a channel join: none of them is a prompt.
		return bot.Message{}, false
	case event.User == "", event.Channel == "", event.TS == "":
		return bot.Message{}, false
	}

	// Slack renders a mention as <@U123>. Removing ours is also how a DM learns it
	// was addressed: an app_mention event is Slack's own statement, and in a DM the
	// text is the only other place a mention can appear.
	text := stripMention(event.Text, botUserID)
	mentioned := event.Type == "app_mention" || text != event.Text

	// A group DM is not a one-to-one conversation: two or more people are in it
	// and they talk to each other, so it behaves like a channel — only a mention
	// turns (DESIGN.md §5). That is why "direct" is only `im`: it decides both the
	// delivery that owns the message and whether the core needs a mention at all.
	direct := event.ChannelType == "im"

	// One message, one delivery (see above), and in a room the delivery that owns a
	// mention is `app_mention` — Slack's own statement that the bot was mentioned,
	// which is what makes it right to trust for a mention composed in rich text.
	//
	// A `message` event that mentions the bot is the twin of that same request, so it
	// is dropped here. One that does *not* mention the bot is the room's own
	// conversation, and it is handed to the core rather than discarded: the core is
	// the side that knows whether this thread has a session, and somebody typing at
	// a bot in a thread it owns has to be told why nothing happens — which cannot
	// happen in a code path that throws the message away (DESIGN.md §5).
	switch {
	case direct && event.Type == "app_mention":
		// Slack does not send this for a DM; the message event owns it.
		return bot.Message{}, false
	case !direct && event.Type == "app_mention":
		// The delivery that owns a mention in a room.
	case !direct && mentioned:
		// The twin of the mention above, arriving as a message event.
		return bot.Message{}, false
	case !direct && event.ThreadTS == "":
		// Plain text at a channel root. It belongs to no thread, so there is nothing
		// for the core to read it back into and nobody to explain anything to; a
		// thread pi-chat owns always has a root of its own.
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
		TS:        event.TS,
		Text:      strings.TrimSpace(text),
		Mentioned: mentioned,
		Direct:    direct,
		Files:     attachments(event.Files),
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
		ChannelName string `json:"channel_name"`
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
		Direct:    isDirectChannel(payload.ChannelID, payload.ChannelName),
		Text:      "/" + strings.TrimSpace(payload.Text),
		ReplyTo:   payload.ResponseURL,
	}, true
}

// isDirectChannel reports whether a channel is a one-to-one conversation.
//
// Slack names it `directmessage` and ids it with a D. A group DM has neither —
// its name is the generated `mpdm-…` — and that is what the grammar wants, since
// with two or more people in it the conversation is no longer only the bot's
// address (DESIGN.md §5). The name decides; the id is only a fallback for a
// payload that omits the name, and never turns a room into a DM on its own.
func isDirectChannel(id, name string) bool {
	if name != "" {
		return name == "directmessage"
	}
	return strings.HasPrefix(id, "D")
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
			ID   string `json:"id"`
			Name string `json:"name"`
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
		// The press carries the id the adapter rendered, which is unique per
		// button; the core named the button by what it means.
		ActionID: coreActionID(payload.Actions[0].ActionID),
		Value:    payload.Actions[0].Value,
		// Slack names a one-to-one conversation `directmessage` and ids it with a
		// D. A group DM is neither (it behaves like a channel), so both have to
		// agree before the core is told this is a DM.
		Direct:    isDirectChannel(payload.Channel.ID, payload.Channel.Name),
		MessageTS: payload.Message.TS,
		ReplyTo:   payload.ResponseURL,
	}, true
}

// parseOpened reads Slack telling us a user opened the bot's own conversation:
// `app_home_opened` with the Messages tab, the event that replaced
// `assistant_thread_started` in the agent messaging experience.
//
// Only the Messages tab counts. The same event fires for the Home tab, which
// pi-chat publishes no view for, and treating that as a conversation opening
// would offer prompts nobody asked for.
func parseOpened(env Envelope) (bot.Opened, bool) {
	if env.Type != "events_api" {
		return bot.Opened{}, false
	}
	var callback struct {
		TeamID  string `json:"team_id"`
		EventID string `json:"event_id"`
		Event   struct {
			Type    string `json:"type"`
			Tab     string `json:"tab"`
			Channel string `json:"channel"`
			User    string `json:"user"`
		} `json:"event"`
	}
	if err := json.Unmarshal(env.Payload, &callback); err != nil {
		return bot.Opened{}, false
	}
	if callback.Event.Type != "app_home_opened" || callback.Event.Tab != "messages" {
		return bot.Opened{}, false
	}
	if callback.Event.Channel == "" || callback.Event.User == "" {
		return bot.Opened{}, false
	}
	return bot.Opened{
		EventID:   callback.EventID,
		Channel:   callback.Event.Channel,
		UserID:    callback.Event.User,
		Workspace: callback.TeamID,
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
