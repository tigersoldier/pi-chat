package slack

import (
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
	for _, button := range buttons {
		if len(elements) == buttonLimit {
			break
		}
		element := block{
			"type":      "button",
			"text":      block{"type": "plain_text", "text": button.Text},
			"action_id": button.ActionID,
			"value":     button.Value,
		}
		if button.Style != "" {
			element["style"] = button.Style
		}
		elements = append(elements, element)
	}
	return []block{{"type": "actions", "elements": elements}}
}
