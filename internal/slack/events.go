package slack

import (
	"encoding/json"
	"strings"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

// ParseMessage turns a Socket Mode envelope into a core message. It reports
// false for everything phase 0 does not answer: non-event envelopes, other
// event types, the bot's own posts, and messages carrying nothing to act on.
//
// botUserID is the bot's own member ID, from auth.test.
func ParseMessage(env Envelope, botUserID string) (bot.Message, bool) {
	if env.Type != "events_api" {
		return bot.Message{}, false
	}

	var callback struct {
		TeamID  string `json:"team_id"`
		EventID string `json:"event_id"`
		Event   struct {
			Type     string `json:"type"`
			Subtype  string `json:"subtype"`
			BotID    string `json:"bot_id"`
			User     string `json:"user"`
			Text     string `json:"text"`
			TS       string `json:"ts"`
			ThreadTS string `json:"thread_ts"`
			Channel  string `json:"channel"`
			Files    []struct {
				ID string `json:"id"`
			} `json:"files"`
		} `json:"event"`
	}
	if err := json.Unmarshal(env.Payload, &callback); err != nil {
		return bot.Message{}, false
	}

	event := callback.Event
	switch {
	case event.Type != "app_mention":
		return bot.Message{}, false
	case event.BotID != "" || event.Subtype != "":
		// Our own message, or an edit of one.
		return bot.Message{}, false
	case event.User == "", event.Channel == "", event.TS == "":
		return bot.Message{}, false
	}

	// A mention at the top of a channel starts the thread pi-chat answers in;
	// a mention inside a thread joins that thread's session (DESIGN.md §7).
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
		UserID: event.User,
		TeamID: callback.TeamID,
		Text:   strings.TrimSpace(stripMention(event.Text, botUserID)),
		Files:  len(event.Files),
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
