package slack

import (
	"strconv"
	"strings"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

// block is one Block Kit block. Nothing outside this package sees Block Kit:
// the core's Notice carries text and buttons, and this file is where those
// become Slack's JSON.
type block map[string]any

// buttonLimit is Slack's cap on the elements of one actions block, and therefore
// the number of sessions /pi resume may offer (bot.resumeLimit). The two must not
// drift: the adapter drops the buttons past its cap, so a longer list would lose
// its tail without an error.
const buttonLimit = 5

// blocksFor renders a notice's buttons, or nothing when it has none: an empty
// actions block is rejected, and a notice usually is text alone.
func blocksFor(buttons []bot.Button) []block {
	if len(buttons) == 0 {
		return nil
	}
	elements := make([]block, 0, len(buttons))
	for i, button := range buttons {
		if len(elements) == buttonLimit {
			break
		}
		element := block{
			"type":      "button",
			"text":      block{"type": "plain_text", "text": button.Text},
			"action_id": wireActionID(button.ActionID, i),
			"value":     button.Value,
		}
		if button.Style != "" {
			element["style"] = button.Style
		}
		elements = append(elements, element)
	}
	return []block{{"type": "actions", "elements": elements}}
}

// wireActionID makes an action id unique within the block it is rendered into.
//
// Slack refuses the whole message with `invalid_blocks` when two buttons in one
// actions block share an action_id, and a picker is exactly that shape: several
// buttons that mean the same thing, each carrying its own choice in its value.
// Measured against the live API while fixing /pi resume, whose picker was
// rejected the minute it had two sessions to offer: two buttons with one id are
// refused, the same two with distinct ids are accepted, and one id repeated
// across two separate blocks is accepted as well.
//
// The core names a button by what it means, so the position in the row is added
// here and taken off again by coreActionID when a press comes back.
func wireActionID(actionID string, index int) string {
	return actionID + ":" + strconv.Itoa(index)
}

// coreActionID undoes wireActionID: the suffix it adds is a colon and digits,
// and an action id of the core's own may contain a colon, so only a trailing
// `:<digits>` is removed and anything else is left alone.
func coreActionID(actionID string) string {
	at := strings.LastIndex(actionID, ":")
	if at < 0 || at == len(actionID)-1 {
		return actionID
	}
	for _, r := range actionID[at+1:] {
		if r < '0' || r > '9' {
			return actionID
		}
	}
	return actionID[:at]
}
