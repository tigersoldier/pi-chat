package slack

import (
	"context"
	"errors"
	"strings"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

const (
	approvalViewCallbackID = "pi_chat_approval"
	approvalModalBlockID   = "pi_chat_approval_input"

	approvalActionsPerBlock = 25
	approvalMaxBlocks       = 50
	approvalPromptBlocks    = 1
	approvalCancelButtons   = 1
	approvalMaxChoices      = (approvalMaxBlocks-approvalPromptBlocks)*approvalActionsPerBlock - approvalCancelButtons
)

// PresentApproval posts a thread-scoped prompt and returns the message
// timestamp used to retire its buttons after the user responds.
func (p *Platform) PresentApproval(ctx context.Context, thread bot.Thread, request bot.ApprovalRequest, buttons []bot.Button) (string, error) {
	if thread.Channel == "" || thread.ThreadTS == "" {
		return "", errors.New("slack: an approval prompt needs a thread")
	}
	fallback := "Pi is waiting for an answer."
	return p.api.PostBlocks(ctx, thread.Channel, thread.ThreadTS, fallback, approvalBlocksFor(request, buttons))
}

// OpenApprovalModal opens the input/editor form from the short-lived handle
// the adapter received with the Respond button interaction.
func (p *Platform) OpenApprovalModal(ctx context.Context, _ bot.Thread, handle, routeID string, request bot.ApprovalRequest) error {
	if handle == "" || routeID == "" {
		return errors.New("slack: an approval modal needs a trigger and dialog id")
	}
	title := request.Title
	if title == "" {
		title = "Pi needs a response"
	}
	title = truncateRunes(title, 24)
	placeholder := request.Placeholder
	if placeholder == "" {
		placeholder = "Type your response"
	}
	placeholder = truncateRunes(placeholder, 150)
	prompt := strings.TrimSpace(request.Message)
	if prompt != "" {
		prompt = truncateRunes(prompt, 3000)
	}
	blocks := make([]block, 0, 2)
	if prompt != "" {
		blocks = append(blocks, block{
			"type": "section",
			"text": block{"type": "plain_text", "text": prompt},
		})
	}
	input := block{
		"type":        "plain_text_input",
		"action_id":   "value",
		"multiline":   request.Method == "editor",
		"placeholder": block{"type": "plain_text", "text": placeholder},
	}
	if request.Prefill != "" {
		input["initial_value"] = truncateRunes(request.Prefill, 3000)
	}
	blocks = append(blocks, block{
		"type":     "input",
		"block_id": approvalModalBlockID,
		"label":    block{"type": "plain_text", "text": "Your response"},
		"element":  input,
	})
	view := map[string]any{
		"type":             "modal",
		"callback_id":      approvalViewCallbackID,
		"private_metadata": routeID,
		"notify_on_close":  true,
		"title":            block{"type": "plain_text", "text": title},
		"submit":           block{"type": "plain_text", "text": "Submit"},
		"close":            block{"type": "plain_text", "text": "Cancel"},
		"blocks":           blocks,
	}
	return p.api.OpenView(ctx, handle, view)
}

// MaxApprovalChoices returns the maximum choices that fit alongside one prompt
// block and the reserved Cancel button within Slack's message block budget.
func (p *Platform) MaxApprovalChoices() int { return approvalMaxChoices }

func approvalBlocksFor(request bot.ApprovalRequest, buttons []bot.Button) []block {
	text := strings.TrimSpace(request.Title)
	if request.Message != "" {
		if text != "" {
			text += "\n\n"
		}
		text += request.Message
	}
	text = truncateRunes(text, 3000)
	if text == "" {
		text = "Pi is waiting for an answer."
	}
	blocks := []block{{
		"type": "section",
		"text": block{"type": "plain_text", "text": text},
	}}
	for start := 0; start < len(buttons); start += approvalActionsPerBlock {
		end := min(start+approvalActionsPerBlock, len(buttons))
		elements := make([]block, 0, end-start)
		for i, button := range buttons[start:end] {
			element := block{
				"type":      "button",
				"text":      block{"type": "plain_text", "text": truncateRunes(button.Text, 75)},
				"action_id": wireActionID(button.ActionID, i),
				"value":     button.Value,
			}
			if button.Style != "" {
				element["style"] = button.Style
			}
			elements = append(elements, element)
		}
		blocks = append(blocks, block{"type": "actions", "elements": elements})
	}
	return blocks
}

func truncateRunes(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	count := 0
	for at := range value {
		if count == maxRunes {
			return value[:at]
		}
		count++
	}
	return value
}
